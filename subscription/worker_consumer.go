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

	"github.com/arloliu/parti/kvutil"
	"github.com/arloliu/parti/types"
	"github.com/zeebo/xxh3"
)

// WorkerConsumer manages one JetStream durable pull consumer per subject (partition).
// It preserves per-subject cursors across reassignments and never deletes durables; idle
// consumers are reclaimed by JetStream via InactiveThreshold.
type WorkerConsumer struct {
	conn    *nats.Conn
	js      jetstream.JetStream
	config  WorkerConsumerConfig
	logger  types.Logger
	handler MessageHandler

	// parsed subject template for subject generation
	subjectTemplate *template.Template

	// template for deriving subject strings lives in config; we reuse the same SubjectTemplate

	mu sync.RWMutex

	workerID string

	// per-subject state
	subjects map[string]*subjectLoop

	// optional processing gate (claim-based resolver)
	gateResolver       types.OwnershipResolver
	gateResolverMu     sync.Mutex
	gateResolverCancel context.CancelFunc
	resolverMetrics    ResolverMetrics

	// iterator factory
	iterFactory func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error)

	// iterator failure tracking for escalation logic
	iterFailureTimes []time.Time
	lastEscalation   time.Time
	iterEscMu        sync.Mutex

	// reverse subject template components for partitionID extraction (prefix/suffix around {{.PartitionID}})
	partitionPrefix string
	partitionSuffix string

	// serialize Consumer.Info calls across goroutines to avoid races in underlying client
	consInfoMu sync.Mutex
}

type subjectLoop struct {
	consumer    jetstream.Consumer
	partitionID string
	cancel      context.CancelFunc
	done        chan struct{}
}

// NewWorkerConsumer creates a new per-subject durable consumer helper.
func NewWorkerConsumer(js jetstream.JetStream, cfg WorkerConsumerConfig, handler MessageHandler) (*WorkerConsumer, error) {
	if js == nil {
		return nil, errors.New("JetStream context is required")
	}
	if handler == nil {
		return nil, errors.New("message handler is required")
	}

	if err := cfg.SetDefaults(); err != nil {
		return nil, fmt.Errorf("failed to set defaults: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// parse subject template
	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse subject template: %w", err)
	}
	// derive prefix/suffix for partitionID extraction (single placeholder assumption)
	prefix, suffix, _ := parseSubjectTemplateParts(cfg.SubjectTemplate)

	wc := &WorkerConsumer{
		conn:            js.Conn(),
		js:              js,
		config:          cfg,
		logger:          cfg.Logger,
		handler:         handler,
		subjects:        make(map[string]*subjectLoop, 64),
		iterFactory:     defaultIterFactory,
		subjectTemplate: tmpl,
		partitionPrefix: prefix,
		partitionSuffix: suffix,
	}

	// allow injection for tests
	if cfg.IteratorFactory != nil {
		wc.iterFactory = cfg.IteratorFactory
	}

	// initialize auto-claim resolver if enabled
	if err := wc.ensureGateResolver(context.Background()); err != nil {
		return nil, err
	}

	return wc, nil
}

// UpdateWorkerConsumer applies the per-subject assignment set.
// It creates/binds durables for added subjects and starts pull loops; it cancels loops
// for removed subjects but does not delete the underlying consumer (server will GC by inactivity).
func (wc *WorkerConsumer) UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []types.Partition) error {
	if err := wc.validateUpdateParams(workerID); err != nil {
		return err
	}

	subjects, err := wc.buildSubjects(partitions)
	if err != nil {
		return err
	}

	// Set workerID and snapshot existing subjects under lock
	existing := wc.setWorkerIDAndSnapshot(workerID)

	toAdd, toRemove := wc.computeSubjectDiff(subjects, existing)

	if err := wc.removeSubjectLoops(ctx, toRemove); err != nil {
		return err
	}

	for _, partitionName := range toAdd {
		if err := wc.addSubjectLoop(ctx, workerID, partitionName); err != nil {
			return err
		}
	}

	// Emit subject set metrics after applying changes
	if wc.config.Metrics != nil {
		if len(toAdd) > 0 {
			wc.config.Metrics.IncrementWorkerConsumerSubjectChange("add", len(toAdd))
		}
		if len(toRemove) > 0 {
			wc.config.Metrics.IncrementWorkerConsumerSubjectChange("remove", len(toRemove))
		}
		wc.mu.RLock()
		currentCount := len(wc.subjects)
		wc.mu.RUnlock()
		wc.config.Metrics.SetWorkerConsumerSubjectsCurrent(currentCount)
	}

	return nil
}

