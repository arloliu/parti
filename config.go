package parti

import (
	"errors"
	"fmt"
	"time"

	"github.com/arloliu/fuda"
	"github.com/go-playground/validator/v10"
)

// KVBucketConfig configures NATS JetStream KV bucket names and TTLs.
type KVBucketConfig struct {
	// StableIDBucket is the bucket name for stable worker ID claims.
	StableIDBucket string `yaml:"stableIdBucket" default:"parti-stableid" validate:"required"`

	// ElectionBucket is the bucket name for leader election.
	ElectionBucket string `yaml:"electionBucket" default:"parti-election" validate:"required"`

	// HeartbeatBucket is the bucket name for worker heartbeats.
	HeartbeatBucket string `yaml:"heartbeatBucket" default:"parti-heartbeat" validate:"required"`

	// AssignmentBucket is the bucket name for partition assignments.
	AssignmentBucket string `yaml:"assignmentBucket" default:"parti-assignment" validate:"required"`

	// AssignmentTTL is how long assignments remain in KV (0 = no expiration).
	// Assignments should persist across leader changes for version continuity.
	// Recommended: 0 (no TTL) or very long (e.g., 1 hour).
	AssignmentTTL time.Duration `yaml:"assignmentTtl" default:"0" validate:"gte=0"`

	// HandoffBucket is the bucket name for two-phase handoff ownership claims.
	// Used only when EnableTwoPhaseHandoff is true. Stores per-partition claim
	// records that track prepare/commit/stable transitions to ensure atomic
	// ownership changes and crash-resumable state. Kept separate from assignment
	// data to allow independent TTL and operational policies (sweeps, compaction).
	// Recommended: distinct bucket to isolate churn from stable assignment data.
	HandoffBucket string `yaml:"handoffBucket" default:"parti-handoff"`

	// HandoffTTL is the advisory staleness threshold the two-phase handoff
	// coordinator uses to recover a STUCK in-flight handoff: the periodic claim
	// sweep resets a prepare/commit claim back to stable once it has gone
	// untouched for longer than HandoffTTL.
	//
	// It is NOT a KV bucket TTL. The handoff bucket is created with no MaxAge —
	// a bucket-level TTL would also age out healthy stable ownership claims
	// (which are written once and never refreshed), permanently suppressing
	// pull-gated consumers. Stable claims therefore never expire.
	//
	// Must be longer than the expected multi-phase handoff duration (including
	// retries). Recommended: 2-5 minutes in production; fast tests may use
	// seconds.
	HandoffTTL time.Duration `yaml:"handoffTtl" default:"2m"`

	// Replicas is the JetStream stream-replication factor applied to
	// Parti-owned KV buckets when the library creates them fresh. Zero
	// (the default) leaves the field unset, which nats.go normalizes to 1
	// server-side — equivalent to no replication, the legacy behavior.
	//
	// Not to be confused with worker-pod replicas: Parti does not
	// configure pod counts (that is the orchestrator's concern). This
	// setting controls JetStream-level data replication only.
	//
	// Applies uniformly to every Parti-owned bucket the library creates
	// (stableID, election, heartbeat, assignment, handoff). Per-bucket
	// precision requires pre-creating buckets with explicit configs (the
	// provision package supports this) — pre-created buckets keep their
	// existing replica count regardless of this setting, because the
	// library's ensure path is get-first.
	//
	// Post-create modification: NATS JetStream DOES support changing the
	// replica count on an existing bucket via `UpdateStream`, but Parti
	// does NOT auto-reconcile Replicas. Unlike MaxAge (which is reconciled
	// because it carries a correctness invariant), Replicas is a
	// per-deployment HA-quality decision; silently rewriting an existing
	// bucket's Replicas could trigger expensive cross-node replication
	// the operator did not ask for, or mask a deliberate downsize. When
	// Manager.Start observes a pre-existing bucket whose Replicas differs
	// from this setting, it emits a WARN log line naming the divergence
	// and pointing at the `nats stream update KV_<bucket> --replicas=<N>`
	// command operators can run to apply the change themselves.
	//
	// Cluster topology must support the value at create time or bucket
	// creation fails loudly at Manager.Start (e.g. Replicas=3 against a
	// single-node NATS server returns a stream-creation error).
	//
	// Recommended values:
	//   - 0 / 1 — single-node NATS or test environments (default)
	//   - 3   — production multi-node cluster (minimum for quorum)
	//   - 5   — high-availability tier
	Replicas int `yaml:"replicas,omitempty" default:"0" validate:"gte=0"`
}

