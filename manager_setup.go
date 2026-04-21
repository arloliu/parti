package parti

import (
	"context"
	"fmt"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/kvutil"
	"github.com/arloliu/parti/v2/types"
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
	kv, err := m.ensureKVBucket(ctx, js, m.cfg.KVBuckets.StableIDBucket, m.cfg.WorkerIDTTL, jetstream.FileStorage)
	if err != nil {
		return nil, fmt.Errorf("failed to create stable ID KV: %w", err)
	}
	return kv, nil
}

// ensureCoreKVBuckets ensures election, heartbeat, and assignment KV buckets.
//
// Storage choices are fixed per bucket to minimize PVC IOPS on file-backed
// JetStream clusters while preserving durability where it matters:
//   - election:   MemoryStorage — a lost leader key simply triggers re-election.
//   - heartbeat:  MemoryStorage — workers re-publish every HeartbeatInterval.
//   - assignment: FileStorage  — must survive NATS restart so followers
//     joining during the outage window can receive their assignment.
//
// Users who require different storage (e.g. FileStorage everywhere for
// pre-existing operational policy) can pre-create the bucket with their
// own KeyValueConfig; kvutil.EnsureKVBucketWithRetry opens existing
// buckets without inspecting their config.
func (m *Manager) ensureCoreKVBuckets( //nolint:revive
	startupCtx context.Context,
	js jetstream.JetStream,
) (
	electionKV jetstream.KeyValue,
	heartbeatKV jetstream.KeyValue,
	assignmentKV jetstream.KeyValue, err error,
) {
	ensure := func(label, bucket string, ttl time.Duration, storage jetstream.StorageType) (jetstream.KeyValue, error) {
		bctx, bcancel := context.WithTimeout(startupCtx, m.cfg.OperationTimeout)
		start := time.Now()
		kv, err := m.ensureKVBucket(bctx, js, bucket, ttl, storage)
		bcancel()
		if err != nil {
			return nil, fmt.Errorf("failed to create %s KV: %w", label, err)
		}
		m.logger.Debug("startup: ensured KV bucket", "bucket", bucket, "ttl", ttl, "storage", storageTypeName(storage), "elapsed", time.Since(start))

		return kv, nil
	}

	electionKV, err = ensure("election", m.cfg.KVBuckets.ElectionBucket, m.cfg.ElectionTimeout, jetstream.MemoryStorage)
	if err != nil {
		return nil, nil, nil, err
	}
	heartbeatKV, err = ensure("heartbeat", m.cfg.KVBuckets.HeartbeatBucket, m.cfg.HeartbeatTTL, jetstream.MemoryStorage)
	if err != nil {
		return nil, nil, nil, err
	}
	assignmentKV, err = ensure("assignment", m.cfg.KVBuckets.AssignmentBucket, m.cfg.KVBuckets.AssignmentTTL, jetstream.FileStorage)
	if err != nil {
		return nil, nil, nil, err
	}

	return electionKV, heartbeatKV, assignmentKV, nil
}

// setupHandoff wires the handoff coordinator and performs hygiene/resume detection.
func (m *Manager) setupHandoff(startupCtx context.Context, js jetstream.JetStream) error {
	bctx, bcancel := context.WithTimeout(startupCtx, m.cfg.OperationTimeout)
	start := time.Now()
	handoffKV, err := m.ensureKVBucket(bctx, js, m.cfg.KVBuckets.HandoffBucket, m.cfg.KVBuckets.HandoffTTL, jetstream.FileStorage)
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

// ensureKVBucket creates or opens a KV bucket with the specified TTL and storage type.
//
// Uses retry logic to handle race conditions when multiple workers
// try to create the same bucket concurrently.
//
// Note: if the bucket already exists, its existing storage type is honored
// (kvutil.EnsureKVBucketWithRetry is get-first). A warning is logged when the
// existing storage type does not match the requested one so operators can
// diagnose why they're not seeing the IOPS profile they expect after upgrading.
func (m *Manager) ensureKVBucket(
	ctx context.Context,
	js jetstream.JetStream,
	bucket string,
	ttl time.Duration,
	storage jetstream.StorageType,
) (jetstream.KeyValue, error) {
	cfg := jetstream.KeyValueConfig{
		Bucket:  bucket,
		History: 1, // Keep only latest value
		Storage: storage,
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

	m.warnOnStorageMismatch(ctx, kv, bucket, storage)

	return kv, nil
}

// warnOnStorageMismatch logs a warning if the existing bucket's storage type
// differs from the type parti would have created. This catches the silent
// non-upgrade path where a pre-existing file-backed bucket continues to
// absorb IOPS even after parti's defaults switched to memory storage.
func (m *Manager) warnOnStorageMismatch(
	ctx context.Context,
	kv jetstream.KeyValue,
	bucket string,
	want jetstream.StorageType,
) {
	status, err := kv.Status(ctx)
	if err != nil {
		return
	}
	bs, ok := status.(*jetstream.KeyValueBucketStatus)
	if !ok || bs.StreamInfo() == nil {
		return
	}
	got := bs.StreamInfo().Config.Storage
	if got == want {
		return
	}
	m.logger.Warn(
		"KV bucket storage type differs from parti's default — "+
			"IOPS reduction from the memory-storage default is NOT active on this bucket. "+
			"To migrate during a maintenance window: `nats kv del "+bucket+"` then restart pods.",
		"bucket", bucket,
		"existing_storage", storageTypeName(got),
		"parti_default_storage", storageTypeName(want),
	)
}

// storageTypeName renders jetstream.StorageType as a human-readable string.
// The underlying type is uint8 and logs would otherwise show opaque integers.
func storageTypeName(s jetstream.StorageType) string {
	switch s {
	case jetstream.FileStorage:
		return "file"
	case jetstream.MemoryStorage:
		return "memory"
	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}
