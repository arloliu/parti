package handoff

import (
	"context"
	"time"

	"github.com/arloliu/parti/types"
)

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

// Config configures a Coordinator.
// All fields are optional; a nil ConsumerUpdater produces a no-op behavior.
type Config struct {
	ConsumerUpdater ConsumerUpdater
	Metrics         MetricsRecorder
	Now             Now
	// Logger is optional structured logger for transition logs.
	// When nil, logging is disabled in the coordinator.
	Logger types.Logger
	// Store optionally provides a claim store for two-phase coordination.
	// When nil, two-phase will operate without external coordination (direct behavior).
	Store ClaimStore
	// TTL provides an advisory TTL to set on created claims.
	TTL time.Duration
	// SweepInterval controls how often stale/expired claims are opportunistically
	// swept during Apply calls. If zero or negative, a sweep is attempted on every Apply.
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
	// Default TTL if store is enabled but TTL is missing
	if cfg.Store != nil && cfg.TTL == 0 {
		cfg.TTL = 1 * time.Minute
	}
	if enableTwoPhase {
		return &twoPhaseCoordinator{cfg: cfg}
	}

	return &direct{cfg: cfg}
}
