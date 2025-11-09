package subscription

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/arloliu/parti/types"
)

// WorkerConsumer manages a single JetStream durable pull consumer per worker for partition-based work distribution.
type WorkerConsumer struct {
	conn   *nats.Conn
	js     jetstream.JetStream
	config WorkerConsumerConfig
	logger types.Logger

	// createOrUpdateFn allows tests to inject a fake CreateOrUpdateConsumer implementation.
	// When nil, defaults to js.CreateOrUpdateConsumer.
	createOrUpdateFn func(ctx context.Context, stream string, cfg jetstream.ConsumerConfig) (jetstream.Consumer, error)

	// Template for subject generation
	subjectTemplate *template.Template

	// State tracking
	mu sync.RWMutex

	// Single-consumer (per worker) mode state
	workerID       string
	workerConsumer jetstream.Consumer
	workerCancel   context.CancelFunc
	workerSubjects []string // last applied subjects (deduped, sorted)
	handler        MessageHandler

	iterFactory func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error)

	// health tracking (internal counters exposed via Health() accessor)
	consecutiveFailures    int // iterator/handler consecutive failures
	consecutiveRecreations int // consecutive recreation failures
	lastError              error
	lastUpdate             time.Time
}

// loopCtrl guides the outer pull loop control flow.
type loopCtrl int

const (
	ctrlContinue loopCtrl = iota
	ctrlRetry
	ctrlExit
)

// subjectContext is the template context for subject generation.
type subjectContext struct {
	PartitionID string
}

// WorkerConsumerHealth represents basic health telemetry for the helper.
type WorkerConsumerHealth struct {
	ConsecutiveFailures    int  // current consecutive iterator/handler failures
	ConsecutiveRecreations int  // current consecutive recreation failures
	Healthy                bool // derived healthy flag based on threshold
	LastError              error
	LastUpdated            time.Time
}

// NewWorkerConsumer creates a new WorkerConsumer using a pre-initialized JetStream context.
//
// This overload enables looser coupling to the nats client by accepting the
// jetstream.JetStream interface instead of a concrete *nats.Conn. The underlying
// connection is captured via js.Conn() for internal status/logging when needed.
//
// Parameters:
//   - js: Pre-configured JetStream context (must be non-nil)
//   - cfg: Helper configuration with required fields
//   - handler: Message handler invoked for each received JetStream message
//
// Returns:
//   - *WorkerConsumer: Initialized helper
//   - error: Configuration or template parsing error
func NewWorkerConsumer(js jetstream.JetStream, cfg WorkerConsumerConfig, handler MessageHandler) (*WorkerConsumer, error) {
	if js == nil {
		return nil, errors.New("JetStream context is required")
	}

	// Validate essential cfg fields and handler (reuse same checks as NewWorkerConsumer)
	if cfg.StreamName == "" {
		return nil, errors.New("stream name is required")
	}
	if cfg.ConsumerPrefix == "" {
		return nil, errors.New("consumer prefix is required")
	}
	if cfg.SubjectTemplate == "" {
		return nil, errors.New("subject template is required")
	}
	if handler == nil {
		return nil, errors.New("message handler is required")
	}

	// Apply defaults and parse template
	cfg.applyDefaults()
	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	if err != nil {
		return nil, fmt.Errorf("invalid subject template: %w", err)
	}

	return &WorkerConsumer{
		conn:            js.Conn(),
		js:              js,
		config:          cfg,
		logger:          cfg.Logger,
		subjectTemplate: tmpl,
		workerSubjects:  nil,
		handler:         handler,
		iterFactory:     defaultIterFactory,
		createOrUpdateFn: func(ctx context.Context, stream string, c jetstream.ConsumerConfig) (jetstream.Consumer, error) {
			return js.CreateOrUpdateConsumer(ctx, stream, c)
		},
	}, nil
}

// defaultIterFactory provides the default messages iterator factory.
func defaultIterFactory(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error) {
	if cons == nil {
		return nil, errors.New("consumer not initialized")
	}

	return cons.Messages(
		jetstream.PullMaxMessages(batch),
		jetstream.PullExpiry(expiry),
		jetstream.PullHeartbeat(expiry/2),
	)
}

// diffCounts computes the number of added and removed items between two sorted string slices.
func diffCounts(prev, next []string) (add, removed int) {
	i, j := 0, 0
	for i < len(prev) && j < len(next) {
		if prev[i] == next[j] {
			i++
			j++

			continue
		}
		if prev[i] < next[j] {
			removed++
			i++

			continue
		}
		// prev[i] > next[j]
		add++
		j++
	}
	if i < len(prev) {
		removed += len(prev) - i
	}
	if j < len(next) {
		add += len(next) - j
	}

	return add, removed
}

