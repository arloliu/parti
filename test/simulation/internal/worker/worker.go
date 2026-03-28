package worker

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/internal/durable"
	"github.com/arloliu/parti/v2/test/simulation/internal/coordinator"
	"github.com/arloliu/parti/v2/test/simulation/internal/logging"
	"github.com/arloliu/parti/v2/test/simulation/internal/metrics"
	"github.com/arloliu/parti/v2/test/simulation/internal/producer"
	"github.com/arloliu/parti/v2/types"
)

// minDuration returns the smaller of a and b.
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

type Worker struct {
	id      string
	nc      *nats.Conn
	js      jetstream.JetStream
	cfg     *parti.Config
	manager *parti.Manager
	// updater is the manager-driven consumer updater)
	updater             parti.WorkerConsumerUpdater
	processingDelayMin  time.Duration
	processingDelayMax  time.Duration
	coordinatorReportCh chan<- coordinator.ReceivedMessage
	assignmentReportCh  chan<- coordinator.AssignmentReport
	metricsCollector    *metrics.Collector
	logger              types.Logger
	currentPartitions   int // Track current partition count for metrics
	ctx                 context.Context
	cancel              context.CancelFunc
	started             atomic.Bool // Prevents multiple Start() calls
	// throughput controls
	handlerConcurrency int
	consumerBatchSize  int
	// keyed-serialization dispatcher (per-partition ordering with concurrency)
	shardCount int
	shardChans []chan jetstream.Msg

	// ordering enforcement per partition
	orderMu      sync.Mutex
	expectedNext map[int]int
	orderBuf     map[int]map[int]struct{}

	// pause control
	pauseMu     sync.RWMutex
	pausedUntil time.Time

	// network control
	netCtrl *NetworkControl

	// slow consumer simulation (processing multiplier)
	processingMultiplier atomic.Int32
}

// SetNetworkControl sets the network control for this worker.
func (w *Worker) SetNetworkControl(ctrl *NetworkControl) {
	w.netCtrl = ctrl
}

// Disconnect simulates a network disconnect.
func (w *Worker) Disconnect() {
	if w.netCtrl != nil {
		w.netCtrl.Disconnect()
	}
}

// Reconnect restores network connectivity.
func (w *Worker) Reconnect() {
	if w.netCtrl != nil {
		w.netCtrl.Reconnect()
	}
}

// SetProcessingMultiplier sets the processing delay multiplier for slow consumer simulation.
// A value of 1 means normal processing, 10 means 10x slower, etc.
func (w *Worker) SetProcessingMultiplier(m int) {
	w.processingMultiplier.Store(int32(m)) //nolint:gosec // m is always small (10-50)
	log.Printf("[%s] Processing multiplier set to %dx", w.id, m)
}

// GetProcessingMultiplier returns the current processing delay multiplier.
func (w *Worker) GetProcessingMultiplier() int {
	m := w.processingMultiplier.Load()
	if m <= 0 {
		return 1 // default
	}
	return int(m)
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
	AssignmentReportCh  chan<- coordinator.AssignmentReport
	MetricsCollector    *metrics.Collector
	// Throughput knobs
	ConsumerBatchSize  int
	HandlerConcurrency int
	// MaxSubjects optionally overrides subject guardrail. If 0, defaults to partition count.
	MaxSubjects int
	// Exclusive consumption (Processing Gate)
	EnforceExclusiveConsumption bool
	GateAllowedStates           []string
	GateWarmupDuration          time.Duration
	GateWarmupAllowedStates     []string
	GateNakDelay                time.Duration
	GateNakJitter               float64
	// Gate debug logging
	GateDebug bool
}