// HandoffConfig controls two-phase handoff coordinator behavior.
//
// These settings are only used when EnableTwoPhaseHandoff is true.
type HandoffConfig struct {
	// SweepInterval controls how often stale/expired claims are opportunistically
	// swept during Apply calls. If zero or negative, a sweep is attempted on every Apply.
	SweepInterval time.Duration `yaml:"sweepInterval" default:"30s" validate:"gte=0"`

	// MaxRetries controls bounded CAS retries for claim updates. Zero uses default (3).
	MaxRetries int `yaml:"maxRetries" default:"3" validate:"gte=0"`

	// BaseBackoff is the initial backoff for CAS retry with exponential backoff.
	BaseBackoff time.Duration `yaml:"baseBackoff" default:"50ms" validate:"gte=0"`

	// MaxBackoff caps the exponential backoff duration.
	MaxBackoff time.Duration `yaml:"maxBackoff" default:"500ms" validate:"gte=0,gtefield=BaseBackoff"`

	// Jitter is a fractional value [0.0, 1.0] to randomize backoff durations.
	Jitter float64 `yaml:"jitter" default:"0.2" validate:"gte=0,lte=1"`

	// DelayAfterPrepare introduces an artificial delay after the prepare phase completes
	// and before the consumer updater is invoked. Useful for making intermediate states
	// observable in tests and demonstrations. Ignored if <= 0.
	DelayAfterPrepare time.Duration `yaml:"delayAfterPrepare" default:"0" validate:"gte=0"`

	// DelayBeforeStable introduces an artificial delay after entering the commit state
	// and before finalizing to stable. Useful for external observation of the commit state.
	// Ignored if <= 0.
	DelayBeforeStable time.Duration `yaml:"delayBeforeStable" default:"0" validate:"gte=0"`

	// PhaseConcurrency caps the number of in-flight per-partition KV operations
	// during each of the prepare, commit, and stabilize phases. Zero uses the
	// default of 20. Note: 0 means "use default", not "no parallelism"; set 1
	// for strictly serial execution.
	PhaseConcurrency int `yaml:"phaseConcurrency" default:"0" validate:"gte=0,lte=256"`
}

// AlertLevel represents the severity level of degraded mode alerts.
type AlertLevel int

const (
	// AlertLevelInfo indicates informational alerts (least severe).
	AlertLevelInfo AlertLevel = iota
	// AlertLevelWarn indicates warning alerts.
	AlertLevelWarn
	// AlertLevelError indicates error alerts.
	AlertLevelError
	// AlertLevelCritical indicates critical alerts (most severe).
	AlertLevelCritical
)

// String returns the string representation of the alert level.
//
// Returns:
//   - string: Alert level name ("Info", "Warn", "Error", "Critical", or "Unknown")
func (l AlertLevel) String() string {
	switch l {
	case AlertLevelInfo:
		return "Info"
	case AlertLevelWarn:
		return "Warn"
	case AlertLevelError:
		return "Error"
	case AlertLevelCritical:
		return "Critical"
	default:
		return "Unknown"
	}
}

// DegradedAlertConfig controls alert emission during degraded mode operation.
type DegradedAlertConfig struct {
	// InfoThreshold is the duration in degraded mode before emitting Info-level alerts.
	// Default: 30 seconds.
	InfoThreshold time.Duration `yaml:"infoThreshold" default:"30s" validate:"gt=0"`

	// WarnThreshold is the duration in degraded mode before emitting Warn-level alerts.
	// Default: 2 minutes.
	WarnThreshold time.Duration `yaml:"warnThreshold" default:"2m" validate:"gt=0,gtefield=InfoThreshold"`

	// ErrorThreshold is the duration in degraded mode before emitting Error-level alerts.
	// Default: 5 minutes.
	ErrorThreshold time.Duration `yaml:"errorThreshold" default:"5m" validate:"gt=0,gtefield=WarnThreshold"`

	// CriticalThreshold is the duration in degraded mode before emitting Critical-level alerts.
	// Default: 10 minutes.
	CriticalThreshold time.Duration `yaml:"criticalThreshold" default:"10m" validate:"gt=0,gtefield=ErrorThreshold"`

	// AlertInterval is the time between repeated alerts at the same severity level.
	// Default: 1 minute.
	AlertInterval time.Duration `yaml:"alertInterval" default:"1m" validate:"gt=0"`
}

// DegradedBehaviorConfig controls when the manager enters and exits degraded mode.
type DegradedBehaviorConfig struct {
	// EnterThreshold is how long NATS connectivity errors must persist before entering degraded mode.
	// Provides hysteresis to prevent flapping during transient issues.
	// Default: 10 seconds.
	EnterThreshold time.Duration `yaml:"enterThreshold" default:"10s" validate:"gte=0"`

	// ExitThreshold is how long NATS connectivity must be stable before exiting degraded mode.
	// Should be shorter than EnterThreshold to recover quickly.
	// Default: 5 seconds.
	ExitThreshold time.Duration `yaml:"exitThreshold" default:"5s" validate:"gte=0"`

	// KVErrorThreshold is the number of consecutive KV operation errors that trigger degraded mode.
	// Default: 5 errors.
	KVErrorThreshold int `yaml:"kvErrorThreshold" default:"5" validate:"gte=0"`

	// KVErrorWindow is the time window for counting consecutive KV errors.
	// Errors outside this window are not counted.
	// Default: 30 seconds.
	KVErrorWindow time.Duration `yaml:"kvErrorWindow" default:"30s" validate:"gte=0"`

	// RecoveryGracePeriod is the minimum time the leader must wait after recovering from
	// degraded mode before declaring missing workers as failed (emergency rebalance).
	// Prevents false emergencies when workers recover slightly slower than the leader.
	// Default: 15 seconds.
	RecoveryGracePeriod time.Duration `yaml:"recoveryGracePeriod" default:"15s" validate:"gte=0"`
}