// UpdateWorkerConsumer reconciles the worker-level durable consumer with the complete
// set of assigned partitions (single-consumer mode).
//
// Behavior:
//   - Builds subjects from partitions via the SubjectTemplate (deduped + sorted)
//   - Diffs against the previously applied subject list; no-op if unchanged
//   - Uses jetstream.CreateOrUpdateConsumer to atomically apply FilterSubjects
//   - Maintains a single long-lived pull loop (loop is started lazily on first update)
//   - Does NOT restart the pull loop on subsequent updates (hot-reload semantics)
//
// Idempotency: Calling with the same partition set is a no-op and returns nil.
//
// Concurrency: Safe for concurrent calls; internal locking ensures consistent state.
//
// Parameters:
//   - ctx: Context for cancellation and retry backoff timing
//   - workerID: Stable worker identifier (forms durable name <ConsumerPrefix>-<workerID>)
//   - partitions: Complete assignment slice (may be empty for no subjects)
//
// Returns:
//   - error: Non-nil only on unrecoverable configuration or JetStream API failure after retries
//
// Example:
//
//	helper, _ := subscription.NewWorkerConsumer(js, subscription.WorkerConsumerConfig{
//	    StreamName:      "events",
//	    ConsumerPrefix:  "worker",
//	    SubjectTemplate: "events.{{.PartitionID}}",
//	}, subscription.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
//	    // process
//	    return msg.Ack()
//	}))
//	// initial assignment
//	_ = helper.UpdateWorkerConsumer(ctx, "worker-7", []types.Partition{{Keys: []string{"a","0"}}, {Keys: []string{"b","3"}}})
//	// later, assignment shrinks
//	_ = helper.UpdateWorkerConsumer(ctx, "worker-7", []types.Partition{{Keys: []string{"a","0"}}})
func (dh *WorkerConsumer) UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []types.Partition) error {
	start := time.Now()
	if workerID == "" {
		return errors.New("workerID is required")
	}

	subjects, err := dh.buildAndValidateSubjects(partitions)
	if err != nil {
		return err
	}

	// Capture previous state and detect no-op / guardrail violations under lock.
	dh.mu.Lock()
	prevWorkerID := dh.workerID
	prevSubjects := append([]string(nil), dh.workerSubjects...)
	if prevWorkerID != "" && prevWorkerID != workerID && !dh.config.AllowWorkerIDChange {
		dh.mu.Unlock()
		if dh.config.Metrics != nil {
			dh.config.Metrics.IncrementWorkerConsumerGuardrailViolation("workerid_mutation")
		}

		return ErrWorkerIDMutation
	}
	if prevWorkerID == workerID && equalStringSlices(subjects, dh.workerSubjects) { // no-op
		dh.mu.Unlock()
		dh.recordUpdateMetrics("noop", start)

		return nil
	}
	dh.mu.Unlock()

	durable := dh.sanitizeConsumerName(dh.config.ConsumerPrefix + "-" + workerID)
	cons, applyErr := dh.applyConsumerUpdateWithRetry(ctx, durable, subjects)
	if applyErr != nil {
		dh.recordUpdateMetrics("failure", start)

		return applyErr
	}

	// Commit new state and ensure loop.
	dh.mu.Lock()
	dh.workerID = workerID
	dh.workerConsumer = cons
	dh.workerSubjects = subjects
	dh.ensurePullLoopLocked()
	dh.mu.Unlock()

	dh.emitSubjectDiffMetrics(prevSubjects, subjects)
	dh.deleteOldDurableAsync(prevWorkerID, workerID)
	dh.recordUpdateMetrics("success", start)

	return nil
}

// Health returns the current health snapshot for the worker consumer.
//
// Parameters:
//   - none
//
// Returns:
//   - WorkerConsumerHealth: Snapshot of current failure count and derived health
func (dh *WorkerConsumer) Health() WorkerConsumerHealth {
	dh.mu.RLock()
	cf := dh.consecutiveFailures
	cr := dh.consecutiveRecreations
	threshold := dh.config.HealthFailureThreshold
	if threshold <= 0 {
		threshold = DefaultHealthFailureThreshold
	}
	le := dh.lastError
	lu := dh.lastUpdate
	dh.mu.RUnlock()

	return WorkerConsumerHealth{ConsecutiveFailures: cf, ConsecutiveRecreations: cr, Healthy: cf <= threshold && cr <= threshold, LastError: le, LastUpdated: lu}
}