// Close cancels all subject loops. Consumers are left intact for server GC.
func (wc *WorkerConsumer) Close(ctx context.Context) error {
	wc.stopGateResolver()
	// Snapshot current loops under lock
	wc.mu.Lock()
	type kv struct {
		s string
		l *subjectLoop
	}
	loops := make([]kv, 0, len(wc.subjects))
	for s, loop := range wc.subjects {
		if loop != nil {
			loops = append(loops, kv{s: s, l: loop})
		}
	}
	wc.mu.Unlock()

	// Cancel outside lock to avoid holding lock during waits
	for _, it := range loops {
		it.l.cancel()
	}
	// Wait for all to finish
	for _, it := range loops {
		<-it.l.done
	}

	// Clean map entries under lock
	wc.mu.Lock()
	for _, it := range loops {
		delete(wc.subjects, it.s)
	}
	wc.mu.Unlock()

	return nil
}

// runSubjectLoop pulls and processes messages for a single subject durable.
func (wc *WorkerConsumer) runSubjectLoop(ctx context.Context, subject, partitionID string, cons jetstream.Consumer, handler MessageHandler, done chan struct{}) {
	wc.logger.Debug("subject loop starting", "subject", subject)
	defer func() {
		wc.logger.Debug("subject loop stopped", "subject", subject)
		close(done)
	}()
	batch := wc.config.BatchSize
	expiry := wc.config.FetchTimeout

	for {
		if ctx.Err() != nil {
			return
		}

		if suppressed, reason := wc.shouldSuppressPull(ctx, partitionID); suppressed {
			wc.logger.Debug("pull suppressed", "subject", subject, "reason", reason)
			select {
			case <-ctx.Done():
				return
			case <-time.After(150 * time.Millisecond):
				continue
			}
		}

		// Create iterator and enter message processing loop.
		iter, err := wc.iterFactory(cons, batch, expiry)
		if err != nil {
			wc.logger.Warn("iterator creation failed", "subject", subject, "error", err)
			if wc.config.Metrics != nil {
				wc.config.Metrics.IncrementWorkerConsumerIteratorRestart("transient")
			}

			if newCons := wc.maybeEscalateIteratorFailures(ctx, subject, cons); newCons != nil {
				cons = newCons
			}

			if wc.delayWithBackoffOrExit(ctx, "iterate") {
				return
			}

			continue
		}

		exit, iterErr := wc.processIterator(ctx, iter, subject, handler)
		if exit {
			return
		}
		if iterErr != nil {
			// Classify restart reason for metrics parity
			if wc.config.Metrics != nil {
				if errors.Is(iterErr, jetstream.ErrNoHeartbeat) {
					wc.config.Metrics.IncrementWorkerConsumerIteratorRestart("heartbeat")
				} else {
					wc.config.Metrics.IncrementWorkerConsumerIteratorRestart("transient")
				}
			}
			if newCons := wc.maybeEscalateIteratorFailures(ctx, subject, cons); newCons != nil {
				cons = newCons
			}
			if wc.delayWithBackoffOrExit(ctx, "iterate") {
				return
			}
		}
		// loop continues to recreate iterator on next iteration
	}
}