// defaultDegradedBehaviorConfig returns default behavior configuration for degraded mode.
// Used internally by DegradedBehaviorPreset("balanced").
func defaultDegradedBehaviorConfig() DegradedBehaviorConfig {
	var cfg DegradedBehaviorConfig
	_ = fuda.SetDefaults(&cfg)
	return cfg
}

// DegradedBehaviorPreset returns a preconfigured DegradedBehaviorConfig based on the preset name.
//
// Supported presets:
//   - "conservative": Slower to enter degraded, safer for production (30s enter, 10s exit, 10 errors)
//   - "balanced": Default behavior, good for most use cases (10s enter, 5s exit, 5 errors)
//   - "aggressive": Faster to enter degraded, better for development (5s enter, 3s exit, 3 errors)
//
// Parameters:
//   - preset: Preset name ("conservative", "balanced", or "aggressive")
//
// Returns:
//   - DegradedBehaviorConfig: Preconfigured behavior settings
//   - error: ErrInvalidPreset if preset name is not recognized
//
// Example:
//
//	cfg, err := DegradedBehaviorPreset("conservative")
//	if err != nil {
//	    log.Fatal(err)
//	}
func DegradedBehaviorPreset(preset string) (DegradedBehaviorConfig, error) {
	switch preset {
	case "conservative":
		return DegradedBehaviorConfig{
			EnterThreshold:      30 * time.Second,
			ExitThreshold:       10 * time.Second,
			KVErrorThreshold:    10,
			KVErrorWindow:       30 * time.Second,
			RecoveryGracePeriod: 20 * time.Second,
		}, nil
	case "balanced":
		return defaultDegradedBehaviorConfig(), nil
	case "aggressive":
		return DegradedBehaviorConfig{
			EnterThreshold:      5 * time.Second,
			ExitThreshold:       3 * time.Second,
			KVErrorThreshold:    3,
			KVErrorWindow:       15 * time.Second,
			RecoveryGracePeriod: 10 * time.Second,
		}, nil
	default:
		return DegradedBehaviorConfig{}, fmt.Errorf("%w: %q (must be one of [conservative, balanced, aggressive])", ErrInvalidPreset, preset)
	}
}

// ============================================================================
// Timing Configuration Model (Three-Tier System)
// ============================================================================
//
// Parti uses a three-tier timing model for predictable rebalancing behavior:
//
// ┌─────────────────────────────────────────────────────────────────────────┐
// │ TIER 1: Detection Speed - How fast we notice topology changes          │
// ├─────────────────────────────────────────────────────────────────────────┤
// │ • WatcherDebounce: 100ms (hardcoded)                                   │
// │   - Batches rapid heartbeat changes before triggering checks           │
// │ • PollingInterval: HeartbeatTTL/2 (calculated)                         │
// │   - Fallback detection if watcher fails                                │
// └─────────────────────────────────────────────────────────────────────────┘
//
// ┌─────────────────────────────────────────────────────────────────────────┐
// │ TIER 2: Stabilization - How long we wait before acting                 │
// ├─────────────────────────────────────────────────────────────────────────┤
// │ • ColdStartWindow: 30s (configurable)                                  │
// │   - Applied when all workers join from zero state                      │
// │   - Allows time for full fleet to come online                          │
// │ • PlannedScaleWindow: 10s (configurable)                               │
// │   - Applied for gradual worker additions                               │
// │   - Allows time for new workers to stabilize                           │
// │ • EmergencyWindow: 0s (immediate)                                      │
// │   - Applied when workers disappear unexpectedly                        │
// │   - No delay - immediate rebalance to restore capacity                 │
// │ • EmergencyGracePeriod: 1.5s (configurable)                            │
// │   - Minimum time worker must be missing before emergency               │
// │   - Prevents flapping from transient network issues                    │
// └─────────────────────────────────────────────────────────────────────────┘
//
// ┌─────────────────────────────────────────────────────────────────────────┐
// │ TIER 3: Rate Limiting - How often we can rebalance                     │
// ├─────────────────────────────────────────────────────────────────────────┤
// │ • RebalanceCooldown: 10s (configurable)                                │
// │   - Enforced BEFORE stabilization windows begin                        │
// │   - Prevents thrashing during rapid successive changes                 │
// │   - If triggered <RebalanceCooldown after last rebalance, defer        │
// └─────────────────────────────────────────────────────────────────────────┘
//
// Execution Flow Example:
//
//	T+0s:  Rebalance completes (lastRebalance = now)
//	T+5s:  Worker joins
//	       ├─ Check: 5s < 10s RebalanceCooldown? YES
//	       └─ Action: Defer (no state change, check again later)
//	T+10s: RebalanceCooldown expires
//	       ├─ Action: Enter Scaling state
//	       └─ Start: 10s PlannedScaleWindow (Tier 2)
//	T+20s: Stabilization complete
//	       ├─ Action: Transition to Rebalancing state
//	       └─ Action: Calculate and publish assignments
//	T+25s: Another worker joins
//	       ├─ Check: 5s < 10s RebalanceCooldown? YES
//	       └─ Action: Defer to T+30s
//	T+30s: Rate limit expires, cycle repeats
//
// Configuration Constraints:
//   - RebalanceCooldown <= PlannedScaleWindow (recommended)
//   - ColdStartWindow >= PlannedScaleWindow (cold start is slower)
//   - EmergencyGracePeriod <= HeartbeatTTL (detection window)
//
// ============================================================================

