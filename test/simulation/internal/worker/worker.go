package worker

import (
	"context"
	"errors"
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
	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/internal/stableid"
	"github.com/arloliu/parti/v2/kvutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
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
	// updater is the manager-driven consumer updater. Holds the same instance
	// as `dynamic` but typed as the parti interface for WithWorkerConsumerUpdater.
	updater parti.WorkerConsumerUpdater
	// dynamic is the typed concrete consumer used for Stop on shutdown.
	dynamic             *consumer.Dynamic
	processingDelayMin  time.Duration
	processingDelayMax  time.Duration
	coordinatorReportCh chan<- coordinator.ReceivedMessage
	assignmentReportCh  chan<- coordinator.AssignmentReport
	startLatencyCh      chan<- coordinator.StartLatencyReport
	metricsCollector    *metrics.Collector
	logger              types.Logger
	currentPartitions   int // Track current partition count for metrics
	ctx                 context.Context
	cancel              context.CancelFunc
	started             atomic.Bool // Prevents multiple Start() calls

	// first-assignment latency tracking: captures the delay between Start()
	// being called and the first OnAssignmentChanged with a non-empty set.
	// Surfaces regressions of leader-takeover hang fixes (commits 5345527 and
	// 7781b9b) where a replacement worker would block in waitForAssignment
	// past its caller's deadline.
	startCalledAt     time.Time
	firstAssignmentOK atomic.Bool
	// isInitialCohort stamps the start-latency report so the coordinator's
	// classifier can bucket the sample without depending on receive-time
	// ordering of channels. True for workers constructed during simulation
	// bootstrap; false for restart / scale_up replacements.
	isInitialCohort bool
	// throughput controls
	handlerConcurrency int
	consumerBatchSize  int
	// keyed-serialization dispatcher (per-partition ordering with concurrency)
	shardCount int
	shardChans []chan jetstream.Msg

	// pause control
	pauseMu     sync.RWMutex
	pausedUntil time.Time

	// network control
	netCtrl *NetworkControl

	// slow consumer simulation (processing multiplier)
	processingMultiplier atomic.Int32

	// degradedReason caches the most recent OnDegraded reason string.
	// Written by the OnDegraded hook; read by DegradedReason().
	degradedReason   atomic.Pointer[string]
	degradedReportCh chan<- coordinator.WorkerDegradedReport

	// claimLostObserved is set true once the OnError hook receives a
	// stableid.ErrClaimLost surfaced from the production claim-loss path
	// (manager_election.go:117). The sim claim-loss oracle reads this via
	// ClaimLostObserved() so it can distinguish a real claim-loss
	// shutdown from a graceful Stop — both terminate in StateShutdown.
	claimLostObserved atomic.Bool

	// revocationReportCh receives a RevocationReport when the manager
	// drives a zero-partition UpdateWorkerConsumer call. Wired via
	// revocationObservingUpdater so the Phase 4 ordering oracle can see
	// the revoke event (OnAssignmentChanged does NOT fire on the
	// claim-loss revoke path).
	revocationReportCh     chan<- coordinator.RevocationReport
	revocationReportDropFn func()
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
	JetStreamWrapper    func(jetstream.JetStream) jetstream.JetStream
	PartitionCount      int
	PartitionWeights    []int64
	AssignmentStrategy  string
	ProcessingDelayMin  time.Duration
	ProcessingDelayMax  time.Duration
	CoordinatorReportCh chan<- coordinator.ReceivedMessage
	AssignmentReportCh  chan<- coordinator.AssignmentReport
	StartLatencyCh      chan<- coordinator.StartLatencyReport
	MetricsCollector    *metrics.Collector
	// Throughput knobs
	ConsumerBatchSize  int
	HandlerConcurrency int
	// AckWait is the JetStream consumer AckWait. Must be < Coordinator.GapAging.
	AckWait time.Duration
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
	// IsInitialCohort tags this worker as part of the simulation's initial
	// cluster (true) or as a later joiner via scale_up / restart (false).
	// Carried through to StartLatencyReport so the coordinator's classifier
	// can pick the right budget without depending on channel-receive timing.
	IsInitialCohort bool
	// DegradedReportCh receives a WorkerDegradedReport each time this worker
	// enters degraded mode. Optional — if nil, degraded events are only cached
	// in the worker's atomic field (readable via DegradedReason()).
	DegradedReportCh chan<- coordinator.WorkerDegradedReport

	// PartitionSource selects the partition source backend:
	//   - "static" (default): in-process source.NewStatic — the historical
	//     behavior; no NATS interaction for partition discovery.
	//   - "nats_kv": ensures a `parti-sim-source` KV bucket, seeds the
	//     `partitions` single key via a transient source.NatsKV.Update
	//     (codec parity with production), then constructs the worker's
	//     runtime source via source.NewNatsKV. Each worker opens its own
	//     watcher against the same bucket+key. Exercises the empirical
	//     reconciler-as-load-bearing-recovery path (see project memory).
	PartitionSource string

	// SourceBucket is the KV bucket name used when PartitionSource ==
	// "nats_kv". If empty, defaults to "parti-sim-source".
	SourceBucket string

	// SourceReconcileInterval overrides the NatsKV reconcile cadence when
	// PartitionSource == "nats_kv". If 0, defaults to 5s (chosen so that
	// reconcileInterval < watcher_stall duration < bucketUnavailableCooldown
	// holds for the Phase 2 scenarios).
	SourceReconcileInterval time.Duration

	// Phase 4: per-worker StableID pool overrides. Each field is consulted
	// with override-if-set semantics: when the field is the zero value
	// ("" / 0), the simulation default is used (prefix="simulation-worker",
	// max=999, min=0, ttl=parti.DefaultConfig().WorkerIDTTL). When non-
	// zero, the caller's value wins and overrides the sim default. This is
	// the load-bearing semantic that lets tiny-pool / freeze-victim chaos
	// scenarios carry a Min==Max==1 pool through worker construction
	// without NewWorker silently restoring the broad pool.
	WorkerIDPrefix string
	WorkerIDMin    int
	WorkerIDMax    int
	WorkerIDTTL    time.Duration

	// RevocationReportCh receives a RevocationReport each time the manager
	// drives a zero-partition UpdateWorkerConsumer call (Phase 4 ordering
	// oracle). The signal is needed because OnAssignmentChanged does NOT
	// fire on claim-loss revoke (revokeWorkerConsumer bypasses the apply
	// loop and the hook). Optional; nil channel disables the signal.
	RevocationReportCh chan<- coordinator.RevocationReport

	// RevocationReportDropFn, if non-nil, is invoked when a RevocationReport
	// could not be enqueued onto RevocationReportCh because the channel was
	// full. The orchestrator gates on the cumulative drop count so the
	// Phase 4 ordering oracle never silently loses its only negative
	// signal under stress.
	RevocationReportDropFn func()
}