// Close stops all pull loops and cleans up resources.
//
// Consumers are NOT deleted from NATS - they will be automatically cleaned up
// by NATS based on InactiveThreshold setting.
//
// Parameters:
//   - ctx: Context for graceful shutdown timeout
//
// Returns:
//   - error: Cleanup error
//
// Example:
//
//	defer helper.Close(context.Background())
func (dh *WorkerConsumer) Close(ctx context.Context) error {
	dh.mu.Lock()
	defer dh.mu.Unlock()

	dh.logger.Info("closing durable helper")

	// Cancel worker single-consumer pull loop if running
	if dh.workerCancel != nil {
		dh.logger.Debug("cancelling worker pull loop", "workerID", dh.workerID)
		dh.workerCancel()
		dh.workerCancel = nil
	}

	dh.workerConsumer = nil
	dh.workerSubjects = nil
	dh.workerID = ""

	// Wait a bit for goroutines to exit gracefully
	select {
	case <-ctx.Done():
		dh.logger.Warn("close context cancelled before graceful shutdown completed")

		return ctx.Err()
	case <-time.After(100 * time.Millisecond):
		// Continue
	}

	dh.logger.Info("durable helper closed successfully")

	return nil
}

// WorkerConsumerInfo returns the JetStream ConsumerInfo for the worker-level durable consumer
// created/managed by UpdateWorkerConsumer.
//
// Behavior:
//   - Returns an error if UpdateWorkerConsumer has not been called yet (consumer uninitialized)
//   - Delegates to the Consumer.Info(ctx) call with the provided context
//
// Parameters:
//   - ctx: Context for cancellation and timeout
//
// Returns:
//   - *jetstream.ConsumerInfo: Consumer metadata and current FilterSubjects
//   - error: Non-nil if consumer is not initialized or Info() call fails
func (dh *WorkerConsumer) WorkerConsumerInfo(ctx context.Context) (*jetstream.ConsumerInfo, error) {
	dh.mu.RLock()
	cons := dh.workerConsumer
	dh.mu.RUnlock()
	if cons == nil {
		return nil, errors.New("worker consumer not initialized")
	}

	info, err := cons.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get worker consumer info: %w", err)
	}

	return info, nil
}

// WorkerSubjects returns a copy of the last applied FilterSubjects list for the worker-level
// durable consumer managed by UpdateWorkerConsumer.
//
// Returns:
//   - []string: Copy of subject list (nil if consumer not yet initialized)
//
// Example:
//
//	subjects := helper.WorkerSubjects()
//	for _, s := range subjects { log.Println("subject", s) }
func (dh *WorkerConsumer) WorkerSubjects() []string {
	dh.mu.RLock()
	defer dh.mu.RUnlock()
	if dh.workerSubjects == nil {
		return nil
	}

	out := make([]string, len(dh.workerSubjects))
	copy(out, dh.workerSubjects)

	return out
}

// buildAndValidateSubjects builds subjects from partitions and enforces guardrails.
func (dh *WorkerConsumer) buildAndValidateSubjects(partitions []types.Partition) ([]string, error) {
	subjects, err := dh.buildSubjects(partitions)
	if err != nil {
		return nil, err
	}
	if err := dh.enforceSubjectGuardrails(subjects); err != nil {
		return nil, err
	}

	return subjects, nil
}

// applyConsumerUpdateWithRetry applies the durable update with jittered retries.
func (dh *WorkerConsumer) applyConsumerUpdateWithRetry(ctx context.Context, durable string, subjects []string) (jetstream.Consumer, error) {
	cfg := dh.buildConsumerConfig(durable, subjects)

	return dh.createOrUpdateConsumerWithRetry(ctx, cfg, durable)
}

// ensurePullLoopLocked starts the pull loop if not already running. Caller must hold dh.mu.
func (dh *WorkerConsumer) ensurePullLoopLocked() {
	if dh.workerCancel != nil {
		return
	}
	pullCtx, cancel := context.WithCancel(context.Background())
	dh.workerCancel = cancel
	go dh.runWorkerPullLoop(pullCtx)
}

// emitSubjectDiffMetrics emits add/remove counts and current subject gauge.
func (dh *WorkerConsumer) emitSubjectDiffMetrics(prevSubjects, subjects []string) {
	dh.emitSubjectMetrics(prevSubjects, subjects)
}

// enforceSubjectGuardrails validates subject count against configured guardrails and emits metrics.
func (dh *WorkerConsumer) enforceSubjectGuardrails(subjects []string) error {
	// Guardrail: MaxSubjects
	if dh.config.MaxSubjects > 0 && len(subjects) > dh.config.MaxSubjects {
		if dh.config.Metrics != nil {
			dh.config.Metrics.IncrementWorkerConsumerGuardrailViolation("max_subjects")
		}

		return ErrMaxSubjectsExceeded
	}

	// Threshold warning at 90% of cap
	if dh.config.MaxSubjects > 0 {
		threshold := int(float64(dh.config.MaxSubjects) * 0.9)
		if len(subjects) >= threshold {
			if dh.config.Metrics != nil {
				dh.config.Metrics.IncrementWorkerConsumerSubjectThresholdWarning()
			}
		}
	}

	return nil
}

