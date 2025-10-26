package heartbeat

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/arloliu/parti/types"
)

// Common errors for heartbeat operations.
var (
	ErrNotStarted     = errors.New("publisher not started")
	ErrAlreadyStarted = errors.New("publisher already started")
	ErrNoWorkerID     = errors.New("worker ID not set")
)

// Publisher publishes periodic heartbeats to NATS KV store.
//
// Heartbeats are used by the leader to detect worker crashes and trigger
// reassignment. Each worker publishes heartbeats at a regular interval,
// and the leader monitors for missed heartbeats.
//
// The heartbeat key contains the worker's last heartbeat timestamp and
// is automatically deleted when the TTL expires, indicating a crash.
type Publisher struct {
	kv       jetstream.KeyValue
	prefix   string
	workerID string
	interval time.Duration
	metrics  types.MetricsCollector

	mu      sync.Mutex
	started bool
	stopCh  chan struct{}
	doneCh  chan struct{}
	ticker  *time.Ticker
}

// New creates a new heartbeat publisher.
//
// The KV bucket should be configured with a TTL of ~3x the heartbeat interval
// to detect crashes after 3 missed heartbeats.
//
// Parameters:
//   - kv: JetStream KV bucket for heartbeat storage
//   - prefix: Key prefix for heartbeat keys (e.g., "worker-hb")
//   - interval: Heartbeat interval (typically 2s)
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
//	publisher := heartbeat.New(kv, "worker-hb", 2*time.Second)
func New(kv jetstream.KeyValue, prefix string, interval time.Duration) *Publisher {
	return &Publisher{
		kv:       kv,
		prefix:   prefix,
		interval: interval,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
}

// SetWorkerID sets the worker ID for heartbeat publishing.
//
// Must be called before Start().
//
// Parameters:
//   - workerID: Worker ID to use in heartbeat key
func (p *Publisher) SetWorkerID(workerID string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.workerID = workerID
}

// SetMetrics sets the metrics collector for heartbeat events.
//
// Optional. If not set, metrics are not recorded.
//
// Parameters:
//   - metrics: Metrics collector instance
func (p *Publisher) SetMetrics(metrics types.MetricsCollector) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.metrics = metrics
}

// Start begins publishing heartbeats in the background.
//
// Publishes the first heartbeat immediately, then at regular intervals.
// Continues until Stop() is called.
//
// Parameters:
//   - ctx: Context for cancellation
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

	p.started = true
	p.ticker = time.NewTicker(p.interval)

	// Publish first heartbeat immediately
	if err := p.publish(ctx); err != nil {
		p.started = false
		return fmt.Errorf("failed to publish initial heartbeat: %w", err)
	}

	// Start background publisher
	go p.publishLoop()

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

	for {
		select {
		case <-p.stopCh:
			return
		case <-p.ticker.C:
			// Use background context since this is a long-running goroutine
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := p.publish(ctx)
			cancel()

			if err != nil {
				p.recordMetric(false)
				// Log error but continue trying
				// In production, this should use the configured logger
				_ = err
			} else {
				p.recordMetric(true)
			}
		}
	}
}

// publish writes the heartbeat to NATS KV.
func (p *Publisher) publish(ctx context.Context) error {
	key := p.keyForWorker(p.workerID)
	value := []byte(time.Now().Format(time.RFC3339Nano))

	// Try to update first (more common case)
	_, err := p.kv.Put(ctx, key, value)
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
