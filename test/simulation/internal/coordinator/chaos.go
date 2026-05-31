package coordinator

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"sync/atomic"
	"time"
)

// ChaosEvent represents a chaos event type.
type ChaosEvent string

const (
	// WorkerCrashEvent simulates a worker process crash (SIGKILL).
	WorkerCrashEvent ChaosEvent = "worker_crash"

	// WorkerRestartEvent simulates a graceful worker restart.
	WorkerRestartEvent ChaosEvent = "worker_restart"

	// ScaleUpEvent adds N workers to the system.
	ScaleUpEvent ChaosEvent = "scale_up"

	// ScaleDownEvent removes N workers from the system.
	ScaleDownEvent ChaosEvent = "scale_down"

	// LeaderFailureEvent kills the current leader worker.
	LeaderFailureEvent ChaosEvent = "leader_failure"

	// ProducerCrashEvent simulates a producer process crash.
	ProducerCrashEvent ChaosEvent = "producer_crash"

	// NetworkDisconnectEvent simulates NATS connection loss on a random worker.
	NetworkDisconnectEvent ChaosEvent = "network_disconnect"

	// NetworkDisconnectLeaderEvent simulates NATS connection loss targeted
	// at the current leader worker — the "Split Brain" scenario the audit's
	// DESIGN_REVIEW.md called out. All-in-one mode only; process-mode
	// dispatch logs and skips because process-mode has no real leader
	// lookup.
	NetworkDisconnectLeaderEvent ChaosEvent = "network_disconnect_leader"

	// WorkerPauseEvent temporarily pauses a worker's processing without removing it.
	WorkerPauseEvent ChaosEvent = "worker_pause"

	// WorkerResumeEvent resumes a previously-paused worker. Process-mode
	// only: sends SIGCONT to the target worker process. Pair with a
	// scheduled WorkerPauseEvent that disables auto-resume (duration<=0).
	// Phase 7b scenarios use this to drive the long-SIGSTOP / same-pool
	// replacement / SIGCONT sequence that exercises the returning-worker
	// claim-loss self-stop path (manager_election.go:claimLostShutdown).
	WorkerResumeEvent ChaosEvent = "worker_resume"

	// SlowConsumerEvent slows down message processing to simulate backpressure.
	SlowConsumerEvent ChaosEvent = "slow_consumer"

	// BucketDeleteEvent deletes a Parti-owned KV bucket out from under the
	// running cluster, exercising the whole-bucket-loss degraded path
	// (recordKVError → enterDegraded("bucket-unavailable:<bucket>")) and
	// the epoch-fence path once the bucket is re-ensured by a worker.
	BucketDeleteEvent ChaosEvent = "bucket_delete"

	// BucketRecreateEvent deletes then immediately re-creates a Parti-owned
	// KV bucket, exercising the bucket-recreate degraded path
	// (monitorBucketEpochs → enterDegraded("bucket-recreated:<bucket>"))
	// via a fresh stream Created timestamp.
	BucketRecreateEvent ChaosEvent = "bucket_recreate"

	// BucketPeerTakeoverEvent steals a specific worker's stable-ID claim
	// key by Putting a fresh value at the worker's claim key, bumping the
	// revision and forcing the victim's next renew to return ErrClaimLost
	// (claimer.go:362-369), driving claimLostShutdown.
	BucketPeerTakeoverEvent ChaosEvent = "bucket_peer_takeover"

	// WatcherStallEvent simulates a stalled KV watcher on the partition
	// source bucket WITHOUT dropping the NATS connection. Implementation:
	// repeatedly deletes the ephemeral JetStream consumers backing the
	// `KV_parti-sim-source` stream's watchers across the duration window;
	// the in-process watcher restart loop (watcherMaxAttempts=6, backoff
	// 2s..30s) exhausts and the reconciler becomes the sole recovery path.
	// All workers' watchers on the source stream are stalled simultaneously
	// (one stream, N ephemeral consumers); `target_worker` is recorded but
	// not honored (documented as a follow-up).
	WatcherStallEvent ChaosEvent = "watcher_stall"

	// StableIDClaimStealEvent is the Phase 4 plan-name alias for
	// BucketPeerTakeoverEvent. Both kinds dispatch to the same primitive
	// (revision-bumping kv.Put against the victim's claim key in
	// parti-stableid) — kept as a distinct constant so scenario YAML and
	// plan terminology stay greppable.
	StableIDClaimStealEvent ChaosEvent = "stableid_claim_steal"

	// StableIDTinyPoolRespawnEvent spawns a fresh worker into the dead
	// worker's tiny stable-ID pool, exercising the stale-takeover
	// Update path in stableid.Claimer (claimer.go:219-243). The fresh
	// worker MUST be scheduled strictly later than staleThreshold =
	// 3 × max(WorkerIDTTL/3, 100ms) plus scheduler margin after the
	// previous holder went offline; scheduling it inside TTL only
	// returns ErrNoAvailableID (claimer.go:244-253). All-in-one mode
	// only for Phase 4a; process-mode propagation handled by Phase 4c.
	StableIDTinyPoolRespawnEvent ChaosEvent = "stableid_tiny_pool_respawn"

	// NetworkDisconnectLongEvent simulates a prolonged NATS connection loss
	// (60–180s) on a random worker, outliving HeartbeatTTL × 2. This drives
	// the full lease-expiry → reassignment → post-recovery convergence path
	// (Gap 12). All-in-one mode only; process-mode dispatch logs and skips
	// because the underlying Worker.Disconnect()/Reconnect() surface requires
	// an in-process NetworkControl handle.
	//
	// Duration is drawn from [60s, 180s] by default and can be overridden
	// via the `duration` param in InjectEventNow or via scenario config.
	NetworkDisconnectLongEvent ChaosEvent = "network_disconnect_long"

	// ProducerDisconnectEvent simulates a NATS network disconnect on the
	// producer side (Gap 10a). It uses the producer's dedicated NetworkControl
	// to sever the underlying TCP connection without touching the worker or
	// manager connections. Duration is drawn from [10s, 30s] by default.
	//
	// INV1: the producer's publish loop must resume within T_pub=5s after
	// reconnect; observed via producer_resume_observed counter. All-in-one
	// only; process-mode logs and skips.
	ProducerDisconnectEvent ChaosEvent = "producer_disconnect"

	// AssignmentCASStormEvent spawns a competing KV writer goroutine that
	// issues stale-revision Update calls against the assignment._commit key
	// in the parti-assignment bucket for a configurable window (5–15s).
	// The writer uses a FRESH JetStream context (dedicated probe handle
	// pattern) so it cannot share cached state with the production manager.
	//
	// INV2: the leader's commit path either succeeds or returns
	// ErrCommitCASFailed; no partial alias-without-commit state outlives the
	// next successful commit. Observed via SnapshotOverlapClassifier (overlap
	// = alias/commit inconsistency) and assignment_cas_storm_unexpected_panic
	// counter (must be 0). All-in-one only; process-mode logs and skips.
	AssignmentCASStormEvent ChaosEvent = "assignment_cas_storm"

	// AssignmentBucketDeleteEvent is a named alias for BucketDeleteEvent
	// with target_bucket fixed to the parti-assignment bucket (Gap 10b).
	// Routing through the same handleBucketDelete primitive ensures INV3
	// (DegradedReasonOracle) is wired automatically, and makes the scenario
	// YAML self-documenting. All-in-one only; process-mode logs and skips.
	AssignmentBucketDeleteEvent ChaosEvent = "assignment_bucket_delete"

	// HandoffOrphanClaimWriteEvent (Phase 6 / Gap 5) writes a synthetic
	// stable handoff claim whose Owner is NOT in any current
	// AssignmentReport. The orphan must survive the sweeper window
	// (≥ 2 × Handoff.SweepInterval) because the sweeper at
	// internal/assignment/handoff/twophase.go:434-488 unconditionally
	// skips claims in ClaimStateStable. Any drift (deletion, owner reset,
	// state change) increments orphan_stable_claim_drift and fails the
	// scenario. All-in-one only; process-mode logs and skips.
	HandoffOrphanClaimWriteEvent ChaosEvent = "handoff_orphan_claim_write"

	// KVUnavailableEvent faults selected manager-owned KV buckets with
	// context.DeadlineExceeded while leaving the NATS connection connected.
	// This exercises the connected-but-KV-unavailable / quorum-loss path
	// that should enter Degraded with reason "kv-unavailable".
	KVUnavailableEvent ChaosEvent = "kv_unavailable"

	// HandoffClaimWriteFaultEvent faults writes to handoff claims/* keys
	// while leaving reads and non-claim keys available. This exercises
	// startup and version-advance apply retry/self-heal paths.
	HandoffClaimWriteFaultEvent ChaosEvent = "handoff_claim_write_fault"
)