// buildConsumerConfig constructs the ConsumerConfig for the worker durable.
func (dh *WorkerConsumer) buildConsumerConfig(durable string, subjects []string) jetstream.ConsumerConfig {
	return jetstream.ConsumerConfig{
		Name:              durable,
		Durable:           durable,
		FilterSubjects:    subjects,
		AckPolicy:         dh.config.AckPolicy,
		AckWait:           dh.config.AckWait,
		MaxDeliver:        dh.config.MaxDeliver,
		InactiveThreshold: dh.config.InactiveThreshold,
		MaxWaiting:        dh.config.MaxWaiting,
	}
}

// createOrUpdateConsumerWithRetry applies the consumer config with jittered retries.
func (dh *WorkerConsumer) createOrUpdateConsumerWithRetry(ctx context.Context, cfg jetstream.ConsumerConfig, durable string) (jetstream.Consumer, error) {
	var cons jetstream.Consumer
	var lastErr error
	delay := time.Duration(0)
	rng := newRetryRNG(dh.config.RetrySeed)
	for attempt := 0; attempt <= dh.config.MaxControlRetries; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		cons, lastErr = dh.js.CreateOrUpdateConsumer(ctx, dh.config.StreamName, cfg)
		if lastErr == nil {
			return cons, nil
		}
		if attempt >= dh.config.MaxControlRetries {
			return nil, fmt.Errorf("failed to create/update worker consumer %s after %d attempts: %w", durable, dh.config.MaxControlRetries+1, lastErr)
		}
		delay = jitterBackoff(delay, dh.config.RetryBase, dh.config.RetryMultiplier, dh.config.RetryMax, rng)
		emitControlRetry(dh.config.Metrics, "create_update")
		emitRetryBackoff(dh.config.Metrics, "create_update", delay.Seconds())
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}

	// Unreachable
	return nil, fmt.Errorf("unexpected retry loop exit for durable %s", durable)
}

// emitSubjectMetrics records current subject count and added/removed deltas.
func (dh *WorkerConsumer) emitSubjectMetrics(prevSubjects, subjects []string) {
	if dh.config.Metrics == nil {
		return
	}

	dh.config.Metrics.SetWorkerConsumerSubjectsCurrent(len(subjects))
	add, removed := diffCounts(prevSubjects, subjects)
	if add > 0 {
		dh.config.Metrics.IncrementWorkerConsumerSubjectChange("add", add)
	}
	if removed > 0 {
		dh.config.Metrics.IncrementWorkerConsumerSubjectChange("remove", removed)
	}
}

// deleteOldDurableAsync best-effort deletes the old durable when workerID changes.
func (dh *WorkerConsumer) deleteOldDurableAsync(prevWorkerID, workerID string) {
	if prevWorkerID == "" || prevWorkerID == workerID {
		return
	}

	oldDurable := dh.sanitizeConsumerName(dh.config.ConsumerPrefix + "-" + prevWorkerID)
	go func(streamName, durable string) {
		delCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := dh.js.DeleteConsumer(delCtx, streamName, durable); err != nil {
			dh.logger.Warn("best-effort delete of old durable failed", "durable", durable, "error", err)
		} else {
			dh.logger.Info("deleted old durable after workerID change", "durable", durable)
		}
	}(dh.config.StreamName, oldDurable)
}

// recordUpdateMetrics records result classification and latency.
func (dh *WorkerConsumer) recordUpdateMetrics(resultClassification string, start time.Time) {
	if dh.config.Metrics == nil {
		return
	}

	lat := time.Since(start).Seconds()
	dh.config.Metrics.RecordWorkerConsumerUpdate(resultClassification)
	dh.config.Metrics.ObserveWorkerConsumerUpdateLatency(lat)
}

// delayWithBackoffOrExit computes a backoff delay for the given purpose, emits metrics, sleeps,
// and returns true if the context was cancelled during the wait (caller should exit), otherwise false.
func (dh *WorkerConsumer) delayWithBackoffOrExit(ctx context.Context, purpose string) bool {
	delay := jitterBackoff(0, dh.config.RetryBase, dh.config.RetryMultiplier, dh.config.RetryMax, nil)
	emitControlRetry(dh.config.Metrics, purpose)
	emitRetryBackoff(dh.config.Metrics, purpose, delay.Seconds())

	select {
	case <-ctx.Done():
		return true
	case <-time.After(delay):
		return false
	}
}