// minWorkerIDTTL is the smallest supported WorkerIDTTL. It is three times the
// stable-ID renewal floor (the renewal loop in internal/stableid renews at most
// every 100ms): below this, a live worker's stable-ID key — whose bucket MaxAge
// is reconciled to WorkerIDTTL — can age out before the renewal loop refreshes
// it, letting another worker claim the still-live ID.
const minWorkerIDTTL = 300 * time.Millisecond

// Config is the configuration for the Manager.
//
// All duration fields accept standard Go duration strings like "30s", "5m", "1h".
type Config struct {
	// WorkerIDPrefix is the prefix for worker IDs (e.g., "worker" produces "worker-0", "worker-1").
	WorkerIDPrefix string `yaml:"workerIdPrefix" default:"worker" validate:"required"`

	// WorkerIDMin is the minimum stable ID number (inclusive).
	// Set to 0 for most use cases.
	WorkerIDMin int `yaml:"workerIdMin" default:"0" validate:"gte=0"`

	// WorkerIDMax is the maximum stable ID number (inclusive).
	// Determines the maximum number of concurrent workers: (WorkerIDMax - WorkerIDMin + 1).
	// For example, WorkerIDMin=0 and WorkerIDMax=999 allows up to 1000 workers.
	WorkerIDMax int `yaml:"workerIdMax" default:"999" validate:"gtefield=WorkerIDMin"`

	// WorkerIDTTL is how long a worker ID claim remains valid in the key-value store.
	// Must be greater than HeartbeatTTL. Renewal writes one KV op per worker every
	// WorkerIDTTL/3, so longer values reduce steady-state IOPS at the cost of slower
	// ID reclamation after a worker exits.
	// Recommended: 3-5x HeartbeatTTL. Default 75s is 5x the default HeartbeatTTL (15s).
	// The stableID KV bucket's MaxAge is reconciled to exactly WorkerIDTTL on
	// Manager.Start: an existing bucket whose TTL differs (most dangerously 0,
	// which leaks worker IDs on ungraceful restart) is corrected, and a
	// deliberately-longer operator TTL is shortened to WorkerIDTTL.
	WorkerIDTTL time.Duration `yaml:"workerIdTtl" default:"75s" validate:"gt=0,gtefield=HeartbeatTTL"`

	// HeartbeatInterval is how often workers publish heartbeat messages.
	// Shorter intervals provide faster failure detection but increase KV write load
	// (one write per worker per interval, amplified by JetStream replication and fsync).
	// Recommended: 3-10 seconds.
	HeartbeatInterval time.Duration `yaml:"heartbeatInterval" default:"5s" validate:"gt=0"`

	// HeartbeatTTL is how long heartbeat messages remain valid before a worker is considered failed.
	// Must be greater than HeartbeatInterval.
	// Recommended: 3x HeartbeatInterval.
	HeartbeatTTL time.Duration `yaml:"heartbeatTtl" default:"15s" validate:"gt=0"`

	// ColdStartWindow is the stabilization period when starting workers from zero.
	// During this window, partition assignment is delayed to allow all initial workers to join.
	// Recommended: 30 seconds.
	ColdStartWindow time.Duration `yaml:"coldStartWindow" default:"30s" validate:"gt=0,gtefield=PlannedScaleWindow"`

	// PlannedScaleWindow is the stabilization period during rolling updates or planned scaling.
	// Shorter than ColdStartWindow to minimize disruption during controlled changes.
	// Recommended: 10 seconds.
	PlannedScaleWindow time.Duration `yaml:"plannedScaleWindow" default:"10s" validate:"gt=0"`

	// EmergencyGracePeriod is the minimum time a worker must be missing before
	// triggering emergency rebalance. Prevents false positives from transient
	// network issues or brief connectivity loss.
	//
	// Default: 0 (auto-calculated as 1.5 * HeartbeatInterval)
	// Recommended: 1.5-2.0 * HeartbeatInterval
	// Constraint: Must be <= HeartbeatTTL
	EmergencyGracePeriod time.Duration `yaml:"emergencyGracePeriod" validate:"ltefield=HeartbeatTTL"`

	// RestartDetectionRatio determines when a restart is classified as cold start vs planned.
	// If (failed workers / total workers) > ratio, it's treated as a cold start.
	// For example, 0.5 means if >50% of workers fail simultaneously, use ColdStartWindow.
	// Recommended: 0.5.
	RestartDetectionRatio float64 `yaml:"restartDetectionRatio" default:"0.5" validate:"gte=0,lte=1"`

	// OperationTimeout is the timeout for KV operations (get, put, delete).
	// Recommended: 10 seconds.
	OperationTimeout time.Duration `yaml:"operationTimeout" default:"10s" validate:"gt=0"`

	// ElectionTimeout is the maximum time to wait for leader election to complete.
	// Also drives leader lease renewal frequency (ElectionTimeout/3). Longer values
	// reduce KV write load at the cost of slower failover detection.
	// Recommended: 5-15 seconds.
	ElectionTimeout time.Duration `yaml:"electionTimeout" default:"10s" validate:"gt=0"`

	// StartupTimeout is the maximum time to wait for the manager to fully start.
	// Includes worker ID claiming, leader election, and initial partition assignment.
	// Must be at least ColdStartWindow + ElectionTimeout with some headroom.
	// Recommended: 60 seconds.
	StartupTimeout time.Duration `yaml:"startupTimeout" default:"60s" validate:"gt=0"`

	// ShutdownTimeout is the maximum time to wait for graceful shutdown.
	// Includes releasing worker ID, stopping heartbeats, and cleanup operations.
	// Recommended: 10 seconds.
	ShutdownTimeout time.Duration `yaml:"shutdownTimeout" default:"10s" validate:"gt=0"`

	// RebalanceCooldown is the minimum time between rebalancing operations.
	//
	// Enforces rate limiting BEFORE stabilization windows to prevent thrashing
	// during rapid topology changes. If a rebalance was completed <RebalanceCooldown
	// ago, new topology changes are deferred until the interval expires.
	//
	// Default: 10 seconds
	// Recommendation: Should be <= PlannedScaleWindow for proper coordination
	//
	// Note: This was renamed from MinRebalanceInterval in v0.x for semantic clarity.
	RebalanceCooldown time.Duration `yaml:"rebalanceCooldown" default:"10s" validate:"gt=0,ltefield=PlannedScaleWindow"`

	// KVBuckets controls NATS JetStream KV bucket configuration.
	KVBuckets KVBucketConfig `yaml:"kvBuckets"`

	// DegradedAlert controls alert emission during degraded mode operation.
	DegradedAlert DegradedAlertConfig `yaml:"degradedAlert"`

	// DegradedBehavior controls when the manager enters and exits degraded mode.
	DegradedBehavior DegradedBehaviorConfig `yaml:"degradedBehavior"`

	// EnableTwoPhaseHandoff gates the manager-side two-phase handoff coordinator.
	//
	// When false (default), assignment changes are applied directly via the
	// WorkerConsumerUpdater in a simple "remove/add" sequence managed by the
	// manager without KV claims. This keeps the control plane minimal but can
	// permit a brief duplicate-consumption window during reassignment.
	//
	// When true, the manager initializes a modular handoff coordinator which
	// can implement a prepare/commit protocol to make ownership transitions
	// atomic, single-owner, auditable, and crash-resumable. This feature is
	// wired behind a clean interface and can be enabled/disabled without
	// scattering conditional logic throughout the manager code path.
	EnableTwoPhaseHandoff bool `yaml:"enableTwoPhaseHandoff" default:"false"`

	// Handoff controls two-phase handoff tuning knobs.
	// Only applied when EnableTwoPhaseHandoff is true.
	Handoff HandoffConfig `yaml:"handoff"`

	// ApplyStartJitter, when > 0, randomly delays fresh-version applies
	// (watcher / commit / alias / initial-bootstrap, all funneled through
	// applyAssignmentWithPrev) by a uniformly distributed duration in
	// [0, ApplyStartJitter) before taking applyStoreMu. This spreads
	// JetStream consumer create/destroy load across a worker fleet that
	// observed the same new assignment version simultaneously (e.g. after
	// a leader re-election).
	//
	// Retries (scheduleApplyRetry) do NOT pay this jitter. The retry path
	// has its own exponential-backoff envelope with decorrelated jitter;
	// compounding apply-start jitter on top would inflate recovery latency
	// without spreading any fleet because a retry is one worker, not a fleet.
	//
	// Default 0 disables jitter (backwards compatible). Recommended starting
	// point for a 20-worker fleet creating 100 consumers each: 500ms.
	//
	// Hard-capped at 10s by Validate(): a larger value would dwarf the
	// LSR-advancement bookkeeping and risk masking apply failures behind
	// what would look like apply latency.
	//
	// Startup-runner consequence: the startup background runner's first
	// apply (applyInitialAssignment, manager_startup_async.go) also funnels
	// through applyAssignmentWithPrev for non-empty commits/aliases. With
	// jitter enabled, the runner sleeps up to ApplyStartJitter before its
	// first apply attempt. The soft watchdog measures StartupTimeout from
	// Start invocation (AGENTS.md "Apply boundedness" section), so operators
	// MUST configure ApplyStartJitter << StartupTimeout. Recommended:
	// ApplyStartJitter <= StartupTimeout / 4. A focused test pins this
	// (TestApplyStartJitter_StartupBudget).
	ApplyStartJitter time.Duration `yaml:"applyStartJitter" default:"0" validate:"gte=0"`

	// AssignmentWatcherDebounce is the idle-window duration used to coalesce
	// rapid bursts of assignment delivery watcher events into a single apply.
	// It applies to both the legacy per-worker assignment alias watcher and
	// the assignment._commit watcher. When > 0, each watcher path keeps the
	// latest observed assignment target in a pending slot and processes it
	// after the stream has been idle for the full window.
	//
	// Early-flush conditions (commit watcher only): the pending commit is
	// flushed BEFORE the idle window elapses when (a) the watcher channel
	// closes mid-window, or (b) a same-or-higher commit arrives that
	// changes this worker's effective partition set — either a Workers
	// membership flip (this worker added or removed) or a
	// Payloads[workerID].PayloadHash difference. Both shapes are treated
	// as ownership transitions; debounce collapses only commits whose
	// per-worker partition set is unchanged. This preserves the two-phase
	// handoff invariant that every ownership transition for this worker is
	// observed as a discrete apply. Stale lower-version commits arriving
	// after a higher pending version are dropped without flushing pending.
	// On context cancellation (Stop), the pending commit is dropped
	// without flushing — see the alias watcher's matching shutdown
	// semantics. The legacy alias watcher does NOT yet have the "flush on
	// assignment change" rule; harmonization is a follow-up.
	//
	// Default 0 disables debouncing (preserves pre-PR-3 behavior). Recommended
	// value derived from the apply-coalescing diagnostic
	// (PARTI_RUN_HERD_DIAGNOSTIC=1) plus a 50 ms safety margin — typically
	// in the 100–300 ms range.
	//
	// Hard-capped at 1 second by Validate(): a larger window would dwarf
	// reasonable reassignment-latency budgets and risk masking real apply
	// failures behind what looks like apply slowness.
	AssignmentWatcherDebounce time.Duration `yaml:"assignmentWatcherDebounce" default:"0" validate:"gte=0"`
}