// ChaosController manages chaos event injection.
type ChaosController struct {
	enabled       bool
	events        []ChaosEvent
	minInterval   time.Duration
	maxInterval   time.Duration
	eventCallback func(ChaosEvent, map[string]any)
	rng           *rand.Rand
	started       atomic.Bool // Prevents multiple Start() calls

	// Burst mode: rapid-fire events followed by quiet periods
	burstEnabled     bool
	burstProbability float64 // probability to enter burst (0.0-1.0)
	burstMode        bool    // currently in burst?
	burstRemaining   int     // events left in current burst

	// bucketDeleteTargetOverride, when non-empty, replaces the default
	// bucket target for BucketDeleteEvent params.
	bucketDeleteTargetOverride string

	// longDisconnectDurationOverride, when > 0, replaces the random 60-180s
	// duration for NetworkDisconnectLongEvent params.
	longDisconnectDurationOverride time.Duration
}

// ChaosConfig configures the chaos controller.
type ChaosConfig struct {
	Enabled       bool
	Events        []string
	MinInterval   time.Duration
	MaxInterval   time.Duration
	EventCallback func(ChaosEvent, map[string]any)

	// Burst mode configuration
	BurstEnabled     bool    // Enable burst mode for variable intensity
	BurstProbability float64 // Probability to enter burst (0.0-1.0), default 0.2

	// BucketDeleteTargetOverride, when non-empty, replaces the default
	// "parti-stableid" target for BucketDeleteEvent's generated params.
	// Phase 2 composed scenarios set this to "parti-sim-source" so the
	// chaos deletes the partition-source bucket (exercising the source-
	// unavailable adapter and INV3) instead of the stableid bucket.
	BucketDeleteTargetOverride string

	// LongDisconnectDurationOverride, when > 0, replaces the random 60-180s
	// duration for NetworkDisconnectLongEvent. Phase 3 scenarios set this to
	// a fixed value (e.g. 60s) to keep the disconnect below WorkerIDTTL (75s
	// default) so the stable-ID claim does not expire and claimLostShutdown
	// does not fire unexpectedly.
	LongDisconnectDurationOverride time.Duration
}