// runWorkerPullLoop runs the single-consumer pull loop using the configured handler.
// NOTE: Complexity will be reduced in upcoming refactor (see TODO list); nolint removed after prior suppression became unused.
func (dh *WorkerConsumer) runWorkerPullLoop(ctx context.Context) {
	// Snapshot durable name once (workerID may change, but pattern remains stable for logging purposes)
	dh.mu.RLock()
	durableName := dh.sanitizeConsumerName(dh.config.ConsumerPrefix + "-" + dh.workerID)
	dh.mu.RUnlock()
	dh.logger.Debug("starting worker pull loop", "durable", durableName)

	consecutiveFailures := 0
	for {
		exit, recreate := dh.runPullCycle(ctx, durableName, &consecutiveFailures)
		if exit {
			return
		}
		if !recreate { // avoid tight spin when nothing signalled a restart
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// ensureConsumerExistsOrRecreate ensures the consumer exists or attempts to recreate it.
// It returns errLoopRetry to signal the loop should continue, or errLoopExit to stop.
func (dh *WorkerConsumer) ensureConsumerExistsOrRecreate(ctx context.Context, cons jetstream.Consumer, durableName string) (jetstream.Consumer, loopCtrl) {
	if cons == nil {
		return nil, ctrlContinue
	}
	if _, infoErr := cons.Info(ctx); infoErr != nil {
		reason := classifyConsumerError(infoErr)
		switch reason {
		case "not_found":
			dh.logger.Warn("consumer missing; attempting recreation", "durable", durableName)
			subjects := dh.WorkerSubjects()
			newCons, recErr := dh.recreateDurableConsumer(ctx, durableName, subjects, reason)
			if recErr != nil {
				dh.logger.Error("consumer recreation failed", "error", recErr)
				if dh.delayWithBackoffOrExit(ctx, "iterate") { // throttle loop
					return nil, ctrlExit
				}

				return nil, ctrlRetry
			}
			dh.mu.Lock()
			dh.workerConsumer = newCons
			dh.mu.Unlock()

			return newCons, ctrlContinue
		case "terminal_policy":
			dh.logger.Error("consumer access denied by policy; stopping pull loop", "error", infoErr)

			return nil, ctrlExit
		default:
			// no-op
		}
	}

	return cons, ctrlContinue
}

// createIteratorOrBackoff creates the messages iterator or handles errors with backoff and metrics.
func (dh *WorkerConsumer) createIteratorOrBackoff(ctx context.Context, factory func(jetstream.Consumer, int, time.Duration) (jetstream.MessagesContext, error), cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, loopCtrl) {
	iter, err := factory(cons, batch, expiry)
	if err == nil {
		return iter, ctrlContinue
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, ctrlExit
	}
	dh.logger.Error("failed to create worker message iterator", "error", err)
	if dh.delayWithBackoffOrExit(ctx, "iterate") {
		return nil, ctrlExit
	}
	if dh.config.Metrics != nil {
		dh.config.Metrics.IncrementWorkerConsumerIteratorRestart("transient")
	}

	return nil, ctrlRetry
}

// sanitizeConsumerName replaces invalid characters from consumer name to underscore (_).
//
// NATS consumer name restrictions:
// - Cannot contain whitespace
// - Cannot contain . (dot)
// - Cannot contain * (asterisk)
// - Cannot contain > (greater than)
// - Cannot contain path separators (/ or \)
// - Cannot contain non-printable characters
//
// We replace invalid characters with underscore (_).
func (dh *WorkerConsumer) sanitizeConsumerName(name string) string {
	var b strings.Builder
	b.Grow(len(name))

	for _, r := range name {
		if isAllowedConsumerRune(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}

	return b.String()
}

// isAllowedConsumerRune returns true for runes allowed in NATS consumer names.
// Allowed: ASCII letters, digits, hyphen and underscore. Everything else is replaced with '_'.
func isAllowedConsumerRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'A' && r <= 'Z':
		return true
	case r >= '0' && r <= '9':
		return true
	case r == '-' || r == '_':
		return true
	default:
		return false
	}
}

// generateSubject generates a subject from the template.
//
// Template context contains PartitionID (keys joined with ".").
// Example: ["source", "region", "us"] → "source.region.us"
func (dh *WorkerConsumer) generateSubject(partition types.Partition) (string, error) {
	if len(partition.Keys) == 0 {
		return "", errors.New("partition has no keys")
	}

	ctx := subjectContext{PartitionID: partition.SubjectKey()}

	// Execute template
	var buf strings.Builder
	if err := dh.subjectTemplate.Execute(&buf, ctx); err != nil {
		return "", fmt.Errorf("failed to execute subject template: %w", err)
	}

	return buf.String(), nil
}

// buildSubjects generates a sorted, deduplicated list of subjects from partitions.
func (dh *WorkerConsumer) buildSubjects(partitions []types.Partition) ([]string, error) {
	if len(partitions) == 0 {
		return []string{}, nil
	}
	// Deduplicate via map
	m := make(map[string]struct{}, len(partitions))
	for _, p := range partitions {
		subj, err := dh.generateSubject(p)
		if err != nil {
			return nil, err
		}
		m[subj] = struct{}{}
	}
	subjects := make([]string, 0, len(m))
	for s := range m {
		subjects = append(subjects, s)
	}
	// Sort for deterministic ordering
	slices.Sort(subjects)

	return subjects, nil
}

// equalStringSlices compares two string slices for equality assuming both are sorted.
func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// setHealthMetrics updates internal counters and emits health-related metrics.
func (dh *WorkerConsumer) setHealthMetrics(consecutiveFailures int) {
	dh.mu.Lock()
	dh.consecutiveFailures = consecutiveFailures
	dh.lastUpdate = time.Now()
	dh.mu.Unlock()
	if dh.config.Metrics != nil {
		dh.config.Metrics.SetWorkerConsumerConsecutiveIteratorFailures(consecutiveFailures)
		dh.updateHealthGauge()
	}
}

// updateHealthGauge recalculates and emits the overall health gauge based on both
// iterator and recreation consecutive failure counters.
func (dh *WorkerConsumer) updateHealthGauge() {
	if dh.config.Metrics == nil {
		return
	}
	dh.mu.RLock()
	cf := dh.consecutiveFailures
	cr := dh.consecutiveRecreations
	threshold := dh.config.HealthFailureThreshold
	if threshold <= 0 {
		threshold = DefaultHealthFailureThreshold
	}
	dh.mu.RUnlock()
	dh.config.Metrics.SetWorkerConsumerHealthStatus(cf <= threshold && cr <= threshold)
}

// classifyConsumerError maps JetStream/NATS errors to recreation reasons.
//
// Classification Table:
//
//	Source                           | apiErr.Code | Match                       | Reason            | Behavior
//	-------------------------------- | ----------- | --------------------------- | ----------------- | ----------------------------------------------
//	jetstream.ErrConsumerNotFound    | n/a         | errors.Is                   | not_found         | Attempt recreation (create/update durable)
//	nats.APIError (authorization)    | 401,403     | errors.As + code match      | terminal_policy   | Abort pull loop (operator action required)
//	nats.APIError (not found)        | 404         | errors.As + code match      | not_found         | Attempt recreation (create/update durable)
//	Other nats.APIError              | any other   | errors.As                   | unknown           | Log, treat as transient, keep looping
//	All other errors                 | n/a         | default                     | unknown           | Log, treat as transient
//
// Future Extensions (planned):
//   - Distinguish stream-not-found vs consumer-not-found (separate reason: stream_missing)
//   - Map resource/account limits (e.g., 409, 429) to throttling classification
//   - Surface limit reasons in metrics labels for operator dashboards
//
// Returns one of: "not_found", "terminal_policy", "unknown" (current set).
func classifyConsumerError(err error) string {
	if err == nil {
		return "unknown"
	}
	if errors.Is(err, jetstream.ErrConsumerNotFound) {
		return "not_found"
	}
	// JetStream API errors contain HTTP-like codes; 401/403 are authorization/policy failures, 404 not found
	var apiErr *nats.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case 401, 403:
			return "terminal_policy"
		case 404:
			// Ambiguous 404 (could be stream or consumer). For now treat as not_found (recreation path).
			return "not_found"
		case 409, 429: // resource/account limits or rate limiting (future refinement)
			return "throttle"
		case 500, 502, 503, 504: // server-side transient errors
			return "transient_server"
		default:
			return "unknown"
		}
	}

	return "unknown"
}

