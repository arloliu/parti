package heartbeat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/arloliu/parti/v2/internal/logging"
	imetrics "github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/types"
)

// Common errors for heartbeat operations.
var (
	ErrNotStarted     = errors.New("publisher not started")
	ErrAlreadyStarted = errors.New("publisher already started")
	ErrNoWorkerID     = errors.New("worker ID not set")
)

const maxPublishTimeout = 5 * time.Second

// AppliedAssignment is a DTO carrying the state of the last successfully applied
// assignment. The manager passes this to Publisher.SetAppliedAssignment after a
// successful Apply call. All fields map 1:1 to the corresponding Heartbeat fields.
type AppliedAssignment struct {
	LeaderRevision        uint64
	AppliedVersion        int64
	AppliedDigest         uint64
	AppliedSourceRevision uint64
	AppliedSourceRevKnown bool
	AppliedAt             time.Time
}

// appliedSnapshot is the internal copy of AppliedAssignment plus SchemaVersion.
// SchemaVersion is set to 1 on the first successful SetAppliedAssignment call.
type appliedSnapshot struct {
	SchemaVersion         uint8
	LeaderRevision        uint64
	AppliedVersion        int64
	AppliedDigest         uint64
	AppliedSourceRevision uint64
	AppliedSourceRevKnown bool
	AppliedAt             time.Time
}

// Publisher publishes periodic heartbeats to NATS KV.
//
// Each worker maintains a heartbeat key in a KV bucket with a TTL.
// If the worker crashes or stops publishing, the key expires and other
// workers detect its absence.
//
// Heartbeats are published as JSON-encoded Heartbeat objects (v1 format).
// Legacy workers that publish raw RFC3339 timestamp strings are supported
// on the reader side via types.DecodeHeartbeat.
type Publisher struct {
	kv       jetstream.KeyValue
	prefix   string
	workerID string
	interval time.Duration

	// capsFn returns the current capability bitmask. Set once at construction
	// via SetCapabilitiesFn before Start is called.
	capsFn func() uint32

	// mu guards lifecycle fields: started, ticker, metrics, workerID.
	mu        sync.Mutex
	started   bool
	stopCh    chan struct{}
	doneCh    chan struct{}
	ticker    *time.Ticker
	logger    types.Logger
	metrics   types.WorkerMetrics
	onError   atomic.Pointer[func(error)]
	onSuccess atomic.Pointer[func()]

	// inFlight guards concurrent publish attempts; set atomically before
	// spawning the goroutine, cleared when the goroutine completes.
	inFlight atomic.Bool
	// inFlightWG tracks in-flight publish goroutines so Stop can drain them.
	inFlightWG sync.WaitGroup

	// appliedMu guards the applied snapshot; separate from mu so the publish
	// loop does not contend with lifecycle operations.
	appliedMu sync.RWMutex
	applied   appliedSnapshot
}