// NewChaosController creates a new chaos controller.
//
// Parameters:
//   - cfg: Chaos configuration
//
// Returns:
//   - *ChaosController: Initialized chaos controller
func NewChaosController(cfg ChaosConfig) *ChaosController {
	events := make([]ChaosEvent, len(cfg.Events))
	for i, e := range cfg.Events {
		events[i] = ChaosEvent(e)
	}

	// Default burst probability if not set
	burstProb := cfg.BurstProbability
	if burstProb <= 0 {
		burstProb = 0.2 // 20% default
	}

	return &ChaosController{
		enabled:                        cfg.Enabled,
		events:                         events,
		minInterval:                    cfg.MinInterval,
		maxInterval:                    cfg.MaxInterval,
		eventCallback:                  cfg.EventCallback,
		rng:                            rand.New(rand.NewSource(time.Now().UnixNano())), //nolint:gosec // Weak RNG acceptable for chaos simulation
		burstEnabled:                   cfg.BurstEnabled,
		burstProbability:               burstProb,
		bucketDeleteTargetOverride:     cfg.BucketDeleteTargetOverride,
		longDisconnectDurationOverride: cfg.LongDisconnectDurationOverride,
	}
}

// Start begins chaos event injection.
//
// Safe to call multiple times - subsequent calls are ignored.
//
// Parameters:
//   - ctx: Context for cancellation
func (cc *ChaosController) Start(ctx context.Context) {
	if !cc.enabled {
		log.Println("[Chaos] Chaos mode disabled")
		return
	}

	if len(cc.events) == 0 {
		log.Println("[Chaos] No chaos events configured")
		return
	}

	// Prevent multiple starts - this fixes the goroutine leak
	if !cc.started.CompareAndSwap(false, true) {
		log.Println("[Chaos] Already started, ignoring duplicate Start() call")
		return
	}

	log.Printf("[Chaos] Starting chaos controller with %d event types, interval: %v-%v",
		len(cc.events), cc.minInterval, cc.maxInterval)

	go cc.run(ctx)
}