// NewWorker creates a new worker.
//
// Parameters:
//   - cfg: Worker configuration
//
// Returns:
//   - *Worker: Initialized worker
//   - error: Error if initialization fails
func NewWorker(cfg Config) (*Worker, error) { //nolint:cyclop
	// Use nop logger by default; switch to std logger when gate debug is enabled
	logger := logging.NewNop()
	if cfg.GateDebug || os.Getenv("PARTI_SIM_LOG") == "1" {
		logger = logging.NewStdLogger(cfg.ID)
	}

	log.Printf("[%s] NewWorker: partitions=%d strategy=%s enforceExclusive=%v handlerConcurrency=%d batchSize=%d",
		cfg.ID, cfg.PartitionCount, cfg.AssignmentStrategy, cfg.EnforceExclusiveConsumption, cfg.HandlerConcurrency, cfg.ConsumerBatchSize)

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
	// Enable two-phase handoff only when exclusive consumption is requested.
	// This publishes ownership claims to KV so the Processing Gate can enforce exclusivity.
	partiCfg.EnableTwoPhaseHandoff = cfg.EnforceExclusiveConsumption
	// Shorten handoff TTL for simulation so stale claims expire quickly if any linger
	// and to speed up steady-state convergence under churn.
	// NOTE: Claims are the source of truth for exclusivity. Using a non-zero KV TTL
	// will purge stable claims and cause the Processing Gate to see unknown ownership,
	// leading to prolonged NAK loops and artificial gaps under sustained load.
	// For simulation correctness and long runs, disable KV TTL (persist claims until superseded).
	if cfg.EnforceExclusiveConsumption {
		partiCfg.KVBuckets.HandoffTTL = 0
	}

	// Use a generous startup timeout to avoid cancellations during
	// concurrent KV bucket creation, hygiene passes, and ID claiming across many workers.
	// Increased for stress scenarios.
	partiCfg.StartupTimeout = 120 * time.Second

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
		assignmentReportCh:  cfg.AssignmentReportCh,
		metricsCollector:    cfg.MetricsCollector,
		logger:              logger,
		currentPartitions:   0,
		handlerConcurrency:  cfg.HandlerConcurrency,
		consumerBatchSize:   cfg.ConsumerBatchSize,
		expectedNext:        make(map[int]int),
		orderBuf:            make(map[int]map[int]struct{}),
	}

	perSubMaxAckPending := worker.consumerBatchSize * 2
	if perSubMaxAckPending < 8 { // preserve a small minimum
		perSubMaxAckPending = 8
	}
	// Create durable helper early so manager can drive updates via option.
	helperConfig := durable.WorkerConsumerConfig{
		ConsumerPrefix:  "simulation",
		SubjectTemplate: "simulation.partition.{{.PartitionID}}",
		StreamName:      "SIMULATION",
		Logger:          logger,
		// Enable manual ack so the handler controls when to ack (after reporting to coordinator).
		// This avoids double-acking and ensures our reporting order is consistent.
		ManualAck: true,
		BatchSize: worker.consumerBatchSize,
		// Short fetch timeout so goroutines blocked in Next() exit rapidly after cancellation,
		// reducing apparent goroutine leak in simulation leak tests.
		FetchTimeout: 1 * time.Second,
		// Drain per-subject on removal to reduce NAK churn during chaos scale-down.
		DrainOnRemove: true,
		// Under exclusivity gating and chaos, messages can be NAKed multiple times.
		// Increase MaxDeliver to avoid premature drop and false gap escalations.
		MaxDeliver: 50,
		// AckWait must be shorter than Coordinator.GapAging (default 45s) to avoid false positives
		// during worker crashes where messages are held by the dead consumer.
		AckWait: 30 * time.Second,
		// Caps to bound per-subject concurrency and reduce disorder.
		MaxWaiting:    2,
		MaxAckPending: perSubMaxAckPending,
		// Suppress pulls when not owner or not in commit/stable to reduce NAK storms.
		PullGatingEnabled: true,
	}

	// Enable Processing Gate for exclusive consumption when configured
	if cfg.EnforceExclusiveConsumption {
		pg := &durable.ProcessingGateConfig{
			Enabled:   true,
			NakDelay:  cfg.GateNakDelay,
			NakJitter: cfg.GateNakJitter,
			Debug:     cfg.GateDebug,
		}
		// Wire gate metrics when collector is available
		if cfg.MetricsCollector != nil {
			pg.Metrics = cfg.MetricsCollector.GateMetricsAdapter()
		}
		// Warm-up configuration (optional)
		if cfg.GateWarmupDuration > 0 {
			pg.WarmupDuration = cfg.GateWarmupDuration
			if len(cfg.GateWarmupAllowedStates) > 0 {
				wstates := make([]types.HandoffState, 0, len(cfg.GateWarmupAllowedStates))
				for _, s := range cfg.GateWarmupAllowedStates {
					switch strings.ToLower(s) {
					case "stable":
						wstates = append(wstates, types.HandoffStateStable)
					case "prepare":
						wstates = append(wstates, types.HandoffStatePrepare)
					case "commit":
						wstates = append(wstates, types.HandoffStateCommit)
					}
				}
				pg.WarmupAllowedStates = wstates
			}
		}
		// Map allowed state strings to types.HandoffState
		if len(cfg.GateAllowedStates) > 0 {
			states := make([]types.HandoffState, 0, len(cfg.GateAllowedStates))
			for _, s := range cfg.GateAllowedStates {
				switch strings.ToLower(s) {
				case "stable":
					states = append(states, types.HandoffStateStable)
				case "prepare":
					states = append(states, types.HandoffStatePrepare)
				case "commit":
					states = append(states, types.HandoffStateCommit)
				}
			}
			pg.AllowedStates = states
		}
		helperConfig.ProcessingGate = pg
		// Use defaults for HandoffBucketName/Prefix; WorkerConsumer will auto-init resolver
	}
	js, err := jetstream.New(cfg.NC)
	if err != nil {
		return nil, fmt.Errorf("failed to init JetStream: %w", err)
	}

	updater, err := durable.NewWorkerConsumer(js, helperConfig, worker.processMessage)
	if err != nil {
		return nil, fmt.Errorf("failed to create worker consumer updater: %w", err)
	}
	worker.updater = updater

	if cfg.MetricsCollector != nil {
		updater.SetResolverMetrics(cfg.MetricsCollector.ResolverMetricsAdapter())
	}

	// Set up hooks to record metrics only (consumer updates handled by manager option)
	hooks := &types.Hooks{OnAssignmentChanged: worker.handleAssignmentChanged}
	// Create manager with hooks
	manager, err := parti.NewManager(&partiCfg, js, partitionSource, assignmentStrategy,
		parti.WithLogger(logger),
		parti.WithHooks(hooks),
		parti.WithWorkerConsumerUpdater(worker.updater))
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

	// Add a small randomized jitter to avoid thundering herd on KV operations
	if d := time.Duration(rand.Intn(750)) * time.Millisecond; d > 0 { //nolint:gosec // Weak RNG acceptable for simulation jitter
		select {
		case <-ctx.Done():
			return fmt.Errorf("startup cancelled before jitter elapsed: %w", ctx.Err())
		case <-time.After(d):
		}
	}

	// If concurrency > 1, start keyed-serialization shards before manager so handler has live queues
	if w.handlerConcurrency > 1 {
		w.shardCount = w.handlerConcurrency
		if w.shardCount < 1 {
			w.shardCount = 1
		}
		w.shardChans = make([]chan jetstream.Msg, w.shardCount)
		for i := 0; i < w.shardCount; i++ {
			buf := 2 * w.consumerBatchSize
			if buf <= 0 {
				buf = 8
			}
			w.shardChans[i] = make(chan jetstream.Msg, buf)
			go w.runShard(i)
		}
	}

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
func (w *Worker) handleAssignmentChanged(ctx context.Context, oldSet, newSet []types.Partition) error {
	start := time.Now()
	w.currentPartitions = len(newSet)
	if w.metricsCollector != nil {
		w.metricsCollector.RecordPartitionsPerWorker(w.currentPartitions)
		w.metricsCollector.RecordRebalanceDuration(time.Since(start))
	}

	// Debug: log change details
	log.Printf("[%s] Assignment change: old=%d new=%d", w.id, len(oldSet), len(newSet))

	// Report new assignment snapshot to coordinator for completeness/locality metrics.
	if w.assignmentReportCh != nil {
		parts := make([]int, 0, len(newSet))
		for _, p := range newSet {
			if len(p.Keys) == 0 {
				continue
			}
			var id int
			_, err := fmt.Sscanf(p.Keys[0], "%d", &id)
			if err == nil {
				parts = append(parts, id)
			}
		}
		select {
		case <-ctx.Done():
		case w.assignmentReportCh <- coordinator.AssignmentReport{WorkerID: w.id, Partitions: parts}:
		}
	}

	return nil
}