func (wc *WorkerConsumer) validateUpdateParams(workerID string) error {
	if workerID == "" {
		return errors.New("workerID is required")
	}
	if wc.handler == nil {
		return errors.New("message handler is required")
	}
	wc.mu.RLock()
	prevWorkerID := wc.workerID
	wc.mu.RUnlock()

	if prevWorkerID != "" && prevWorkerID != workerID && !wc.config.AllowWorkerIDChange {
		if wc.config.Metrics != nil {
			wc.config.Metrics.IncrementWorkerConsumerGuardrailViolation("workerid_mutation")
		}
		return ErrWorkerIDMutation
	}

	return nil
}

func (wc *WorkerConsumer) setWorkerIDAndSnapshot(workerID string) map[string]struct{} {
	wc.mu.Lock()
	defer wc.mu.Unlock()

	wc.workerID = workerID
	existing := make(map[string]struct{}, len(wc.subjects))
	for s := range wc.subjects {
		existing[s] = struct{}{}
	}

	return existing
}

func (wc *WorkerConsumer) computeSubjectDiff(subjects []string, existing map[string]struct{}) (toAdd []string, toRemove []string) {
	toAdd = make([]string, 0, len(subjects))
	seen := make(map[string]struct{}, len(subjects))

	for _, s := range subjects {
		seen[s] = struct{}{}
		if _, ok := existing[s]; !ok {
			toAdd = append(toAdd, s)
		}
	}
	for s := range existing {
		if _, ok := seen[s]; !ok {
			toRemove = append(toRemove, s)
		}
	}

	return toAdd, toRemove
}

func (wc *WorkerConsumer) removeSubjectLoops(ctx context.Context, toRemove []string) error {
	if len(toRemove) == 0 {
		return nil
	}

	// Snapshot loops to stop under lock
	wc.mu.Lock()
	loops := make([]*subjectLoop, 0, len(toRemove))
	for _, s := range toRemove {
		if loop := wc.subjects[s]; loop != nil {
			loops = append(loops, loop)
		}
	}
	wc.mu.Unlock()

	// Optionally drain before cancel to reduce NAK churn and gaps
	if wc.config.DrainOnRemove {
		deadline := wc.config.DrainOnRemoveTimeout
		if deadline <= 0 {
			deadline = 10 * time.Second
		}
		dctx, cancel := context.WithTimeout(ctx, deadline)
		defer cancel()

		for _, l := range loops {
			wc.drainPerSubject(dctx, l.consumer)
		}
	}

	// Cancel outside lock after optional drain
	for _, l := range loops {
		l.cancel()
	}
	// Wait for completion
	for _, l := range loops {
		<-l.done
	}

	// Delete keys under lock
	wc.mu.Lock()
	for _, s := range toRemove {
		delete(wc.subjects, s)
	}
	wc.mu.Unlock()

	return nil
}

// drainPerSubject waits until the server reports no pending acknowledgements for the given consumer
// or the context times out. This is a best-effort drain used during subject removal to minimize
// redeliveries caused by abrupt cancellation.
func (wc *WorkerConsumer) drainPerSubject(ctx context.Context, cons jetstream.Consumer) {
	if cons == nil {
		return
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Serialize Info calls to avoid underlying client races
			wc.consInfoMu.Lock()
			info, err := cons.Info(ctx)
			wc.consInfoMu.Unlock()
			if err != nil {
				// If info fails, break drain early to avoid blocking removals
				return
			}
			if info.NumAckPending == 0 {
				return
			}
		}
	}
}

