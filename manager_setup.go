package parti

import (
	"context"
	"fmt"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/kvbuckets"
	"github.com/arloliu/parti/v2/internal/recovery"
	"github.com/arloliu/parti/v2/kvutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
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
	m.startedAt = time.Now()
	m.mu.Unlock()

	// Install the stream-missing observer bridge on the registered
	// consumer updater BEFORE any worker-consumer interaction. A
	// stream-missing exhaustion event surfacing later in the
	// partition-consumer goroutine then reaches m.onStreamMissingError
	// instead of being silently dropped by the durable layer's
	// indirection dispatcher. Updaters that do not implement the
	// observer interface (legacy or test doubles) silently pass
	// through; the cross-feature contract is unaffected.
	if obs, ok := m.consumerUpdater.(recovery.StreamMissingObserver); ok {
		obs.SetOnStreamMissingError(m.onStreamMissingError)
	}

	if m.cfg.StartupTimeout > 0 {
		sctx, cancel := context.WithTimeout(ctx, m.cfg.StartupTimeout)
		return sctx, cancel, nil
	}
	// No startup timeout; return passthrough context and no-op cancel
	return ctx, func() {}, nil
}

// onStreamMissingError is the manager-side observer registered with
// the consumer updater (Dynamic or CompositeConsumerUpdater) during
// prepareStart. It fires exactly once per partition consumer when the
// stream-missing recovery envelope exhausts its attempt budget — i.e.
// when no StreamMissingHook is configured, the hook returned an error
// for MaxAttempts in a row, or the hook returned nil but the post-
// recreate consumer rebuild kept failing (incompatible config).
//
// Routing through m.logError preserves the existing manager wait-group
// + Hooks.OnError lifecycle semantics. enterDegraded with the named
// reason distinguishes stream-missing from generic KV-bucket loss
// (recordKVError's threshold path) so the cross-feature contract that
// whole-bucket loss is the ONLY path to "KV error threshold exceeded"
// remains intact (see AGENTS.md "Cross-feature contracts").
//
// Shutdown guard: the closure may be invoked from a partition-consumer
// goroutine OWNED BY AN EXTERNALLY-CONSTRUCTED Dynamic that outlives
// Manager.Stop. Once the manager has transitioned to StateShutdown we
// must not enqueue Hooks.OnError via m.logError (the wait-group is
// being awaited) and must not enter enterDegraded (rejected anyway,
// but skipping the call keeps the post-shutdown surface quiet).
// Manager.Stop also clears the observer via SetOnStreamMissingError(nil)
// during shutdown so the bridge is removed before m.wg.Wait, but the
// guard remains as a belt-and-braces defense in case a late fire
// races the clear.
func (m *Manager) onStreamMissingError(streamName string, cause error) {
	if m.State() == StateShutdown {
		return
	}
	m.logError(
		fmt.Sprintf("dynamic-consumer stream %q recovery exhausted", streamName),
		"stream", streamName,
		"error", cause,
	)
	// Set the latch BEFORE enterDegraded so it is visible even when the CAS
	// no-ops because the worker is ALREADY Degraded for another reason. In
	// that case the degradedRecord reason string never records the exhaustion,
	// but the latch persists and keeps the terminal hold active on the next
	// attemptRecoveryFromDegraded tick.
	m.streamMissingExhausted.Store(true)
	m.enterDegraded(DegradeReasonStreamMissingRecoveryExhausted)
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

// coreKVBuckets bundles the three always-present Parti-owned coordination
// buckets that Start opens together, so ensureCoreKVBuckets returns named
// handles instead of a positional triple (which tripped revive's result limit).
type coreKVBuckets struct {
	election   jetstream.KeyValue
	heartbeat  jetstream.KeyValue
	assignment jetstream.KeyValue
}

// ensureCoreKVBuckets ensures election, heartbeat, and assignment KV buckets.
//
// Storage choices are fixed per bucket to balance durability against PVC
// IOPS cost:
//   - election:   FileStorage   — the leadership lease key must survive a
//     NATS node restart, otherwise every restart triggers fleet-wide
//     leadership churn. The switch from MemoryStorage is effectively
//     IOPS-free at production scales (see
//     docs/plans/iops-investigation/findings.md §M1.9).
//   - heartbeat:  FileStorage   — the heartbeat stream must survive a single-node
//     NATS restart. With MemoryStorage the stream was lost on restart and the
//     fleet flapped Degraded<->Stable (the publisher Put kept failing against the
//     dead stream). The added write IOPS is a flat, partition-count-independent
//     term measured within the provisioned envelope (see
//     docs/plans/iops-investigation/findings.md and 06-deep-gap-fix-plan Task 0.1).
//   - assignment: FileStorage  — must survive NATS restart so followers
//     joining during the outage window can receive their assignment.
//
// Users who require different storage (e.g. FileStorage everywhere for
// pre-existing operational policy) can pre-create the bucket with their
// own KeyValueConfig; kvutil.EnsureKVBucketWithRetry opens existing
// buckets without inspecting their config.
func (m *Manager) ensureCoreKVBuckets(startupCtx context.Context, js jetstream.JetStream) (coreKVBuckets, error) {
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

	// Election bucket uses FileStorage (see the package comment above)
	// so the leadership lease key survives a NATS node restart.
	// Replicas remain at the server default (1) here; operators who
	// need cross-node HA pre-create the bucket with --replicas=3
	// (EnsureKVBucketWithRetry is get-first and honors the existing
	// config). See docs/OPERATIONS.md for the migration runbook.
	electionKV, err := ensure("election", m.cfg.KVBuckets.ElectionBucket, m.cfg.ElectionTimeout, jetstream.FileStorage)
	if err != nil {
		return coreKVBuckets{}, err
	}
	heartbeatKV, err := ensure("heartbeat", m.cfg.KVBuckets.HeartbeatBucket, m.cfg.HeartbeatTTL, jetstream.FileStorage)
	if err != nil {
		return coreKVBuckets{}, err
	}
	assignmentKV, err := ensure("assignment", m.cfg.KVBuckets.AssignmentBucket, m.cfg.KVBuckets.AssignmentTTL, jetstream.FileStorage)
	if err != nil {
		return coreKVBuckets{}, err
	}

	return coreKVBuckets{election: electionKV, heartbeat: heartbeatKV, assignment: assignmentKV}, nil
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
	m.handoffStore = store
	m.handoffCoordinator = handoff.New(handoff.Config{
		ConsumerUpdater:   m.consumerUpdater,
		Metrics:           m.handoffMetrics,
		Store:             store,
		RemovalGuard:      m.guardHandoffRemoval,
		TTL:               m.cfg.KVBuckets.HandoffTTL,
		SweepInterval:     m.cfg.Handoff.SweepInterval,
		MaxRetries:        m.cfg.Handoff.MaxRetries,
		BaseBackoff:       m.cfg.Handoff.BaseBackoff,
		MaxBackoff:        m.cfg.Handoff.MaxBackoff,
		Jitter:            m.cfg.Handoff.Jitter,
		DelayAfterPrepare: m.cfg.Handoff.DelayAfterPrepare,
		DelayBeforeStable: m.cfg.Handoff.DelayBeforeStable,
		PhaseConcurrency:  m.cfg.Handoff.PhaseConcurrency,
		Logger:            m.logger,
		// Orphan-claim reaping: leader-vouched live set + a generous grace.
		// See livePartitionSet and orphanClaimGrace (manager_handoff.go).
		LivePartitions: m.livePartitionSet,
		OrphanGrace:    orphanClaimGrace,
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
// Note: if the bucket already exists, its existing storage type and
// replica count are honored (kvutil.EnsureKVBucketWithRetry is
// get-first). A warning is logged when the existing storage type does
// not match the requested one so operators can diagnose why they're
// not seeing the IOPS profile they expect after upgrading.
//
// Replicas: m.cfg.KVBuckets.Replicas is stamped onto the canonical
// builder output when > 0 (mirrors the provision package's pattern —
// see internal/kvbuckets/builder.go Godoc). Zero leaves the field
// unset, which nats.go normalizes to 1 server-side (legacy behavior).
// Pre-created buckets keep their existing replica count regardless of
// this setting.
func (m *Manager) ensureKVBucket(
	ctx context.Context,
	js jetstream.JetStream,
	bucket string,
	ttl time.Duration,
	storage jetstream.StorageType,
) (jetstream.KeyValue, error) {
	cfg := kvbuckets.BuildKeyValueConfig(bucket, ttl, storage)
	if m.cfg.KVBuckets.Replicas > 0 {
		cfg.Replicas = m.cfg.KVBuckets.Replicas
	}

	// Use retry logic to handle concurrent creation
	const maxRetries = 5
	kv, err := kvutil.EnsureKVBucketWithRetry(ctx, js, cfg, maxRetries)
	if err != nil {
		return nil, fmt.Errorf("failed to create/open KV bucket %s: %w", bucket, err)
	}

	m.warnOnStorageMismatch(ctx, kv, bucket, storage)
	m.warnOnReplicasMismatch(ctx, kv, bucket)
	// Record this bucket's stream-Created timestamp so
	// monitorBucketEpochs can detect a future wipe-and-recreate event.
	// Failures are logged but never block Start.
	m.captureBucketEpoch(ctx, bucket, kv)

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

// bucketMaxAgeReconciled reports whether kv's backing stream now carries
// MaxAge == want. Both MaxAge reconcilers call it after a failed UpdateStream to
// absorb the benign race where a concurrent worker reconciled the same bucket
// first — a re-read that confirms the desired MaxAge means the goal was reached
// regardless of which worker's update won.
func bucketMaxAgeReconciled(ctx context.Context, kv jetstream.KeyValue, want time.Duration) bool {
	cur, err := kvStreamConfig(ctx, kv)
	return err == nil && cur.MaxAge == want
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
		if bucketMaxAgeReconciled(ctx, kv, 0) {
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
		if bucketMaxAgeReconciled(ctx, kv, wantMaxAge) {
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
// differs from the type parti would have created. parti opens a pre-existing
// bucket as-is ("pre-creation wins"), so this surfaces the divergence — for
// example a bucket still on MemoryStorage after parti's defaults moved to
// FileStorage — and points at the manual migration path.
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
		"KV bucket storage type differs from parti's default; parti is using the "+
			"existing bucket as-is. To align it, remove the bucket during a "+
			"maintenance window (`nats kv rm "+bucket+"`) and restart pods so parti "+
			"recreates it with the expected storage type.",
		"bucket", bucket,
		"existing_storage", storageTypeName(got),
		"parti_default_storage", storageTypeName(want),
	)
}

// warnOnReplicasMismatch logs a warning when an existing bucket's
// replica count differs from Config.KVBuckets.Replicas. Mirrors
// warnOnStorageMismatch.
//
// The library does NOT auto-reconcile Replicas (unlike MaxAge, which IS
// reconciled because it carries a correctness invariant). Replicas is a
// per-deployment HA-quality decision: silently rewriting an operator's
// existing bucket config could trigger expensive cross-node replication
// they did not ask for, or mask a deliberate downsize. The library's
// contract is "pre-creation wins" (get-first); this warning makes a
// silent divergence visible so operators can run
// `nats stream update KV_<bucket> --replicas=N` themselves if they
// want the change.
//
// Silent when Config.KVBuckets.Replicas is 0 (legacy default — no
// expectation expressed) or when the existing bucket's replica count
// matches.
func (m *Manager) warnOnReplicasMismatch(
	ctx context.Context,
	kv jetstream.KeyValue,
	bucket string,
) {
	want := m.cfg.KVBuckets.Replicas
	if want <= 0 {
		return // operator expressed no preference; nothing to compare against
	}
	cfg, err := kvStreamConfig(ctx, kv)
	if err != nil {
		return
	}
	got := cfg.Replicas
	if got == want {
		return
	}
	m.logger.Warn(
		"KV bucket replica count differs from Config.KVBuckets.Replicas — "+
			"the library does NOT auto-reconcile Replicas (pre-creation wins). "+
			"To apply the requested value run "+
			"`nats stream update KV_"+bucket+" --replicas=<N>` against your cluster.",
		"bucket", bucket,
		"existing_replicas", got,
		"requested_replicas", want,
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

// warnOnFiniteMaxReconnects emits a one-shot WARN when the caller-owned
// nats.Conn is configured with a finite MaxReconnect budget. A finite
// budget turns a sustained NATS outage into a permanently CLOSED zombie
// connection — the manager's connection monitor then enters degraded
// mode and the readiness probe rotates the pod. The recommended posture
// (documented in docs/OPERATIONS.md "NATS Client Connection") is
// nats.MaxReconnects(-1) (unlimited), which lets the client ride
// through an outage instead.
//
// The warning is read-only; finite MaxReconnect is not rejected (some
// deployments may consciously choose pod rotation on outage). The plan
// is to make the foot-gun visible at first deploy rather than during
// an actual outage.
//
// Negative MaxReconnect (-1 or any negative) is unlimited and silent.
// A nil conn is silent (defensive — test doubles that bypass the real
// JetStream surface must not panic here).
func warnOnFiniteMaxReconnects(conn *nats.Conn, logger types.Logger) {
	if conn == nil {
		return
	}
	limit := conn.Opts.MaxReconnect
	if limit < 0 {
		return // unlimited; recommended posture
	}
	logger.Warn(
		"nats.Conn is configured with a finite MaxReconnect; on a sustained "+
			"NATS outage the connection exhausts its budget, stays CLOSED, "+
			"forces degraded mode and rotates the pod. Configure with "+
			"nats.MaxReconnects(-1) plus a reasonable nats.ReconnectWait/"+
			"ReconnectJitter (see docs/OPERATIONS.md \"NATS Client Connection\").",
		"max_reconnect", limit,
	)
}

// bucketEpoch records a Parti-owned KV bucket's identity at Start for the
// epoch fence. The Created timestamp changes precisely when the bucket's
// backing stream is recreated — exactly the wipe-and-recreate case the
// fence must detect.
type bucketEpoch struct {
	kv      jetstream.KeyValue
	created time.Time
}

// captureBucketEpoch records a bucket's stream-Created timestamp into
// m.bucketEpochs for the monitorBucketEpochs goroutine to compare against.
// A failure to read the stream info at Start is logged but not fatal — the
// epoch fence simply does not protect that bucket; the existing readiness-
// degraded paths (recordKVError, connection monitor, source-unavailable
// hook) still apply. This keeps the change additive and never blocks Start.
//
// The probe stores a DEDICATED [jetstream.KeyValue] handle, separate from
// the one the rest of the manager uses. nats.go's KeyValue / stream objects
// cache state internally and are NOT documented as safe for concurrent use
// across goroutines — calling kv.Status() (which internally calls
// stream.Info()) from the monitorBucketEpochs ticker while assignment /
// heartbeat / election code paths concurrently read or write through the
// SAME handle races on the cached *stream fields. Opening a fresh handle
// via [jetstream.JetStream.KeyValue] returns a new *kvs with its own
// *stream, so the monitor goroutine never shares state with production
// paths. If the per-bucket dedicated handle cannot be opened (e.g. an
// account-permission edge), the epoch fence falls back to the bucket-not-
// protected behaviour described above.
func (m *Manager) captureBucketEpoch(ctx context.Context, bucket string, kv jetstream.KeyValue) {
	if kv == nil {
		return
	}
	if m.js == nil {
		// No JetStream context wired (tests that exercise ensureKVBucket
		// without setting m.js take this path). The epoch fence cannot
		// open its own probe handle, so disable bucket-recreate
		// detection for this bucket; production code paths always wire
		// m.js via NewManager.
		return
	}
	if m.bucketEpochs == nil {
		m.bucketEpochs = make(map[string]bucketEpoch, 5)
	}
	probeKV, err := m.js.KeyValue(ctx, bucket)
	if err != nil {
		m.logger.Warn("epoch fence: failed to open dedicated probe handle; bucket-recreate detection disabled for this bucket",
			"bucket", bucket, "error", err)
		return
	}
	created, err := kvutil.BucketStreamCreated(ctx, probeKV)
	if err != nil {
		m.logger.Warn("epoch fence: failed to capture bucket stream Created at Start; bucket-recreate detection disabled for this bucket",
			"bucket", bucket, "error", err)
		return
	}
	m.bucketEpochs[bucket] = bucketEpoch{kv: probeKV, created: created}
}

// monitorBucketEpochs polls each Parti-owned bucket's stream-Created
// timestamp on a periodic ticker. A mismatch against the cached value
// means the bucket was wiped and recreated under us; we enter degraded
// mode with reason "bucket-recreated:<bucket>" so the readiness probe
// can rotate the pod and start re-provisions the missing buckets.
//
// The poll interval is OperationTimeout (defaults to 10s; the same knob
// that bounds the ensure-bucket calls at Start). A stream-info read that
// itself fails is not degraded-mode worthy — the connection monitor,
// source-hook, and generic kvErrorCount paths handle connectivity
// errors. The epoch fence is solely concerned with the wipe-and-recreate
// case where the operation succeeds but returns a different Created.
//
// Exits when m.ctx is cancelled. Started by Start after the five Parti-
// owned buckets have been captured.
func (m *Manager) monitorBucketEpochs(ctx context.Context) {
	if len(m.bucketEpochs) == 0 {
		return
	}
	interval := m.cfg.OperationTimeout
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkBucketEpochs(ctx)
		}
	}
}

// probeBucketCreated reads bucket's live stream-Created timestamp, hard-bounded
// by a single OperationTimeout deadline that covers BOTH the handle open AND the
// status read — js.KeyValue/BucketStreamCreated each do a stream-info round-trip,
// so an unbounded ctx would let an unreachable bucket block on nats.go's ~5s
// internal default and turn the probe into an unbounded inline stall on the
// caller's goroutine. A failed probe returns a non-nil error and is never
// actionable — callers skip it; only a successful read whose Created differs from
// the captured epoch is a wipe-and-recreate.
//
// Handle choice is the caller's: the epoch-fence MONITOR passes its dedicated
// per-bucket cached handle (ep.kv, read only from the monitor goroutine), while
// the recovery re-probe passes nil so a FRESH handle is opened — nats.go KeyValue
// handles cache *stream state and are not safe to share across goroutines, and the
// re-probe may run on the connection-monitor goroutine.
func (m *Manager) probeBucketCreated(ctx context.Context, bucket string, cached jetstream.KeyValue) (time.Time, error) {
	probeCtx, cancel := context.WithTimeout(ctx, m.cfg.OperationTimeout)
	defer cancel()

	kv := cached
	if kv == nil {
		var err error
		kv, err = m.js.KeyValue(probeCtx, bucket)
		if err != nil {
			return time.Time{}, err
		}
	}

	return kvutil.BucketStreamCreated(probeCtx, kv)
}

// checkBucketEpochs is one tick of monitorBucketEpochs, split out so a
// unit test can drive it deterministically without waiting on the
// ticker.
func (m *Manager) checkBucketEpochs(ctx context.Context) {
	for bucket, ep := range m.bucketEpochs {
		select {
		case <-ctx.Done():
			return
		default:
		}
		live, err := m.probeBucketCreated(ctx, bucket, ep.kv)
		if err != nil {
			m.logger.Debug("epoch fence: probe failed; relying on next tick",
				"bucket", bucket, "error", err)
			continue
		}
		if !live.Equal(ep.created) {
			m.logger.Warn("epoch fence: bucket stream Created mismatch — bucket was wiped and recreated",
				"bucket", bucket,
				"cached_created", ep.created,
				"live_created", live)
			m.enterDegraded(degradeReasonBucketRecreated(bucket))
			return // once degraded; no need to probe the rest this tick
		}
	}
}

// warnOnOperationTimeoutVsElection emits a one-shot WARN when
// OperationTimeout is too close to ElectionTimeout. The leader's renew
// loop has three attempts within ElectionTimeout to refresh the lease;
// if a single attempt's timeout (OperationTimeout) exceeds
// ElectionTimeout/3, one slow renew consumes the lease's entire budget
// and produces a false leadership flip.
//
// The boundary is non-strict (OperationTimeout == ElectionTimeout/3
// is acceptable). Anything strictly greater warns.
func warnOnOperationTimeoutVsElection(cfg Config, logger types.Logger) {
	if cfg.OperationTimeout <= cfg.ElectionTimeout/3 {
		return
	}
	logger.Warn(
		"OperationTimeout exceeds ElectionTimeout/3; a single slow "+
			"renew can consume the lease's three-attempt budget and "+
			"trigger a false leadership flip. Set OperationTimeout "+
			"<= ElectionTimeout/3.",
		"OperationTimeout", cfg.OperationTimeout,
		"ElectionTimeout", cfg.ElectionTimeout,
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
