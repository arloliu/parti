package parti

import (
	"context"
	"fmt"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/kvbuckets"
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
	m.ctx, m.cancel = context.WithCancel(context.Background()) //nolint:gosec // G118: cancel stored in m.cancel; called by Manager.Stop
	m.mu.Unlock()

	if m.cfg.StartupTimeout > 0 {
		sctx, cancel := context.WithTimeout(ctx, m.cfg.StartupTimeout)
		return sctx, cancel, nil
	}
	// No startup timeout; return passthrough context and no-op cancel
	return ctx, func() {}, nil
}

// ensureStableIDKV ensures the StableID KV bucket exists and that its MaxAge
// matches WorkerIDTTL. The bucket relies entirely on MaxAge to expire
// abandoned claims; an operator-created bucket with a divergent MaxAge (most
// dangerously 0) is reconciled here, since ensureKVBucket is get-first and
// does not correct an existing bucket's config.
func (m *Manager) ensureStableIDKV(ctx context.Context, js jetstream.JetStream) (jetstream.KeyValue, error) {
	kv, err := m.ensureKVBucket(ctx, js, m.cfg.KVBuckets.StableIDBucket, m.cfg.WorkerIDTTL, jetstream.FileStorage)
	if err != nil {
		return nil, fmt.Errorf("failed to create stable ID KV: %w", err)
	}

	rctx, rcancel := context.WithTimeout(ctx, m.cfg.OperationTimeout)
	rErr := reconcileStableIDBucketMaxAge(rctx, js, kv, m.cfg.KVBuckets.StableIDBucket, m.cfg.WorkerIDTTL, m.logger)
	rcancel()
	if rErr != nil {
		return nil, rErr
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
	// The handoff bucket must NOT carry a MaxAge. Stable ownership claims are
	// written once and never refreshed; a bucket-level TTL would age them out
	// and permanently suppress pull-gated consumers. HandoffTTL governs only
	// the coordinator's advisory sweep TTL (handoff.Config.TTL, set below).
	handoffKV, err := m.ensureKVBucket(bctx, js, m.cfg.KVBuckets.HandoffBucket, 0, jetstream.FileStorage)
	bcancel()
	if err != nil {
		return fmt.Errorf("failed to create handoff KV: %w", err)
	}
	m.logger.Debug("startup: ensured KV bucket", "bucket", m.cfg.KVBuckets.HandoffBucket, "elapsed", time.Since(start))

	// Heal a handoff bucket created by an older parti version (or other tooling)
	// that still carries a MaxAge — clear it so stable claims never expire.
	rctx, rcancel := context.WithTimeout(startupCtx, m.cfg.OperationTimeout)
	rErr := reconcileHandoffBucketMaxAge(rctx, js, handoffKV, m.cfg.KVBuckets.HandoffBucket, m.logger)
	rcancel()
	if rErr != nil {
		return rErr
	}

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
	cfg := kvbuckets.BuildKeyValueConfig(bucket, ttl, storage)

	// Use retry logic to handle concurrent creation
	const maxRetries = 5
	kv, err := kvutil.EnsureKVBucketWithRetry(ctx, js, cfg, maxRetries)
	if err != nil {
		return nil, fmt.Errorf("failed to create/open KV bucket %s: %w", bucket, err)
	}

	m.warnOnStorageMismatch(ctx, kv, bucket, storage)

	return kv, nil
}

// kvStreamUpdate applies a JetStream stream config update. It is a
// package-level indirection so the MaxAge reconcilers' fail-loud branches are
// unit-testable without configuring NATS account permissions. Production code
// never reassigns it.
var kvStreamUpdate = func(ctx context.Context, js jetstream.JetStream, cfg jetstream.StreamConfig) error {
	_, err := js.UpdateStream(ctx, cfg)
	return err
}

// kvStreamConfig returns the JetStream stream config backing a KV bucket. It
// returns an error when the bucket status cannot be read or introspected.
func kvStreamConfig(ctx context.Context, kv jetstream.KeyValue) (jetstream.StreamConfig, error) {
	status, err := kv.Status(ctx)
	if err != nil {
		return jetstream.StreamConfig{}, fmt.Errorf("read KV bucket status: %w", err)
	}
	bs, ok := status.(*jetstream.KeyValueBucketStatus)
	if !ok || bs.StreamInfo() == nil {
		return jetstream.StreamConfig{}, fmt.Errorf("KV bucket status %T is not introspectable", status)
	}

	return bs.StreamInfo().Config, nil
}

// reconcileHandoffBucketMaxAge clears a non-zero MaxAge on an already-existing
// handoff KV bucket. A handoff bucket created by an older parti version carries
// MaxAge == HandoffTTL, which expires stable ownership claims and permanently
// suppresses pull-gated consumers. Bucket creation is get-first, so opening
// such a bucket does not fix it — this does.
//
// It returns nil only when the bucket is positively confirmed to carry no
// MaxAge — either it already had none, or the update to clear it succeeded.
// It returns an actionable error (so Manager.Start fails loudly rather than
// continuing into a delayed, silent outage) when the bucket's MaxAge cannot be
// verified, or when a non-zero MaxAge cannot be cleared (e.g. a least-privilege
// NATS user without stream-update permission).
func reconcileHandoffBucketMaxAge(
	ctx context.Context,
	js jetstream.JetStream,
	kv jetstream.KeyValue,
	bucket string,
	logger types.Logger,
) error {
	cfg, err := kvStreamConfig(ctx, kv)
	if err != nil {
		// The bucket was just opened, so a status failure here is unexpected.
		// Fail loud rather than silently skip: an unverified bucket may still
		// carry a MaxAge that expires stable claims.
		return fmt.Errorf(
			"handoff KV bucket %q: cannot verify MaxAge: %w — a non-zero MaxAge "+
				"expires partition ownership claims and permanently suppresses "+
				"pull-gated consumers; retry startup, or verify the bucket has no TTL",
			bucket, err,
		)
	}
	if cfg.MaxAge == 0 {
		return nil
	}

	next := cfg
	next.MaxAge = 0
	if uerr := kvStreamUpdate(ctx, js, next); uerr != nil {
		// A concurrent worker may have reconciled the bucket already.
		if cur, rerr := kvStreamConfig(ctx, kv); rerr == nil && cur.MaxAge == 0 {
			return nil
		}

		return fmt.Errorf(
			"handoff KV bucket %q has MaxAge=%v, which expires partition ownership "+
				"claims and permanently suppresses pull-gated consumers; parti could "+
				"not clear it: %w — recreate the bucket with no TTL (e.g. via "+
				"partictl) or grant this NATS user stream-update permission",
			bucket, cfg.MaxAge, uerr,
		)
	}

	if logger != nil {
		logger.Info("cleared stale MaxAge on handoff KV bucket",
			"bucket", bucket, "previous_max_age", cfg.MaxAge)
	}

	return nil
}

// reconcileStableIDBucketMaxAge aligns an already-existing stableID KV bucket's
// MaxAge to wantMaxAge (the configured WorkerIDTTL). The stableID bucket relies
// entirely on MaxAge to expire abandoned claims: a worker that crashes without
// releasing leaves its key behind, and only the bucket TTL frees the ID for
// reuse. An operator-created bucket with MaxAge=0 (unlimited) silently disables
// that — every ungraceful restart then leaks a worker ID until the pool is
// exhausted. Bucket creation is get-first, so opening such a bucket does not
// fix it — this does.
//
// The update also clamps the backing stream's Duplicates window to wantMaxAge
// when it would otherwise exceed it — JetStream rejects an UpdateStream whose
// Duplicates window is larger than MaxAge.
//
// It returns nil only when the bucket is positively confirmed to carry
// MaxAge == wantMaxAge — either it already did, or the update to align it
// succeeded. It returns an actionable error (so Manager.Start fails loudly
// rather than continuing into a delayed worker-ID leak) when the bucket's
// MaxAge cannot be verified, or when a divergent MaxAge cannot be corrected
// (e.g. a least-privilege NATS user without stream-update permission).
func reconcileStableIDBucketMaxAge(
	ctx context.Context,
	js jetstream.JetStream,
	kv jetstream.KeyValue,
	bucket string,
	wantMaxAge time.Duration,
	logger types.Logger,
) error {
	cfg, err := kvStreamConfig(ctx, kv)
	if err != nil {
		// The bucket was just opened, so a status failure here is unexpected.
		// Fail loud rather than silently skip: an unverified bucket may carry
		// MaxAge=0 and leak a worker ID on every ungraceful restart.
		return fmt.Errorf(
			"stableID KV bucket %q: cannot verify MaxAge: %w — a MaxAge that "+
				"differs from WorkerIDTTL leaks stable worker IDs on ungraceful "+
				"restart; retry startup, or verify the bucket TTL matches WorkerIDTTL",
			bucket, err,
		)
	}
	if cfg.MaxAge == wantMaxAge {
		return nil
	}

	next := cfg
	next.MaxAge = wantMaxAge
	// A KV bucket's backing stream carries a Duplicates window — 2m by default
	// for a bucket created with no TTL. JetStream rejects UpdateStream with
	// "duplicates window can not be larger than max age" (err 10052) whenever
	// Duplicates > MaxAge, so Duplicates must be clamped in the same call.
	// This matches what CreateKeyValue(TTL=wantMaxAge) itself produces: a
	// TTL'd KV bucket is created with Duplicates == MaxAge. (Clearing MaxAge to
	// 0 — what the handoff reconciler does — needs no clamp, since 0 means
	// "unlimited" and no Duplicates value can exceed it.)
	if next.Duplicates > wantMaxAge {
		next.Duplicates = wantMaxAge
	}
	if uerr := kvStreamUpdate(ctx, js, next); uerr != nil {
		// A concurrent worker may have reconciled the bucket already.
		if cur, rerr := kvStreamConfig(ctx, kv); rerr == nil && cur.MaxAge == wantMaxAge {
			return nil
		}

		return fmt.Errorf(
			"stableID KV bucket %q has MaxAge=%v, which differs from WorkerIDTTL=%v "+
				"and leaks stable worker IDs on ungraceful restart; parti could not "+
				"correct it: %w — recreate the bucket with TTL=WorkerIDTTL (e.g. via "+
				"partictl) or grant this NATS user stream-update permission",
			bucket, cfg.MaxAge, wantMaxAge, uerr,
		)
	}

	if logger != nil {
		logger.Info("reconciled MaxAge on stableID KV bucket",
			"bucket", bucket, "previous_max_age", cfg.MaxAge, "new_max_age", wantMaxAge)
	}

	return nil
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
	cfg, err := kvStreamConfig(ctx, kv)
	if err != nil {
		return
	}
	got := cfg.Storage
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

// resolverReconcileDefault mirrors durable.defaultReconcileInterval. It is
// duplicated here (rather than imported) because the internal/durable package
// is not part of the manager's public dependency set and a private numeric
// constant is sufficient context for the warning threshold.
const resolverReconcileDefault = 30 * time.Second

// warnOnShortAuditGrace emits a one-shot WARN when two-phase handoff is
// enabled and the effective audit grace (5 × HeartbeatTTL) is shorter than
// the claim resolver's default reconcile cadence. See manager.Start for the
// motivating failure mode (silent KV watcher stall after a NATS server
// restart).
func (m *Manager) warnOnShortAuditGrace() {
	if !m.cfg.EnableTwoPhaseHandoff {
		return
	}
	auditGrace := 5 * m.cfg.HeartbeatTTL
	if auditGrace >= resolverReconcileDefault {
		return
	}
	m.logger.Warn(
		"audit grace (5 × HeartbeatTTL) is shorter than the default claim "+
			"resolver reconcile interval (30s); after a silent watcher stall "+
			"the leader can escalate audit_repair before the worker's resolver "+
			"cache has recovered. Set consumer.ResolverConfig.ReconcileInterval "+
			"to at most HeartbeatTTL to close this gap.",
		"heartbeat_ttl", m.cfg.HeartbeatTTL,
		"audit_grace", auditGrace,
		"resolver_reconcile_default", resolverReconcileDefault,
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