func (wc *WorkerConsumer) addSubjectLoop(ctx context.Context, workerID string, subject string) error {
	if wc.config.MaxConcurrentSubjects > 0 {
		wc.mu.RLock()
		current := len(wc.subjects)
		wc.mu.RUnlock()

		if current >= wc.config.MaxConcurrentSubjects {
			wc.logger.Warn("per-subject consumer cap reached; skipping subject",
				"subject", subject,
				"cap", wc.config.MaxConcurrentSubjects,
			)
			if wc.config.Metrics != nil {
				wc.config.Metrics.IncrementWorkerConsumerSubjectThresholdWarning()
			}

			return nil
		}
	}

	durable := wc.perSubjectDurableName(wc.config.ConsumerPrefix, subject)
	cons, err := wc.ensurePerSubjectConsumer(ctx, durable, subject)
	if err != nil {
		return fmt.Errorf("create/bind per-subject consumer %s: %w", durable, err)
	}

	wc.logger.Info("per-subject durable ready",
		"op", "create_subject_consumer",
		"subject", subject,
		"durable", durable,
	)
	effectiveHandler := wc.handler
	if resolver := wc.getEffectiveResolver(); wc.config.ProcessingGate != nil && wc.config.ProcessingGate.Enabled && resolver != nil {
		g, err := newProcessingGate(workerID, wc.config.SubjectTemplate, resolver, *wc.config.ProcessingGate, wc.logger)
		if err != nil {
			return fmt.Errorf("create processing gate: %w", err)
		}
		effectiveHandler = g.Wrap(wc.handler)
	}

	partitionID := wc.extractPartitionID(subject)
	loopCtx, cancel := context.WithCancel(context.Background())
	sl := &subjectLoop{
		consumer:    cons,
		partitionID: partitionID,
		cancel:      cancel,
		done:        make(chan struct{}),
	}

	wc.mu.Lock()
	wc.subjects[subject] = sl
	wc.mu.Unlock()

	go wc.runSubjectLoop(loopCtx, subject, partitionID, cons, effectiveHandler, sl.done)

	return nil
}

func (wc *WorkerConsumer) shouldSuppressPull(ctx context.Context, partitionID string) (bool, string) {
	if !wc.config.PullGatingEnabled {
		return false, ""
	}
	resolver := wc.getEffectiveResolver()
	if resolver == nil {
		return false, ""
	}
	if partitionID == "" {
		return false, ""
	}

	owner, state, _, ok := resolver.GetOwner(partitionID)
	allowedState := state == types.HandoffStateCommit || state == types.HandoffStateStable || state == types.HandoffStatePrepare
	if ok && owner == wc.workerID && allowedState {
		return false, ""
	}

	// Aggressive refresh when we are suppressed due to ownership/state. This accelerates
	// visibility of claim flips during handoff without waiting for the periodic cooldown.
	_ = resolver.ForceRefreshPartition(ctx, partitionID)

	reason := "not_owner"
	if ok && owner == wc.workerID && !allowedState {
		reason = "state_blocked"
	}
	if wc.config.Metrics != nil {
		wc.config.Metrics.IncrementWorkerConsumerPullSuppressed(reason)
	}

	return true, reason
}

func (wc *WorkerConsumer) processIterator(
	ctx context.Context,
	iter jetstream.MessagesContext,
	subject string,
	handler MessageHandler,
) (exit bool, iterErr error) {
	stopperCh := wc.startIterStopper(ctx, iter)
	defer func() {
		select {
		case <-stopperCh:
		default:
			close(stopperCh)
		}
	}()

	for {
		if ctx.Err() != nil {
			iter.Stop()
			return true, nil
		}

		msg, err := iter.Next()
		if err != nil {
			iter.Stop()
			wc.logger.Debug("iterator next error", "subject", subject, "error", err)
			return false, err
		}

		if handler == nil {
			_ = msg.Nak()
			continue
		}

		if wc.config.ManualAck {
			_ = handler.Handle(ctx, msg)
			continue
		}

		if err := handler.Handle(ctx, msg); err != nil {
			_ = msg.Nak()
		} else {
			_ = msg.Ack()
		}
	}
}

func (wc *WorkerConsumer) startIterStopper(ctx context.Context, iter jetstream.MessagesContext) chan struct{} {
	stopperCh := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			iter.Stop()
		case <-stopperCh:
			return
		}
	}()

	return stopperCh
}