// recreateDurableConsumer attempts to recreate a missing or invalid consumer with jittered backoff.
// Emits metrics for attempts, outcomes, and duration.
func (dh *WorkerConsumer) recreateDurableConsumer(ctx context.Context, durable string, subjects []string, reason string) (jetstream.Consumer, error) {
	start := time.Now()
	maxAttempts := dh.resolveMaxRecreationAttempts()
	createFn := dh.resolveCreateFn()
	rng := newRetryRNG(dh.config.RetrySeed)
	var lastErr error
	delay := time.Duration(0)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		dh.emitRecreationAttempt(reason)
		cons, err := createFn(ctx, dh.config.StreamName, dh.buildConsumerConfig(durable, subjects))
		if err == nil {
			dh.onRecreationSuccess(reason, start)

			return cons, nil
		}
		lastErr = err
		dh.onRecreationFailure(reason)
		delay = jitterBackoff(delay, dh.config.RetryBase, dh.config.RetryMultiplier, dh.config.RetryMax, rng)
		emitControlRetry(dh.config.Metrics, "create_update")
		emitRetryBackoff(dh.config.Metrics, "create_update", delay.Seconds())
		if dh.sleepOrCancelled(ctx, delay) {
			return nil, ctx.Err()
		}
	}
	dh.onRecreationTerminalFailure(start, lastErr)

	return nil, fmt.Errorf("consumer recreation failed for %s after %d attempts: %w", durable, maxAttempts, lastErr)
}

