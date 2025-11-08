package coordinator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/arloliu/parti/test/simulation/internal/metrics"
	"github.com/arloliu/parti/test/simulation/internal/producer"
)

// Coordinator manages simulation tracking and validation.
type Coordinator struct {
	tracker          *MessageTracker
	metricsCollector *metrics.Collector
	sentCh           chan producer.ReportMessage
	receivedCh       chan ReceivedMessage
	stopCh           chan struct{}
}

// ReceivedMessage reports a received message from workers.
type ReceivedMessage struct {
	PartitionID       int
	PartitionSequence int64
}

// NewCoordinator creates a new coordinator.
//
// Parameters:
//   - metricsCollector: Optional metrics collector (can be nil)
//
// Returns:
//   - *Coordinator: Initialized coordinator
func NewCoordinator(metricsCollector *metrics.Collector) *Coordinator {
	return &Coordinator{
		tracker:          NewMessageTracker(),
		metricsCollector: metricsCollector,
		sentCh:           make(chan producer.ReportMessage, 10000),
		receivedCh:       make(chan ReceivedMessage, 10000),
		stopCh:           make(chan struct{}),
	}
}

// GetSentChannel returns the channel for reporting sent messages.
//
// Returns:
//   - chan<- producer.ReportMessage: Channel for producer reports
func (c *Coordinator) GetSentChannel() chan<- producer.ReportMessage {
	return c.sentCh
}

// GetReceivedChannel returns the channel for reporting received messages.
//
// Returns:
//   - chan<- ReceivedMessage: Channel for worker reports
func (c *Coordinator) GetReceivedChannel() chan<- ReceivedMessage {
	return c.receivedCh
}

// Start begins coordinator tracking.
//
// Parameters:
//   - ctx: Context for cancellation
func (c *Coordinator) Start(ctx context.Context) {
	log.Println("[Coordinator] Starting")

	for {
		select {
		case <-ctx.Done():
			log.Println("[Coordinator] Stopping")
			close(c.stopCh)
			return

		case msg := <-c.sentCh:
			c.tracker.RecordSent(msg.PartitionID, msg.ProducerID, msg.PartitionSequence, msg.ProducerSequence)

		case msg := <-c.receivedCh:
			if err := c.tracker.RecordReceived(msg.PartitionID, msg.PartitionSequence); err != nil {
				log.Printf("[Coordinator] ERROR: %v", err)

				// Record gap/duplicate metrics using proper error type checking
				if c.metricsCollector != nil {
					if errors.Is(err, ErrMessageGap) {
						c.metricsCollector.RecordGap()
					}

					if errors.Is(err, ErrMessageDuplicate) {
						c.metricsCollector.RecordDuplicate()
					}
				}
			}

			// Record metrics
			if c.metricsCollector != nil {
				c.metricsCollector.RecordMessageReceived(msg.PartitionID)
				// Debug: Log first few received messages
				// stats := c.tracker.GetStats()
				// if stats.TotalReceived <= 5 {
				// 	log.Printf("[Coordinator] Recorded received metric: partition=%d seq=%d (total: %d)",
				// 		msg.PartitionID, msg.PartitionSequence, stats.TotalReceived)
				// }
			}
		}
	}
}

// GetStats returns current statistics.
//
// Returns:
//   - TrackerStats: Current statistics
func (c *Coordinator) GetStats() TrackerStats {
	return c.tracker.GetStats()
}

// GetTracker returns the message tracker for checkpointing.
//
// Returns:
//   - *MessageTracker: The message tracker
func (c *Coordinator) GetTracker() *MessageTracker {
	return c.tracker
}

// PrintReport prints a summary report.
func (c *Coordinator) PrintReport() {
	stats := c.GetStats()

	// Get system metrics if collector is available
	var goroutines int
	var memoryMiB float64
	var activeWorkers int
	if c.metricsCollector != nil {
		goroutines, memoryMiB = c.metricsCollector.GetSystemMetrics()
		activeWorkers = c.metricsCollector.GetActiveWorkers()
	}

	fmt.Printf("\n=== Simulation Report [%s] ===\n", time.Now().Format(time.RFC3339))
	fmt.Printf("Total Partitions:     %d\n", stats.TotalPartitions)
	fmt.Printf("Total Sent:           %d\n", stats.TotalSent)
	fmt.Printf("Total Received:       %d\n", stats.TotalReceived)
	fmt.Printf("In-Flight:            %d\n", stats.InFlight)
	fmt.Printf("Gaps Detected:        %d\n", stats.GapCount)
	fmt.Printf("Duplicates:           %d\n", stats.DuplicateCount)

	if c.metricsCollector != nil {
		fmt.Printf("Active Workers:       %d\n", activeWorkers)
		fmt.Printf("Active Goroutines:    %d\n", goroutines)
		fmt.Printf("Memory Usage:         %.2f MiB\n", memoryMiB)

		// Get aggregated metrics
		avgPartitionsPerWorker, avgRebalanceDuration, avgProcessingLatency := c.metricsCollector.GetAggregatedMetrics()
		fmt.Printf("Avg Partitions/Worker: %.1f\n", avgPartitionsPerWorker)
		fmt.Printf("Avg Rebalance Duration: %.2fs\n", avgRebalanceDuration)
		fmt.Printf("Avg Processing Latency: %.2fms\n", avgProcessingLatency*1000)
	}

	if stats.GapCount == 0 && stats.DuplicateCount == 0 && stats.InFlight == 0 {
		fmt.Println("SUCCESS: No message loss or duplication detected")
	} else if stats.GapCount > 0 || stats.DuplicateCount > 0 {
		fmt.Println("FAILURE: Message loss or duplication detected")
	} else {
		fmt.Printf("PENDING: %d messages in-flight (not yet received)\n", stats.InFlight)
	}
}