// NewWorker creates a new worker.
//
// Parameters:
//   - cfg: Worker configuration
//
// Returns:
//   - *Worker: Initialized worker
//   - error: Error if initialization fails
func NewWorker(cfg Config) (*Worker, error) { //nolint:cyclop,gocyclo // dispatch grows with each new partition-source backend
	// Safety net: AckWait must be supplied by the caller. The
	// config.validateConfig pass enforces AckWait < Coordinator.GapAging,
	// but a forgetful caller of NewWorker (e.g., a missing field in a
	// worker.Config literal) would silently fall back to the durable
	// helper's 30s default, reintroducing the false-positive gap-escalation
	// window that the simulation config validation exists to prevent.
	if cfg.AckWait <= 0 {
		return nil, errors.New("worker.Config.AckWait must be > 0 (caller forgot to plumb cfg.Workers.AckWait?)")
	}

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

	// The partition source is constructed AFTER the Worker struct (below)
	// so the WithLeadershipProbe closure can capture &worker for the
	// "nats_kv" branch. See partitionSource construction further down.

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

	// Use default config and customize.
	//
	// Phase 4 override-if-set semantics: the hardcoded defaults
	// ("simulation-worker" / Max=999) apply ONLY when the corresponding
	// cfg.WorkerID* field is the zero value. A non-zero config field WINS.
	// This is what lets tiny-pool / freeze-victim scenarios carry a
	// Min==Max==1 pool through worker construction without NewWorker
	// silently restoring the broad pool. WorkerIDMin and WorkerIDTTL are
	// not hardcoded by the sim (they default via parti.DefaultConfig());
	// they're still mirrored as override-if-set for symmetry so scenario
	// configs can shorten TTL for MaxAge-expiry tests.
	partiCfg := parti.DefaultConfig()
	if cfg.WorkerIDPrefix != "" {
		partiCfg.WorkerIDPrefix = cfg.WorkerIDPrefix
	} else {
		partiCfg.WorkerIDPrefix = "simulation-worker"
	}
	if cfg.WorkerIDMax != 0 {
		partiCfg.WorkerIDMax = cfg.WorkerIDMax
	} else {
		partiCfg.WorkerIDMax = 999 // Support up to 1000 workers
	}
	if cfg.WorkerIDMin != 0 {
		partiCfg.WorkerIDMin = cfg.WorkerIDMin
	}
	if cfg.WorkerIDTTL != 0 {
		partiCfg.WorkerIDTTL = cfg.WorkerIDTTL
	}
	// Enable two-phase handoff only when exclusive consumption is requested.
	// This publishes ownership claims to KV so the Processing Gate can enforce exclusivity.
	partiCfg.EnableTwoPhaseHandoff = cfg.EnforceExclusiveConsumption
	// The handoff bucket's effective KV TTL is set by the all-in-one pre-create
	// path in cmd/simulation/main.go; manager-level overrides here are no-ops
	// because parti.SetDefaults restores the struct-tag default before Validate.

	// Use a generous startup timeout to avoid cancellations during
	// concurrent KV bucket creation, hygiene passes, and ID claiming across many workers.
	// Increased for stress scenarios.
	partiCfg.StartupTimeout = 120 * time.Second

	// Reduce cold start window for faster rebalancing in simulation
	// Default is 30s which is too long for fast testing
	partiCfg.ColdStartWindow = 10 * time.Second

	// Create worker struct first so we can reference it in hooks
	worker := &Worker{
		id:                     cfg.ID,
		nc:                     cfg.NC,
		js:                     cfg.JS,
		cfg:                    &partiCfg,
		processingDelayMin:     cfg.ProcessingDelayMin,
		processingDelayMax:     cfg.ProcessingDelayMax,
		coordinatorReportCh:    cfg.CoordinatorReportCh,
		assignmentReportCh:     cfg.AssignmentReportCh,
		startLatencyCh:         cfg.StartLatencyCh,
		metricsCollector:       cfg.MetricsCollector,
		logger:                 logger,
		currentPartitions:      0,
		handlerConcurrency:     cfg.HandlerConcurrency,
		consumerBatchSize:      cfg.ConsumerBatchSize,
		isInitialCohort:        cfg.IsInitialCohort,
		degradedReportCh:       cfg.DegradedReportCh,
		revocationReportCh:     cfg.RevocationReportCh,
		revocationReportDropFn: cfg.RevocationReportDropFn,
	}

	perSubMaxAckPending := max(worker.consumerBatchSize*2,
		// preserve a small minimum
		8)

	// Build the consumer options. Plumbed verbatim from the prior
	// durable.WorkerConsumerConfig literal — see
	// docs/plans/sim-oracle-phase2/00-plan.md for the field-by-field mapping.
	consumerOpts := []consumer.DynamicOption{
		consumer.WithLogger(logger),
		// Enable manual ack so the handler controls when to ack (after
		// reporting to coordinator). This avoids double-acking and ensures
		// our reporting order is consistent.
		consumer.WithManualAck(true),
		consumer.WithBatchSize(worker.consumerBatchSize),
		// Short fetch timeout so goroutines blocked in Next() exit rapidly
		// after cancellation, reducing apparent goroutine leak in simulation
		// leak tests.
		consumer.WithFetchTimeout(1 * time.Second),
		// Drain per-subject on removal to reduce NAK churn during chaos
		// scale-down. Timeout=0 → use the durable default (10s).
		consumer.WithDrainOnRemove(true, 0),
		// Under exclusivity gating and chaos, messages can be NAKed multiple
		// times. Increase MaxDeliver to avoid premature drop and false gap
		// escalations.
		consumer.WithMaxDeliver(50),
		// AckWait must be < Coordinator.GapAging; cross-validated in
		// config.validateConfig so this can never reintroduce false-positive
		// gap escalations during chaos.
		consumer.WithAckWait(cfg.AckWait),
		// Caps to bound per-subject concurrency and reduce disorder.
		consumer.WithMaxWaiting(2),
		consumer.WithMaxAckPending(perSubMaxAckPending),
		// Suppress pulls when not owner or not in commit/stable to reduce
		// NAK storms.
		consumer.WithPullGating(true),
	}

	// Enable Processing Gate for exclusive consumption when configured.
	if cfg.EnforceExclusiveConsumption {
		pg := &consumer.ProcessingGateConfig{
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
		consumerOpts = append(consumerOpts, consumer.WithProcessingGate(pg))
	}

	js, err := jetstream.New(cfg.NC)
	if err != nil {
		return nil, fmt.Errorf("failed to init JetStream: %w", err)
	}
	if cfg.JetStreamWrapper != nil {
		js = cfg.JetStreamWrapper(js)
	}

	dynamic, err := consumer.NewDynamic(
		js,
		"SIMULATION",
		"simulation",
		"simulation.partition.{{.PartitionID}}",
		consumer.MessageHandlerFunc(worker.processMessage),
		consumerOpts...,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create consumer.Dynamic: %w", err)
	}
	worker.dynamic = dynamic
	// Wrap the inner updater so zero-partition UpdateWorkerConsumer calls
	// emit a RevocationReport (Phase 4 ordering oracle). The wrapper is
	// transparent for non-zero calls.
	worker.updater = &revocationObservingUpdater{
		inner:    dynamic,
		workerID: worker.id,
		stableFn: worker.StableWorkerID,
		ch:       worker.revocationReportCh,
		onDrop:   worker.revocationReportDropFn,
	}

	if cfg.MetricsCollector != nil {
		dynamic.SetResolverMetrics(cfg.MetricsCollector.ResolverMetricsAdapter())
	}

	// Build partition source. The "nats_kv" branch captures the worker
	// pointer in its leadership-probe and unavailable-hook closures so the
	// hooks can resolve worker.manager.IsLeader at tick time (the manager
	// itself is constructed below, but the closures are not invoked until
	// after manager.Start). Mirrors the existing OnAssignmentChanged hook
	// pattern at worker.go:400.
	var partitionSource types.PartitionSource
	switch cfg.PartitionSource {
	case "", "static":
		partitionSource = source.NewStatic(partitions)
	case "nats_kv":
		bucket := cfg.SourceBucket
		if bucket == "" {
			bucket = "parti-sim-source"
		}
		recon := cfg.SourceReconcileInterval
		if recon <= 0 {
			recon = 5 * time.Second
		}
		ensureCtx, ensureCancel := context.WithTimeout(context.Background(), 10*time.Second)
		kv, eerr := kvutil.EnsureKVBucket(ensureCtx, cfg.JS, bucket, 0)
		ensureCancel()
		if eerr != nil {
			return nil, fmt.Errorf("failed to ensure source KV bucket %q: %w", bucket, eerr)
		}
		// Seed the SAME codec/CAS path as production NatsKV.Update by
		// constructing a transient instance and invoking Update once.
		// internal/partcodec is unreachable from this package, so the
		// transient-instance approach is the only way to guarantee codec
		// parity. Multiple workers seeding concurrently is benign: the
		// initial slice is identical so CAS conflicts last-writer-wins on
		// the same content.
		seed := source.NewNatsKV(kv, "partitions", logger)
		seedCtx, seedCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = seed.Update(seedCtx, partitions)
		seedCancel()

		// SourceUnavailableHook fires when the watcher restart loop or
		// reconciler observes the bucket is gone. The production
		// OnDegraded path does NOT trigger on source-bucket loss
		// (see source/nats_kv.go:123-148), so the sim fans out a
		// synthetic WorkerDegradedReport with reason
		// "source-unavailable:<bucket>" so the coordinator oracles can
		// observe it. Phase 2 INV3 (composed bucket_delete scenario)
		// relies on this adapter.
		workerID := cfg.ID
		reportCh := cfg.DegradedReportCh
		// NOTE: WithLeadershipProbe is intentionally NOT wired. When set,
		// the source's reconcileLoop ignores WithReconcileInterval and
		// uses leaderInterval=30s / followerInterval=5min instead — see
		// source/nats_kv.go:1005-1014. For the Phase 2 NATSKV-source
		// scenarios we want EVERY worker (leader or follower) to
		// reconcile at the explicit 5s cadence so the watcher_stall and
		// bucket_delete recovery paths can be observed within the
		// scenario's 3-4 minute budget. The leadership probe is a
		// production knob for reducing IOPS on followers in long-running
		// clusters and is not load-bearing for these chaos tests.
		// Documented as a follow-up: expose a per-source override so
		// the probe can be wired without overriding the explicit
		// reconcile interval.
		partitionSource = source.NewNatsKV(kv, "partitions", logger,
			source.WithReconcileInterval(recon),
			source.WithUnavailableHook(func(err error) {
				reason := fmt.Sprintf("source-unavailable:%s: %v", bucket, err)
				if reportCh == nil {
					return
				}
				select {
				case reportCh <- coordinator.WorkerDegradedReport{
					WorkerID: workerID,
					Reason:   reason,
					At:       time.Now(),
				}:
				default:
					log.Printf("[%s] source-unavailable degraded channel full; dropping (%s)", workerID, reason)
				}
			}),
		)
	default:
		return nil, fmt.Errorf("unsupported PartitionSource %q (allowed: static, nats_kv)", cfg.PartitionSource)
	}

	// Set up hooks to record metrics only (consumer updates handled by manager option)
	hooks := &types.Hooks{
		OnAssignmentChanged: worker.handleAssignmentChanged,
		OnDegraded:          worker.handleDegraded,
		OnError:             worker.handleError,
	}
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

	// Timestamp Start() entry so handleAssignmentChanged can measure the
	// Start()-to-first-assignment latency that the coordinator audits.
	w.startCalledAt = time.Now()
	w.firstAssignmentOK.Store(false)

	// Derive a Worker-owned child context. Cancelling the caller's context
	// (e.g., chaos events) still cascades to w.ctx → manager + shard
	// goroutines. The dedicated cancel is needed for internal failure
	// paths that must tear down shard goroutines (which select on
	// w.ctx.Done()) without depending on the caller cancelling its own
	// context — see WaitState-failure handling below.
	w.ctx, w.cancel = context.WithCancel(ctx)

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
		w.shardCount = max(w.handlerConcurrency, 1)
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
		w.cancel() // tear down any shard goroutines started above
		w.started.Store(false)

		return fmt.Errorf("failed to start manager: %w", err)
	}

	// Start returns after the synchronous sanity phase; wait for the
	// background runner to reach StateStable so the simulation does not
	// generate load before assignment is applied. Manager.Start's
	// auto-cleanup only fires on its own non-nil return — once Start
	// succeeded, a later wait failure must call Stop to tear down the
	// running background goroutines (heartbeat, runner, watchdog) and
	// to cancel m.ctx (created from context.Background, not w.ctx). The
	// shard goroutines started above select on w.ctx.Done(), so
	// w.cancel() is also required to prevent them leaking.
	//
	// Timeout matches partiCfg.StartupTimeout (120s above) so the sim's
	// failure path is engaged only when the manager's own soft watchdog
	// would also have declared the worker degraded. A shorter sim-side
	// timeout produces a flaky "manager did not reach StateStable" path
	// on chaos scenarios that perturb the cold-start cohort
	// (sim-discovered Issue 2): chaos firing during the first ~30s of
	// chaos_comprehensive can stretch the worker's wait through
	// rebalance churn even though the manager itself recovers.
	if err := <-w.manager.WaitState(parti.StateStable, 120*time.Second); err != nil {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = w.manager.Stop(stopCtx)
		stopCancel()
		w.cancel()
		w.started.Store(false)

		return fmt.Errorf("manager did not reach StateStable: %w", err)
	}

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

	// Report the first non-empty assignment back as a start-latency sample.
	// Guarded with CAS so we emit exactly once per Worker instance even if
	// the first rebalance arrives with 0 partitions and the second finally
	// assigns some (the sample we care about is "time to first real work").
	if len(newSet) > 0 && w.firstAssignmentOK.CompareAndSwap(false, true) {
		if w.startLatencyCh != nil && !w.startCalledAt.IsZero() {
			latency := time.Since(w.startCalledAt)
			// Block on send so the SlowStartExceedances invariant the
			// coordinator gates pass/fail on can never lose samples under
			// chaos bursts. Symmetric with the assignmentReportCh send below.
			select {
			case <-ctx.Done():
			case w.startLatencyCh <- coordinator.StartLatencyReport{WorkerID: w.id, Latency: latency, IsInitialCohort: w.isInitialCohort}:
			}
		}
	}

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

func (w *Worker) processAndAck(msg jetstream.Msg) error {
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

// WorkerStateInt returns the current manager state as an int (castable to parti.State /
// types.State). Returns 0 (StateInit) if the manager is nil.
// Implements coordinator.WorkerObserver.
func (w *Worker) WorkerStateInt() int {
	if w.manager == nil {
		return 0
	}
	return int(w.manager.State())
}

// StableWorkerID returns the stable worker ID claimed by the manager, or "" if
// the manager is nil or the ID has not yet been claimed.
// Implements coordinator.WorkerObserver.
func (w *Worker) StableWorkerID() string {
	if w.manager == nil {
		return ""
	}
	return w.manager.WorkerID()
}

// DegradedReason returns the most recently cached degraded reason captured by
// the OnDegraded hook, or "" if the worker has not entered degraded mode.
func (w *Worker) DegradedReason() string {
	p := w.degradedReason.Load()
	if p == nil {
		return ""
	}
	return *p
}

// ClaimLostObserved reports whether the OnError hook has seen a
// stableid.ErrClaimLost from the production claim-loss path. The sim
// claim-loss oracle uses this as the discriminator between a real
// claim-loss shutdown and a graceful Manager.Stop() — both terminate
// in StateShutdown, so state alone is not a sufficient signal.
// Implements coordinator.WorkerObserver.
func (w *Worker) ClaimLostObserved() bool {
	return w.claimLostObserved.Load()
}

// handleError is the OnError hook implementation. It captures
// stableid.ErrClaimLost surfaced by manager_election.go:117 so the
// claim-loss oracle can distinguish real claim-loss shutdowns from
// graceful Stop transitions. Other errors are ignored here — the
// degraded path already covers the broader error surface.
func (w *Worker) handleError(_ context.Context, err error) error {
	if errors.Is(err, stableid.ErrClaimLost) {
		w.claimLostObserved.Store(true)
	}

	return nil
}

// handleDegraded is the OnDegraded hook implementation. It caches the reason in
// the atomic field and fans out a non-blocking send to DegradedReportCh if set.
func (w *Worker) handleDegraded(_ context.Context, reason string) error {
	w.degradedReason.Store(&reason)
	if w.degradedReportCh != nil {
		select {
		case w.degradedReportCh <- coordinator.WorkerDegradedReport{
			WorkerID: w.StableWorkerID(),
			Reason:   reason,
			At:       time.Now(),
		}:
		default:
			log.Printf("[%s] degradedReportCh full, dropping degraded event (reason=%s)", w.id, reason)
		}
	}

	return nil
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

	// Proactively stop the consumer.Dynamic to prevent subject-loop goroutine
	// leaks (one per partition). consumer.Dynamic.Stop delegates to the
	// underlying durable WorkerConsumer.Close. Pass the bounded shutdown
	// ctx (5s timeout above) so the stop is time-bounded.
	if w.dynamic != nil {
		_ = w.dynamic.Stop(ctx)
	}

	w.started.Store(false)
}

// revocationObservingUpdater is a transparent wrapper around the inner
// WorkerConsumerUpdater that emits a RevocationReport on each zero-partition
// UpdateWorkerConsumer call. Used by the Phase 4 ordering oracle to detect the
// claim-loss revoke event (manager_election.go:79 revokeWorkerConsumer →
// UpdateWorkerConsumer(workerID, nil)). The revoke happens AFTER Manager.Stop
// has torn down the apply loop, so OnAssignmentChanged is NOT fired for it.
//
// Forwards all calls unchanged; the report send is best-effort (non-blocking
// drop if the channel is full or nil).
type revocationObservingUpdater struct {
	inner    parti.WorkerConsumerUpdater
	workerID string
	stableFn func() string
	ch       chan<- coordinator.RevocationReport
	// onDrop, if non-nil, is invoked whenever a report cannot be enqueued
	// (channel full). Wired to coordinator.IncRevocationReportDropped so the
	// orchestrator can fail closed instead of silently losing the only
	// negative signal feeding the Phase 4 ordering oracle.
	onDrop func()
}

// UpdateWorkerConsumer delegates to inner and, if partitions is empty, emits a
// RevocationReport. Returning the inner's error preserves end-to-end behavior.
func (r *revocationObservingUpdater) UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []parti.Partition) error {
	err := r.inner.UpdateWorkerConsumer(ctx, workerID, partitions)
	if len(partitions) == 0 && r.ch != nil {
		stableID := ""
		if r.stableFn != nil {
			stableID = r.stableFn()
		}
		report := coordinator.RevocationReport{
			WorkerID:       r.workerID,
			StableWorkerID: stableID,
			At:             time.Now(),
		}
		select {
		case r.ch <- report:
		default:
			// Drop if buffer full; the channel is sized at 4096 (see
			// EnableShutdownOracles) so an actual drop indicates the
			// coordinator-side drain is wedged. Surface the drop via
			// onDrop so the orchestrator fails closed — Phase 4
			// ordering depends on this signal.
			log.Printf("[revocationObservingUpdater] revocation_report_dropped worker=%s stable=%s buffer_full",
				r.workerID, stableID)
			if r.onDrop != nil {
				r.onDrop()
			}
		}
	}

	return err
}