// resolveCreateFn returns the create/update function or a default.
func (dh *WorkerConsumer) resolveCreateFn() func(context.Context, string, jetstream.ConsumerConfig) (jetstream.Consumer, error) {
	if dh.createOrUpdateFn != nil {
		return dh.createOrUpdateFn
	}

	return func(cctx context.Context, stream string, cc jetstream.ConsumerConfig) (jetstream.Consumer, error) {
		return dh.js.CreateOrUpdateConsumer(cctx, stream, cc)
	}
}

// resolveMaxRecreationAttempts computes the maximum recreation attempts respecting defaults.
func (dh *WorkerConsumer) resolveMaxRecreationAttempts() int {
	attempts := dh.config.MaxRecreationRetries
	if attempts <= 0 {
		attempts = dh.config.MaxControlRetries
		if attempts <= 0 {
			attempts = DefaultMaxControlRetries
		}
	}

	return attempts
}

// emitRecreationAttempt records an attempt metric if metrics are enabled.
func (dh *WorkerConsumer) emitRecreationAttempt(reason string) {
	if dh.config.Metrics != nil {
		dh.config.Metrics.IncrementWorkerConsumerRecreationAttempt(reason)
	}
}

// onRecreationSuccess handles state+metrics updates for a successful recreation.
func (dh *WorkerConsumer) onRecreationSuccess(reason string, start time.Time) {
	if dh.config.Metrics != nil {
		dh.config.Metrics.RecordWorkerConsumerRecreation("success", reason)
		dh.config.Metrics.ObserveWorkerConsumerRecreationDuration(time.Since(start).Seconds())
	}
	dh.mu.Lock()
	dh.consecutiveRecreations = 0
	dh.lastError = nil
	dh.lastUpdate = time.Now()
	dh.mu.Unlock()
	dh.updateHealthGauge()
}

// onRecreationFailure handles metrics for a single failed attempt.
func (dh *WorkerConsumer) onRecreationFailure(reason string) {
	if dh.config.Metrics != nil {
		dh.config.Metrics.RecordWorkerConsumerRecreation("failure", reason)
	}
}

// onRecreationTerminalFailure records duration and updates health state after exhausting attempts.
func (dh *WorkerConsumer) onRecreationTerminalFailure(start time.Time, lastErr error) {
	if dh.config.Metrics != nil {
		dh.config.Metrics.ObserveWorkerConsumerRecreationDuration(time.Since(start).Seconds())
	}
	dh.mu.Lock()
	dh.consecutiveRecreations++
	dh.lastError = lastErr
	dh.lastUpdate = time.Now()
	dh.mu.Unlock()
	dh.updateHealthGauge()
}

// sleepOrCancelled waits for the delay or returns true if context cancelled.
func (dh *WorkerConsumer) sleepOrCancelled(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(d):
		return false
	}
}

// processMessages consumes messages from the iterator until an exit or recreate condition occurs.
// It updates consecutiveFailures and emits appropriate metrics.
// Returns:
//   - exit: true if the pull loop should exit
//   - recreate: true if the iterator should be recreated (e.g., heartbeat or transient error)
func (dh *WorkerConsumer) processMessages(
	ctx context.Context,
	iter jetstream.MessagesContext,
	handler MessageHandler,
	consecutiveFailures *int,
) (exit bool, recreate bool) {
	for {
		if ctx.Err() != nil {
			iter.Stop()

			return true, false
		}
		msg, err := iter.Next()
		if err != nil {
			return dh.handleIteratorError(ctx, iter, err, consecutiveFailures)
		}
		if handler == nil {
			_ = msg.Nak()

			continue
		}
		dh.handleMessage(ctx, msg, handler, consecutiveFailures)
	}
}

// runPullCycle performs a single cycle: consumer validation, iterator creation, message processing.
// Returns (exit,recreate) where recreate=true asks outer loop to immediately create a new iterator.
func (dh *WorkerConsumer) runPullCycle(ctx context.Context, durableName string, consecutiveFailures *int) (exit bool, recreate bool) {
	ci := dh.snapshotCycleInputs()
	if e, wait := dh.waitForInitialization(ctx, ci.cons, ci.factory); e {
		return true, false
	} else if wait {
		return false, false
	}
	ci.factory = dh.resolveIterFactory(ci.factory)
	if ci.cons != nil {
		updated, recreateFlag, exitFlag := dh.ensureCycleConsumer(ctx, ci.cons, durableName)
		ci.cons = updated
		if exitFlag || recreateFlag {
			return exitFlag, recreateFlag
		}
	}
	iter, ctrl := dh.createIteratorOrBackoff(ctx, ci.factory, ci.cons, ci.batch, ci.expiry)
	switch ctrl {
	case ctrlExit:
		return true, false
	case ctrlRetry:
		return false, true
	case ctrlContinue:
		// proceed
	}
	exit, recreate = dh.processMessages(ctx, iter, ci.handler, consecutiveFailures)
	if exit {
		return true, false
	}
	if dh.verifyPostCycle(ctx, ci.cons, durableName, recreate) {
		return true, false
	}

	return false, recreate
}

