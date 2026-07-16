package durable

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/arloliu/parti/v2/internal/dynamicbuild"
	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/arloliu/parti/v2/internal/ratelimit"
	"github.com/arloliu/parti/v2/jsutil"
	"github.com/arloliu/parti/v2/kvutil"
	"github.com/arloliu/parti/v2/types"
)

// WorkerConsumer manages one JetStream durable pull consumer per subject (partition).
// It preserves per-subject cursors across reassignments and never deletes durables; idle
// consumers are reclaimed by JetStream via InactiveThreshold.
//
// Thread Safety:
//   - UpdateWorkerConsumer is serialized; concurrent calls block on an internal mutex
//   - Close is safe to call concurrently with UpdateWorkerConsumer
//   - All other public methods are safe for concurrent use
//
// Blocking Behavior:
//   - UpdateWorkerConsumer may block up to ~2× DrainOnRemoveTimeout (default 10s)
//     when removing subjects with DrainOnRemove enabled: one budget for draining
//     buffered messages, then a separate equal wait for the pull loops to stop.
//     If the loops have not stopped within the second bound, UpdateWorkerConsumer
//     returns an error; map entries are still cleared so the caller (the manager)
//     can retry and converge. An in-flight handler invocation may still run to
//     completion — this is best-effort, not a zero-overlap guarantee.
//   - Close may block up to the context deadline waiting for active pull loops to stop
//
// Lifecycle (Close semantics):
//   - Close is terminal: after Close returns, UpdateWorkerConsumer returns
//     [types.ErrConsumerStopped]. The gate resolver is permanently torn down
//     by Close (it is not re-initialized on a subsequent call), so a post-Close
//     update would resurrect loops without the configured processing gate — a
//     silent safety downgrade. Create a new WorkerConsumer to consume again.
//   - Close is idempotent; repeated calls return nil.
type WorkerConsumer struct {
	conn    *nats.Conn
	js      jetstream.JetStream
	config  WorkerConsumerConfig
	logger  types.Logger
	handler messageHandler

	// parsed subject template for subject generation
	subjectTemplate *template.Template

	// template for deriving subject strings lives in config; we reuse the same SubjectTemplate

	mu       sync.RWMutex
	updateMu sync.Mutex // Serializes UpdateWorkerConsumer calls

	closed bool // set by Close; terminal — UpdateWorkerConsumer returns ErrConsumerStopped after this

	workerID string

	// per-subject state
	subjects map[string]*partitionConsumer

	// optional processing gate (claim-based resolver)
	gateResolver       types.OwnershipResolver
	gateResolverMu     sync.Mutex
	gateResolverCancel context.CancelFunc
	resolverMetrics    ResolverMetrics

	// iterator factory
	iterFactory func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error)

	// limiter gates every physical CreateOrUpdateConsumer attempt; nil = unlimited.
	limiter ratelimit.Limiter

	// reverse subject template components for partitionID extraction (prefix/suffix around {{.PartitionID}})
	partitionPrefix string
	partitionSuffix string

	// gateWired is flipped to true after the first successful processing-gate
	// handler wrap in addSubjectLoop. Monotonic: never cleared once set. Read
	// by Capabilities() to report types.CapProcessingGate.
	gateWired atomic.Bool
}

