package parti

import (
	"context"
	"fmt"
	"time"

	"github.com/arloliu/parti/internal/assignment/handoff"
	"github.com/arloliu/parti/internal/kvutil"
	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go/jetstream"
)

// prepareStart sets up the manager lifetime context and returns a startup-scoped context
// honoring StartupTimeout. Caller must defer the returned cancel.
func (m *Manager) prepareStart(ctx context.Context) (context.Context, context.CancelFunc, error) {
	m.mu.Lock()
	if m.ctx != nil {
		m.mu.Unlock()
		return nil, func() {}, types.ErrAlreadyStarted
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.mu.Unlock()

	if m.cfg.StartupTimeout > 0 {
		sctx, cancel := context.WithTimeout(ctx, m.cfg.StartupTimeout)
		return sctx, cancel, nil
	}
	// No startup timeout; return passthrough context and no-op cancel
	return ctx, func() {}, nil
}

// ensureStableIDKV ensures the StableID KV bucket exists.
func (m *Manager) ensureStableIDKV(ctx context.Context, js jetstream.JetStream) (jetstream.KeyValue, error) {
	kv, err := m.ensureKVBucket(ctx, js, m.cfg.KVBuckets.StableIDBucket, m.cfg.WorkerIDTTL)
	if err != nil {
		return nil, fmt.Errorf("failed to create stable ID KV: %w", err)
	}
	return kv, nil
}

// ensureCoreKVBuckets ensures election, heartbeat, and assignment KV buckets.
func (m *Manager) ensureCoreKVBuckets( //nolint:revive
	startupCtx context.Context,
	js jetstream.JetStream,
) (
	electionKV jetstream.KeyValue,
	heartbeatKV jetstream.KeyValue,
	assignmentKV jetstream.KeyValue, err error,
) {
	ensure := func(label, bucket string, ttl time.Duration) (jetstream.KeyValue, error) {
		bctx, bcancel := context.WithTimeout(startupCtx, m.cfg.OperationTimeout)
		start := time.Now()
		kv, err := m.ensureKVBucket(bctx, js, bucket, ttl)
		bcancel()
		if err != nil {
			return nil, fmt.Errorf("failed to create %s KV: %w", label, err)
		}
		m.logger.Debug("startup: ensured KV bucket", "bucket", bucket, "ttl", ttl, "elapsed", time.Since(start))

		return kv, nil
	}

	electionKV, err = ensure("election", m.cfg.KVBuckets.ElectionBucket, m.cfg.ElectionTimeout)
	if err != nil {
		return nil, nil, nil, err
	}
	heartbeatKV, err = ensure("heartbeat", m.cfg.KVBuckets.HeartbeatBucket, m.cfg.HeartbeatTTL)
	if err != nil {
		return nil, nil, nil, err
	}
	assignmentKV, err = ensure("assignment", m.cfg.KVBuckets.AssignmentBucket, m.cfg.KVBuckets.AssignmentTTL)
	if err != nil {
		return nil, nil, nil, err
	}

	return electionKV, heartbeatKV, assignmentKV, nil
}

// setupHandoff wires the handoff coordinator and performs hygiene/resume detection.
func (m *Manager) setupHandoff(startupCtx context.Context, js jetstream.JetStream) error {
	bctx, bcancel := context.WithTimeout(startupCtx, m.cfg.OperationTimeout)
	start := time.Now()
	handoffKV, err := m.ensureKVBucket(bctx, js, m.cfg.KVBuckets.HandoffBucket, m.cfg.KVBuckets.HandoffTTL)
	bcancel()
	if err != nil {
		return fmt.Errorf("failed to create handoff KV: %w", err)
	}
	m.logger.Debug("startup: ensured KV bucket", "bucket", m.cfg.KVBuckets.HandoffBucket, "ttl", m.cfg.KVBuckets.HandoffTTL, "elapsed", time.Since(start))

	store := handoff.NewNATSClaimStore(handoffKV, "claims/")
	m.handoffCoordinator = handoff.New(handoff.Config{
		ConsumerUpdater:   m.consumerUpdater,
		Metrics:           m.handoffMetrics,
		Store:             store,
		TTL:               m.cfg.KVBuckets.HandoffTTL,
		SweepInterval:     m.cfg.Handoff.SweepInterval,
		MaxRetries:        m.cfg.Handoff.MaxRetries,
		BaseBackoff:       m.cfg.Handoff.BaseBackoff,
		MaxBackoff:        m.cfg.Handoff.MaxBackoff,
		Jitter:            m.cfg.Handoff.Jitter,
		DelayAfterPrepare: m.cfg.Handoff.DelayAfterPrepare,
		DelayBeforeStable: m.cfg.Handoff.DelayBeforeStable,
		Logger:            m.logger,
	}, true)

	// Hygiene + resumable detection
	hctx, hcancel := context.WithTimeout(startupCtx, m.cfg.OperationTimeout)
	m.logger.Debug("startup: handoff hygiene start")
	resumable := m.handoffStartupHygiene(hctx, store)
	hcancel()
	m.logger.Debug("startup: handoff hygiene done", "resumable", resumable)
	if resumable {
		m.pendingHandoffResume.Store(true)
	}

	return nil
}

// ensureKVBucket creates or opens a KV bucket with the specified TTL.
//
// Uses retry logic to handle race conditions when multiple workers
// try to create the same bucket concurrently.
func (m *Manager) ensureKVBucket(ctx context.Context, js jetstream.JetStream, bucket string, ttl time.Duration) (jetstream.KeyValue, error) {
	cfg := jetstream.KeyValueConfig{
		Bucket:  bucket,
		History: 1, // Keep only latest value
	}

	if ttl > 0 {
		cfg.TTL = ttl
	}

	// Use retry logic to handle concurrent creation
	const maxRetries = 5
	kv, err := kvutil.EnsureKVBucketWithRetry(ctx, js, cfg, maxRetries)
	if err != nil {
		return nil, fmt.Errorf("failed to create/open KV bucket %s: %w", bucket, err)
	}

	return kv, nil
}