// snapshotCycleInputs grabs the current consumer-related inputs under lock.
type cycleInputs struct {
	cons    jetstream.Consumer
	handler MessageHandler
	batch   int
	expiry  time.Duration
	factory func(jetstream.Consumer, int, time.Duration) (jetstream.MessagesContext, error)
}

func (dh *WorkerConsumer) snapshotCycleInputs() cycleInputs {
	dh.mu.RLock()
	ci := cycleInputs{
		cons:    dh.workerConsumer,
		handler: dh.handler,
		batch:   dh.config.BatchSize,
		expiry:  dh.config.FetchTimeout,
		factory: dh.iterFactory,
	}
	dh.mu.RUnlock()

	return ci
}

// waitForInitialization waits briefly when consumer/factory uninitialized. Returns (exit,wait).
func (dh *WorkerConsumer) waitForInitialization(ctx context.Context, cons jetstream.Consumer, factory func(jetstream.Consumer, int, time.Duration) (jetstream.MessagesContext, error)) (exit bool, wait bool) {
	if cons != nil || factory != nil {
		return false, false
	}
	select {
	case <-ctx.Done():
		return true, false
	case <-time.After(50 * time.Millisecond):
		return false, true
	}
}

// resolveIterFactory picks the default factory if none provided.
func (dh *WorkerConsumer) resolveIterFactory(factory func(jetstream.Consumer, int, time.Duration) (jetstream.MessagesContext, error)) func(jetstream.Consumer, int, time.Duration) (jetstream.MessagesContext, error) {
	if factory == nil {
		return defaultIterFactory
	}

	return factory
}

// ensureCycleConsumer validates consumer existence and handles recreation outcome mapping.
// Returns (updatedCons, recreate, exit).
func (dh *WorkerConsumer) ensureCycleConsumer(ctx context.Context, cons jetstream.Consumer, durableName string) (updated jetstream.Consumer, recreate bool, exit bool) {
	updated, ctrl := dh.ensureConsumerExistsOrRecreate(ctx, cons, durableName)
	switch ctrl {
	case ctrlExit:
		return nil, false, true
	case ctrlRetry:
		return updated, true, false
	case ctrlContinue:
		return updated, false, false
	}

	return updated, false, false
}

// verifyPostCycle validates consumer after an iterator-signalled recreation.
func (dh *WorkerConsumer) verifyPostCycle(ctx context.Context, cons jetstream.Consumer, durableName string, recreate bool) (exit bool) {
	if !recreate || cons == nil {
		return false
	}
	if _, c := dh.ensureConsumerExistsOrRecreate(ctx, cons, durableName); c == ctrlExit {
		return true
	}

	return false
}

// handleIteratorError classifies iterator errors and updates metrics/state.
func (dh *WorkerConsumer) handleIteratorError(ctx context.Context, iter jetstream.MessagesContext, err error, consecutiveFailures *int) (exit bool, recreate bool) {
	iter.Stop()
	if errors.Is(err, jetstream.ErrMsgIteratorClosed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true, false
	}
	if errors.Is(err, jetstream.ErrNoHeartbeat) {
		dh.logger.Error("worker pull loop: no heartbeat", "error", err)
		if dh.config.Metrics != nil {
			dh.config.Metrics.IncrementWorkerConsumerIteratorRestart("heartbeat")
		}

		return false, true
	}
	dh.logger.Warn("worker pull loop: iterator error, retrying", "error", err)
	if dh.delayWithBackoffOrExit(ctx, "iterate") {
		return true, false
	}
	*consecutiveFailures++
	dh.setHealthMetrics(*consecutiveFailures)
	if dh.config.Metrics != nil {
		dh.config.Metrics.IncrementWorkerConsumerIteratorRestart("transient")
	}

	return false, true
}

// handleMessage invokes the handler and updates failure counters & health metrics.
func (dh *WorkerConsumer) handleMessage(ctx context.Context, msg jetstream.Msg, handler MessageHandler, consecutiveFailures *int) {
	if err := handler.Handle(ctx, msg); err != nil {
		_ = msg.Nak()
		*consecutiveFailures++
		dh.setHealthMetrics(*consecutiveFailures)

		return
	}
	_ = msg.Ack()
	if *consecutiveFailures > 0 {
		*consecutiveFailures = 0
		dh.setHealthMetrics(*consecutiveFailures)
	}
}