func (w *Worker) processMessage(_ context.Context, msg jetstream.Msg) error {
	// If concurrency > 1, enqueue by shard and return (per-shard workers preserve partition order)
	if w.handlerConcurrency > 1 && len(w.shardChans) > 0 {
		idx := w.selectShard(msg)
		ch := w.shardChans[idx]
		select {
		case <-w.ctx.Done():
			// Context cancelled while trying to enqueue; NAK so message can be redelivered.
			meta, _ := msg.Metadata()
			if err := msg.Nak(); err != nil {
				log.Printf("[%s] NAK enqueue cancelled: subject=%s stream_seq=%d err=%v", w.id, msg.Subject(), meta.Sequence.Stream, err)
			} else {
				log.Printf("[%s] NAK enqueue cancelled: subject=%s stream_seq=%d", w.id, msg.Subject(), meta.Sequence.Stream)
			}
			return nil
		case ch <- msg:
			return nil
		}
	}

	return w.processAndAck(msg)
}

func (w *Worker) runShard(i int) {
	ch := w.shardChans[i]
	for {
		select {
		case <-w.ctx.Done():
			return
		case m := <-ch:
			_ = w.processAndAck(m)
		}
	}
}

// selectShard picks a shard index based on partition ID to preserve per-partition order.
func (w *Worker) selectShard(msg jetstream.Msg) int {
	if w.shardCount <= 0 {
		return 0
	}
	if pid, ok := partitionIDFromSubject(msg.Subject()); ok {
		return pid % w.shardCount
	}
	if simMsg, err := producer.Unmarshal(msg.Data()); err == nil {
		return simMsg.PartitionID % w.shardCount
	}
	// Fallback: route to shard 0 to avoid unsafe integer conversions in hashing path
	return 0
}