// NewWorkerConsumer creates a new per-subject durable consumer helper.
func NewWorkerConsumer(js jetstream.JetStream, cfg WorkerConsumerConfig, fn func(context.Context, jetstream.Msg) error) (*WorkerConsumer, error) {
	if js == nil {
		return nil, errors.New("JetStream context is required")
	}
	if fn == nil {
		return nil, errors.New("message handler is required")
	}

	handler := messageHandlerFunc(fn)

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
		subjects:        make(map[string]*partitionConsumer, 64),
		iterFactory:     makeDefaultIterFactory(cfg.PullHeartbeatCap),
		subjectTemplate: tmpl,
		partitionPrefix: prefix,
		partitionSuffix: suffix,
		limiter:         cfg.ConsumerCreateLimiter,
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
	// Serialize updates to prevent race conditions (e.g. lost assignments or zombie consumers)
	wc.updateMu.Lock()
	defer wc.updateMu.Unlock()

	if wc.closed {
		return fmt.Errorf("worker consumer: %w", types.ErrConsumerStopped)
	}

	if err := wc.validateUpdateParams(workerID); err != nil {
		return err
	}

	subjects, err := wc.buildSubjects(partitions)
	if err != nil {
		return err
	}

	// Enforce the subject cap BEFORE any mutation (workerID store, removals,
	// adds). Failing the whole update keeps the manager's apply pipeline
	// honest: the apply fails pre-commit and retries with backoff, the
	// two-phase removal guard keeps the previous owner consuming, and the
	// un-acked heartbeat makes the over-capped worker visible to the leader.
	// A partial apply (skipping excess subjects) must never report success —
	// ownership of a skipped partition would commit with no loop started.
	if maxSubjects := wc.config.MaxConcurrentSubjects; maxSubjects > 0 && len(subjects) > maxSubjects {
		if wc.config.Metrics != nil {
			wc.config.Metrics.IncrementWorkerConsumerSubjectThresholdWarning()
			wc.config.Metrics.IncrementWorkerConsumerGuardrailViolation("max_subjects")
		}
		return fmt.Errorf("stream %s: %d subjects over cap %d: %w",
			wc.config.StreamName, len(subjects), maxSubjects, ErrMaxSubjectsExceeded)
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
//
// Close is terminal. After Close returns, any subsequent call to [UpdateWorkerConsumer]
// returns [types.ErrConsumerStopped]. Create a new [WorkerConsumer] to consume again.
//
// Close is idempotent; calling it multiple times is safe.
func (wc *WorkerConsumer) Close(ctx context.Context) error {
	// Serialize updates to prevent race conditions during shutdown
	wc.updateMu.Lock()
	defer wc.updateMu.Unlock()

	// Mark closed before stopping loops so concurrent callers observe the
	// terminal flag immediately. Idempotent: repeated Close returns nil.
	wc.closed = true

	wc.stopGateResolver()

	// Snapshot current loops under lock
	wc.mu.Lock()
	type kv struct {
		s string
		l *partitionConsumer
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
		it.l.Stop()
	}

	// Wait for all to finish with context respect
	doneCh := make(chan struct{})
	go func() {
		for _, it := range loops {
			it.l.Wait()
		}
		close(doneCh)
	}()

	var err error
	select {
	case <-doneCh:
		// All loops stopped successfully
	case <-ctx.Done():
		err = ctx.Err()
	}

	// Clean map entries under lock regardless of wait outcome
	wc.mu.Lock()
	for _, it := range loops {
		delete(wc.subjects, it.s)
	}
	wc.mu.Unlock()

	return err
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
	loops := make([]*partitionConsumer, 0, len(toRemove))
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
			l.Drain(dctx)
		}
	}

	// Cancel outside lock after optional drain, and wait with a safety timeout per loop
	for _, l := range loops {
		l.Stop()
	}

	// Wait for completion with a bounded timeout per loop to avoid deadlocks
	waitTimeout := wc.config.DrainOnRemoveTimeout
	if waitTimeout <= 0 {
		waitTimeout = 10 * time.Second
	}

	var err error
	doneCh := make(chan struct{})
	go func() {
		for _, l := range loops {
			l.Wait()
		}
		close(doneCh)
	}()

	select {
	case <-doneCh:
		// loops finished normally
	case <-ctx.Done():
		// caller's context canceled; stop waiting
		err = ctx.Err()
	case <-time.After(waitTimeout):
		// Surface the timeout so the caller's apply fails pre-commit and is
		// retried with backoff. This delays the handoff commit by at least
		// one retry cycle and makes the stall observable; it does NOT
		// guarantee the old handler has finished — Stop only cancels the
		// loop context, and an in-flight handler invocation runs to
		// completion. Entries are still deleted below: a retained-but-
		// stopped entry would make a later re-add a silent no-op.
		wc.logger.Warn("timeout waiting for subject loops to stop",
			"count", len(loops),
			"timeout", waitTimeout,
		)
		err = fmt.Errorf("timed out after %v waiting for %d subject loops to stop", waitTimeout, len(loops))
	}

	// Delete keys under lock regardless of wait outcome to prevent zombie entries
	wc.mu.Lock()
	for _, s := range toRemove {
		delete(wc.subjects, s)
	}
	wc.mu.Unlock()

	return err
}

