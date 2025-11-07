package producer

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti/test/simulation/internal/metrics"
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

	// Record metrics
	if p.metricsCollector != nil {
		p.metricsCollector.RecordMessageSent(partitionID)
		// Debug: Log first few messages
		// if partSeq <= 3 {
		// 	log.Printf("[%s] Recorded sent metric: partition=%d seq=%d", p.id, partitionID, partSeq)
		// }
	}

	// Report to coordinator
	select {
	case p.coordinatorReportCh <- ReportMessage{
		PartitionID:       partitionID,
		ProducerID:        p.id,
		PartitionSequence: partSeq,
		ProducerSequence:  prodSeq,
	}:
	default:
		// Non-blocking send
	}

	return nil
}