func partitionIDFromSubject(subject string) (int, bool) {
	// Expected: "simulation.partition.<id>"
	if subject == "" {
		return 0, false
	}
	parts := strings.Split(subject, ".")
	if len(parts) == 0 {
		return 0, false
	}
	last := parts[len(parts)-1]
	id, err := strconv.Atoi(last)
	if err != nil {
		return 0, false
	}

	return id, true
}

func (w *Worker) processAndAck(msg jetstream.Msg) error { //nolint:cyclop
	// Record start time for metrics
	startTime := time.Now()

	// Parse message
	simMsg, err := producer.Unmarshal(msg.Data())
	if err != nil {
		if w.metricsCollector != nil {
			w.metricsCollector.RecordProcessingError()
		}

		log.Printf("[%s] NAK bad payload: subject=%s err=%v", w.id, msg.Subject(), err)
		if nakErr := msg.Nak(); nakErr != nil {
			log.Printf("[%s] NAK bad payload failed: subject=%s err=%v", w.id, msg.Subject(), nakErr)
		}

		return fmt.Errorf("failed to unmarshal message: %w", err)
	}

	// If paused, block until pause interval elapses to create inactivity/backlog.
	w.pauseMu.RLock()
	pu := w.pausedUntil
	w.pauseMu.RUnlock()
	if !pu.IsZero() && time.Now().Before(pu) {
		// Sleep until unpaused or context cancelled
		for {
			remaining := time.Until(pu)
			if remaining <= 0 {
				break
			}
			select {
			case <-w.ctx.Done():
				return nil
			case <-time.After(minDuration(remaining, 500*time.Millisecond)):
			}
		}
	}

	// Enforce per-partition ordering before reporting. We ACK out-of-order
	// messages and buffer their sequences to emit to the coordinator later
	// in order, avoiding redelivery storms and global disorder.
	pid := simMsg.PartitionID
	seq := int(simMsg.PartitionSequence)
	w.orderMu.Lock()
	next := w.expectedNext[pid]
	if next == 0 {
		// Baseline to first observed sequence for this partition on this worker.
		// We'll report from this baseline and keep local order via buffering.
		next = seq
		w.expectedNext[pid] = next
	}
	if seq > next {
		// Buffer and ACK; do not report yet
		if w.orderBuf[pid] == nil {
			w.orderBuf[pid] = make(map[int]struct{})
		}
		w.orderBuf[pid][seq] = struct{}{}
		w.orderMu.Unlock()
		_ = msg.Ack()
		return nil
	}
	if seq < next {
		// Duplicate/old message: ack and drop without reporting
		w.orderMu.Unlock()
		_ = msg.Ack()
		return nil
	}
	// seq == next: we'll report this and then flush any buffered consecutive seqs
	w.orderMu.Unlock()

	// Simulate processing delay (with optional slow consumer multiplier)
	delay := w.processingDelayMin
	if w.processingDelayMax > w.processingDelayMin {
		delayRange := w.processingDelayMax - w.processingDelayMin
		delay = w.processingDelayMin + time.Duration(rand.Int63n(int64(delayRange))) //nolint:gosec // Weak RNG acceptable for simulation
	}
	// Apply slow consumer multiplier if set
	multiplier := w.GetProcessingMultiplier()
	if multiplier > 1 {
		delay *= time.Duration(multiplier)
	}
	time.Sleep(delay)

	// Report to coordinator (do not drop). Block unless context is cancelled.
	if w.coordinatorReportCh != nil {
		select {
		case <-w.ctx.Done():
			// Context cancelled; stop processing gracefully and NAK for redelivery.
			simMsg, _ := producer.Unmarshal(msg.Data())
			meta, _ := msg.Metadata()
			log.Printf("[%s] NAK on context cancel: partition=%d seq=%d subject=%s stream_seq=%d", w.id, simMsg.PartitionID, simMsg.PartitionSequence, msg.Subject(), meta.Sequence.Stream)
			if err := msg.Nak(); err != nil {
				log.Printf("[%s] NAK on context cancel failed: subject=%s stream_seq=%d err=%v", w.id, msg.Subject(), meta.Sequence.Stream, err)
			}
			return nil
		case w.coordinatorReportCh <- coordinator.ReceivedMessage{
			PartitionID:       simMsg.PartitionID,
			PartitionSequence: simMsg.PartitionSequence,
			WorkerID:          w.id,
		}:
			// delivered
		}
	}

	// Record processing duration metric
	if w.metricsCollector != nil {
		processingDuration := time.Since(startTime)
		w.metricsCollector.RecordMessageProcessingDuration(processingDuration)
		// End-to-end publish -> consume latency
		pubTime := time.UnixMicro(simMsg.Timestamp)
		endToEnd := time.Since(pubTime)
		w.metricsCollector.ObservePublishToConsumeLatency(endToEnd)
		w.metricsCollector.ObservePublishToConsumeLatencyPartition(simMsg.PartitionID, endToEnd)
		// Debug: Log first few processed messages
		// if simMsg.PartitionSequence <= 2 {
		// 	log.Printf("[%s] Recorded processing metric: partition=%d duration=%v",
		// 		w.id, simMsg.PartitionID, processingDuration)
		// }
	}

	// ACK message after reporting
	meta, _ := msg.Metadata()
	if err := msg.Ack(); err != nil {
		log.Printf("[%s] ACK failed: partition=%d seq=%d subject=%s stream_seq=%d err=%v", w.id, simMsg.PartitionID, simMsg.PartitionSequence, msg.Subject(), meta.Sequence.Stream, err)
		return err
	}

	// Advance expected sequence after successful ack/report and flush buffered
	w.orderMu.Lock()
	next++
	w.expectedNext[pid] = next
	// Flush buffered consecutive sequences in order, reporting to coordinator
	for {
		if _, ok := w.orderBuf[pid][next]; !ok {
			break
		}
		// Emit buffered report
		if w.coordinatorReportCh != nil {
			select {
			case <-w.ctx.Done():
				// Context cancelled; stop flushing
				w.orderMu.Unlock()
				return nil
			case w.coordinatorReportCh <- coordinator.ReceivedMessage{
				PartitionID:       pid,
				PartitionSequence: int64(next),
				WorkerID:          w.id,
			}:
			}
		}
		delete(w.orderBuf[pid], next)
		next++
		w.expectedNext[pid] = next
	}
	w.orderMu.Unlock()

	return nil
}

// Pause pauses message processing for duration d without releasing assignments.
// Multiple pauses extend the existing pause if the new end is later.
func (w *Worker) Pause(d time.Duration) {
	if d <= 0 {
		return
	}
	until := time.Now().Add(d)
	w.pauseMu.Lock()
	if until.After(w.pausedUntil) {
		w.pausedUntil = until
	}
	w.pauseMu.Unlock()
}

// PausedUntil returns the time until which the worker is paused (zero if not paused).
func (w *Worker) PausedUntil() time.Time {
	w.pauseMu.RLock()
	defer w.pauseMu.RUnlock()
	return w.pausedUntil
}

// IsLeader returns true if this worker is the current leader.
func (w *Worker) IsLeader() bool {
	if w.manager == nil {
		return false
	}
	return w.manager.IsLeader()
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

	// Proactively close the updater if it exposes a Close(ctx) API to prevent
	// subject loop goroutine leaks (WorkerConsumer spawns one per partition).
	if c, ok := w.updater.(interface{ Close(context.Context) error }); ok {
		_ = c.Close(context.Background())
	}

	w.started.Store(false)
}