func (wc *WorkerConsumer) addSubjectLoop(ctx context.Context, workerID string, subject string) error {
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
		// Monotonic: once any subject has been wrapped with the gate, the
		// bit stays set even if a later per-subject create fails. Reported
		// out via Capabilities() so the Manager can advertise
		// types.CapProcessingGate to the leader.
		wc.gateWired.Store(true)
	}

	partitionID := wc.extractPartitionID(subject)

	pcConfig := partitionConsumerConfig{
		BatchSize:                   wc.config.BatchSize,
		FetchTimeout:                wc.config.FetchTimeout,
		ManualAck:                   wc.config.ManualAck,
		RecoveryStrategy:            wc.config.RecoveryStrategy,
		Retry:                       wc.config.Retry,
		IteratorEscalationWindow:    wc.config.IteratorEscalationWindow,
		IteratorEscalationThreshold: wc.config.IteratorEscalationThreshold,
		RecoveryRetry:               wc.config.RecoveryRetry,
		OnPermanentFailure:          wc.config.OnPermanentFailure,
		StreamMissingHook:           wc.config.StreamMissingHook,
		OnStreamRecreated:           wc.config.OnStreamRecreated,
		Metrics:                     wc.config.Metrics,
		ConsumerCreateLimiter:       wc.limiter,
	}

	consumerConfig := dynamicbuild.ConsumerConfig(durable, subject, wc.defaults())

	pc := newPartitionConsumer(
		wc.logger,
		wc.js,
		pcConfig,
		partitionConsumerOpts{
			streamName:     wc.config.StreamName,
			durableName:    durable,
			subject:        subject,
			partitionID:    partitionID,
			consumerConfig: consumerConfig,
			consumer:       cons,
			iterFactory:    wc.iterFactory,
			checkPullSuppression: func(ctx context.Context) (bool, string) {
				return wc.shouldSuppressPull(partitionID)
			},
		},
	)

	wc.mu.Lock()
	wc.subjects[subject] = pc
	wc.mu.Unlock()

	// partitionConsumer has its own Stop() method that cancels an internal
	// context; using a request-scoped ctx here would kill the subscription
	// as soon as the caller's request returns.
	go pc.Run(context.Background(), effectiveHandler) //nolint:gosec // G118: lifecycle managed by partitionConsumer.Stop

	return nil
}

// ensurePerSubjectConsumer creates or updates a per-subject durable with FilterSubject.
// It employs a short retry strategy to handle transient NATS errors.
// When a limiter is configured, it gates every physical RPC attempt via EnsureConsumerWithOptions.
func (wc *WorkerConsumer) ensurePerSubjectConsumer(ctx context.Context, durable string, subject string) (jetstream.Consumer, error) {
	cfg := dynamicbuild.ConsumerConfig(durable, subject, wc.defaults())
	if wc.limiter == nil {
		return jsutil.EnsureConsumer(ctx, wc.js, wc.config.StreamName, cfg)
	}
	return jsutil.EnsureConsumerWithOptions(
		ctx,
		wc.js,
		wc.config.StreamName,
		cfg,
		jsutil.WithBeforeAttempt(wc.limiter.Wait),
	)
}

