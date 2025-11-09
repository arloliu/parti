package worker

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/source"
	"github.com/arloliu/parti/strategy"
	"github.com/arloliu/parti/subscription"
	"github.com/arloliu/parti/test/simulation/internal/coordinator"
	"github.com/arloliu/parti/test/simulation/internal/logging"
	"github.com/arloliu/parti/test/simulation/internal/metrics"
	"github.com/arloliu/parti/test/simulation/internal/producer"
	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Worker consumes messages from assigned partitions.
type Worker struct {
	id                  string
	nc                  *nats.Conn
	js                  jetstream.JetStream
	cfg                 *parti.Config
	manager             *parti.Manager
	helper              *subscription.WorkerConsumer
	processingDelayMin  time.Duration
	processingDelayMax  time.Duration
	coordinatorReportCh chan<- coordinator.ReceivedMessage
	metricsCollector    *metrics.Collector
	logger              types.Logger
	currentPartitions   int // Track current partition count for metrics
	ctx                 context.Context
	cancel              context.CancelFunc
	started             atomic.Bool // Prevents multiple Start() calls
}

// Config configures a worker.
type Config struct {
	ID                  string
	NC                  *nats.Conn
	JS                  jetstream.JetStream
	PartitionCount      int
	PartitionWeights    []int64
	AssignmentStrategy  string
	ProcessingDelayMin  time.Duration
	ProcessingDelayMax  time.Duration
	CoordinatorReportCh chan<- coordinator.ReceivedMessage
	MetricsCollector    *metrics.Collector
}

// NewWorker creates a new worker.
//
// Parameters:
//   - cfg: Worker configuration
//
// Returns:
//   - *Worker: Initialized worker
//   - error: Error if initialization fails
func NewWorker(cfg Config) (*Worker, error) {
	// Use nop logger to reduce verbosity (use NewStdLogger for debugging)
	logger := logging.NewNop()

	// Create partition source
	partitions := make([]types.Partition, cfg.PartitionCount)
	for i := 0; i < cfg.PartitionCount; i++ {
		weight := int64(1)
		if i < len(cfg.PartitionWeights) {
			weight = cfg.PartitionWeights[i]
		}
		partitions[i] = types.Partition{
			Keys:   []string{fmt.Sprintf("%d", i)},
			Weight: weight,
		}
	}
	partitionSource := source.NewStatic(partitions)

	// Create assignment strategy
	var assignmentStrategy types.AssignmentStrategy
	switch cfg.AssignmentStrategy {
	case "WeightedConsistentHash":
		assignmentStrategy = strategy.NewWeightedConsistentHash()
	case "RoundRobin":
		assignmentStrategy = strategy.NewRoundRobin()
	default:
		// ConsistentHash or unspecified
		assignmentStrategy = strategy.NewConsistentHash()
	}

	// Use default config and customize
	partiCfg := parti.DefaultConfig()
	partiCfg.WorkerIDPrefix = "simulation-worker"
	partiCfg.WorkerIDMax = 999 // Support up to 1000 workers

	// Reduce startup timeout for faster worker startup in simulation
	// Default is 30s which causes long delays when many workers start concurrently
	// 5s is enough for ID claiming in a simulation environment
	partiCfg.StartupTimeout = 5 * time.Second

	// Reduce cold start window for faster rebalancing in simulation
	// Default is 30s which is too long for fast testing
	partiCfg.ColdStartWindow = 10 * time.Second

	// Create worker struct first so we can reference it in hooks
	worker := &Worker{
		id:                  cfg.ID,
		nc:                  cfg.NC,
		js:                  cfg.JS,
		cfg:                 &partiCfg,
		processingDelayMin:  cfg.ProcessingDelayMin,
		processingDelayMax:  cfg.ProcessingDelayMax,
		coordinatorReportCh: cfg.CoordinatorReportCh,
		metricsCollector:    cfg.MetricsCollector,
		logger:              logger,
		currentPartitions:   0,
	}

	// Create durable helper early so manager can drive updates via option.
	helperConfig := subscription.WorkerConsumerConfig{
		ConsumerPrefix:  "simulation",
		SubjectTemplate: "simulation.partition.{{.PartitionID}}",
		StreamName:      "SIMULATION",
	}
	helper, err := subscription.NewWorkerConsumer(cfg.NC, helperConfig, subscription.MessageHandlerFunc(worker.processMessage))
	if err != nil {
		return nil, fmt.Errorf("failed to create durable helper: %w", err)
	}
	worker.helper = helper

	// Set up hooks to record metrics only (consumer updates handled by manager option)
	hooks := &types.Hooks{OnAssignmentChanged: worker.handleAssignmentChanged}
	// Create manager with hooks
	js, err := jetstream.New(cfg.NC)
	if err != nil {
		return nil, fmt.Errorf("failed to init JetStream: %w", err)
	}
	manager, err := parti.NewManager(&partiCfg, js, partitionSource, assignmentStrategy,
		parti.WithLogger(logger),
		parti.WithHooks(hooks),
		parti.WithWorkerConsumerUpdater(helper))
	if err != nil {
		return nil, fmt.Errorf("failed to create manager: %w", err)
	}

	// Set manager reference
	worker.manager = manager

	return worker, nil
}