// DefaultConfig returns a Config with sensible defaults.
//
// This function panics only if the library's own struct tags are malformed,
// which indicates a programming error in Parti itself.
//
// Returns:
//   - Config: Configuration with default values
func DefaultConfig() Config {
	var cfg Config
	if err := SetDefaults(&cfg); err != nil {
		panic(fmt.Errorf("parti: DefaultConfig: %w", err))
	}
	return cfg
}

// SetDefaults applies default values to zero-valued configuration fields.
// If a field is zero-valued, it will be set to the corresponding default value.
//
// Returns:
//   - error: Non-nil if the default tags on the Config struct are malformed
func SetDefaults(cfg *Config) error {
	if err := fuda.SetDefaults(cfg); err != nil {
		return fmt.Errorf("failed to set defaults: %w", err)
	}

	// Dynamic defaults that cannot be expressed via struct tags
	if cfg.EmergencyGracePeriod == 0 {
		// Default: 1.5x HeartbeatInterval (allows one missed heartbeat)
		cfg.EmergencyGracePeriod = time.Duration(float64(cfg.HeartbeatInterval) * 1.5)
	}

	return nil
}

// TTL Configuration Guide
// =======================
//
// This library uses three different TTLs with specific purposes and constraints:
//
// 1. WorkerIDTTL (Default: 30s)
//    Purpose: Stable worker identity lease duration in NATS KV
//    Renewal: Automatically renewed every WorkerIDTTL/3 (~10s)
//    Expiry Impact: Worker loses ID claim and must re-acquire (causes disruption)
//    Recommendation: Set to 3-5x HeartbeatInterval
//
// 2. HeartbeatTTL (Default: 6s)
//    Purpose: Worker liveness detection window
//    Renewal: Heartbeat published every HeartbeatInterval (2s)
//    Expiry Impact: Worker considered dead → Emergency rebalance triggered
//    Recommendation: Set to 3x HeartbeatInterval
//
// 3. AssignmentTTL (Default: 0 = infinite)
//    Purpose: Assignment persistence across leader changes
//    Renewal: Never (assignments persist indefinitely)
//    Expiry Impact: Lost assignment history → Version counter reset
//    Recommendation: 0 (infinite) or very long (1h+) for production

