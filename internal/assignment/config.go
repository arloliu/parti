package assignment

import (
	"context"
	"errors"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// CalculatorAndAssignmentMetrics combines the metrics interfaces needed by the calculator
// and its sub-components (e.g., AssignmentPublisher and the payload GC loop).
//
// The calculator itself records CalculatorMetrics (rebalance durations, worker changes, etc.)
// and delegates AssignmentMetrics, PublisherMetrics, and GCMetrics to the
// AssignmentPublisher / GC.
type CalculatorAndAssignmentMetrics interface {
	types.CalculatorMetrics
	types.AssignmentMetrics
	types.PublisherMetrics
	types.GCMetrics
	types.AuditMetrics
}

// Config holds calculator configuration.
//
// Use NewCalculatorWithConfig(cfg) to create a calculator with validated
// configuration and sensible defaults for optional fields.
//
// Required fields must be set before calling NewCalculatorWithConfig.
// Optional fields will be set to sensible defaults if zero-valued.
type Config struct {
	// Required dependencies
	AssignmentKV jetstream.KeyValue // NATS KV bucket for assignments
	HeartbeatKV  jetstream.KeyValue // NATS KV bucket for heartbeats
	Source       types.PartitionSource
	Strategy     types.AssignmentStrategy

	// Required configuration
	AssignmentPrefix string        // Key prefix for assignments (e.g., "assignment")
	HeartbeatPrefix  string        // Key prefix for heartbeats (e.g., "heartbeat")
	HeartbeatTTL     time.Duration // Heartbeat TTL for worker health detection

	// Optional configuration (with defaults)
	EmergencyGracePeriod time.Duration // Minimum time before emergency rebalance (default: 5s)
	Cooldown             time.Duration // Minimum time between rebalances (default: 10s)
	ColdStartWindow      time.Duration // Stabilization window for cold start (default: 30s)
	PlannedScaleWindow   time.Duration // Stabilization window for planned scale (default: 10s)

	// Now returns the current time and backs the cooldown gate
	// (time-since-last-rebalance vs Cooldown) and the publisher's
	// lastRebalance stamp. When nil, time.Now is used. Tests inject a
	// controllable clock so cooldown timing is deterministic under load;
	// production leaves it nil.
	Now func() time.Time

	// Partition-input credibility (see
	// errSuspiciousPartitionObservation Godoc in calculator.go):
	//
	// PartitionShrinkConfirmationCount is the number of consecutive
	// suspicious partition-source observations the calculator requires
	// before trusting an empty / sharply-shrunk shape. Default: 3.
	// Setting this to 1 disables the confirmation window (every
	// observation is acted on immediately).
	PartitionShrinkConfirmationCount int

	// PartitionShrinkConfirmationThresholdPct defines "sharply shrunk".
	// A new partition count where
	//   observed * 100 < lastKnownPartitionCount * Pct
	// is suspicious. Default: 50 (a >=50% drop in one poll is
	// suspicious). An empty observation is always suspicious
	// regardless of this threshold.
	PartitionShrinkConfirmationThresholdPct int

	// Worker-input credibility (see errSuspiciousWorkerObservation Godoc
	// in calculator.go):
	//
	// WorkerShrinkConfirmationCount is the number of consecutive
	// suspicious worker-set observations the calculator requires before
	// trusting a sharply-shrunk heartbeat scan. Default: 2. Setting this
	// to 1 disables the confirmation window (every observation is acted
	// on immediately). The default is lower than
	// PartitionShrinkConfirmationCount because heartbeat scans run on
	// every monitor poll (~HeartbeatTTL/2) whereas partition-source
	// observations are watcher-driven and arrive only on source change —
	// a smaller worker window converges faster on a real shrink without
	// stranding correctness.
	WorkerShrinkConfirmationCount int

	// WorkerShrinkConfirmationThresholdPct defines "sharply shrunk" for
	// the heartbeat scan. A new worker count where
	//   observed * 100 < lastKnownWorkerCount * Pct
	// is suspicious. Default: 50 (a >=50% drop in one scan is
	// suspicious). The defense fires only when lastKnownWorkerCount > 0
	// (the first ever scan is always trusted).
	WorkerShrinkConfirmationThresholdPct int

	// RebalanceGraceDrainInterval is the period at which monitorPartitions
	// checks for a partition-source update that was deferred because the
	// leader was in recovery grace. When grace lifts, the deferred update
	// is drained on the next tick. Default: min(Cooldown, 30s), capped
	// below at 1s.
	RebalanceGraceDrainInterval time.Duration

	// ApplyGracePeriod is the time after THIS leader observed the current
	// commit (via successful CAS or BootstrapLastCommit) before the audit
	// loop emits retry-pressure metrics for behind workers. Measured against
	// a monotonic clock so cross-leader wall-clock skew does not influence
	// the grace window. Default: 2 × HeartbeatTTL.
	ApplyGracePeriod time.Duration

	// ExtendedApplyGracePeriod is the time after THIS leader observed the
	// current commit before the audit may escalate via two-phase handoff
	// (audit_repair rebalance). Measured against a monotonic clock; see
	// ApplyGracePeriod. Default: 5 × HeartbeatTTL.
	ExtendedApplyGracePeriod time.Duration

	// AuditInterval is the period between audit passes. Default: HeartbeatTTL.
	AuditInterval time.Duration

	// EnableTwoPhaseHandoff mirrors the manager's flag so the audit can skip
	// escalation when two-phase mode is off. The audit only escalates when
	// this flag is true AND the full safety chain is present on both the
	// behind worker and at least one target.
	EnableTwoPhaseHandoff bool

	// Optional dependencies
	Metrics       CalculatorAndAssignmentMetrics // Metrics collector (default: no-op)
	Logger        types.Logger                   // Logger (default: no-op)
	StateProvider types.StateProvider            // Manager state provider for degraded mode checks (default: nil)
	// LeaderRevision, if set, is called before each rebalance to obtain the current
	// leader revision (NATS KV revision of the leader key). The value is embedded in
	// every published assignment so workers can detect assignments from a former
	// leader. When nil, LeaderRevision defaults to 0 in published assignments.
	LeaderRevision func() uint64

	// LeaderCheck, if set, is invoked by the publisher's pre-alias and
	// post-alias leadership fences (publish steps 5 and 7 of §3.5). It must
	// perform a LIVE verification of the leader-key revision (e.g., a KV read
	// of the election key) and return nil iff the live revision matches the
	// claimed value. Returning a wrapped types.ErrLeadershipRevisionMismatch
	// signals a takeover; any other error is treated as transient/abort.
	//
	// When nil, the publisher's leadership fences are no-ops (test-only). In
	// production, the manager wires this to its election agent's CheckLeadership
	// method so a former leader cannot pass the fence after another worker has
	// taken over.
	LeaderCheck func(ctx context.Context, claimed uint64) error

	// OnEnumerationError, if set, is invoked when worker enumeration (the
	// heartbeat Keys scan in WorkerMonitor.GetActiveWorkers) has failed
	// EnumerationFailureThreshold times in a row with a non-connectivity error —
	// notably a context.DeadlineExceeded on the stream-wide scan while single-key
	// heartbeat Puts still succeed (NP-10). The manager wires this to a DIRECT
	// degrade (NOT the transient KV-error window, which a succeeding heartbeat Put
	// would clear). When nil, sustained enumeration failure remains logged-only
	// (back-compat / test default). Must be safe for concurrent use.
	OnEnumerationError func(err error)

	// OnEnumerationSuccess, if set, is invoked on every SUCCESSFUL worker
	// enumeration — i.e. whenever the heartbeat Keys scan (GetActiveWorkers)
	// returns a nil error — REGARDLESS of the F10-A worker-credibility decision
	// (it fires even when a sharply-shrunk result is treated as suspicious). The
	// manager stamps it as the "enumeration recovered" signal its recovery exit
	// gate keys on. Must be safe for concurrent use.
	OnEnumerationSuccess func()

	// EnumerationFailureThreshold is the number of consecutive non-connectivity
	// enumeration failures before OnEnumerationError fires. Default: 3 (also
	// applied to any value < 1, so a zero/negative config cannot fire on the
	// first failure). Set explicitly to 1 to fire on the first failure.
	EnumerationFailureThreshold int
}

// Validate checks configuration validity.
//
// Returns an error if any required field is missing or invalid.
func (c *Config) Validate() error {
	if c.AssignmentKV == nil {
		return errors.New("the AssignmentKV is required")
	}
	if c.HeartbeatKV == nil {
		return errors.New("the HeartbeatKV is required")
	}
	if c.Source == nil {
		return errors.New("the Source is required")
	}
	if c.Strategy == nil {
		return errors.New("the Strategy is required")
	}
	if c.AssignmentPrefix == "" {
		return errors.New("the AssignmentPrefix is required")
	}
	if c.HeartbeatPrefix == "" {
		return errors.New("the HeartbeatPrefix is required")
	}
	if c.HeartbeatTTL == 0 {
		return errors.New("the HeartbeatTTL is required")
	}

	return nil
}

// SetDefaults applies default values for optional fields.
//
// This method is called automatically by NewCalculatorWithConfig.
// Fields that are already set (non-zero) are not overwritten.
func (c *Config) SetDefaults() {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.EmergencyGracePeriod == 0 {
		c.EmergencyGracePeriod = 5 * time.Second
	}
	if c.Cooldown == 0 {
		c.Cooldown = 10 * time.Second
	}
	if c.ColdStartWindow == 0 {
		c.ColdStartWindow = 30 * time.Second
	}
	if c.PlannedScaleWindow == 0 {
		c.PlannedScaleWindow = 10 * time.Second
	}
	if c.RebalanceGraceDrainInterval == 0 {
		c.RebalanceGraceDrainInterval = max(min(c.Cooldown, 30*time.Second), time.Second)
	}
	if c.ApplyGracePeriod == 0 {
		c.ApplyGracePeriod = 2 * c.HeartbeatTTL
	}
	if c.ExtendedApplyGracePeriod == 0 {
		c.ExtendedApplyGracePeriod = 5 * c.HeartbeatTTL
	}
	if c.AuditInterval == 0 {
		c.AuditInterval = c.HeartbeatTTL
	}
	if c.Metrics == nil {
		c.Metrics = metrics.NewNop()
	}
	if c.Logger == nil {
		c.Logger = logging.NewNop()
	}
	if c.PartitionShrinkConfirmationCount == 0 {
		c.PartitionShrinkConfirmationCount = 3
	}
	if c.PartitionShrinkConfirmationThresholdPct == 0 {
		c.PartitionShrinkConfirmationThresholdPct = 50
	}
	if c.WorkerShrinkConfirmationCount == 0 {
		c.WorkerShrinkConfirmationCount = 2
	}
	if c.WorkerShrinkConfirmationThresholdPct == 0 {
		c.WorkerShrinkConfirmationThresholdPct = 50
	}
	if c.EnumerationFailureThreshold < 1 {
		c.EnumerationFailureThreshold = 3
	}
}