// Start begins consuming messages.
// Safe to call multiple times; subsequent calls will be ignored if already started.
//
// Parameters:
//   - ctx: Context for cancellation
//
// Returns:
//   - error: Error if startup fails
func (w *Worker) Start(ctx context.Context) error {
	log.Printf("[%s] Starting worker", w.id)

	// Prevent multiple starts
	if !w.started.CompareAndSwap(false, true) {
		log.Printf("[%s] Already started, ignoring duplicate Start() call", w.id)
		return nil
	}

	// Use the provided context directly (don't create child context)
	// This ensures that when the parent context is cancelled (e.g., by chaos events),
	// the manager and all its goroutines will properly shut down
	w.ctx = ctx
	w.cancel = nil // No cancel function needed; caller controls lifecycle via context

	// Start manager
	if err := w.manager.Start(w.ctx); err != nil {
		w.started.Store(false) // Reset on error
		return fmt.Errorf("failed to start manager: %w", err)
	}

	// Start is non-blocking; the manager runs in background goroutines.
	// Lifecycle is controlled by the context passed from caller.
	return nil
}

// consumeLoop and diff-based updates removed; OnAssignmentChanged hook handles updates now.

// handleAssignmentChanged updates metrics on assignment changes.
func (w *Worker) handleAssignmentChanged(ctx context.Context, _ []types.Partition, newSet []types.Partition) error {
	start := time.Now()
	w.currentPartitions = len(newSet)
	if w.metricsCollector != nil {
		w.metricsCollector.RecordPartitionsPerWorker(w.currentPartitions)
		w.metricsCollector.RecordRebalanceDuration(time.Since(start))
	}

	return nil
}

func (w *Worker) processMessage(_ context.Context, msg jetstream.Msg) error {
	// Record start time for metrics
	startTime := time.Now()

	// Parse message
	simMsg, err := producer.Unmarshal(msg.Data())
	if err != nil {
		if w.metricsCollector != nil {
			w.metricsCollector.RecordProcessingError()
		}

		return fmt.Errorf("failed to unmarshal message: %w", err)
	} // Simulate processing delay
	delay := w.processingDelayMin
	if w.processingDelayMax > w.processingDelayMin {
		delayRange := w.processingDelayMax - w.processingDelayMin
		delay = w.processingDelayMin + time.Duration(rand.Int63n(int64(delayRange))) //nolint:gosec // Weak RNG acceptable for simulation
	}
	time.Sleep(delay)

	// Report to coordinator
	select {
	case w.coordinatorReportCh <- coordinator.ReceivedMessage{
		PartitionID:       simMsg.PartitionID,
		PartitionSequence: simMsg.PartitionSequence,
	}:
	default:
		// Non-blocking send
	}

	// Record processing duration metric
	if w.metricsCollector != nil {
		processingDuration := time.Since(startTime)
		w.metricsCollector.RecordMessageProcessingDuration(processingDuration)
		// Debug: Log first few processed messages
		// if simMsg.PartitionSequence <= 2 {
		// 	log.Printf("[%s] Recorded processing metric: partition=%d duration=%v",
		// 		w.id, simMsg.PartitionID, processingDuration)
		// }
	}

	// ACK message
	return msg.Ack()
}

// Stop gracefully stops the worker.
func (w *Worker) Stop() {
	// Note: We don't cancel context here because the worker uses the caller's context.
	// The caller is responsible for cancelling the context.

	// Give manager time to shutdown gracefully
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if w.manager != nil {
		_ = w.manager.Stop(ctx)
	}

	w.started.Store(false)
}