// New creates a new heartbeat publisher with dependency injection.
//
// The KV bucket should be configured with a TTL of ~3x the heartbeat interval
// to detect crashes after 3 missed heartbeats.
//
// Parameters:
//   - kv: JetStream KV bucket for heartbeat storage
//   - prefix: Key prefix for heartbeat keys (e.g., "worker-hb")
//   - workerID: Worker ID for heartbeat publishing
//   - interval: Heartbeat interval (typically 2s)
//   - metrics: Worker metrics collector (nil creates no-op metrics)
//   - logger: Logger instance (nil creates no-op logger)
//
// Returns:
//   - *Publisher: New heartbeat publisher instance
//
// Example:
//
//	kv, _ := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
//	    Bucket:  "parti-heartbeats",
//	    TTL:     6 * time.Second,  // 3x interval
//	    Storage: jetstream.FileStorage,
//	})
//	publisher := heartbeat.New(kv, "worker-hb", "worker-1", 2*time.Second, metrics, logger)
func New(
	kv jetstream.KeyValue,
	prefix string,
	workerID string,
	interval time.Duration,
	metrics types.WorkerMetrics,
	logger types.Logger,
) *Publisher {
	// Provide no-op implementations if nil
	if metrics == nil {
		metrics = imetrics.NewNop()
	}
	if logger == nil {
		logger = logging.NewNop()
	}

	return &Publisher{
		kv:       kv,
		prefix:   prefix,
		workerID: workerID,
		interval: interval,
		logger:   logger,
		metrics:  metrics,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
}

// SetCapabilitiesFn registers a function that returns the current capability
// bitmask. The publisher calls this function on every heartbeat composition.
//
// Call before Start. The function must be safe to call concurrently.
//
// Parameters:
//   - fn: Returns the current capability bitmask; nil is treated as "no capabilities"
func (p *Publisher) SetCapabilitiesFn(fn func() uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.capsFn = fn
}

// SetOnError registers a callback invoked on each heartbeat publish failure.
//
// Safe to call concurrently with the running publish goroutine; the update is
// atomic and the hot path reads without locking. Passing nil clears the
// callback. The callback must not block on the Publisher's own lifecycle
// (do not call Stop from inside it).
//
// Parameters:
//   - fn: Callback invoked with the raw error; ignored if nil
func (p *Publisher) SetOnError(fn func(error)) {
	if fn == nil {
		p.onError.Store(nil)
		return
	}
	p.onError.Store(&fn)
}

// SetOnSuccess registers a callback invoked after each successful periodic
// heartbeat publish (the background loop only — not the initial Start publish or
// PublishNow, mirroring SetOnError). The manager wires this to its degraded-mode
// circuit's healthy-op reset so a run of intermittent KV-unavailable timeouts
// only trips Degraded when no success intervenes.
//
// Safe to call concurrently with the running publish goroutine; the update is
// atomic and the hot path reads without locking. Passing nil clears the
// callback. The callback must not block on the Publisher's own lifecycle
// (do not call Stop from inside it).
//
// Parameters:
//   - fn: Callback invoked after a successful publish; ignored if nil
func (p *Publisher) SetOnSuccess(fn func()) {
	if fn == nil {
		p.onSuccess.Store(nil)
		return
	}
	p.onSuccess.Store(&fn)
}

// SetAppliedAssignment records a successfully applied assignment.
//
// Thread-safe and idempotent; monotone in AppliedVersion — calls with a
// version lower than the current snapshot are ignored (no regression).
// Equal versions are accepted (idempotent re-application).
//
// Call this from the manager after the handoff coordinator's Apply succeeds.
//
// Parameters:
//   - snap: The applied assignment state to record
func (p *Publisher) SetAppliedAssignment(snap AppliedAssignment) {
	p.appliedMu.Lock()
	defer p.appliedMu.Unlock()

	if snap.AppliedVersion < p.applied.AppliedVersion {
		return // monotone — do not regress
	}

	p.applied = appliedSnapshot{
		SchemaVersion:         1,
		LeaderRevision:        snap.LeaderRevision,
		AppliedVersion:        snap.AppliedVersion,
		AppliedDigest:         snap.AppliedDigest,
		AppliedSourceRevision: snap.AppliedSourceRevision,
		AppliedSourceRevKnown: snap.AppliedSourceRevKnown,
		AppliedAt:             snap.AppliedAt,
	}
}

// PublishNow forces an immediate out-of-band heartbeat publish.
//
// Used by the manager after SetAppliedAssignment so the leader's audit
// observes the new applied state within one heartbeat round-trip instead
// of waiting up to HeartbeatInterval.
//
// Parameters:
//   - ctx: Context for the KV Put operation
//
// Returns:
//   - error: KV write error, or ErrNotStarted if publisher is not running
func (p *Publisher) PublishNow(ctx context.Context) error {
	p.mu.Lock()
	if !p.started {
		p.mu.Unlock()
		return ErrNotStarted
	}
	p.mu.Unlock()

	return p.publish(ctx)
}

// Start begins publishing heartbeats in the background.
//
// Publishes the first heartbeat immediately, then at regular intervals.
// Continues until Stop() is called.
//
// Parameters:
//   - ctx: Context for initial heartbeat publish
//
// Returns:
//   - error: ErrAlreadyStarted if already running, ErrNoWorkerID if worker ID not set
func (p *Publisher) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.started {
		return ErrAlreadyStarted
	}

	if p.workerID == "" {
		return ErrNoWorkerID
	}

	// Re-create channels so Publisher can be restarted after Stop().
	p.stopCh = make(chan struct{})
	p.doneCh = make(chan struct{})
	p.started = true
	p.ticker = time.NewTicker(p.interval)

	// Publish first heartbeat immediately with provided context (prevents startup hang)
	if err := p.publish(ctx); err != nil {
		p.ticker.Stop()
		p.started = false
		return fmt.Errorf("failed to publish initial heartbeat: %w", err)
	}

	// Start background publisher. Lifecycle is managed by stopCh (via Stop),
	// not by the context passed to Start — the publisher outlives any
	// startup/request context and runs until Stop is called.
	go p.publishLoop() //nolint:gosec // G118: goroutine lifecycle is stopCh-driven, not context-driven

	return nil
}

