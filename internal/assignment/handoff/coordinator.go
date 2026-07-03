package handoff

import (
	"context"
	"errors"
	"time"

	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/arloliu/parti/v2/internal/ratelimit"
	"github.com/arloliu/parti/v2/types"
)

// ErrRemovalPending is returned by a RemovalGuard to signal that the handoff
// removal is not yet safe to execute (i.e. the in-flight transfer has not
// committed). Callers may treat this as a retryable condition.
var ErrRemovalPending = errors.New("handoff removal pending")

// Coordinator abstracts partition handoff application for a single worker.
//
// Implementations may perform simple direct updates (no-op coordinator) or
// orchestrate a two-phase prepare/commit protocol using external coordination.
// The goal is to keep Manager integration minimal and feature-flagged.
//
// Contract:
//   - Apply must be idempotent. Re-invocation with the same old/new assignments
//     must not produce incorrect side effects.
//   - Apply is responsible only for the local worker's consumer update path.
//     Manager hooks/metrics are invoked separately by the caller.
//   - Apply should return fast; long-running orchestration should use internal
//     timeouts and backoffs.
//
// Error handling:
//   - Return an error if the local consumer update failed; callers may choose
//     to log and continue or retry based on higher-level policy.
//
// Inputs:
//   - workerID: stable worker identifier (e.g., "worker-3")
//   - old: previous assignment (may be zero-value on first apply)
//   - new: new assignment (version strictly increasing)
//
// Returns:
//   - error: nil on success; error on update/orchestration failure
//
// Note: Implementations must not block indefinitely.
//
//go:generate echo "handoff.Coordinator generated placeholder"
type Coordinator interface {
	// Start begins any background maintenance goroutines (e.g., periodic claim sweep).
	// The provided ctx controls the lifetime of those goroutines.
	// Implementations that have no background work may treat this as a no-op.
	Start(ctx context.Context)

	// Apply performs the handoff-aware application of a new assignment for a worker.
	//
	// Parameters:
	//   - ctx: Cancellation context
	//   - workerID: Stable worker identifier
	//   - previous: Previous assignment
	//   - next: New assignment (version must be strictly increasing)
	//
	// Returns:
	//   - error: Consumer update or orchestration failure, nil on success
	Apply(ctx context.Context, workerID string, previous, next types.Assignment) error
}

// ConsumerUpdater is the narrow dependency this package needs to update the
// local JetStream durable consumer.
// Kept as an internal alias to avoid importing the root package and cycles.
//
// Matches parti.WorkerConsumerUpdater.
//
// Compile-time compatibility: see internal linkage via manager.
type ConsumerUpdater interface {
	// UpdateWorkerConsumer applies partition subjects for a worker's durable consumer.
	// Must be idempotent for identical partitions.
	UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []types.Partition) error
}

// RemovalGuard is an optional hook invoked immediately before the consumer-updater
// phase of Apply. It allows the caller to block partition-subject removal while an
// in-flight transfer to another worker has not yet committed. When the guard returns
// a non-nil error (typically ErrRemovalPending for retryable waits), Apply returns
// that error without calling the ConsumerUpdater. A nil RemovalGuard is a complete
// no-op.
type RemovalGuard func(ctx context.Context, workerID string, previous, next types.Assignment) error