// defaults captures the runtime tunables that feed dynamicbuild.ConsumerConfig.
// Keeping this in one place ensures the two callsites (ensurePerSubjectConsumer
// and addSubjectLoop) cannot drift from each other.
func (wc *WorkerConsumer) defaults() dynamicbuild.Defaults {
	return dynamicbuild.Defaults{
		AckPolicy:             wc.config.AckPolicy,
		AckWait:               wc.config.AckWait,
		MaxDeliver:            wc.config.MaxDeliver,
		InactiveThreshold:     wc.config.InactiveThreshold,
		MaxWaiting:            wc.config.MaxWaiting,
		MaxAckPending:         wc.config.MaxAckPending,
		ConsumerMemoryStorage: wc.config.ConsumerMemoryStorage,
		ConsumerReplicas:      wc.config.ConsumerReplicas,
	}
}

// perSubjectDurableName returns a stable, sanitized durable for a given subject.
// Delegates to internal/dynamicbuild so the runtime and provision SDK share
// one implementation.
func (wc *WorkerConsumer) perSubjectDurableName(prefix, subject string) string {
	return dynamicbuild.PerSubjectDurableName(prefix, subject, wc.partitionPrefix, wc.partitionSuffix)
}

// sanitizeConsumerName sanitizes a consumer name according to allowed runes.
// Kept as a package-level wrapper so cross-file callers (broadcast_consumer.go)
// and tests (worker_consumer_test.go) continue to work unchanged.
func sanitizeConsumerName(name string) string {
	return dynamicbuild.SanitizeConsumerName(name)
}

// isAllowedConsumerRune reports whether r is allowed in a NATS consumer name.
// Wrapper around dynamicbuild.IsAllowedConsumerRune for cross-file callers
// (broadcast_config.go, config.go).
func isAllowedConsumerRune(r rune) bool {
	return dynamicbuild.IsAllowedConsumerRune(r)
}

