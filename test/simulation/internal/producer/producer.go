package producer

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti/v2/test/simulation/internal/metrics"
	"github.com/nats-io/nats.go/jetstream"
)

// Producer sends messages to assigned partitions.
type Producer struct {
	id                  string
	js                  jetstream.JetStream
	partitionIDs        []int
	weights             []int64
	baseRate            float64
	coordinatorReportCh chan<- ReportMessage
	metricsCollector    *metrics.Collector
	mu                  sync.Mutex
	partitionSequences  map[int]int64
	producerSequence    int64
	started             atomic.Bool // Prevents multiple Start() calls

	// networkControl is non-nil when this producer owns a dedicated
	// NATS connection whose connectivity can be toggled. It is set by
	// SetNetworkControl and used by Disconnect/Reconnect.
	networkControl *NetworkControl

	// publishedSinceReconnect is incremented on every successful js.Publish
	// and atomically reset to 0 by Disconnect so the post-reconnect watchdog
	// can detect whether the publish loop resumed within T_pub=5s.
	publishedSinceReconnect atomic.Int64

	// INV1 counters — incremented by the reconnect watchdog
	resumeObserved atomic.Int64 // successful resume detected
	resumeMissing  atomic.Int64 // T_pub window elapsed without a publish
}

// ReportMessage is sent to coordinator to track message sending.
type ReportMessage struct {
	PartitionID       int
	ProducerID        string
	PartitionSequence int64
	ProducerSequence  int64
}

// NewProducer creates a new producer.
//
// Parameters:
//   - id: Producer ID (e.g., "producer-0")
//   - js: JetStream context
//   - partitionIDs: List of partition IDs this producer manages
//   - weights: Partition weights matching partitionIDs
//   - baseRate: Base message rate per partition (msg/sec)
//   - coordinatorReportCh: Channel to report sent messages to coordinator
//   - metricsCollector: Optional metrics collector (can be nil)
//
// Returns:
//   - *Producer: Initialized producer
func NewProducer(
	id string,
	js jetstream.JetStream,
	partitionIDs []int,
	weights []int64,
	baseRate float64,
	coordinatorReportCh chan<- ReportMessage,
	metricsCollector *metrics.Collector,
) *Producer {
	sequences := make(map[int]int64, len(partitionIDs))
	for _, partID := range partitionIDs {
		sequences[partID] = 0
	}

	return &Producer{
		id:                  id,
		js:                  js,
		partitionIDs:        partitionIDs,
		weights:             weights,
		baseRate:            baseRate,
		coordinatorReportCh: coordinatorReportCh,
		metricsCollector:    metricsCollector,
		partitionSequences:  sequences,
		producerSequence:    0,
	}
}

// SetNetworkControl attaches a NetworkControl to this producer.
// Must be called before the first Start() to ensure the dialer is wired.
func (p *Producer) SetNetworkControl(nc *NetworkControl) {
	p.networkControl = nc
}

// Disconnect simulates a network disconnect for the configured duration d.
// It resets publishedSinceReconnect so the watchdog can detect resume.
// After d it calls Reconnect automatically. Panics if no NetworkControl
// is wired (call SetNetworkControl first).
func (p *Producer) Disconnect(d time.Duration) {
	if p.networkControl == nil {
		log.Printf("[%s] Disconnect called but no NetworkControl wired; ignoring", p.id)
		return
	}
	log.Printf("[%s] Simulating network disconnect for %v", p.id, d)
	p.publishedSinceReconnect.Store(0)
	p.networkControl.Disconnect()

	time.AfterFunc(d, func() {
		p.Reconnect()
	})
}

// Reconnect restores network connectivity and starts the post-reconnect
// watchdog. T_pub=5s: if no publish succeeds within that window,
// resumeMissing is incremented; otherwise resumeObserved is incremented.
func (p *Producer) Reconnect() {
	if p.networkControl == nil {
		return
	}
	log.Printf("[%s] Reconnecting network", p.id)
	p.networkControl.Reconnect()

	// Watchdog: wait T_pub=5s then check whether the publish loop resumed.
	const tPub = 5 * time.Second
	time.AfterFunc(tPub, func() {
		if p.publishedSinceReconnect.Load() > 0 {
			p.resumeObserved.Add(1)
			log.Printf("[%s] INV1 producer_resume_observed: publish loop resumed within %v post-reconnect", p.id, tPub)
		} else {
			p.resumeMissing.Add(1)
			log.Printf("[%s] INV1 producer_resume_missing: publish loop did NOT resume within %v post-reconnect", p.id, tPub)
		}
	})
}