// Stop stops the heartbeat publisher and deletes the heartbeat entry from KV.
//
// Blocks until the publisher goroutine exits and cleanup completes.
// The heartbeat entry is deleted to immediately signal worker shutdown
// instead of waiting for TTL expiration.
//
// Returns:
//   - error: ErrNotStarted if not running, or cleanup error if delete fails
func (p *Publisher) Stop() error {
	p.mu.Lock()

	if !p.started {
		p.mu.Unlock()
		return ErrNotStarted
	}

	// Stop ticker and signal goroutine
	p.ticker.Stop()
	close(p.stopCh)
	p.started = false

	p.mu.Unlock()

	// Wait for goroutine to finish
	<-p.doneCh

	// Delete heartbeat entry from KV (cleanup)
	// Use background context with timeout since worker is shutting down
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	key := p.keyForWorker(p.workerID)
	if err := p.kv.Delete(ctx, key); err != nil {
		// Don't fail on cleanup errors, but return them for logging
		return fmt.Errorf("stopped but failed to delete heartbeat: %w", err)
	}

	return nil
}

// publishLoop is the background goroutine that publishes heartbeats.
func (p *Publisher) publishLoop() {
	defer close(p.doneCh)
	defer p.inFlightWG.Wait() // drain any in-flight goroutine before signalling done

	for {
		select {
		case <-p.stopCh:
			return
		case <-p.ticker.C:
			if !p.inFlight.CompareAndSwap(false, true) {
				p.recordSkippedPublish()
				continue
			}
			p.inFlightWG.Go(func() {
				defer p.inFlight.Store(false)

				timeout := min(maxPublishTimeout, p.interval/2)
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				err := p.publish(ctx)
				cancel()

				if err != nil {
					p.recordMetric(false)
					p.logger.Warn("heartbeat publish failed", "worker_id", p.workerID, "error", err)
					if onError := p.onError.Load(); onError != nil {
						(*onError)(err)
					}
				} else {
					p.recordMetric(true)
					if onSuccess := p.onSuccess.Load(); onSuccess != nil {
						(*onSuccess)()
					}
				}
			})
		}
	}
}

// recordSkippedPublish logs a warning when a tick is skipped because the
// prior publish goroutine is still in flight.
func (p *Publisher) recordSkippedPublish() {
	p.logger.Warn("heartbeat publish in flight, skipping tick", "worker_id", p.workerID)
}

// build composes a Heartbeat value from the current applied snapshot and the
// capabilities function. Timestamp is always set to time.Now().
func (p *Publisher) build() types.Heartbeat {
	p.appliedMu.RLock()
	snap := p.applied
	p.appliedMu.RUnlock()

	var caps uint32
	if p.capsFn != nil {
		caps = p.capsFn()
	}
	caps |= types.CapAckV1 // intrinsic: this publisher always emits v1 JSON, which is the definition of CapAckV1

	return types.Heartbeat{
		WorkerID:              p.workerID,
		SchemaVersion:         1, // v1 JSON writers always emit SchemaVersion=1; 0 is only for decoded legacy payloads
		Capabilities:          caps,
		LeaderRevision:        snap.LeaderRevision,
		AppliedVersion:        snap.AppliedVersion,
		AppliedDigest:         snap.AppliedDigest,
		AppliedSourceRevision: snap.AppliedSourceRevision,
		AppliedSourceRevKnown: snap.AppliedSourceRevKnown,
		AppliedAt:             snap.AppliedAt,
		Timestamp:             time.Now(),
	}
}

// publish writes the heartbeat to NATS KV as a v1 JSON payload.
func (p *Publisher) publish(ctx context.Context) error {
	key := p.keyForWorker(p.workerID)

	hb := p.build()
	value, err := json.Marshal(hb)
	if err != nil {
		return fmt.Errorf("failed to marshal heartbeat for %s: %w", p.workerID, err)
	}

	_, err = p.kv.Put(ctx, key, value)
	if err != nil {
		return fmt.Errorf("failed to publish heartbeat for %s: %w", p.workerID, err)
	}

	return nil
}

// keyForWorker generates the KV key for a worker's heartbeat.
func (p *Publisher) keyForWorker(workerID string) string {
	return fmt.Sprintf("%s.%s", p.prefix, workerID)
}

// recordMetric records heartbeat success/failure to metrics collector.
func (p *Publisher) recordMetric(success bool) {
	p.mu.Lock()
	metrics := p.metrics
	workerID := p.workerID
	p.mu.Unlock()

	if metrics != nil {
		metrics.RecordHeartbeat(workerID, success)
	}
}

// WorkerID returns the current worker ID.
//
// Returns:
//   - string: Worker ID
func (p *Publisher) WorkerID() string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.workerID
}

// IsStarted returns whether the publisher is currently running.
//
// Returns:
//   - bool: true if started, false otherwise
func (p *Publisher) IsStarted() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.started
}