// run is the main chaos injection loop.
func (cc *ChaosController) run(ctx context.Context) {
	defer cc.started.Store(false) // Allow restart after context cancellation

	for {
		// Calculate next event time
		interval := cc.randomInterval()
		log.Printf("[Chaos] Next event in %v", interval)

		select {
		case <-ctx.Done():
			log.Println("[Chaos] Stopping chaos controller")
			return
		case <-time.After(interval):
			cc.injectEvent()
		}
	}
}

// injectEvent selects and injects a random chaos event.
func (cc *ChaosController) injectEvent() {
	if len(cc.events) == 0 {
		return
	}

	// Select random event
	event := cc.events[cc.rng.Intn(len(cc.events))]

	// Generate event parameters
	params := cc.generateEventParams(event)

	log.Printf("[Chaos] Injecting event: %s with params: %v", event, params)

	// Trigger event
	if cc.eventCallback != nil {
		cc.eventCallback(event, params)
	}
}

// generateEventParams generates parameters for a specific chaos event.
//
//nolint:cyclop // exhaustive switch over chaos-event defaults; complexity grows linearly.
func (cc *ChaosController) generateEventParams(event ChaosEvent) map[string]any {
	params := make(map[string]any)

	switch event {
	case WorkerCrashEvent, ProducerCrashEvent:
		// Crash random worker or producer
		params["target"] = "random"
		params["signal"] = "SIGKILL"

	case WorkerRestartEvent:
		params["target"] = "random"
		params["graceful"] = true

	case ScaleUpEvent:
		// Add 1-10 workers
		params["count"] = cc.rng.Intn(10) + 1

	case ScaleDownEvent:
		// Remove 1-5 workers
		params["count"] = cc.rng.Intn(5) + 1

	case LeaderFailureEvent:
		params["target"] = "leader"
		params["signal"] = "SIGKILL"

	case NetworkDisconnectEvent, NetworkDisconnectLeaderEvent:
		// Disconnect for 5-15 seconds (both random and leader-target
		// variants share the same duration range — the only difference
		// is which worker the dispatcher selects). Upper bound is capped
		// at 15s so a single disconnect cannot exceed the simulation's
		// slow-start budgets when chaos fires during cold start; longer
		// outages don't add new coverage but do produce flaky start-
		// latency exceedances for chaos-delayed initial-cohort workers.
		params["duration"] = time.Duration(cc.rng.Intn(11)+5) * time.Second

	case NetworkDisconnectLongEvent:
		// Disconnect for 60-180s, exceeding HeartbeatTTL (15s) × 2 = 30s.
		// This forces lease expiry, reassignment to remaining workers, and
		// post-reconnect convergence — the full Gap 12 scenario.
		// Duration is capped below WorkerIDTTL (75s default) by scenario
		// config override to avoid triggering claimLostShutdown (which
		// requires duration > WorkerIDTTL to fire). When override is set,
		// it replaces the random range entirely.
		if cc.longDisconnectDurationOverride > 0 {
			params["duration"] = cc.longDisconnectDurationOverride
		} else {
			params["duration"] = time.Duration(cc.rng.Intn(121)+60) * time.Second // 60–180s
		}

	case WorkerPauseEvent:
		// Pause for 5-8 seconds to build backlog
		params["duration"] = time.Duration(cc.rng.Intn(4)+5) * time.Second

	case WorkerResumeEvent:
		// Scenario-driven only. Random-cadence chaos never synthesizes
		// WorkerResumeEvent (it pairs with an open-ended WorkerPauseEvent
		// via scheduled_events). Mark the default target so a misrouted
		// invocation logs an actionable message instead of silently
		// SIGCONT'ing nothing.
		params["target_worker"] = ""

	case SlowConsumerEvent:
		// Slow down processing by 3x-10x for 5-15 seconds. The upper bound is
		// capped so that a slow window's accumulated backlog can drain within
		// the 240s gap_aging threshold even under worst-case scheduling; prior
		// ranges (10x-50x / 10-30s) produced backlogs that could not catch up
		// before aging out into unresolved gaps when chaos reassigned the
		// partition mid-slow-window.
		params["multiplier"] = cc.rng.Intn(8) + 3 // 3-10x
		params["duration"] = time.Duration(cc.rng.Intn(11)+5) * time.Second

	case BucketDeleteEvent:
		// Default to parti-stableid for delete: exercises the
		// onClaimerError routing boundary (recordKVError vs
		// claimLostShutdown). Scenarios override via InjectEventNow,
		// OR via ChaosConfig.BucketDeleteTargetOverride for
		// scenario-wide replacement (Phase 2 composed scenario).
		if cc.bucketDeleteTargetOverride != "" {
			params["target_bucket"] = cc.bucketDeleteTargetOverride
		} else {
			params["target_bucket"] = "parti-stableid"
		}

	case BucketRecreateEvent, AssignmentBucketDeleteEvent:
		// BucketRecreateEvent: default to parti-assignment for recreate —
		// exercises the epoch-fence path without forcing all workers into
		// claim-lost shutdown (which happens when parti-stableid is
		// recreated). Scenarios that want the stableid recreate path must
		// pass target_bucket explicitly via InjectEventNow.
		//
		// AssignmentBucketDeleteEvent: always targets parti-assignment
		// (Gap 10b named alias). Fixed target makes scenario YAML self-
		// documenting.
		params["target_bucket"] = "parti-assignment"

	case BucketPeerTakeoverEvent, StableIDClaimStealEvent:
		// Default target_worker "random" — handler picks a live worker.
		params["target_worker"] = "random"

	case StableIDTinyPoolRespawnEvent:
		// Default target_role "" — scenario-driven via InjectEventNow.
		// The handler will look up the role's WorkerID* overrides from
		// the scenario config and respawn an all-in-one worker against
		// the same tiny pool.
		params["target_role"] = ""

	case WatcherStallEvent:
		// Default subject prefix targets the parti-sim-source KV stream;
		// duration must exceed reconcileInterval (5s in NATSKV-source
		// scenarios) AND outlive the watcherMaxAttempts × backoff window
		// (>= ~30s) so the reconciler is the only viable recovery path,
		// while staying under bucketUnavailableCooldown (30s default) to
		// avoid spurious source-unavailable trips per INV2.
		params["subject_prefix"] = "$KV.parti-sim-source.>"
		params["duration"] = 45 * time.Second

	case ProducerDisconnectEvent:
		// Disconnect for 10–30s (Gap 10a). Upper bound is 30s so the
		// disconnect does not swallow the entire scenario window; the
		// NATS client will attempt reconnect across this window and the
		// publish loop must resume within T_pub=5s post-reconnect (INV1).
		params["duration"] = time.Duration(cc.rng.Intn(21)+10) * time.Second

	case AssignmentCASStormEvent:
		// Storm window 5–15s: enough to race several publisher commits
		// without outlasting the scenario's measurement window. Use
		// rng.Intn(10)+6 (6..15s) — functionally equivalent but a distinct
		// expression from the NetworkDisconnect 5..15s range above.
		params["duration"] = time.Duration(cc.rng.Intn(10)+6) * time.Second

	case HandoffOrphanClaimWriteEvent:
		// Phase 6 / Gap 5 one-shot. Drift observation window defaults to
		// 60s (= 2 × default Handoff.SweepInterval). Scenarios override
		// via the YAML scheduled_events params.
		params["observation_window"] = 60 * time.Second

	case KVUnavailableEvent:
		// Sustained connected-but-KV-unavailable fault. The dispatcher faults
		// these buckets while the NATS connection itself stays up.
		params["duration"] = 20 * time.Second
		params["buckets"] = []string{"parti-election", "parti-heartbeat", "parti-stableid"}
		params["expect_degraded"] = true

	case HandoffClaimWriteFaultEvent:
		// Claim-write fault window; scenarios choose the exact schedule.
		params["duration"] = 20 * time.Second

	default:
		// Unknown event type, return empty params
	}

	return params
}

