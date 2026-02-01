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
	"github.com/zeebo/xxh3"

	"github.com/arloliu/parti/jsutil"
	"github.com/arloliu/parti/kvutil"
	"github.com/arloliu/parti/types"
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
//   - UpdateWorkerConsumer may block up to DrainOnRemoveTimeout (default 10s) when
//     removing subjects with DrainOnRemove enabled
//   - Close may block up to the context deadline waiting for active pull loops to stop
type WorkerConsumer struct {
	conn    *nats.Conn
	js      jetstream.JetStream
	config  WorkerConsumerConfig
	logger  types.Logger
	handler MessageHandler

	// parsed subject template for subject generation
	subjectTemplate *template.Template

	// template for deriving subject strings lives in config; we reuse the same SubjectTemplate

	mu       sync.RWMutex
	updateMu sync.Mutex // Serializes UpdateWorkerConsumer calls

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

	// reverse subject template components for partitionID extraction (prefix/suffix around {{.PartitionID}})
	partitionPrefix string
	partitionSuffix string
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
		subjects:        make(map[string]*partitionConsumer, 64),
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
	// Serialize updates to prevent race conditions (e.g. lost assignments or zombie consumers)
	wc.updateMu.Lock()
	defer wc.updateMu.Unlock()

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
	// Serialize updates to prevent race conditions during shutdown
	wc.updateMu.Lock()
	defer wc.updateMu.Unlock()

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
		// give up waiting on this loop but continue cleanup for others
		wc.logger.Warn("timeout waiting for subject loops to stop",
			"count", len(loops),
			"timeout", waitTimeout,
		)
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

	pcConfig := partitionConsumerConfig{
		BatchSize:                   wc.config.BatchSize,
		FetchTimeout:                wc.config.FetchTimeout,
		ManualAck:                   wc.config.ManualAck,
		Retry:                       wc.config.Retry,
		IteratorEscalationWindow:    wc.config.IteratorEscalationWindow,
		IteratorEscalationThreshold: wc.config.IteratorEscalationThreshold,
		Metrics:                     wc.config.Metrics,
	}

	consumerConfig := jetstream.ConsumerConfig{
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

	go pc.Run(context.Background(), effectiveHandler)

	return nil
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

	return jsutil.EnsureConsumer(ctx, wc.js, wc.config.StreamName, cfg)
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
		for _, s := range wc.config.ProcessingGate.AllowedStates {
			if state == s {
				allowed = true
				break
			}
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

// defaultIterFactory provides the default messages iterator factory.
func defaultIterFactory(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error) {
	return cons.Messages(jetstream.PullMaxMessages(batch), jetstream.PullExpiry(expiry))
}