// ensurePerSubjectConsumer creates or updates a per-subject durable with FilterSubject.
// It employs a short retry strategy to handle transient NATS errors.
func (wc *WorkerConsumer) ensurePerSubjectConsumer(ctx context.Context, durable string, subject string) (jetstream.Consumer, error) {
	cfg := jetstream.ConsumerConfig{
		Name:              durable,
		Durable:           durable,
		FilterSubject:     subject,
		AckPolicy:         wc.config.AckPolicy,
		AckWait:           wc.config.AckWait,
		MaxDeliver:        wc.config.MaxDeliver,
		InactiveThreshold: wc.config.InactiveThreshold,
		MaxWaiting:        wc.config.MaxWaiting,
		MaxAckPending:     wc.config.MaxAckPending,
	}

	var lastErr error
	const maxAttempts = 3

	for i := 0; i < maxAttempts; i++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		cons, err := wc.js.CreateOrUpdateConsumer(ctx, wc.config.StreamName, cfg)
		if err == nil {
			return cons, nil
		}

		lastErr = err
		// If stream not found, it's a configuration error, not transient.
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			return nil, err
		}

		if i < maxAttempts-1 {
			delay := jitterBackoff(time.Duration(i)*50*time.Millisecond, 50*time.Millisecond, 2.0, 200*time.Millisecond, nil)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
				continue
			}
		}
	}

	return nil, fmt.Errorf("failed after %d attempts: %w", maxAttempts, lastErr)
}

// perSubjectDurableName returns a stable, sanitized durable for a given subject.
func (wc *WorkerConsumer) perSubjectDurableName(prefix, subject string) string {
	partID := wc.extractPartitionID(subject)
	if partID == "" {
		partID = subject
	}

	// Hash the subject to ensure uniqueness even if sanitization causes collisions
	h := xxh3.HashString(subject)

	sanitizedPartID := sanitizeConsumerName(partID)
	if len(sanitizedPartID) > 50 {
		sanitizedPartID = sanitizedPartID[:50]
	}

	// Format: <Prefix>_<SanitizedPartitionID>_<Hash>
	return fmt.Sprintf("%s_%s_%016x", prefix, sanitizedPartID, h)
}