// randomInterval returns a random interval between min and max.
// When burst mode is enabled, periodically enters rapid-fire mode with
// 5-10 events at 1-5s intervals, followed by a quiet period of 60-120s.
func (cc *ChaosController) randomInterval() time.Duration {
	// Burst mode logic
	if cc.burstEnabled {
		// Check if we should enter burst mode (only when not already in one)
		if !cc.burstMode && cc.rng.Float64() < cc.burstProbability {
			cc.burstMode = true
			cc.burstRemaining = cc.rng.Intn(5) + 5 // 5-10 rapid events
			log.Println("[Chaos] Entering burst mode")
		}

		if cc.burstMode {
			cc.burstRemaining--
			if cc.burstRemaining <= 0 {
				cc.burstMode = false
				// Long quiet period after burst (60-120s)
				quietDuration := time.Duration(cc.rng.Intn(60)+60) * time.Second
				log.Printf("[Chaos] Exiting burst mode, quiet period: %v", quietDuration)
				return quietDuration
			}
			// Rapid fire during burst (1-5s)
			return time.Duration(cc.rng.Intn(4)+1) * time.Second
		}
	}

	// Normal interval
	if cc.minInterval == cc.maxInterval {
		return cc.minInterval
	}

	diff := cc.maxInterval - cc.minInterval
	random := time.Duration(cc.rng.Int63n(int64(diff)))

	return cc.minInterval + random
}