// ResumeObserved returns the INV1 successful-resume counter.
func (p *Producer) ResumeObserved() int64 { return p.resumeObserved.Load() }

// ResumeMissing returns the INV1 failed-resume counter.
func (p *Producer) ResumeMissing() int64 { return p.resumeMissing.Load() }

// Start begins producing messages.
// Safe to call multiple times; subsequent calls will be ignored if already started.
//
// Parameters:
//   - ctx: Context for cancellation
func (p *Producer) Start(ctx context.Context) {
	log.Printf("[%s] Starting producer for %d partitions", p.id, len(p.partitionIDs))

	// Prevent multiple starts
	if !p.started.CompareAndSwap(false, true) {
		log.Printf("[%s] Already started, ignoring duplicate Start() call", p.id)
		return
	}
	defer p.started.Store(false) // Allow restart after stop

	// Calculate interval to achieve baseRate per partition
	// Since we round-robin through all partitions, we need to tick faster
	totalRate := p.baseRate * float64(len(p.partitionIDs))
	if totalRate <= 0 {
		// Idle if configured rate is zero or negative
		for {
			select {
			case <-ctx.Done():
				log.Printf("[%s] Stopping producer", p.id)
				return
			case <-time.After(500 * time.Millisecond):
				// periodic wake to check cancellation
			}
		}
	}
	interval := time.Duration(float64(time.Second) / totalRate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	partitionIndex := 0

	for {
		select {
		case <-ctx.Done():
			log.Printf("[%s] Stopping producer", p.id)
			return
		case <-ticker.C:
			if len(p.partitionIDs) == 0 {
				continue
			}

			partitionID := p.partitionIDs[partitionIndex]
			if err := p.sendMessage(ctx, partitionID); err != nil {
				log.Printf("[%s] Error sending message to partition %d: %v", p.id, partitionID, err)
			}

			partitionIndex = (partitionIndex + 1) % len(p.partitionIDs)
		}
	}
}

func (p *Producer) sendMessage(ctx context.Context, partitionID int) error {
	p.mu.Lock()
	partSeq := p.partitionSequences[partitionID] + 1
	p.partitionSequences[partitionID] = partSeq
	prodSeq := p.producerSequence + 1
	p.producerSequence = prodSeq

	weight := p.weights[0]
	for i, pid := range p.partitionIDs {
		if pid == partitionID {
			weight = p.weights[i]
			break
		}
	}
	p.mu.Unlock()

	msg := &SimulationMessage{
		PartitionID:       partitionID,
		ProducerID:        p.id,
		PartitionSequence: partSeq,
		ProducerSequence:  prodSeq,
		Timestamp:         time.Now().UnixMicro(),
		Weight:            weight,
	}

	data, err := msg.Marshal()
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	subject := fmt.Sprintf("simulation.partition.%d", partitionID)
	_, err = p.js.Publish(ctx, subject, data)
	if err != nil {
		return fmt.Errorf("failed to publish message: %w", err)
	}

	// INV1: bump counter so post-reconnect watchdog can detect resume.
	p.publishedSinceReconnect.Add(1)

	// Record metrics
	if p.metricsCollector != nil {
		p.metricsCollector.RecordMessageSent(partitionID)
		// Debug: Log first few messages
		// if partSeq <= 3 {
		// 	log.Printf("[%s] Recorded sent metric: partition=%d seq=%d", p.id, partitionID, partSeq)
		// }
	}

	// Report to coordinator (do not drop). Block unless context cancelled.
	if p.coordinatorReportCh != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case p.coordinatorReportCh <- ReportMessage{
			PartitionID:       partitionID,
			ProducerID:        p.id,
			PartitionSequence: partSeq,
			ProducerSequence:  prodSeq,
		}:
			// delivered
		}
	}

	return nil
}
