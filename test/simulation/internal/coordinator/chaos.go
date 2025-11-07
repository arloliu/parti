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

	// NetworkDisconnectEvent simulates NATS connection loss.
	NetworkDisconnectEvent ChaosEvent = "network_disconnect"
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
}

// ChaosConfig configures the chaos controller.
type ChaosConfig struct {
	Enabled       bool
	Events        []string
	MinInterval   time.Duration
	MaxInterval   time.Duration
	EventCallback func(ChaosEvent, map[string]any)
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

	return &ChaosController{
		enabled:       cfg.Enabled,
		events:        events,
		minInterval:   cfg.MinInterval,
		maxInterval:   cfg.MaxInterval,
		eventCallback: cfg.EventCallback,
		rng:           rand.New(rand.NewSource(time.Now().UnixNano())), //nolint:gosec // Weak RNG acceptable for chaos simulation
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

	case NetworkDisconnectEvent:
		// Disconnect for 5-30 seconds
		params["duration"] = time.Duration(cc.rng.Intn(26)+5) * time.Second

	default:
		// Unknown event type, return empty params
	}

	return params
}

// randomInterval returns a random interval between min and max.
func (cc *ChaosController) randomInterval() time.Duration {
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
		return "Network Disconnect"
	default:
		return fmt.Sprintf("Unknown Event: %s", string(e))
	}
}
