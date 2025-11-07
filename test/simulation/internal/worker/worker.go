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
	helper              *subscription.DurableHelper
	processingDelayMin  time.Duration
	processingDelayMax  time.Duration
	coordinatorReportCh chan<- coordinator.ReceivedMessage
	metricsCollector    *metrics.Collector
	logger              types.Logger
	prevAssigned        []types.Partition
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
		prevAssigned:        []types.Partition{},
		currentPartitions:   0,
	}

	// Set up hooks to record metrics
	hooks := &types.Hooks{
		OnAssignmentChanged: func(ctx context.Context, added, removed []types.Partition) error {
			start := time.Now()

			// Update running partition count
			worker.currentPartitions = worker.currentPartitions + len(added) - len(removed)

			// Record metrics
			if worker.metricsCollector != nil {
				// Record partition distribution (histogram observes the distribution)
				worker.metricsCollector.RecordPartitionsPerWorker(worker.currentPartitions)

				// Record rebalancing duration
				duration := time.Since(start)
				worker.metricsCollector.RecordRebalanceDuration(duration)
			}

			return nil
		},
	} // Create manager with hooks
	manager, err := parti.NewManager(&partiCfg, cfg.NC, partitionSource, assignmentStrategy,
		parti.WithLogger(logger),
		parti.WithHooks(hooks))
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

	w.ctx, w.cancel = context.WithCancel(ctx)

	// Start manager
	if err := w.manager.Start(w.ctx); err != nil {
		w.started.Store(false) // Reset on error
		return fmt.Errorf("failed to start manager: %w", err)
	}

	// Create durable helper config
	helperConfig := subscription.DurableConfig{
		ConsumerPrefix:  "simulation",
		SubjectTemplate: "simulation.partition.{{.PartitionID}}",
		StreamName:      "SIMULATION",
	}

	// Create durable helper
	helper, err := subscription.NewDurableHelper(w.nc, helperConfig)
	if err != nil {
		w.started.Store(false) // Reset on error
		return fmt.Errorf("failed to create durable helper: %w", err)
	}
	w.helper = helper

	// Run consume loop (blocks until context cancelled)
	w.consumeLoop()

	return nil
}

func (w *Worker) consumeLoop() {
	defer w.started.Store(false) // Allow restart after stop

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			log.Printf("[%s] Stopping worker", w.id)
			return
		case <-ticker.C:
			// Get current assignment
			assignment := w.manager.CurrentAssignment()
			current := assignment.Partitions

			// Calculate diff
			added, removed := w.diffPartitions(w.prevAssigned, current)

			if len(added) > 0 || len(removed) > 0 {
				handler := subscription.MessageHandlerFunc(w.processMessage)
				if err := w.helper.UpdateSubscriptions(w.ctx, added, removed, handler); err != nil {
					log.Printf("[%s] Error updating subscriptions: %v", w.id, err)
				} else {
					w.prevAssigned = current
				}
			}
		}
	}
}

func (w *Worker) diffPartitions(old, newPartitions []types.Partition) (added, removed []types.Partition) {
	oldMap := make(map[string]types.Partition)
	for _, p := range old {
		key := fmt.Sprintf("%v", p.Keys)
		oldMap[key] = p
	}

	newMap := make(map[string]types.Partition)
	for _, p := range newPartitions {
		key := fmt.Sprintf("%v", p.Keys)
		newMap[key] = p
		if _, exists := oldMap[key]; !exists {
			added = append(added, p)
		}
	}

	for _, p := range old {
		key := fmt.Sprintf("%v", p.Keys)
		if _, exists := newMap[key]; !exists {
			removed = append(removed, p)
		}
	}

	return added, removed
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
	if w.cancel != nil {
		w.cancel()
	}

	// Give manager time to shutdown gracefully
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if w.manager != nil {
		_ = w.manager.Stop(ctx)
	}
}