// Disable disables chaos event injection.
func (cc *ChaosController) Disable() {
	cc.enabled = false
	log.Println("[Chaos] Chaos mode disabled")
}

// Enable enables chaos event injection.
func (cc *ChaosController) Enable() {
	cc.enabled = true
	log.Println("[Chaos] Chaos mode enabled")
}

// InjectEventNow immediately injects a specific event.
//
// Parameters:
//   - event: Event type to inject
//   - params: Event parameters (optional)
func (cc *ChaosController) InjectEventNow(event ChaosEvent, params map[string]any) {
	if params == nil {
		params = cc.generateEventParams(event)
	}

	log.Printf("[Chaos] Manually injecting event: %s with params: %v", event, params)

	if cc.eventCallback != nil {
		cc.eventCallback(event, params)
	}
}

// GetAvailableEvents returns the list of configured chaos events.
//
// Returns:
//   - []ChaosEvent: List of configured chaos events
func (cc *ChaosController) GetAvailableEvents() []ChaosEvent {
	return cc.events
}

// IsEnabled returns whether chaos mode is currently enabled.
//
// Returns:
//   - bool: true if chaos mode is enabled
func (cc *ChaosController) IsEnabled() bool {
	return cc.enabled
}

// String returns a human-readable description of a chaos event.
//
//nolint:cyclop // exhaustive switch over chaos-event kinds; complexity grows linearly.
func (e ChaosEvent) String() string {
	switch e {
	case WorkerCrashEvent:
		return "Worker Crash (SIGKILL)"
	case WorkerRestartEvent:
		return "Worker Restart (Graceful)"
	case ScaleUpEvent:
		return "Scale Up (Add Workers)"
	case ScaleDownEvent:
		return "Scale Down (Remove Workers)"
	case LeaderFailureEvent:
		return "Leader Failure (Kill Leader)"
	case ProducerCrashEvent:
		return "Producer Crash (SIGKILL)"
	case NetworkDisconnectEvent:
		return "Network Disconnect (Random)"
	case NetworkDisconnectLeaderEvent:
		return "Network Disconnect (Leader)"
	case WorkerPauseEvent:
		return "Worker Pause"
	case WorkerResumeEvent:
		return "Worker Resume (SIGCONT)"
	case SlowConsumerEvent:
		return "Slow Consumer"
	case BucketDeleteEvent:
		return "Bucket Delete"
	case BucketRecreateEvent:
		return "Bucket Recreate"
	case BucketPeerTakeoverEvent:
		return "Bucket Peer Takeover"
	case StableIDClaimStealEvent:
		return "StableID Claim Steal (alias of bucket_peer_takeover)"
	case StableIDTinyPoolRespawnEvent:
		return "StableID Tiny Pool Respawn (stale-takeover)"
	case WatcherStallEvent:
		return "Watcher Stall (KV source watcher)"
	case NetworkDisconnectLongEvent:
		return "Network Disconnect Long (60-180s, lease expiry path)"
	case ProducerDisconnectEvent:
		return "Producer Disconnect (10-30s, Gap 10a)"
	case AssignmentCASStormEvent:
		return "Assignment CAS Storm (competing _commit writer, Gap 10b)"
	case AssignmentBucketDeleteEvent:
		return "Assignment Bucket Delete (parti-assignment, Gap 10b alias)"
	case HandoffOrphanClaimWriteEvent:
		return "Handoff Orphan Claim Write (Gap 5; stable-claim sweep skip)"
	case KVUnavailableEvent:
		return "KV Unavailable (connected KV timeout)"
	case HandoffClaimWriteFaultEvent:
		return "Handoff Claim Write Fault"
	default:
		return fmt.Sprintf("Unknown Event: %s", string(e))
	}
}