// Config configures a Coordinator.
// All fields are optional; a nil ConsumerUpdater produces a no-op behavior.
type Config struct {
	ConsumerUpdater ConsumerUpdater
	// RemovalGuard optionally blocks consumer removal of partitions whose transfer
	// has not committed yet. Called immediately before the consumer-updater phase.
	// Must return ErrRemovalPending for retryable handoff-transfer waits.
	// A nil RemovalGuard is a complete no-op.
	RemovalGuard RemovalGuard
	Metrics      MetricsRecorder
	Now          Now
	// Logger is optional structured logger for transition logs.
	// When nil, logging is disabled in the coordinator.
	Logger types.Logger
	// Store optionally provides a claim store for two-phase coordination.
	// When nil, two-phase will operate without external coordination (direct behavior).
	Store ClaimStore
	// TTL provides an advisory TTL to set on created claims.
	TTL time.Duration
	// SweepInterval controls the cadence of the background claim-sweep
	// ticker started by Start, and the throttle on opportunistic sweeps
	// attempted during Apply (at most one sweep body per interval).
	// Zero is defaulted to 30s by New. A NEGATIVE value disables the
	// background ticker AND the throttle: a sweep is then attempted on
	// every Apply. Ticker sweeps are scan-gated when the store can
	// probe its backing stream position; Apply-origin sweeps never are.
	SweepInterval time.Duration
	// Retry/backoff settings for CAS operations. Zero values get sensible defaults in New().
	MaxRetries  int
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	Jitter      float64 // 0.0-1.0 fractional jitter applied to backoff
	// DelayAfterPrepare introduces an artificial delay after the prepare phase completes
	// and before the consumer updater is invoked. Useful for making intermediate states
	// observable in tests and demonstrations. Ignored if <= 0.
	DelayAfterPrepare time.Duration
	// DelayBeforeStable introduces an artificial delay after entering the commit state
	// and before finalizing to stable. Useful for external observation of the commit state.
	// Ignored if <= 0.
	DelayBeforeStable time.Duration
	// PhaseConcurrency caps in-flight per-partition KV ops per phase. Zero or
	// negative is the "use default 20" sentinel set by the parti-side
	// HandoffConfig contract; New() normalizes the value in place.
	PhaseConcurrency int
	// ClaimWriteLimiter optionally gates every physical claim-write
	// (PutIfEpoch) attempt in updateClaim — including CAS retries — across the
	// prepare/commit/stabilize/reap phases. nil (the default) is unlimited.
	// The manager threads the same per-worker limiter it uses for the startup
	// hygiene/resume loops, so all of a worker's claim-writes share one rate
	// budget. PhaseConcurrency caps simultaneity; this caps throughput.
	//
	// Contract: Wait(ctx) is invoked inside the phase errgroup goroutines. The
	// coordinator holds no manager-side locks across the call, but it MUST
	// honour ctx cancellation so a shutdown/timeout unwinds the apply promptly.
	ClaimWriteLimiter ratelimit.Limiter
	// LivePartitions optionally supplies the authoritative current partition
	// set, keyed by Partition.SubjectKey() (the same identity claims are keyed
	// by). ok=false means the caller cannot vouch for the set right now — not
	// the leader, or the source is unavailable — and the sweep skips orphan
	// reaping entirely for that pass, including absence-clock bookkeeping.
	//
	// The supplier MUST be authoritative when it answers ok=true: the manager
	// wires it leader-only, so a config-skewed follower (e.g. an old Static
	// partition list mid-rolling-upgrade) can never reap claims for partitions
	// it does not know about. A nil supplier disables reaping.
	LivePartitions func(ctx context.Context) (map[string]struct{}, bool)
	// OrphanGrace is how long a stable claim must be continuously observed
	// absent from the vouched LivePartitions set before the sweep deletes it.
	// The grace turns transient source churn (remove-then-readd) into a
	// no-op; the revision-CAS delete additionally guarantees any concurrent
	// claim transition wins over the reaper. <= 0 disables orphan reaping.
	OrphanGrace time.Duration
}

// New returns a Coordinator implementation.
// If enableTwoPhase is false, returns a direct (no-op) coordinator.
// If true, returns a two-phase coordinator that orchestrates prepare/commit
// transitions with optional observability delays and opportunistic claim sweep.
func New(cfg Config, enableTwoPhase bool) Coordinator {
	if cfg.Metrics == nil {
		cfg.Metrics = NopMetrics{}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	// Apply defaults for retry/backoff
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 3
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = 50 * time.Millisecond
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 500 * time.Millisecond
	}
	if cfg.Jitter < 0 {
		cfg.Jitter = 0
	}
	// Default sweep interval
	if cfg.SweepInterval == 0 {
		cfg.SweepInterval = 30 * time.Second
	}
	if cfg.PhaseConcurrency <= 0 {
		cfg.PhaseConcurrency = 20
	}
	// Default TTL if store is enabled but TTL is missing
	if cfg.Store != nil && cfg.TTL == 0 {
		cfg.TTL = 1 * time.Minute
	}
	if enableTwoPhase {
		coord := &twoPhaseCoordinator{
			cfg:               cfg,
			orphanAbsentSince: make(map[string]time.Time),
			sweepConfirmGap:   natsutil.ScanGateDefaultConfirmGap,
			sweepMaxSkips:     natsutil.ScanGateMaxSkippedPasses,
		}
		// Optional store capability: probing the backing stream position
		// lets ticker sweeps skip provably-no-op ListKeys+Get storms.
		coord.prober, _ = cfg.Store.(bucketPosProber)

		return coord
	}

	return &direct{cfg: cfg}
}