// buildSubjects generates a sorted, deduplicated list of subjects from partitions.
// Delegates to internal/dynamicbuild for the shared pure-helper implementation;
// the wrapper exists so callers and tests can continue to invoke it on the
// receiver and so wc.subjectTemplate stays populated for downstream use.
func (wc *WorkerConsumer) buildSubjects(partitions []types.Partition) ([]string, error) {
	// Ensure we have a parsed template even if constructed manually in tests.
	if wc.subjectTemplate == nil {
		t, err := template.New("subject").Parse(wc.config.SubjectTemplate)
		if err != nil {
			return nil, fmt.Errorf("parse subject template: %w", err)
		}
		wc.subjectTemplate = t
	}

	return dynamicbuild.BuildSubjects(wc.config.SubjectTemplate, partitions)
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

	// ReconcileInterval is normalised to the 30s default by
	// WorkerConsumerConfig.SetDefaults via the `default:"30s"` struct tag,
	// so the value is always positive here. The resolver-package contract
	// is preserved: direct callers of NewClaimBasedResolver who pass 0 via
	// WithReconcileInterval still get polling disabled.

	// Dedicated probe handle for the reconcile scan gate. The gate must
	// never probe through the resolver's production handle (kv.Status
	// mutates shared *stream state under concurrent Get/Watch — the
	// epoch-monitor race class); a missing probe handle just leaves
	// reconcile ungated, the pre-gate behavior.
	resolverOpts := make([]ResolverOption, 0, 2)
	resolverOpts = append(resolverOpts, WithReconcileInterval(wc.config.Resolver.ReconcileInterval))
	probeKV, perr := wc.js.KeyValue(ctx, wc.config.Resolver.HandoffBucketName)
	if perr != nil {
		wc.logger.Debug("reconcile scan gate disabled: probe handle unavailable", "error", perr)
	}
	// WithStreamPosProbe is a no-op on a nil handle, so an unavailable
	// probe leaves the gate inert (full scans, the pre-gate behavior).
	resolverOpts = append(resolverOpts, WithStreamPosProbe(probeKV))
	resolver := NewClaimBasedResolver(
		kv,
		wc.config.Resolver.HandoffClaimsPrefix,
		wc.logger,
		resolverOpts...,
	)
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

	wc.gateResolverMu.Lock()
	defer wc.gateResolverMu.Unlock()

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

// Capabilities reports the runtime capability bits this consumer has
// successfully wired. Currently:
//   - types.CapProcessingGate: set after the first successful handler
//     wrap via newProcessingGate(...) + g.Wrap in addSubjectLoop.
//
// Safe for concurrent use; non-blocking (atomic load); monotonic — once
// a bit has been set it remains set for the lifetime of this
// WorkerConsumer, even if a later per-subject create fails. The bit
// reflects "this consumer has at least one wired component", not "all
// components are currently wired".
//
// Returns:
//   - uint32: OR of capability bits successfully wired; 0 if none.
func (wc *WorkerConsumer) Capabilities() uint32 {
	var bits uint32
	if wc.gateWired.Load() {
		bits |= types.CapProcessingGate
	}
	return bits
}

// ConsumerCreateLimiter returns the configured rate limiter, or nil when none
// has been configured. Used by tests and by consumer.Dynamic to inspect the
// resolved limiter after option processing.
func (wc *WorkerConsumer) ConsumerCreateLimiter() ratelimit.Limiter {
	return wc.limiter
}

// Closed reports whether this consumer has been closed.
//
// Used by the public Dynamic wrapper to give ErrConsumerStopped precedence
// over the WorkQueue compatibility preflight.
func (wc *WorkerConsumer) Closed() bool {
	wc.updateMu.Lock()
	defer wc.updateMu.Unlock()
	return wc.closed
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

// shouldSuppressPull checks if pulling should be suppressed for a partition.
func (wc *WorkerConsumer) shouldSuppressPull(partitionID string) (bool, string) {
	if !wc.config.PullGatingEnabled {
		return false, ""
	}

	resolver := wc.getEffectiveResolver()
	if resolver == nil {
		return false, ""
	}

	wc.mu.RLock()
	currentWorkerID := wc.workerID
	wc.mu.RUnlock()

	// Check ownership status
	owner, state, _, ok := resolver.GetOwner(partitionID)
	if !ok {
		wc.logger.Warn("pull gating resolve failed: partition not found", "partition", partitionID)
		return true, "resolve_error"
	}

	// Only pull if we are the owner and state is allowed
	if owner != currentWorkerID {
		return true, fmt.Sprintf("not_owner(owner=%s)", owner)
	}

	allowed := false
	if wc.config.ProcessingGate != nil {
		if slices.Contains(wc.config.ProcessingGate.AllowedStates, state) {
			allowed = true
		}
	} else {
		// Fallback if ProcessingGate is not configured but PullGatingEnabled is true
		allowed = (state == types.HandoffStateStable)
	}

	if !allowed {
		return true, fmt.Sprintf("state_not_allowed(%v)", state)
	}

	return false, ""
}

// makeDefaultIterFactory returns the default messages iterator factory, with
// PullHeartbeat derived by natsutil.DerivePullHeartbeat from the expiry
// passed at call time and heartbeatCap fixed at construction time.
func makeDefaultIterFactory(heartbeatCap time.Duration) func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error) {
	return func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error) {
		heartbeat := natsutil.DerivePullHeartbeat(expiry, heartbeatCap)

		return cons.Messages(
			jetstream.PullMaxMessages(batch),
			jetstream.PullExpiry(expiry),
			jetstream.PullHeartbeat(heartbeat),
		)
	}
}

// defaultIterFactory is the uncapped default iterator factory (equivalent to
// makeDefaultIterFactory(0)), kept as a package-level value for call sites
// that construct a WorkerConsumer or BroadcastConsumer literal directly
// without a PullHeartbeatCap.
var defaultIterFactory = makeDefaultIterFactory(0)
