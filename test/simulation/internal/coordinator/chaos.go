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
		enabled:          cfg.Enabled,
		events:           events,
		minInterval:      cfg.MinInterval,
		maxInterval:      cfg.MaxInterval,
		eventCallback:    cfg.EventCallback,
		rng:              rand.New(rand.NewSource(time.Now().UnixNano())), //nolint:gosec // Weak RNG acceptable for chaos simulation
		burstEnabled:     cfg.BurstEnabled,
		burstProbability: burstProb,
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

	case WorkerPauseEvent:
		// Pause for 5-8 seconds to build backlog
		params["duration"] = time.Duration(cc.rng.Intn(4)+5) * time.Second

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
		// claimLostShutdown). Scenarios override via InjectEventNow.
		params["target_bucket"] = "parti-stableid"

	case BucketRecreateEvent:
		// Default to parti-assignment for recreate: exercises the
		// epoch-fence path without forcing all workers into
		// claim-lost shutdown (which happens when parti-stableid is
		// recreated). Scenarios that want the stableid recreate path
		// must pass target_bucket explicitly via InjectEventNow.
		params["target_bucket"] = "parti-assignment"

	case BucketPeerTakeoverEvent:
		// Default target_worker "random" — handler picks a live worker.
		params["target_worker"] = "random"

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
	case SlowConsumerEvent:
		return "Slow Consumer"
	case BucketDeleteEvent:
		return "Bucket Delete"
	case BucketRecreateEvent:
		return "Bucket Recreate"
	case BucketPeerTakeoverEvent:
		return "Bucket Peer Takeover"
	default:
		return fmt.Sprintf("Unknown Event: %s", string(e))
	}
}