// sanitizeConsumerName sanitizes a consumer name according to allowed runes.
func sanitizeConsumerName(name string) string {
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

// buildSubjects generates a sorted, deduplicated list of subjects from partitions,
// ensuring the subject template is parsed.
func (wc *WorkerConsumer) buildSubjects(partitions []types.Partition) ([]string, error) {
	// Ensure we have a parsed template even if constructed manually in tests
	tmpl := wc.subjectTemplate
	if tmpl == nil {
		t, err := template.New("subject").Parse(wc.config.SubjectTemplate)
		if err != nil {
			return nil, fmt.Errorf("parse subject template: %w", err)
		}
		wc.subjectTemplate = t
	}

	return wc.doBuildSubjects(partitions)
}

// buildSubjects generates a sorted, deduplicated list of subjects from partitions.
func (wc *WorkerConsumer) doBuildSubjects(partitions []types.Partition) ([]string, error) {
	if len(partitions) == 0 {
		return []string{}, nil
	}

	// Deduplicate via map
	m := make(map[string]struct{}, len(partitions))
	for _, p := range partitions {
		subj, err := wc.generateSubject(p)
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

// generateSubject generates a subject from the template.
//
// Template context contains PartitionID (keys joined with ".").
// Example: ["source", "region", "us"] → "source.region.us"
func (wc *WorkerConsumer) generateSubject(partition types.Partition) (string, error) {
	if len(partition.Keys) == 0 {
		return "", errors.New("partition has no keys")
	}

	// subjectContext is the template context for subject generation.
	type subjectContext struct {
		PartitionID string
	}

	ctx := subjectContext{PartitionID: partition.SubjectKey()}

	// Execute template
	var buf strings.Builder
	if err := wc.subjectTemplate.Execute(&buf, ctx); err != nil {
		return "", fmt.Errorf("failed to execute subject template: %w", err)
	}

	return buf.String(), nil
}

// ensureGateResolver lazily initializes the automatic claim-based resolver.
func (wc *WorkerConsumer) ensureGateResolver(ctx context.Context) error {
	if wc.config.ProcessingGate == nil || !wc.config.ProcessingGate.Enabled {
		return nil
	}
	if wc.config.Resolver.OwnershipResolver != nil {
		wc.gateResolver = wc.config.Resolver.OwnershipResolver
		return nil
	}
	if wc.config.Resolver.HandoffBucketName == "" {
		return nil
	}

	wc.gateResolverMu.Lock()
	defer wc.gateResolverMu.Unlock()

	if wc.gateResolver != nil {
		return nil
	}

	wc.logger.Info("initializing automatic claim-based resolver",
		"bucket", wc.config.Resolver.HandoffBucketName,
		"prefix", wc.config.Resolver.HandoffClaimsPrefix,
	)

	kv, err := kvutil.EnsureKVBucket(ctx, wc.js, wc.config.Resolver.HandoffBucketName, 0)
	if err != nil {
		return fmt.Errorf("ensure handoff KV bucket %s: %w", wc.config.Resolver.HandoffBucketName, err)
	}

	resolver := NewClaimBasedResolver(kv, wc.config.Resolver.HandoffClaimsPrefix, wc.logger)
	resolver.SetBatching(wc.config.Resolver.BatchWindow, wc.config.Resolver.BatchMaxItems)
	if wc.resolverMetrics != nil {
		resolver.SetMetrics(wc.resolverMetrics)
	}

	rCtx, rCancel := context.WithCancel(context.Background())
	if err := resolver.Start(rCtx); err != nil {
		rCancel()
		return fmt.Errorf("start claim resolver: %w", err)
	}

	wc.gateResolver = resolver
	wc.gateResolverCancel = rCancel
	wc.logger.Info("claim-based resolver started successfully")

	return nil
}

// stopGateResolver stops the automatic claim-based resolver if it is running.
func (wc *WorkerConsumer) stopGateResolver() {
	wc.gateResolverMu.Lock()
	defer wc.gateResolverMu.Unlock()

	if wc.gateResolver != nil {
		if wc.gateResolverCancel != nil {
			wc.gateResolverCancel()
			wc.gateResolverCancel = nil
		}
		if resolver, ok := wc.gateResolver.(*ClaimBasedResolver); ok {
			resolver.Stop()
		}
		wc.gateResolver = nil
	}
}

func (wc *WorkerConsumer) getEffectiveResolver() types.OwnershipResolver {
	if wc.config.Resolver.OwnershipResolver != nil {
		return wc.config.Resolver.OwnershipResolver
	}
	return wc.gateResolver
}

// SetResolverMetrics sets optional metrics for the auto-created claim resolver.
// If called before the resolver is initialized, the metrics will be applied on creation.
func (wc *WorkerConsumer) SetResolverMetrics(m ResolverMetrics) {
	wc.gateResolverMu.Lock()
	defer wc.gateResolverMu.Unlock()
	wc.resolverMetrics = m
	if r, ok := wc.gateResolver.(*ClaimBasedResolver); ok {
		r.SetMetrics(m)
	}
}

// delayWithBackoffOrExit applies jittered exponential backoff according to config.
// It returns true if the context is done during the wait.
func (wc *WorkerConsumer) delayWithBackoffOrExit(ctx context.Context, purpose string) bool {
	delay := jitterBackoff(0, wc.config.Retry.Base, wc.config.Retry.Multiplier, wc.config.Retry.Max, nil)
	wc.logger.Debug("backoff", "purpose", purpose, "delay_ms", delay.Milliseconds())
	emitControlRetry(wc.config.Metrics, purpose)
	emitRetryBackoff(wc.config.Metrics, purpose, delay.Seconds())

	select {
	case <-ctx.Done():
		return true
	case <-time.After(delay):
		return false
	}
}

// extractPartitionID returns the partition id parsed from subject using the configured
// template parts. It returns an empty string when parsing fails.
// Note: retained for test compatibility and internal usage; delegates to shared helper.
func (wc *WorkerConsumer) extractPartitionID(subject string) string {
	pid, ok := extractPartitionIDFromSubject(subject, wc.partitionPrefix, wc.partitionSuffix)
	if !ok {
		return ""
	}
	return pid
}

// maybeEscalateIteratorFailures records an iterator failure timestamp and attempts per-subject
// remediation (recreate/bind the durable) when a burst occurs within the configured window.
// Returns a new consumer when remediation performed successfully; otherwise returns nil.
func (wc *WorkerConsumer) maybeEscalateIteratorFailures(ctx context.Context, subject string, cons jetstream.Consumer) jetstream.Consumer {
	wc.iterEscMu.Lock()
	defer wc.iterEscMu.Unlock()

	window := wc.config.IteratorEscalationWindow
	if window <= 0 {
		window = DefaultIteratorEscalationWindow
	}
	threshold := wc.config.IteratorEscalationThreshold
	if threshold <= 0 {
		threshold = DefaultIteratorEscalationThreshold
	}

	now := time.Now()
	wc.iterFailureTimes = append(wc.iterFailureTimes, now)
	cutoff := now.Add(-window)
	i := 0
	for ; i < len(wc.iterFailureTimes); i++ {
		if wc.iterFailureTimes[i].After(cutoff) {
			break
		}
	}
	if i > 0 && i <= len(wc.iterFailureTimes) {
		wc.iterFailureTimes = append([]time.Time(nil), wc.iterFailureTimes[i:]...)
	}
	count := len(wc.iterFailureTimes)
	lastEsc := wc.lastEscalation
	canEscalate := count >= threshold && (lastEsc.IsZero() || now.Sub(lastEsc) >= window)
	if !canEscalate {
		return nil
	}

	if wc.config.Metrics != nil {
		wc.config.Metrics.IncrementWorkerConsumerIteratorEscalation("burst")
	}

	wc.lastEscalation = now
	wc.logger.Info("iterator escalation triggered",
		"op", "iterator_escalation",
		"subject", subject,
		"count_in_window", count,
		"threshold", threshold,
		"window_ms", window.Milliseconds(),
	)

	// Attempt remediation: check consumer info; if missing or error, recreate/rebind.
	if cons != nil {
		if _, err := cons.Info(ctx); err == nil {
			return nil
		}
	}

	durable := wc.perSubjectDurableName(wc.config.ConsumerPrefix, subject)
	newCons, err := wc.ensurePerSubjectConsumer(ctx, durable, subject)
	if err != nil {
		wc.logger.Warn("per-subject consumer remediation failed", "subject", subject, "durable", durable, "error", err)
		return nil
	}

	// Update loop reference under lock (best-effort; loop may be shutting down)
	wc.mu.Lock()
	if sl := wc.subjects[subject]; sl != nil {
		sl.consumer = newCons
	}
	wc.mu.Unlock()

	wc.logger.Info("per-subject consumer remediated", "subject", subject, "durable", durable)

	return newCons
}

// defaultIterFactory provides the default messages iterator factory.
func defaultIterFactory(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error) {
	if cons == nil {
		return nil, errors.New("consumer not initialized")
	}
	// JetStream requires PullExpiry to be at least 1s.
	if expiry > 0 && expiry < time.Second {
		expiry = time.Second
	}

	return cons.Messages(
		jetstream.PullMaxMessages(batch),
		jetstream.PullExpiry(expiry),
		jetstream.PullHeartbeat(expiry/2),
	)
}