// 4. HandoffTTL (Default: 2m)
//    Purpose: Advisory staleness threshold for the two-phase handoff claim sweep
//    Renewal: The sweep resets a stuck prepare/commit claim to stable after this
//    Expiry Impact: NOT a KV bucket TTL — the handoff bucket has no MaxAge and
//                   stable ownership claims never expire
//    Recommendation: Longer than a multi-phase handoff (2-5m); tests may use seconds
//
// Constraint Hierarchy:
//   WorkerIDTTL >= HeartbeatTTL >= 2 * HeartbeatInterval
//
// Example Valid Configurations:
//
//   // Production (default) — tuned for low PVC IOPS on file-backed JetStream
//   WorkerIDTTL: 75s, HeartbeatInterval: 5s, HeartbeatTTL: 15s
//
//   // Fast failover (higher write load)
//   WorkerIDTTL: 30s, HeartbeatInterval: 2s, HeartbeatTTL: 6s
//
//   // Fast (testing)
//   WorkerIDTTL: 5s, HeartbeatInterval: 500ms, HeartbeatTTL: 1.5s

// Validate checks configuration constraints and returns error for invalid values.
//
// Hard Validation Rules:
//   - HeartbeatTTL >= 2 * HeartbeatInterval (allow 1 missed heartbeat)
//   - WorkerIDTTL >= 3 * HeartbeatInterval (stable ID renewal)
//   - WorkerIDTTL >= HeartbeatTTL (ID must outlive heartbeat)
//   - WorkerIDTTL >= 300ms (stable-ID renewal floor)
//   - RebalanceCooldown > 0 (prevent thrashing)
//   - ColdStartWindow >= PlannedScaleWindow (cold start is slower)
//   - RebalanceCooldown <= PlannedScaleWindow (rate limit coordination)
//   - RebalanceCooldown <= ColdStartWindow (rate limit coordination)
//   - EmergencyGracePeriod <= HeartbeatTTL (detection window)
//
// Returns:
//   - error: Validation error with clear explanation, nil if valid
func (cfg *Config) Validate() error {
	validate := validator.New(validator.WithRequiredStructEnabled())
	if err := validate.Struct(cfg); err != nil {
		return fmt.Errorf("configuration validation failed: %w", err)
	}

	// Custom validation logic that cannot be expressed with tags
	// Rule 1: HeartbeatTTL sanity
	if cfg.HeartbeatTTL < 2*cfg.HeartbeatInterval {
		return fmt.Errorf(
			"HeartbeatTTL (%v) must be >= 2*HeartbeatInterval (%v) to allow one missed heartbeat",
			cfg.HeartbeatTTL, cfg.HeartbeatInterval,
		)
	}

	// Rule 2: WorkerIDTTL vs HeartbeatInterval
	if cfg.WorkerIDTTL < 3*cfg.HeartbeatInterval {
		return fmt.Errorf(
			"WorkerIDTTL (%v) must be >= 3*HeartbeatInterval (%v) for stable ID renewal",
			cfg.WorkerIDTTL, cfg.HeartbeatInterval,
		)
	}

	// Rule 2b: WorkerIDTTL floor for the stable-ID renewal cadence.
	if cfg.WorkerIDTTL < minWorkerIDTTL {
		return fmt.Errorf(
			"WorkerIDTTL (%v) must be >= %v: below this the stable-ID key can "+
				"expire before the renewal loop refreshes it and a live worker "+
				"can lose its ID",
			cfg.WorkerIDTTL, minWorkerIDTTL,
		)
	}

	// Rule 10: Validate two-phase handoff KV configuration when enabled
	if cfg.EnableTwoPhaseHandoff {
		if cfg.KVBuckets.HandoffBucket == "" {
			return errors.New("HandoffBucket must be set when EnableTwoPhaseHandoff is true")
		}
		if cfg.KVBuckets.HandoffTTL <= 0 {
			return errors.New("HandoffTTL must be > 0 when EnableTwoPhaseHandoff is true")
		}
	}

	// Rule 11: ApplyStartJitter range
	if cfg.ApplyStartJitter < 0 {
		return errors.New("ApplyStartJitter must be >= 0")
	}
	if cfg.ApplyStartJitter > 10*time.Second {
		return errors.New("ApplyStartJitter must be <= 10s")
	}

	// Rule 12: AssignmentWatcherDebounce range
	if cfg.AssignmentWatcherDebounce < 0 {
		return errors.New("AssignmentWatcherDebounce must be >= 0")
	}
	if cfg.AssignmentWatcherDebounce > 1*time.Second {
		return errors.New("AssignmentWatcherDebounce must be <= 1s")
	}

	return nil
}

// ValidateWithWarnings checks configuration and logs warnings for non-recommended values.
//
// This is called after Validate() in NewManager() to provide operator guidance.
//
// Parameters:
//   - logger: Logger instance for warning output
func (cfg *Config) ValidateWithWarnings(logger Logger) {
	// Warn if WorkerIDTTL is less than recommended 2x HeartbeatTTL
	if cfg.WorkerIDTTL < 2*cfg.HeartbeatTTL {
		logger.Warn(
			"WorkerIDTTL is below recommended minimum",
			"workerIDTTL", cfg.WorkerIDTTL,
			"heartbeatTTL", cfg.HeartbeatTTL,
			"recommended", 2*cfg.HeartbeatTTL,
		)
	}

	// Warn if RebalanceCooldown is very short
	if cfg.RebalanceCooldown < 5*time.Second {
		logger.Warn(
			"RebalanceCooldown is very short, may cause frequent rebalancing",
			"cooldown", cfg.RebalanceCooldown,
			"recommended", "10s or higher",
		)
	}

	// Warn if exit threshold is larger than enter threshold (unusual)
	if cfg.DegradedBehavior.ExitThreshold > cfg.DegradedBehavior.EnterThreshold {
		logger.Warn(
			"degraded exit threshold is greater than enter threshold (unusual configuration)",
			"exit_threshold", cfg.DegradedBehavior.ExitThreshold,
			"enter_threshold", cfg.DegradedBehavior.EnterThreshold,
			"note", "typically exit threshold should be shorter for faster recovery",
		)
	}

	// Warn when the spread between HeartbeatTTL and EmergencyGracePeriod is
	// tight enough that a GC pause or a single stalled heartbeat publish can
	// chew through the entire hysteresis budget and trigger a false emergency
	// on a healthy worker. The detector fires when a worker has been missing
	// for EmergencyGracePeriod; the heartbeat key only stays present for
	// HeartbeatTTL. The remaining headroom (HeartbeatTTL - EmergencyGracePeriod)
	// is the operator's budget for transient stalls.
	//
	// Recommendation: HeartbeatTTL - EmergencyGracePeriod >= HeartbeatTTL/2.
	if cfg.HeartbeatTTL > 0 && cfg.EmergencyGracePeriod > cfg.HeartbeatTTL/2 {
		logger.Warn(
			"EmergencyGracePeriod leaves little headroom under HeartbeatTTL — a GC pause or stalled publish may trigger false emergencies",
			"heartbeat_ttl", cfg.HeartbeatTTL,
			"emergency_grace_period", cfg.EmergencyGracePeriod,
			"hysteresis_budget", cfg.HeartbeatTTL-cfg.EmergencyGracePeriod,
			"recommended_max_grace", cfg.HeartbeatTTL/2,
		)
	}

	// Warn if recovery grace period is very short
	if cfg.DegradedBehavior.RecoveryGracePeriod < 5*time.Second {
		logger.Warn(
			"recovery grace period is very short, may trigger false emergencies after recovery",
			"recovery_grace_period", cfg.DegradedBehavior.RecoveryGracePeriod,
			"recommended", "15s or higher",
		)
	}

	// Warn if StartupTimeout is not long enough to accommodate the cold-start
	// stabilization window plus leader election. A pod starting during a true
	// cold start must wait for the leader's ColdStartWindow to fire the final
	// rebalance; if its Start(ctx) or StartupTimeout expires sooner, it will
	// appear to hang with "context deadline exceeded". On leader takeover the
	// post-takeover window is now PlannedScaleWindow, so the takeover path
	// doesn't require this headroom — but a true cold-start fleet still does.
	minStartup := cfg.ColdStartWindow + cfg.ElectionTimeout + 5*time.Second
	if cfg.StartupTimeout < minStartup {
		logger.Warn(
			"StartupTimeout is shorter than ColdStartWindow + ElectionTimeout + 5s headroom",
			"startup_timeout", cfg.StartupTimeout,
			"cold_start_window", cfg.ColdStartWindow,
			"election_timeout", cfg.ElectionTimeout,
			"recommended_minimum", minStartup,
			"note", "workers starting during the leader's cold-start stabilization window may time out before they receive their first assignment",
		)
	}
}

// TestConfig returns a configuration optimized for fast test execution.
//
// Test timings are 10-100x faster than production defaults to enable
// rapid iteration without sacrificing test coverage. Use DefaultConfig()
// for production deployments.
//
// Returns:
//   - Config: Configuration with fast timings for tests
//
// Example:
//
//	cfg := parti.TestConfig()
//	cfg.WorkerIDPrefix = "test-worker"
//	manager, err := parti.NewManager(nc, cfg)
func TestConfig() Config {
	cfg := DefaultConfig()

	// Fast timings for test execution (10-100x faster)
	cfg.RebalanceCooldown = 100 * time.Millisecond    // 100x faster
	cfg.ColdStartWindow = 1 * time.Second             // 30x faster
	cfg.PlannedScaleWindow = 500 * time.Millisecond   // 20x faster
	cfg.HeartbeatInterval = 500 * time.Millisecond    // 4x faster
	cfg.HeartbeatTTL = 1500 * time.Millisecond        // 4x faster
	cfg.EmergencyGracePeriod = 750 * time.Millisecond // Scaled with HeartbeatInterval
	cfg.WorkerIDTTL = 5 * time.Second                 // 6x faster

	return cfg
}
