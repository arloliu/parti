package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/kvutil"
	"github.com/arloliu/parti/v2/test/simulation/internal/config"
	"github.com/arloliu/parti/v2/test/simulation/internal/coordinator"
	"github.com/arloliu/parti/v2/test/simulation/internal/metrics"
	"github.com/arloliu/parti/v2/test/simulation/internal/natsutil"
	"github.com/arloliu/parti/v2/test/simulation/internal/producer"
	"github.com/arloliu/parti/v2/test/simulation/internal/worker"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
)

// Globals for all-in-one dynamic scale control (minimize invasive changes).
var (
	aioNS             *nats.Conn
	aioJS             jetstream.JetStream
	aioEmbeddedServer *server.Server
	aioCfg            *config.Config
	aioCoord          *coordinator.Coordinator
	aioMetrics        *metrics.Collector
	aioCheckpoint     *coordinator.CheckpointManager
	aioRegistry       *coordinator.GoroutineRegistry
	aioWeights        []int64
	aioScaleUpOnce    int
	aioMinWorkers     int
	aioMaxWorkers     int
)

// durationFromParams extracts a duration from a chaos params map, tolerating
// both time.Duration (set by ChaosController.generateEventParams) and string
// (set by YAML scheduled_events params). Falls back to defaultDur when the
// key is absent, the type is unrecognised, or the string parse fails.
func durationFromParams(params map[string]any, key string, defaultDur time.Duration) time.Duration {
	v, ok := params[key]
	if !ok {
		return defaultDur
	}
	switch d := v.(type) {
	case time.Duration:
		if d > 0 {
			return d
		}
	case string:
		if parsed, err := time.ParseDuration(d); err == nil && parsed > 0 {
			return parsed
		}
	}

	return defaultDur
}

// parseChaosInterval parses a chaos interval string like "10-30m" into min and max durations.
func parseChaosInterval(interval string) (minDur, maxDur time.Duration, err error) {
	parts := strings.Split(interval, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid interval format: %s (expected format: '10-30m')", interval)
	}

	minDur, err = time.ParseDuration(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid min duration: %w", err)
	}

	maxDur, err = time.ParseDuration(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid max duration: %w", err)
	}

	if minDur >= maxDur {
		return 0, 0, errors.New("min interval must be less than max interval")
	}

	return minDur, maxDur, nil
}

func main() {
	configPath := flag.String("config", "configs/dev.yaml", "Path to configuration file")
	// In coordinator-only mode, allow overriding expected worker count via CLI.
	workersOverride := flag.Int("workers", -1, "Expected worker count (coordinator-only). Overrides config if >= 0")
	scaleUpOnce := flag.Int("scale-up-once", 0, "In all-in-one mode, spawn this many extra workers after startup")
	// Duration override to speed up ad-hoc tests (e.g., -duration=30s)
	durationOverride := flag.Duration("duration", 0, "Override simulation duration (e.g., 30s, 2m). 0 uses config value")
	// Cooldown period to stop chaos & producers before end to allow healing
	cooldown := flag.Duration("cooldown", 0, "Stop chaos and producers this long before end to allow healing (e.g., 30s). 0 disables")
	// Stop on failure override
	stopOnFailure := flag.Bool("stop-on-failure", false, "Stop simulation immediately on failure")
	flag.Parse()

	// Load config
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Apply CLI override for workers if provided
	if workersOverride != nil && *workersOverride >= 0 {
		cfg.Workers.Count = *workersOverride
		log.Printf("Overriding expected workers from CLI flag: %d", cfg.Workers.Count)
	}

	// Apply CLI override for duration if provided
	if durationOverride != nil && *durationOverride > 0 {
		cfg.Simulation.Duration = *durationOverride
		log.Printf("Overriding simulation duration from CLI flag: %v", cfg.Simulation.Duration)
	}

	// Apply CLI override for stop-on-failure if provided
	if *stopOnFailure {
		cfg.Coordinator.StopOnFailure = true
		log.Println("Overriding stop-on-failure from CLI flag: true")
	}

	log.Printf("Starting simulation in %s mode", cfg.Simulation.Mode)
	if cfg.Simulation.Duration > 0 {
		log.Printf("Simulation will run for %v", cfg.Simulation.Duration)
	} else {
		log.Println("Simulation will run indefinitely (press Ctrl+C to stop)")
	}

	// Create context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Apply duration timeout if configured
	if cfg.Simulation.Duration > 0 {
		ctx, cancel = context.WithTimeout(ctx, cfg.Simulation.Duration)
		defer cancel()
	}

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Record scale-up-once for all-in-one
	aioScaleUpOnce = *scaleUpOnce

	// Determine effective cooldown: CLI flag overrides config when provided
	effectiveCooldown := time.Duration(0)
	if cooldown != nil && *cooldown > 0 {
		effectiveCooldown = *cooldown
	} else if cfg.Simulation.Cooldown > 0 {
		effectiveCooldown = cfg.Simulation.Cooldown
	}

	// Start components based on mode
	errCh := make(chan error, 1)
	go func() {
		var runErr error
		switch cfg.Simulation.Mode {
		case "all-in-one":
			runErr = runAllInOne(ctx, cfg, *configPath, effectiveCooldown)
		case "producer":
			runErr = runProducer(ctx, cfg)
		case "worker":
			runErr = runWorker(ctx, cfg)
		case "coordinator":
			runErr = runCoordinator(ctx, cfg)
		default:
			runErr = fmt.Errorf("unknown mode: %s", cfg.Simulation.Mode)
		}
		errCh <- runErr
	}()

	// Wait for either completion, timeout, or signal
	var runErr error
	select {
	case runErr = <-errCh:
		// runAllInOne completed (either success or error)
	case <-ctx.Done():
		if cfg.Simulation.Duration > 0 {
			log.Printf("Simulation duration (%v) reached, shutting down...", cfg.Simulation.Duration)
		} else {
			log.Println("Context cancelled, shutting down...")
		}
		// Wait for runAllInOne to complete and get its error
		runErr = <-errCh
	case sig := <-sigCh:
		log.Printf("Received signal %v, shutting down...", sig)
		cancel()
		// Wait for runAllInOne to complete and get its error
		runErr = <-errCh
	}

	// Give components time to shut down gracefully
	time.Sleep(2 * time.Second)
	if runErr != nil {
		log.Printf("Simulation failed: %v", runErr)
		os.Exit(1) //nolint:gocritic // Intentional: defer cancel() not running is acceptable on exit
	}
	log.Println("Shutdown complete")
}

func runAllInOne(ctx context.Context, cfg *config.Config, cfgPath string, cooldown time.Duration) error { //nolint:cyclop,revive,gocyclo,nolintlint
	// Quiesced-drain flag toggled at cooldown start to suppress gap escalations
	var inCooldown atomic.Bool
	// Start embedded NATS if configured
	var ns *nats.Conn
	var embeddedServer *server.Server
	var err error
	if cfg.NATS.Mode == "embedded" {
		srv, nc, err := natsutil.StartEmbeddedNATS()
		if err != nil {
			return fmt.Errorf("failed to start embedded NATS: %w", err)
		}
		embeddedServer = srv
		ns = nc

		// Create stream
		if err := natsutil.CreateStream(nc, cfg.Partitions.Count); err != nil {
			return fmt.Errorf("failed to create stream: %w", err)
		}
	} else {
		ns, err = nats.Connect(cfg.NATS.URL)
		if err != nil {
			return fmt.Errorf("failed to connect to NATS: %w", err)
		}
	}

	js, err := jetstream.New(ns)
	if err != nil {
		return fmt.Errorf("failed to get JetStream: %w", err)
	}

	// Pre-create coordination KV buckets to avoid thundering herd on startup
	// when many workers ensure the same buckets concurrently.
	{
		pc := parti.DefaultConfig()
		// LOAD-BEARING: disable JetStream KV TTL for the handoff bucket in
		// simulation runs. This is the only path that actually controls the
		// bucket's storage-level TTL — kvutil opens an existing bucket without
		// reconciling config, so this pre-create value sticks. Without it,
		// auto-purging would drop ownership entries in steady state, forcing
		// the Processing Gate into unknown-ownership NAK loops and inflating
		// apparent gaps. The manager's in-memory cfg.KVBuckets.HandoffTTL is
		// purely advisory once the bucket exists.
		pc.KVBuckets.HandoffTTL = 0
		// Phase 6 / Gap 5: when chaos.handoff.precreate_maxage > 0, seed
		// the handoff bucket with the configured MaxAge BEFORE any
		// worker's manager runs. This emulates a handoff bucket created
		// by an older parti version that still carries a MaxAge, so the
		// manager's reconcileHandoffBucketMaxAge healing path
		// (manager_setup.go:reconcileHandoffBucketMaxAge) is exercised.
		// EnsureKVBucket is get-first, so the value passed here is what
		// actually sticks on the backing stream.
		handoffPrecreateMaxAge := cfg.Chaos.Handoff.PrecreateMaxAge
		handoffTTL := pc.KVBuckets.HandoffTTL
		if handoffPrecreateMaxAge > 0 {
			handoffTTL = handoffPrecreateMaxAge
			log.Printf("[Phase6] pre-creating handoff bucket %q with MaxAge=%v (scenario knob)",
				pc.KVBuckets.HandoffBucket, handoffPrecreateMaxAge)
		}
		// Use short, independent timeouts per bucket to avoid blocking startup
		timeBuckets := []struct {
			name string
			ttl  time.Duration
		}{
			{pc.KVBuckets.StableIDBucket, pc.WorkerIDTTL},
			{pc.KVBuckets.ElectionBucket, pc.ElectionTimeout},
			{pc.KVBuckets.HeartbeatBucket, pc.HeartbeatTTL},
			{pc.KVBuckets.AssignmentBucket, pc.KVBuckets.AssignmentTTL},
			{pc.KVBuckets.HandoffBucket, handoffTTL},
		}
		for _, b := range timeBuckets {
			bctx, bcancel := context.WithTimeout(ctx, 5*time.Second)
			_, kerr := kvutil.EnsureKVBucket(bctx, js, b.name, b.ttl)
			bcancel()
			if kerr != nil {
				return fmt.Errorf("failed to ensure KV bucket %s: %w", b.name, kerr)
			}
		}
		// Phase 6 / Gap 5: after seeding the handoff bucket with the
		// scenario's PrecreateMaxAge, confirm the value actually stuck
		// on the backing stream before any manager starts. If the
		// inherited MaxAge is NOT what we requested, the downstream
		// oracle is meaningless — log loudly so the run can be
		// diagnosed. We use the same Status() → KeyValueBucketStatus
		// surface the manager uses at manager_setup.go:kvStreamConfig.
		if handoffPrecreateMaxAge > 0 {
			vctx, vcancel := context.WithTimeout(ctx, 5*time.Second)
			seeded, verr := readHandoffBucketMaxAge(vctx, js, pc.KVBuckets.HandoffBucket)
			vcancel()
			if verr != nil {
				log.Printf("[Phase6] WARN: could not verify seeded MaxAge on %q: %v",
					pc.KVBuckets.HandoffBucket, verr)
			} else if seeded != handoffPrecreateMaxAge {
				log.Printf("[Phase6] WARN: seeded MaxAge mismatch on %q: requested=%v actual=%v "+
					"(get-first reused a pre-existing bucket; oracle may not fire)",
					pc.KVBuckets.HandoffBucket, handoffPrecreateMaxAge, seeded)
			} else {
				log.Printf("[Phase6] seeded MaxAge=%v on %q confirmed; reconciler healing path armed",
					seeded, pc.KVBuckets.HandoffBucket)
			}
		}
	}

	// No explicit KV bucket preparation here; the WorkerConsumer's Processing Gate
	// will ensure the handoff KV bucket exists when enabled.

	// Create metrics collector with per-partition metric gates from config
	metricsCollector := metrics.NewCollectorWithRegistryAndOptions(
		prometheus.DefaultRegisterer,
		cfg.Metrics.PerPartition.Latency,
		cfg.Metrics.PerPartition.Duplicates,
		cfg.Metrics.PerPartition.BucketCount,
	)
	var prometheusServer *metrics.PrometheusServer
	if cfg.Metrics.Prometheus.Enabled {
		addr := fmt.Sprintf(":%d", cfg.Metrics.Prometheus.Port)
		prometheusServer = metrics.NewPrometheusServer(addr, metricsCollector)

		go func() {
			if err := prometheusServer.Start(ctx); err != nil {
				log.Printf("Prometheus server error: %v", err)
			}
		}()

		log.Printf("Prometheus metrics server started on %s", addr)
		log.Printf("Access metrics at: http://localhost%s/metrics", addr)
	}

	// Start system metrics updater regardless of Prometheus to populate report fields
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				metricsCollector.UpdateSystemMetrics(runtime.NumGoroutine(), m.Alloc)
			}
		}
	}()

	// Create coordinator (wire duplicate tracing settings)
	dupCfg := coordinator.DupTraceSettings{
		Enabled:         cfg.Coordinator.DupTrace.Enabled,
		Window:          cfg.Coordinator.DupTrace.Window,
		ThresholdPerMin: cfg.Coordinator.DupTrace.ThresholdPerMin,
		TopN:            cfg.Coordinator.DupTrace.TopN,
	}
	coord := coordinator.NewCoordinator(cfg.Partitions.Count, metricsCollector, dupCfg, cfg.Coordinator.StopOnFailure, cfg.Coordinator.FailureReportPath)
	coord.GetTracker().SetWorkerCacheMax(cfg.Coordinator.WorkerCacheMaxPerPartition)
	// Configure optional SLO threshold for oldest pending hole age
	coord.SetSLOHoleMaxAge(cfg.Coordinator.SLO.HoleMaxAge)
	// Wire catch-up SLO (healing latency after absence)
	coord.ConfigureCatchUpSLO(
		cfg.Coordinator.SLO.EnableCatchUp,
		cfg.Coordinator.SLO.CatchUpDeadline,
		cfg.Coordinator.SLO.CatchUpPercent,
		cfg.Coordinator.SLO.AbsenceThreshold,
	)
	// Create goroutine registry for all-in-one mode chaos.
	// Must be created BEFORE coord.Start so EnableShutdownOracles can attach
	// the registry to the oracle watchers before the coordinator's goroutines start.
	goroutineRegistry := coordinator.NewGoroutineRegistry()
	coord.EnableShutdownOracles(goroutineRegistry)

	go coord.Start(ctx)

	// Create checkpoint manager if enabled
	var checkpointMgr *coordinator.CheckpointManager
	if cfg.Checkpoint.Enabled {
		checkpointMgr = coordinator.NewCheckpointManager(
			cfg.Checkpoint.Path,
			cfg.Checkpoint.Interval,
			coord.GetTracker(),
		)
		// Initialize with configured counts
		checkpointMgr.SetActiveWorkers(cfg.Workers.Count)
		checkpointMgr.SetActiveProducers(cfg.Producers.Count)
		go checkpointMgr.Start(ctx)
		log.Printf("Checkpoint manager started (interval: %v, directory: %s)",
			cfg.Checkpoint.Interval, cfg.Checkpoint.Path)
	}

	// Determine binary path and config path for process manager
	binaryPath := os.Args[0] // Current binary

	// Create process manager for chaos engineering
	processMgr := coordinator.NewProcessManager(binaryPath, cfgPath)

	// Persist globals for dynamic scale control (all-in-one)
	aioNS = ns
	aioJS = js
	aioEmbeddedServer = embeddedServer
	aioCfg = cfg
	aioCoord = coord
	aioMetrics = metricsCollector
	aioCheckpoint = checkpointMgr
	aioRegistry = goroutineRegistry
	aioMinWorkers = cfg.Chaos.MinWorkers
	aioMaxWorkers = cfg.Chaos.MaxWorkers

	// Create chaos controller if enabled
	var chaosCtrl *coordinator.ChaosController
	var chaosCancel context.CancelFunc
	if cfg.Chaos.Enabled {
		minInterval, maxInterval, err := parseChaosInterval(cfg.Chaos.Interval)
		if err != nil {
			return fmt.Errorf("failed to parse chaos interval: %w", err)
		}

		chaosConfig := coordinator.ChaosConfig{
			Enabled:                        cfg.Chaos.Enabled,
			Events:                         cfg.Chaos.Events,
			MinInterval:                    minInterval,
			MaxInterval:                    maxInterval,
			BurstEnabled:                   cfg.Chaos.BurstEnabled,
			BurstProbability:               cfg.Chaos.BurstProbability,
			BucketDeleteTargetOverride:     cfg.Chaos.BucketDeleteTargetOverride,
			LongDisconnectDurationOverride: cfg.Chaos.LongDisconnectDurationOverride,
			EventCallback: func(event coordinator.ChaosEvent, params map[string]any) {
				// Mark the first chaos event so the classifier can route
				// unobserved-owner duplicates into the post-chaos bucket.
				// Must fire BEFORE the handler runs so any receipts
				// triggered by this event are correctly attributed.
				if coord != nil {
					coord.MarkChaosStarted()
				}
				handleChaosEvent(ctx, coord, event, params, processMgr, metricsCollector, checkpointMgr, goroutineRegistry)
			},
		}

		chaosCtrl = coordinator.NewChaosController(chaosConfig)
		// Run chaos controller on its own cancelable context to support cooldown stop
		cctx, ccancel := context.WithCancel(ctx)
		chaosCancel = ccancel
		go chaosCtrl.Start(cctx)
		log.Printf("Chaos controller started (events: %v, interval: %v-%v)",
			cfg.Chaos.Events, minInterval, maxInterval)

		// Phase 4: scenario-scheduled one-shot events. Used to coordinate
		// SIGKILL → stale-takeover-respawn pairs where random chaos timing
		// is insufficient (the respawn MUST land strictly after the
		// staleThreshold and BEFORE any other chaos turn).
		for _, sev := range cfg.Chaos.ScheduledEvents {
			go func() {
				select {
				case <-cctx.Done():
					return
				case <-time.After(sev.At):
				}
				log.Printf("[ScheduledChaos] firing %s at +%v params=%v", sev.Event, sev.At, sev.Params)
				if chaosCtrl != nil {
					chaosCtrl.InjectEventNow(coordinator.ChaosEvent(sev.Event), sev.Params)
				}
			}()
		}
	}

	// Calculate partitions per producer (auto-distribute)
	partitionsPerProducer := cfg.Partitions.Count / cfg.Producers.Count
	if cfg.Partitions.Count%cfg.Producers.Count != 0 {
		partitionsPerProducer++ // Round up to handle remainder
	}

	// Generate partition weights
	var weights []int64
	if cfg.Partitions.Distribution == "exponential" {
		gen := producer.NewExponentialWeightGenerator(
			cfg.Partitions.Weights.Exponential.ExtremePercent,
			cfg.Partitions.Weights.Exponential.ExtremeWeight,
			cfg.Partitions.Weights.Exponential.NormalWeight,
		)
		weights = gen.GenerateWeights(cfg.Partitions.Count)
	} else {
		gen := producer.NewUniformWeightGenerator(1)
		weights = gen.GenerateWeights(cfg.Partitions.Count)
	}
	// Save weights for dynamic spawns
	aioWeights = weights

	// (moved) Producers start after workers (to reduce early duplicates)

	// Create workers (start concurrently for faster stable ID claiming)
	for i := 0; i < cfg.Workers.Count; i++ {
		workerID := fmt.Sprintf("worker-%d", i)

		// Create network control for this worker
		netCtrl := worker.NewNetworkControl()

		// Helper to create connection
		createNC := func() (*nats.Conn, error) {
			opts := []nats.Option{
				nats.SetCustomDialer(netCtrl),
				nats.ReconnectWait(100 * time.Millisecond),
				nats.MaxReconnects(-1),
			}
			if cfg.NATS.Mode == "embedded" {
				return nats.Connect(embeddedServer.ClientURL(), opts...)
			}

			return nats.Connect(cfg.NATS.URL, opts...)
		}

		workerNC, err := createNC()
		if err != nil {
			return fmt.Errorf("failed to create NATS connection for worker %d: %w", i, err)
		}

		workerCfg := worker.Config{
			ID:                  workerID,
			NC:                  workerNC,
			JS:                  js,
			PartitionCount:      cfg.Partitions.Count,
			PartitionWeights:    weights,
			AssignmentStrategy:  cfg.Workers.AssignmentStrategy,
			ProcessingDelayMin:  cfg.Workers.ProcessingDelay.Min,
			ProcessingDelayMax:  cfg.Workers.ProcessingDelay.Max,
			CoordinatorReportCh: coord.GetReceivedChannel(),
			AssignmentReportCh:  coord.GetAssignmentsChannel(),
			DegradedReportCh:    coord.GetDegradedReportsChannel(),
			StartLatencyCh:      coord.GetStartLatencyChannel(),
			MetricsCollector:    metricsCollector,
			ConsumerBatchSize:   cfg.Workers.ConsumerBatchSize,
			HandlerConcurrency:  cfg.Workers.HandlerConcurrency,
			AckWait:             cfg.Workers.AckWait,
			MaxSubjects:         cfg.Workers.MaxSubjects,
			// Exclusive consumption (Processing Gate)
			EnforceExclusiveConsumption: cfg.Workers.EnforceExclusiveConsumption,
			GateAllowedStates:           cfg.Workers.ProcessingGate.AllowedStates,
			GateWarmupDuration:          cfg.Workers.ProcessingGate.WarmupDuration,
			GateWarmupAllowedStates:     cfg.Workers.ProcessingGate.WarmupAllowedStates,
			GateNakDelay:                cfg.Workers.ProcessingGate.NakDelay,
			GateNakJitter:               cfg.Workers.ProcessingGate.NakJitter,
			GateDebug:                   cfg.Workers.ProcessingGate.Debug,
			IsInitialCohort:             true,
			PartitionSource:             cfg.Workers.PartitionSource,
			SourceBucket:                cfg.Workers.SourceBucket,
			SourceReconcileInterval:     cfg.Workers.SourceReconcileInterval,
			RevocationReportCh:          coord.GetRevocationReportsChannel(),
		}
		applyWorkerOverrides(&workerCfg, cfg, workerID)

		w, err := worker.NewWorker(workerCfg)
		if err != nil {
			return fmt.Errorf("failed to create worker %d: %w", i, err)
		}
		w.SetNetworkControl(netCtrl)

		// Create cancelable context for this worker
		workerCtx, workerCancel := context.WithCancel(ctx)
		// Define start callback for restart (declare, then assign for self-reference)
		var workerStart func(parent context.Context)
		workerStart = func(parent context.Context) {
			wc, wcancel := context.WithCancel(parent)

			// Create NEW connection for restarted worker
			newNC, err := createNC()
			if err != nil {
				log.Printf("Failed to create NATS connection for restarted worker %s: %v", workerID, err)
				wcancel()
				return
			}

			newCfg := workerCfg
			newCfg.NC = newNC
			newCfg.IsInitialCohort = false // restart is not part of the initial cohort

			// Recreate a new worker instance for clean restart
			nw, err := worker.NewWorker(newCfg)
			if err != nil {
				log.Printf("Failed to recreate worker %s: %v", workerID, err)
				wcancel()
				return
			}
			nw.SetNetworkControl(netCtrl)

			goroutineRegistry.Register(workerID, coordinator.WorkerGoroutine, wcancel, workerStart, nw)
			go func(worker *worker.Worker, id string, wctx context.Context) {
				if err := worker.Start(wctx); err != nil {
					log.Printf("Failed to start worker %s: %v", id, err)
					goroutineRegistry.MarkInactive(id)
					wcancel()
					return
				}
				<-wctx.Done()
				worker.Stop()
				goroutineRegistry.MarkInactive(id)
			}(nw, workerID, wc)
		}
		goroutineRegistry.Register(workerID, coordinator.WorkerGoroutine, workerCancel, workerStart, w)

		// Start worker in goroutine; lightly pace launches to reduce startup stampede
		go func(worker *worker.Worker, id string, workerCtx context.Context) {
			if err := worker.Start(workerCtx); err != nil {
				log.Printf("Failed to start worker %s: %v", id, err)
				goroutineRegistry.MarkInactive(id)
				return
			}

			// Wait for context cancellation (from chaos events or shutdown)
			<-workerCtx.Done()

			// Give worker time to cleanup
			worker.Stop()

			// Worker stopped, mark as inactive
			goroutineRegistry.MarkInactive(id)
		}(w, workerID, workerCtx)

		// Small pacing delay between starting workers
		time.Sleep(20 * time.Millisecond)
	}

	// Record initial worker count
	if metricsCollector != nil {
		metricsCollector.SetWorkersActive(cfg.Workers.Count)
		log.Printf("Recorded initial worker count: %d", cfg.Workers.Count)
	}

	// Phase 6 / Gap 5: fire the live-MaxAge oracle once any worker reaches
	// StateStable. The reconciler runs during each manager's Start path,
	// so a single Stable observation is sufficient to validate the heal.
	startHandoffBucketMaxAgeOracle(ctx, goroutineRegistry)

	// Optional: immediate scale-up via CLI flag
	if aioScaleUpOnce > 0 {
		spawnN := aioScaleUpOnce
		for range spawnN {
			id := fmt.Sprintf("worker-%d", nextWorkerIndex(goroutineRegistry))
			if spawnAllInOneWorker(ctx, id) {
				if metricsCollector != nil {
					metricsCollector.SetWorkersActive(goroutineRegistry.GetActiveCount(coordinator.WorkerGoroutine))
				}
				if checkpointMgr != nil {
					checkpointMgr.SetActiveWorkers(goroutineRegistry.GetActiveCount(coordinator.WorkerGoroutine))
				}
			}
		}
	}

	// Phase 2: when running the NATSKV source mode, start the source-
	// convergence driver. It periodically mutates the partition list via
	// the same codec/CAS path as production NatsKV.Update and registers
	// expectations against the coordinator's source-convergence oracle.
	if cfg.Workers.PartitionSource == "nats_kv" && !cfg.Chaos.DisableSourceConvergenceDriver {
		bucket := cfg.Workers.SourceBucket
		if bucket == "" {
			bucket = "parti-sim-source"
		}
		// Build the full partition slice (mirrors worker.NewWorker).
		baseParts := make([]types.Partition, cfg.Partitions.Count)
		for i := 0; i < cfg.Partitions.Count; i++ {
			w := int64(1)
			if i < len(weights) {
				w = weights[i]
			}
			baseParts[i] = types.Partition{
				Keys:   []string{fmt.Sprintf("%d", i)},
				Weight: w,
			}
		}
		// period: mutate every 60s so each expectation has time to
		// converge (T_converge = 30s) AND the watcher_stall window
		// (45s scheduled every 30-45s) overlaps at least one mutation.
		// t_converge = max(reconcileInterval × 3, 30s) with the
		// worker's default 5s reconcileInterval = 30s.
		startSourceConvergenceDriver(ctx, bucket, baseParts, 60*time.Second, 30*time.Second)
	}

	// Start producers AFTER workers, with a brief grace period
	time.Sleep(500 * time.Millisecond)
	for i := 0; i < cfg.Producers.Count; i++ {
		// Calculate partition range for this producer
		startPartition := i * partitionsPerProducer
		endPartition := min((i+1)*partitionsPerProducer, cfg.Partitions.Count)

		partitionIDs := make([]int, 0, endPartition-startPartition)
		partitionWeights := make([]int64, 0, endPartition-startPartition)
		for j := startPartition; j < endPartition; j++ {
			partitionIDs = append(partitionIDs, j)
			partitionWeights = append(partitionWeights, weights[j])
		}

		producerID := fmt.Sprintf("producer-%d", i)

		// Give each producer its own NATS connection + NetworkControl so
		// ProducerDisconnectEvent (Phase 5 / Gap 10a) can sever only the
		// producer's connection without touching worker or manager conns.
		// The NetworkControl implements nats.CustomDialer.
		prodNetCtrl := producer.NewNetworkControl()
		var prodNC *nats.Conn
		if cfg.NATS.Mode == "embedded" {
			prodNC, err = nats.Connect(embeddedServer.ClientURL(),
				nats.SetCustomDialer(prodNetCtrl),
				nats.ReconnectWait(100*time.Millisecond),
				nats.MaxReconnects(-1),
			)
		} else {
			prodNC, err = nats.Connect(cfg.NATS.URL,
				nats.SetCustomDialer(prodNetCtrl),
				nats.ReconnectWait(100*time.Millisecond),
				nats.MaxReconnects(-1),
			)
		}
		if err != nil {
			return fmt.Errorf("failed to create NATS connection for producer %d: %w", i, err)
		}
		prodJS, err := jetstream.New(prodNC)
		if err != nil {
			return fmt.Errorf("failed to create JetStream for producer %d: %w", i, err)
		}

		prod := producer.NewProducer(
			producerID,
			prodJS,
			partitionIDs,
			partitionWeights,
			cfg.Partitions.MessageRatePerPartition,
			coord.GetSentChannel(),
			metricsCollector,
		)
		prod.SetNetworkControl(prodNetCtrl)

		// Create cancelable context for this producer
		producerCtx, producerCancel := context.WithCancel(ctx)
		// Define start callback to allow restart (declare, then assign to allow self-reference)
		var producerStart func(parent context.Context)
		producerStart = func(parent context.Context) {
			pc, pcancel := context.WithCancel(parent)
			goroutineRegistry.Register(producerID, coordinator.ProducerGoroutine, pcancel, producerStart, prod)
			go func(p *producer.Producer, id string, prodCtx context.Context) {
				p.Start(prodCtx)
				// Producer stopped (context cancelled), mark as inactive
				goroutineRegistry.MarkInactive(id)
			}(prod, producerID, pc)
		}
		goroutineRegistry.Register(producerID, coordinator.ProducerGoroutine, producerCancel, producerStart, prod)

		go func(p *producer.Producer, id string, prodCtx context.Context) {
			p.Start(prodCtx)
			// Producer stopped (context cancelled), mark as inactive
			goroutineRegistry.MarkInactive(id)
		}(prod, producerID, producerCtx)
	}

	// If a cooldown is configured and we have a deadline, stop chaos & producers early to allow healing
	if dl, ok := ctx.Deadline(); ok && cooldown > 0 {
		delay := max(time.Until(dl)-cooldown, 0)
		time.AfterFunc(delay, func() {
			log.Printf("Entering cooldown window (%v before end): stopping chaos and producers to allow healing", cooldown)
			// Mark cooldown active to enable quiesced drain behavior
			inCooldown.Store(true)
			// Stop chaos controller if running
			if chaosCtrl != nil && chaosCancel != nil {
				log.Println("[Chaos] Stopping chaos controller (cooldown)")
				chaosCancel()
			}
			// Stop all producers
			if goroutineRegistry != nil {
				producers := goroutineRegistry.GetByType(coordinator.ProducerGoroutine)
				for _, p := range producers {
					log.Printf("[Cooldown] Stopping producer goroutine: %s", p.ID)
					p.Cancel()
					goroutineRegistry.MarkInactive(p.ID)
				}
				if aioCheckpoint != nil {
					aioCheckpoint.SetActiveProducers(0)
				}
			}
		})
	}

	// Print report periodically
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// Early snapshot (t+5s) to capture initial churn before first 30s interval.
	// We intentionally do NOT age out holes here to avoid
	// misclassifying short-lived out-of-order sequences as gaps.
	time.AfterFunc(5*time.Second, func() {
		coord.PrintReport()
	})

	for {
		select {
		case <-ctx.Done():
			// Escalate aged holes to gaps before final report, but only when NOT in cooldown.
			// During cooldown, hole escalation is deliberately suppressed to allow healing.
			// Re-escalating at shutdown would contradict the cooldown intent and count
			// holes that are expected to remain (e.g., messages in-flight from stopped producers).
			if !inCooldown.Load() {
				cutoff := time.Now().Add(-cfg.Coordinator.GapAging)
				if escalations := coord.GetTracker().AgeOut(cutoff); len(escalations) > 0 {
					for range escalations {
						if metricsCollector != nil {
							metricsCollector.RecordGap()
						}
					}
					// If gaps are found at the end, trigger failure report if configured
					if cfg.Coordinator.StopOnFailure {
						coord.TriggerFailure("Gap detected (final scan)", escalations[0])
					}
				}
			}
			coord.PrintReport()
			// Evaluate stability audit results via metrics (workers logged pass/fail individually)
			if metricsCollector != nil {
				var invariantsErr error
				late := metricsCollector.GetLateMessagesTotal()
				lost := metricsCollector.GetLostMessagesTotal()
				failures := metricsCollector.GetAuditFailuresTotal()
				initialExc, takeoverExc := coord.SlowStartExceedances()
				tr := coord.GetTracker()
				violations := tr.GetOwnershipViolationCount()
				concurrent := tr.GetConcurrentOwnersViolationCount()
				inconclusive := tr.GetOwnershipInconclusiveCount()
				_, unobservedPost := tr.GetOwnershipUnobservedCounts()
				snapshotOverlaps := coord.GetSnapshotOverlapCount()
				doubleLeaders := coord.GetDoubleLeaderObservations()
				stateReconcileViol := coord.GetStateReconcileViolations()
				expDegMissing := coord.GetExpectedDegradedMissing()
				expDegObserved := coord.GetExpectedDegradedObserved()
				unexpClaimLost := coord.GetUnexpectedClaimLostShutdown()
				srcConvObserved := coord.GetSourceConvergenceObserved()
				srcConvMissing := coord.GetSourceConvergenceMissing()
				spuriousUnavail := coord.GetSpuriousSourceUnavailable()
				coord.CheckLongDisconnectExpectations()
				ldReassignObserved := coord.GetLongDisconnectReassignmentObserved()
				ldReassignMissing := coord.GetLongDisconnectReassignmentMissing()
				claimLossOrderViol := coord.GetClaimLossOrderingViolations()
				// INV1/INV2 (Phase 5): aggregate producer-resume-missing and CAS-storm-panic counters.
				var producerResumeMissing int64
				if goroutineRegistry != nil {
					for _, info := range goroutineRegistry.GetByType(coordinator.ProducerGoroutine) {
						if p, ok := info.Obj.(*producer.Producer); ok {
							producerResumeMissing += p.ResumeMissing()
						}
					}
				}
				casStormPanics := casStormUnexpectedPanics.Load()
				// Phase 6 / Gap 5 counters.
				handoffMaxAgeViol := handoffBucketMaxAgeViolations.Load()
				orphanClaimDrift := orphanStableClaimDrift.Load()
				if failures > 0 || late > 0 || lost > 0 || initialExc > 0 || takeoverExc > 0 || violations > 0 || inconclusive > 0 || unobservedPost > 0 || snapshotOverlaps > 0 || doubleLeaders > 0 || stateReconcileViol > 0 || expDegMissing > 0 || unexpClaimLost > 0 || srcConvMissing > 0 || spuriousUnavail > 0 || ldReassignMissing > 0 || claimLossOrderViol > 0 || producerResumeMissing > 0 || casStormPanics > 0 || handoffMaxAgeViol > 0 || orphanClaimDrift > 0 {
					// Record error but proceed to ordered shutdown
					invariantsErr = fmt.Errorf("stability invariants failed: audit_failures=%d late=%d lost=%d slow_start_initial=%d slow_start_takeover=%d ownership_violations=%d concurrent_owners=%d inconclusive=%d unobserved_post_chaos=%d snapshot_overlaps=%d double_leaders=%d state_reconcile_violations=%d expected_degraded_observed=%d expected_degraded_missing=%d unexpected_claim_lost_shutdown=%d source_convergence_observed=%d source_convergence_missing=%d spurious_source_unavailable=%d long_disconnect_reassignment_observed=%d long_disconnect_reassignment_missing=%d claim_loss_stop_ordering_violations=%d producer_resume_missing=%d assignment_cas_storm_unexpected_panic=%d handoff_bucket_maxage_violation=%d orphan_stable_claim_drift=%d",
						failures, late, lost, initialExc, takeoverExc, violations, concurrent, inconclusive, unobservedPost, snapshotOverlaps, doubleLeaders, stateReconcileViol, expDegObserved, expDegMissing, unexpClaimLost, srcConvObserved, srcConvMissing, spuriousUnavail, ldReassignObserved, ldReassignMissing, claimLossOrderViol, producerResumeMissing, casStormPanics, handoffMaxAgeViol, orphanClaimDrift)
				}
				if invariantsErr == nil {
					log.Printf("Stability invariants passed: audit_failures=%d late=%d lost=%d slow_start_initial=%d slow_start_takeover=%d ownership_violations=%d concurrent_owners=%d inconclusive=%d unobserved_post_chaos=%d snapshot_overlaps=%d double_leaders=%d state_reconcile_violations=%d expected_degraded_observed=%d expected_degraded_missing=%d unexpected_claim_lost_shutdown=%d source_convergence_observed=%d source_convergence_missing=%d spurious_source_unavailable=%d long_disconnect_reassignment_observed=%d long_disconnect_reassignment_missing=%d claim_loss_stop_ordering_violations=%d producer_resume_missing=%d assignment_cas_storm_unexpected_panic=%d handoff_bucket_maxage_violation=%d orphan_stable_claim_drift=%d",
						failures, late, lost, initialExc, takeoverExc, violations, concurrent, inconclusive, unobservedPost, snapshotOverlaps, doubleLeaders, stateReconcileViol, expDegObserved, expDegMissing, unexpClaimLost, srcConvObserved, srcConvMissing, spuriousUnavail, ldReassignObserved, ldReassignMissing, claimLossOrderViol, producerResumeMissing, casStormPanics, handoffMaxAgeViol, orphanClaimDrift)
				} else {
					log.Printf("Stability invariants failed: %v", invariantsErr)
				}
			}
			// Ordered shutdown: wait for controllers/managers and workers/producers to stop before NATS
			shutdownDeadline := time.Now().Add(3 * time.Second)
			for {
				activeW := goroutineRegistry.GetActiveCount(coordinator.WorkerGoroutine)
				activeP := goroutineRegistry.GetActiveCount(coordinator.ProducerGoroutine)
				if activeW == 0 && activeP == 0 {
					break
				}
				if time.Now().After(shutdownDeadline) {
					log.Printf("Timed out waiting for goroutines to stop (workers=%d producers=%d)", activeW, activeP)
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			// Close NATS connection last
			if ns != nil {
				ns.Close()
			}
			if embeddedServer != nil {
				embeddedServer.Shutdown()
			}
			// Return success or failure based on invariants (re-evaluate to be safe)
			// Option A safety net: check for unresolved gaps (gaps that were never healed)
			stats := coord.GetStats()
			gapsHealed := coord.GetTracker().GetGapsHealedCount()
			unresolvedGaps := int64(stats.GapCount) - gapsHealed
			if unresolvedGaps > 0 {
				return fmt.Errorf("unresolved gaps at shutdown: detected=%d healed=%d unresolved=%d", stats.GapCount, gapsHealed, unresolvedGaps)
			}
			log.Printf("Gap resolution check passed: detected=%d healed=%d", stats.GapCount, gapsHealed)

			if metricsCollector != nil {
				late := metricsCollector.GetLateMessagesTotal()
				lost := metricsCollector.GetLostMessagesTotal()
				failures := metricsCollector.GetAuditFailuresTotal()
				initialExc, takeoverExc := coord.SlowStartExceedances()
				tr := coord.GetTracker()
				violations := tr.GetOwnershipViolationCount()
				concurrent := tr.GetConcurrentOwnersViolationCount()
				inconclusive := tr.GetOwnershipInconclusiveCount()
				_, unobservedPost := tr.GetOwnershipUnobservedCounts()
				snapshotOverlaps := coord.GetSnapshotOverlapCount()
				doubleLeaders := coord.GetDoubleLeaderObservations()
				stateReconcileViol := coord.GetStateReconcileViolations()
				expDegMissing := coord.GetExpectedDegradedMissing()
				expDegObserved := coord.GetExpectedDegradedObserved()
				unexpClaimLost := coord.GetUnexpectedClaimLostShutdown()
				srcConvObserved := coord.GetSourceConvergenceObserved()
				srcConvMissing := coord.GetSourceConvergenceMissing()
				spuriousUnavail := coord.GetSpuriousSourceUnavailable()
				coord.CheckLongDisconnectExpectations()
				ldReassignObserved := coord.GetLongDisconnectReassignmentObserved()
				ldReassignMissing := coord.GetLongDisconnectReassignmentMissing()
				claimLossOrderViol := coord.GetClaimLossOrderingViolations()
				// INV1/INV2 (Phase 5): aggregate producer-resume-missing and CAS-storm-panic counters.
				var producerResumeMissing int64
				if goroutineRegistry != nil {
					for _, info := range goroutineRegistry.GetByType(coordinator.ProducerGoroutine) {
						if p, ok := info.Obj.(*producer.Producer); ok {
							producerResumeMissing += p.ResumeMissing()
						}
					}
				}
				casStormPanics := casStormUnexpectedPanics.Load()
				// Phase 6 / Gap 5 counters.
				handoffMaxAgeViol := handoffBucketMaxAgeViolations.Load()
				orphanClaimDrift := orphanStableClaimDrift.Load()
				if failures > 0 || late > 0 || lost > 0 || initialExc > 0 || takeoverExc > 0 || violations > 0 || inconclusive > 0 || unobservedPost > 0 || snapshotOverlaps > 0 || doubleLeaders > 0 || stateReconcileViol > 0 || expDegMissing > 0 || unexpClaimLost > 0 || srcConvMissing > 0 || spuriousUnavail > 0 || ldReassignMissing > 0 || claimLossOrderViol > 0 || producerResumeMissing > 0 || casStormPanics > 0 || handoffMaxAgeViol > 0 || orphanClaimDrift > 0 {
					invariantsErr := fmt.Errorf("stability invariants failed: audit_failures=%d late=%d lost=%d slow_start_initial=%d slow_start_takeover=%d ownership_violations=%d concurrent_owners=%d inconclusive=%d unobserved_post_chaos=%d snapshot_overlaps=%d double_leaders=%d state_reconcile_violations=%d expected_degraded_observed=%d expected_degraded_missing=%d unexpected_claim_lost_shutdown=%d source_convergence_observed=%d source_convergence_missing=%d spurious_source_unavailable=%d long_disconnect_reassignment_observed=%d long_disconnect_reassignment_missing=%d claim_loss_stop_ordering_violations=%d producer_resume_missing=%d assignment_cas_storm_unexpected_panic=%d handoff_bucket_maxage_violation=%d orphan_stable_claim_drift=%d",
						failures, late, lost, initialExc, takeoverExc, violations, concurrent, inconclusive, unobservedPost, snapshotOverlaps, doubleLeaders, stateReconcileViol, expDegObserved, expDegMissing, unexpClaimLost, srcConvObserved, srcConvMissing, spuriousUnavail, ldReassignObserved, ldReassignMissing, claimLossOrderViol, producerResumeMissing, casStormPanics, handoffMaxAgeViol, orphanClaimDrift)
					// Emit the structured failure_report.json so CI artifacts
					// include InconclusiveOwnerEvents / unobserved counters /
					// FirstChaosEventAt — auditability that the new
					// shutdown-invariant counters demand. TriggerFailure
					// is idempotent via failureOnce, safe to call even if
					// an earlier critical failure already wrote the report.
					coord.TriggerFailure("Stability invariants failed at shutdown", invariantsErr)

					return invariantsErr
				}
			}

			return nil
		case <-ticker.C:
			// During cooldown, suppress escalation of pending holes into gaps to allow healing
			if !inCooldown.Load() {
				cutoff := time.Now().Add(-cfg.Coordinator.GapAging)
				if escalations := coord.GetTracker().AgeOut(cutoff); len(escalations) > 0 {
					for range escalations {
						if metricsCollector != nil {
							metricsCollector.RecordGap()
						}
					}
					if cfg.Coordinator.StopOnFailure {
						coord.TriggerFailure("Gap detected (aged out)", escalations[0])
						return errors.New("simulation stopped due to gap detection")
					}
				}
			} else {
				// Quiesced drain: mark head-of-line aged holes as suppressed to avoid
				// escalation and prevent double-counting across intervals.
				cutoff := time.Now().Add(-cfg.Coordinator.GapAging)
				supp := coord.GetTracker().MarkHeadOfLineAgedHolesSuppressed(cutoff)
				if supp > 0 {
					coord.GetTracker().IncrementSuppressedHoles(supp)
					log.Printf("[Cooldown] Suppressed %d head-of-line aged holes (deferred gap escalations)", supp)
				}
			}
			coord.PrintReport()
		}
	}
}

func runProducer(ctx context.Context, cfg *config.Config) error {
	// Connect to NATS
	ns, err := nats.Connect(cfg.NATS.URL)
	if err != nil {
		return fmt.Errorf("failed to connect to NATS: %w", err)
	}
	defer ns.Close()

	js, err := jetstream.New(ns)
	if err != nil {
		return fmt.Errorf("failed to get JetStream: %w", err)
	}

	// Generate weights
	var weights []int64
	if cfg.Partitions.Distribution == "exponential" {
		gen := producer.NewExponentialWeightGenerator(
			cfg.Partitions.Weights.Exponential.ExtremePercent,
			cfg.Partitions.Weights.Exponential.ExtremeWeight,
			cfg.Partitions.Weights.Exponential.NormalWeight,
		)
		weights = gen.GenerateWeights(cfg.Partitions.Count)
	} else {
		gen := producer.NewUniformWeightGenerator(1)
		weights = gen.GenerateWeights(cfg.Partitions.Count)
	}

	// Create producer
	// Large buffer to prevent dropping reports during high load.
	// With 1500 partitions and high message rates, coordinator might lag temporarily.
	reportCh := make(chan producer.ReportMessage, 100000)
	producerID := os.Getenv("PRODUCER_ID")
	if producerID == "" {
		producerID = "producer-0"
	}

	// Calculate this producer's partition range
	producerIndex := 0
	_, _ = fmt.Sscanf(producerID, "producer-%d", &producerIndex)

	// Calculate partitions per producer (auto-distribute)
	partitionsPerProducer := cfg.Partitions.Count / cfg.Producers.Count
	if cfg.Partitions.Count%cfg.Producers.Count != 0 {
		partitionsPerProducer++ // Round up to handle remainder
	}
	startPartition := producerIndex * partitionsPerProducer
	endPartition := min((producerIndex+1)*partitionsPerProducer, cfg.Partitions.Count)

	partitionIDs := make([]int, 0, endPartition-startPartition)
	partitionWeights := make([]int64, 0, endPartition-startPartition)
	for j := startPartition; j < endPartition; j++ {
		partitionIDs = append(partitionIDs, j)
		partitionWeights = append(partitionWeights, weights[j])
	}

	prod := producer.NewProducer(
		producerID,
		js,
		partitionIDs,
		partitionWeights,
		cfg.Partitions.MessageRatePerPartition,
		reportCh,
		nil, // No metrics in standalone mode
	)

	prod.Start(ctx)

	return nil
}

func runWorker(ctx context.Context, cfg *config.Config) error {
	// Connect to NATS
	ns, err := nats.Connect(cfg.NATS.URL)
	if err != nil {
		return fmt.Errorf("failed to connect to NATS: %w", err)
	}
	defer ns.Close()

	js, err := jetstream.New(ns)
	if err != nil {
		return fmt.Errorf("failed to get JetStream: %w", err)
	}

	// Generate weights
	var weights []int64
	if cfg.Partitions.Distribution == "exponential" {
		gen := producer.NewExponentialWeightGenerator(
			cfg.Partitions.Weights.Exponential.ExtremePercent,
			cfg.Partitions.Weights.Exponential.ExtremeWeight,
			cfg.Partitions.Weights.Exponential.NormalWeight,
		)
		weights = gen.GenerateWeights(cfg.Partitions.Count)
	} else {
		gen := producer.NewUniformWeightGenerator(1)
		weights = gen.GenerateWeights(cfg.Partitions.Count)
	}

	// Create worker
	// Large buffer to prevent dropping reports during high load.
	// With 100 workers processing messages from 1500 partitions, coordinator might lag.
	reportCh := make(chan coordinator.ReceivedMessage, 100000)
	workerID := os.Getenv("WORKER_ID")
	if workerID == "" {
		workerID = "worker-0"
	}

	workerCfg := worker.Config{
		ID:                  workerID,
		NC:                  ns,
		JS:                  js,
		PartitionCount:      cfg.Partitions.Count,
		PartitionWeights:    weights,
		AssignmentStrategy:  cfg.Workers.AssignmentStrategy,
		ProcessingDelayMin:  cfg.Workers.ProcessingDelay.Min,
		ProcessingDelayMax:  cfg.Workers.ProcessingDelay.Max,
		CoordinatorReportCh: reportCh,
		MetricsCollector:    nil, // No metrics in standalone worker mode
		ConsumerBatchSize:   cfg.Workers.ConsumerBatchSize,
		HandlerConcurrency:  cfg.Workers.HandlerConcurrency,
		AckWait:             cfg.Workers.AckWait,
		MaxSubjects:         cfg.Workers.MaxSubjects,
	}
	// Phase 4c: read STABLEID_* env vars and apply them as per-worker
	// overrides. Parse failures are loud (return Start error) so silent
	// fallback cannot mask scenario bugs.
	if err := applyStableIDEnv(&workerCfg, os.Getenv); err != nil {
		return fmt.Errorf("invalid STABLEID_* env: %w", err)
	}

	w, err := worker.NewWorker(workerCfg)
	if err != nil {
		return fmt.Errorf("failed to create worker: %w", err)
	}

	return w.Start(ctx)
}

func runCoordinator(ctx context.Context, cfg *config.Config) error {
	// In standalone coordinator mode, use config values where helpful but avoid starting metrics server, etc.
	partCount := 0
	stopOnFailure := false
	failureReportPath := ""
	if cfg != nil {
		if cfg.Partitions.Count > 0 {
			partCount = cfg.Partitions.Count
		}
		stopOnFailure = cfg.Coordinator.StopOnFailure
		failureReportPath = cfg.Coordinator.FailureReportPath
	}
	coord := coordinator.NewCoordinator(partCount, nil, coordinator.DupTraceSettings{}, stopOnFailure, failureReportPath)
	// Configure optional SLO threshold for oldest pending hole age
	if cfg != nil {
		coord.SetSLOHoleMaxAge(cfg.Coordinator.SLO.HoleMaxAge)
		coord.ConfigureCatchUpSLO(
			cfg.Coordinator.SLO.EnableCatchUp,
			cfg.Coordinator.SLO.CatchUpDeadline,
			cfg.Coordinator.SLO.CatchUpPercent,
			cfg.Coordinator.SLO.AbsenceThreshold,
		)
	}

	// If an expected worker count is known (from config or CLI override), pass it as a hint.
	if cfg != nil && cfg.Workers.Count > 0 {
		coord.SetExpectedWorkers(cfg.Workers.Count)
	}

	// Print report periodically
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	go coord.Start(ctx)

	for {
		select {
		case <-ctx.Done():
			coord.PrintReport()
			return nil
		case <-ticker.C:
			coord.PrintReport()
		}
	}
}

// handleChaosEvent handles chaos events by interacting with the process manager.
//
//nolint:cyclop // Branching mirrors the chaos-event matrix.
func handleChaosEvent(
	ctx context.Context,
	coord *coordinator.Coordinator,
	event coordinator.ChaosEvent,
	params map[string]any,
	processMgr *coordinator.ProcessManager,
	metricsCollector *metrics.Collector,
	checkpointMgr *coordinator.CheckpointManager,
	goroutineRegistry *coordinator.GoroutineRegistry,
) {
	log.Printf("[Chaos] Handling event: %s with params: %v", event, params)

	// Try goroutine-level chaos first (for all-in-one mode)
	if goroutineRegistry != nil {
		handleGoroutineChaos(ctx, event, params, goroutineRegistry, metricsCollector, checkpointMgr)
		return
	}

	// Fall back to process-level chaos (for distributed mode)
	switch event {
	case coordinator.WorkerCrashEvent, coordinator.ProducerCrashEvent:
		// Kill random worker or producer
		handleCrashEvent(event, processMgr, metricsCollector)

	case coordinator.WorkerRestartEvent:
		count, ok := params["count"].(int)
		if !ok {
			log.Println("[Chaos] Invalid count parameter for scale_up event")
			return
		}
		if count <= 0 {
			return
		}
		for range count {
			id := fmt.Sprintf("worker-%d", nextWorkerIndex(goroutineRegistry))
			spawnAllInOneWorker(ctx, id)
		}
		if metricsCollector != nil {
			metricsCollector.SetWorkersActive(goroutineRegistry.GetActiveCount(coordinator.WorkerGoroutine))
		}
		if checkpointMgr != nil {
			checkpointMgr.SetActiveWorkers(goroutineRegistry.GetActiveCount(coordinator.WorkerGoroutine))
		}

	case coordinator.ScaleUpEvent:
		// Add workers
		count, ok := params["count"].(int)
		if !ok {
			log.Println("[Chaos] Invalid count parameter for scale_up event")
			return
		}
		handleScaleUpEvent(ctx, count, processMgr, metricsCollector)

	case coordinator.ScaleDownEvent:
		// Remove workers
		count, ok := params["count"].(int)
		if !ok {
			log.Println("[Chaos] Invalid count parameter for scale_down event")
			return
		}
		handleScaleDownEvent(count, processMgr, metricsCollector)

	case coordinator.LeaderFailureEvent:
		if coord != nil {
			coord.StartRecovery("leader_failure")
		}
		// Kill leader worker (just kill first running worker for now)
		handleLeaderFailure(processMgr, metricsCollector)

	case coordinator.NetworkDisconnectEvent:
		// Simulate network disconnect
		duration, ok := params["duration"].(time.Duration)
		if !ok {
			log.Println("[Chaos] Invalid duration parameter for network_disconnect event")
			return
		}
		if coord != nil {
			coord.StartRecovery("network_disconnect")
		}
		handleNetworkDisconnect(duration, processMgr, metricsCollector)

	case coordinator.NetworkDisconnectLeaderEvent:
		// Process-mode has no real leader lookup (handleLeaderFailure
		// just kills the first running worker). Rather than approximate,
		// skip — the event is all-in-one only.
		log.Println("[Chaos] process mode does not support leader-disconnect; skipping")

	case coordinator.NetworkDisconnectLongEvent:
		// Process-mode has no in-process Worker.Disconnect() surface.
		// Skip — all-in-one only.
		log.Println("[Chaos] process mode does not support network_disconnect_long; skipping")

	case coordinator.WorkerPauseEvent:
		// Simulate worker pause
		duration, ok := params["duration"].(time.Duration)
		if !ok {
			log.Println("[Chaos] Invalid duration parameter for worker_pause event")
			return
		}
		handleWorkerPause(duration, processMgr)

	case coordinator.SlowConsumerEvent:
		// SlowConsumer only works in all-in-one (goroutine) mode - skip in process mode
		log.Println("[Chaos] slow_consumer event only supported in all-in-one mode")

	case coordinator.BucketDeleteEvent, coordinator.BucketRecreateEvent, coordinator.BucketPeerTakeoverEvent,
		coordinator.WatcherStallEvent, coordinator.StableIDClaimStealEvent, coordinator.StableIDTinyPoolRespawnEvent,
		coordinator.ProducerDisconnectEvent, coordinator.AssignmentCASStormEvent, coordinator.AssignmentBucketDeleteEvent,
		coordinator.HandoffOrphanClaimWriteEvent:
		// Process-mode dispatch is intentionally a log-and-skip per the
		// plan's risk note (lines 386-388). Whole-bucket actions in
		// process mode would need cross-process visibility into the
		// worker or manager JetStream contexts which the simulation does
		// not currently provide. Phase 5 events additionally require
		// in-process producer NetworkControl or aioNS handles unavailable
		// in process mode. Phase 4c plumbing exists for the
		// tiny-pool-respawn case so Phase 7b can wire it in process mode.
		log.Printf("[Chaos] %s event only supported in all-in-one mode", event)

	default:
		log.Printf("[Chaos] Unknown event type: %s", event)
	}
}

// handleGoroutineChaos handles chaos events for goroutine-level (all-in-one mode).
func handleGoroutineChaos( //nolint:cyclop,gocyclo,funlen,revive // dispatch grows with each new chaos kind
	ctx context.Context,
	event coordinator.ChaosEvent,
	params map[string]any,
	registry *coordinator.GoroutineRegistry,
	metricsCollector *metrics.Collector,
	checkpointMgr *coordinator.CheckpointManager,
) {
	// Guard: skip worker-related events when no active workers
	activeWorkers := registry.GetActiveCount(coordinator.WorkerGoroutine)
	switch event {
	case coordinator.WorkerCrashEvent, coordinator.WorkerRestartEvent, coordinator.LeaderFailureEvent, coordinator.ScaleDownEvent, coordinator.NetworkDisconnectEvent, coordinator.NetworkDisconnectLeaderEvent, coordinator.NetworkDisconnectLongEvent, coordinator.WorkerPauseEvent, coordinator.SlowConsumerEvent, coordinator.BucketPeerTakeoverEvent, coordinator.StableIDClaimStealEvent:
		if activeWorkers == 0 {
			log.Printf("[Chaos] Skipping %s: no active workers", event)
			return
		}
	case coordinator.ScaleUpEvent, coordinator.ProducerCrashEvent, coordinator.BucketDeleteEvent, coordinator.BucketRecreateEvent, coordinator.WatcherStallEvent, coordinator.StableIDTinyPoolRespawnEvent, coordinator.ProducerDisconnectEvent, coordinator.AssignmentCASStormEvent, coordinator.AssignmentBucketDeleteEvent, coordinator.HandoffOrphanClaimWriteEvent:
		_ = 0 // Dummy op to make branch different
	default:
		// Fallback for any other events
	}

	switch event {
	case coordinator.WorkerCrashEvent:
		// Phase 4: if the scenario passes an explicit target_worker AND
		// no_restart=true, route to the no-restart variant so the
		// stableid_tiny_pool_respawn scheduled later can take over the
		// dead worker's stable-ID via the stale-takeover Update path.
		targetWorker, _ := params["target_worker"].(string)
		noRestart, _ := params["no_restart"].(bool)
		if targetWorker != "" && noRestart {
			handleTargetedWorkerCrash(registry, targetWorker, metricsCollector, checkpointMgr)
			return
		}
		handleWorkerGoroutineCrash(ctx, registry, metricsCollector, checkpointMgr)

	case coordinator.LeaderFailureEvent:
		handleLeaderGoroutineFailure(ctx, registry, metricsCollector, checkpointMgr)

	case coordinator.WorkerRestartEvent:
		handleWorkerGoroutineRestart(ctx, registry, metricsCollector, checkpointMgr)

	case coordinator.ProducerCrashEvent:
		handleProducerGoroutineCrash(registry)

	case coordinator.ScaleDownEvent:
		count, ok := params["count"].(int)
		if !ok {
			log.Println("[Chaos] Invalid count parameter for scale_down event")
			return
		}
		if count <= 0 {
			return
		}
		current := registry.GetActiveCount(coordinator.WorkerGoroutine)
		minWorkers := max(aioMinWorkers, 0)
		maxRemovable := current - minWorkers
		if maxRemovable <= 0 {
			log.Printf("[Chaos] Skipping scale_down: active=%d min_workers=%d", current, minWorkers)
			return
		}
		if count > maxRemovable {
			log.Printf("[Chaos] Clamping scale_down from %d to %d to respect min_workers=%d", count, maxRemovable, minWorkers)
			count = maxRemovable
		}
		handleWorkerGoroutineScaleDown(count, registry, metricsCollector, checkpointMgr)

	case coordinator.ScaleUpEvent:
		// Add new worker goroutines dynamically
		count, ok := params["count"].(int)
		if !ok || count <= 0 {
			count = 1
		}
		if aioMaxWorkers > 0 {
			current := registry.GetActiveCount(coordinator.WorkerGoroutine)
			available := aioMaxWorkers - current
			if available <= 0 {
				log.Printf("[Chaos] Skipping scale_up: active=%d max_workers=%d", current, aioMaxWorkers)
				return
			}
			if count > available {
				log.Printf("[Chaos] Clamping scale_up from %d to %d to respect max_workers=%d", count, available, aioMaxWorkers)
				count = available
			}
		}
		spawned := 0
		for i := 0; i < count; i++ {
			id := fmt.Sprintf("worker-%d", nextWorkerIndex(registry))
			if spawnAllInOneWorker(ctx, id) {
				spawned++
			}
		}
		if spawned > 0 {
			if metricsCollector != nil {
				metricsCollector.SetWorkersActive(registry.GetActiveCount(coordinator.WorkerGoroutine))
			}
			if checkpointMgr != nil {
				checkpointMgr.SetActiveWorkers(registry.GetActiveCount(coordinator.WorkerGoroutine))
			}
			log.Printf("[Chaos] Scaled up by %d workers (active=%d)", spawned, registry.GetActiveCount(coordinator.WorkerGoroutine))
		} else {
			log.Println("[Chaos] Scale up requested but no workers spawned")
		}

	case coordinator.WorkerPauseEvent:
		// Pause random worker without removing it to build backlog and trigger catch-up.
		workers := registry.GetByType(coordinator.WorkerGoroutine)
		if len(workers) == 0 {
			log.Println("[Chaos] No active workers to pause")
			return
		}
		target := workers[time.Now().UnixNano()%int64(len(workers))]
		dur, ok := params["duration"].(time.Duration)
		if !ok || dur <= 0 {
			dur = 5 * time.Second
		}
		if wobj, ok := target.Obj.(*worker.Worker); ok {
			wobj.Pause(dur)
			log.Printf("[Chaos] Paused worker %s for %v (until %v)", target.ID, dur, wobj.PausedUntil())
		} else {
			log.Printf("[Chaos] Worker %s missing underlying object; cannot pause", target.ID)
		}
	case coordinator.NetworkDisconnectEvent:
		workers := registry.GetByType(coordinator.WorkerGoroutine)
		if len(workers) == 0 {
			log.Println("[Chaos] No active workers to disconnect")
			return
		}
		target := workers[time.Now().UnixNano()%int64(len(workers))]
		dur, ok := params["duration"].(time.Duration)
		if !ok || dur <= 0 {
			dur = 10 * time.Second
		}

		if wobj, ok := target.Obj.(*worker.Worker); ok {
			log.Printf("[Chaos] Disconnecting worker %s for %v", target.ID, dur)
			wobj.Disconnect()

			time.AfterFunc(dur, func() {
				log.Printf("[Chaos] Reconnecting worker %s", target.ID)
				wobj.Reconnect()
			})
		} else {
			log.Printf("[Chaos] Worker %s missing underlying object; cannot disconnect", target.ID)
		}

	case coordinator.NetworkDisconnectLeaderEvent:
		handleLeaderGoroutineNetworkDisconnect(registry, params)

	case coordinator.NetworkDisconnectLongEvent:
		handleLongGoroutineNetworkDisconnect(registry, params)

	case coordinator.SlowConsumerEvent:
		// Slow down a random worker's processing
		workers := registry.GetByType(coordinator.WorkerGoroutine)
		if len(workers) == 0 {
			log.Println("[Chaos] No active workers for slow_consumer")
			return
		}
		target := workers[time.Now().UnixNano()%int64(len(workers))]
		multiplier, ok := params["multiplier"].(int)
		if !ok || multiplier <= 0 {
			multiplier = 10 // default 10x slowdown
		}
		dur, ok := params["duration"].(time.Duration)
		if !ok || dur <= 0 {
			dur = 15 * time.Second
		}
		// Type assert to slowable interface
		type slowable interface {
			SetProcessingMultiplier(int)
		}
		if wobj, ok := target.Obj.(slowable); ok {
			log.Printf("[Chaos] Slowing consumer %s: %dx for %v", target.ID, multiplier, dur)
			wobj.SetProcessingMultiplier(multiplier)
			time.AfterFunc(dur, func() {
				log.Printf("[Chaos] Ending slow consumer on %s", target.ID)
				wobj.SetProcessingMultiplier(1)
			})
		} else {
			log.Printf("[Chaos] Worker %s missing underlying object; cannot slow", target.ID)
		}

	case coordinator.BucketDeleteEvent:
		bucket, _ := params["target_bucket"].(string)
		if bucket == "" {
			bucket = parti.DefaultConfig().KVBuckets.StableIDBucket
		}
		handleBucketDelete(ctx, bucket)

	case coordinator.BucketRecreateEvent:
		bucket, _ := params["target_bucket"].(string)
		if bucket == "" {
			bucket = parti.DefaultConfig().KVBuckets.AssignmentBucket
		}
		handleBucketRecreate(ctx, bucket)

	case coordinator.BucketPeerTakeoverEvent, coordinator.StableIDClaimStealEvent:
		// StableIDClaimStealEvent is the Phase 4 plan-name alias for
		// BucketPeerTakeoverEvent — both fire the same revision-bumping
		// kv.Put against the victim's stable-ID claim key.
		target, _ := params["target_worker"].(string)
		handleBucketPeerTakeover(ctx, registry, target)

	case coordinator.StableIDTinyPoolRespawnEvent:
		// Phase 4: spawn a fresh all-in-one worker into the dead worker's
		// tiny stable-ID pool. The handler looks up the role's override
		// from aioCfg.Workers.PerWorker (keyed by target_role, which is
		// the dead worker's sim ID).
		role, _ := params["target_role"].(string)
		handleStableIDTinyPoolRespawn(ctx, registry, role)

	case coordinator.WatcherStallEvent:
		prefix, _ := params["subject_prefix"].(string)
		if prefix == "" {
			prefix = "$KV.parti-sim-source.>"
		}
		dur, ok := params["duration"].(time.Duration)
		if !ok || dur <= 0 {
			dur = 45 * time.Second
		}
		handleWatcherStall(ctx, prefix, dur)

	case coordinator.ProducerDisconnectEvent:
		// Gap 10a: sever the producer's dedicated NATS connection.
		// Use durationFromParams to handle both time.Duration (from
		// generateEventParams) and string (from YAML scheduled_events).
		dur := durationFromParams(params, "duration", 15*time.Second)
		handleProducerDisconnect(registry, dur)

	case coordinator.AssignmentCASStormEvent:
		// Gap 10b: spawn a competing _commit writer using a fresh JS context.
		dur := durationFromParams(params, "duration", 10*time.Second)
		handleAssignmentCASStorm(ctx, dur)

	case coordinator.AssignmentBucketDeleteEvent:
		// Gap 10b alias: delete parti-assignment via the Phase 1 primitive
		// so DegradedReasonOracle wiring is applied automatically.
		bucket, _ := params["target_bucket"].(string)
		if bucket == "" {
			bucket = "parti-assignment"
		}
		handleBucketDelete(ctx, bucket)

	case coordinator.HandoffOrphanClaimWriteEvent:
		// Phase 6 / Gap 5: one-shot. Write a synthetic stable claim with
		// an orphan owner into parti-handoff and watch it for drift over
		// at least 2 × SweepInterval. INV2: the sweeper at
		// twophase.go:434-488 must skip ClaimStateStable claims.
		obsWindow := durationFromParams(params, "observation_window", 60*time.Second)
		handleHandoffOrphanClaimWrite(ctx, obsWindow)

	default:
		log.Printf("[Chaos] Unknown event type: %s", event)
	}
}

// handleWorkerGoroutineRestart cancels a random worker then restarts it via the registry Start callback.
func handleWorkerGoroutineRestart(
	ctx context.Context,
	registry *coordinator.GoroutineRegistry,
	metricsCollector *metrics.Collector,
	checkpointMgr *coordinator.CheckpointManager,
) {
	workers := registry.GetByType(coordinator.WorkerGoroutine)
	if len(workers) == 0 {
		log.Println("[Chaos] No active worker goroutines to restart")
		return
	}

	target := workers[time.Now().UnixNano()%int64(len(workers))]
	log.Printf("[Chaos] Restarting worker goroutine: %s", target.ID)

	// Crash
	target.Cancel()
	registry.MarkInactive(target.ID)

	// Restart
	registry.Restart(ctx, target.ID)

	// Update metrics/checkpoints
	if metricsCollector != nil {
		newCount := registry.GetActiveCount(coordinator.WorkerGoroutine)
		metricsCollector.SetWorkersActive(newCount)
		log.Printf("[Chaos] Updated worker count after restart: %d", newCount)
	}
	if checkpointMgr != nil {
		newCount := registry.GetActiveCount(coordinator.WorkerGoroutine)
		checkpointMgr.SetActiveWorkers(newCount)
	}
}

// handleWorkerGoroutineCrash cancels a random worker goroutine and restarts it
// after a brief delay, simulating supervisor/k8s restart behavior.
// Respects min_workers guard to prevent the fleet from shrinking too far.
func handleWorkerGoroutineCrash(
	ctx context.Context,
	registry *coordinator.GoroutineRegistry,
	metricsCollector *metrics.Collector,
	checkpointMgr *coordinator.CheckpointManager,
) {
	workers := registry.GetByType(coordinator.WorkerGoroutine)
	if len(workers) == 0 {
		log.Println("[Chaos] No active worker goroutines to crash")
		return
	}

	// Respect min_workers guard (same as scale_down)
	minWorkers := max(aioMinWorkers, 0)
	if len(workers) <= minWorkers {
		log.Printf("[Chaos] Skipping worker_crash: active=%d <= min_workers=%d", len(workers), minWorkers)
		return
	}

	// Select random worker
	target := workers[time.Now().UnixNano()%int64(len(workers))]

	log.Printf("[Chaos] Crashing worker goroutine: %s", target.ID)

	// Cancel the worker's context (ungraceful crash)
	target.Cancel()
	registry.MarkInactive(target.ID)

	// Auto-restart after a brief delay to simulate supervisor restart.
	// In production, crashed workers are restarted by k8s/systemd/etc.
	registry.Restart(ctx, target.ID)

	// Update metrics
	if metricsCollector != nil {
		newCount := registry.GetActiveCount(coordinator.WorkerGoroutine)
		metricsCollector.SetWorkersActive(newCount)
		log.Printf("[Chaos] Updated worker count after crash/restart: %d", newCount)
	}

	// Update checkpoint manager
	if checkpointMgr != nil {
		newCount := registry.GetActiveCount(coordinator.WorkerGoroutine)
		checkpointMgr.SetActiveWorkers(newCount)
	}
}

// handleProducerGoroutineCrash cancels a random producer goroutine.
// Keeps at least 1 producer alive to prevent total message production halt.
func handleProducerGoroutineCrash(registry *coordinator.GoroutineRegistry) {
	producers := registry.GetByType(coordinator.ProducerGoroutine)
	if len(producers) == 0 {
		log.Println("[Chaos] No active producer goroutines to crash")
		return
	}

	// Keep at least 1 producer alive to avoid permanent message production halt.
	// In production, producers are independent services and rarely all crash simultaneously.
	if len(producers) <= 1 {
		log.Printf("[Chaos] Skipping producer_crash: only %d producer(s) remaining", len(producers))
		return
	}

	// Select random producer
	target := producers[time.Now().UnixNano()%int64(len(producers))]

	log.Printf("[Chaos] Crashing producer goroutine: %s", target.ID)

	// Cancel the producer's context
	target.Cancel()
	registry.MarkInactive(target.ID)

	newCount := registry.GetActiveCount(coordinator.ProducerGoroutine)
	log.Printf("[Chaos] Updated producer count after crash: %d", newCount)
}

// applyWorkerOverrides applies cluster-wide WorkerIDTTL and per-worker
// StableID overrides from the scenario config to wcfg, with per-worker
// values winning over cluster-wide values winning over defaults. Each
// override is set ONLY when non-zero so worker.NewWorker's override-if-set
// semantics restore the sim default (e.g. "simulation-worker" / Max=999)
// for unset fields.
func applyWorkerOverrides(wcfg *worker.Config, scfg *config.Config, workerID string) {
	if scfg == nil {
		return
	}
	// Cluster-wide TTL (applies to ALL workers — required for MaxAge-expiry
	// scenarios where every manager must agree on TTL).
	if scfg.Workers.WorkerIDTTL > 0 {
		wcfg.WorkerIDTTL = scfg.Workers.WorkerIDTTL
	}
	// Per-worker overrides take precedence over cluster-wide.
	if scfg.Workers.PerWorker != nil {
		if pw, ok := scfg.Workers.PerWorker[workerID]; ok {
			if pw.WorkerIDPrefix != "" {
				wcfg.WorkerIDPrefix = pw.WorkerIDPrefix
			}
			if pw.WorkerIDMin != 0 {
				wcfg.WorkerIDMin = pw.WorkerIDMin
			}
			if pw.WorkerIDMax != 0 {
				wcfg.WorkerIDMax = pw.WorkerIDMax
			}
			if pw.WorkerIDTTL != 0 {
				wcfg.WorkerIDTTL = pw.WorkerIDTTL
			}
		}
	}
}

// spawnAllInOneWorker creates, registers, and starts a new worker goroutine with the given ID.
func spawnAllInOneWorker(parent context.Context, workerID string) bool {
	if aioNS == nil || aioJS == nil || aioCfg == nil || aioCoord == nil || aioRegistry == nil || len(aioWeights) == 0 {
		log.Printf("[ScaleUp] prerequisites not ready; cannot spawn %s", workerID)
		return false
	}

	// Create network control for this worker
	netCtrl := worker.NewNetworkControl()

	// Helper to create connection
	createNC := func() (*nats.Conn, error) {
		opts := []nats.Option{
			nats.SetCustomDialer(netCtrl),
			nats.ReconnectWait(100 * time.Millisecond),
			nats.MaxReconnects(-1),
		}
		if aioCfg.NATS.Mode == "embedded" {
			return nats.Connect(aioEmbeddedServer.ClientURL(), opts...)
		}

		return nats.Connect(aioCfg.NATS.URL, opts...)
	}

	workerNC, err := createNC()
	if err != nil {
		log.Printf("[ScaleUp] failed to create NATS connection for worker %s: %v", workerID, err)
		return false
	}

	wcfg := worker.Config{
		ID:                          workerID,
		NC:                          workerNC,
		JS:                          aioJS,
		PartitionCount:              aioCfg.Partitions.Count,
		PartitionWeights:            aioWeights,
		AssignmentStrategy:          aioCfg.Workers.AssignmentStrategy,
		ProcessingDelayMin:          aioCfg.Workers.ProcessingDelay.Min,
		ProcessingDelayMax:          aioCfg.Workers.ProcessingDelay.Max,
		CoordinatorReportCh:         aioCoord.GetReceivedChannel(),
		AssignmentReportCh:          aioCoord.GetAssignmentsChannel(),
		DegradedReportCh:            aioCoord.GetDegradedReportsChannel(),
		StartLatencyCh:              aioCoord.GetStartLatencyChannel(),
		MetricsCollector:            aioMetrics,
		ConsumerBatchSize:           aioCfg.Workers.ConsumerBatchSize,
		HandlerConcurrency:          aioCfg.Workers.HandlerConcurrency,
		AckWait:                     aioCfg.Workers.AckWait,
		MaxSubjects:                 aioCfg.Workers.MaxSubjects,
		EnforceExclusiveConsumption: aioCfg.Workers.EnforceExclusiveConsumption,
		GateAllowedStates:           aioCfg.Workers.ProcessingGate.AllowedStates,
		GateWarmupDuration:          aioCfg.Workers.ProcessingGate.WarmupDuration,
		GateWarmupAllowedStates:     aioCfg.Workers.ProcessingGate.WarmupAllowedStates,
		GateNakDelay:                aioCfg.Workers.ProcessingGate.NakDelay,
		GateNakJitter:               aioCfg.Workers.ProcessingGate.NakJitter,
		GateDebug:                   aioCfg.Workers.ProcessingGate.Debug,
		IsInitialCohort:             false, // scale_up / restart is a takeover-cohort worker
		PartitionSource:             aioCfg.Workers.PartitionSource,
		SourceBucket:                aioCfg.Workers.SourceBucket,
		SourceReconcileInterval:     aioCfg.Workers.SourceReconcileInterval,
		RevocationReportCh:          aioCoord.GetRevocationReportsChannel(),
	}
	applyWorkerOverrides(&wcfg, aioCfg, workerID)
	w, err := worker.NewWorker(wcfg)
	if err != nil {
		log.Printf("[ScaleUp] failed to create worker %s: %v", workerID, err)
		return false
	}
	w.SetNetworkControl(netCtrl)

	wctx, wcancel := context.WithCancel(parent)
	var startFn func(context.Context)
	startFn = func(p context.Context) {
		pc, pcancel := context.WithCancel(p)

		// Create NEW connection for restarted worker
		newNC, err := createNC()
		if err != nil {
			log.Printf("[ScaleUp] failed to create NATS connection for restarted worker %s: %v", workerID, err)
			pcancel()
			return
		}

		newCfg := wcfg
		newCfg.NC = newNC

		nw, err := worker.NewWorker(newCfg)
		if err != nil {
			log.Printf("[ScaleUp] failed to recreate worker %s: %v", workerID, err)
			pcancel()
			return
		}
		nw.SetNetworkControl(netCtrl)

		aioRegistry.Register(workerID, coordinator.WorkerGoroutine, pcancel, startFn, nw)
		go func(ww *worker.Worker, id string, c context.Context) {
			if err := ww.Start(c); err != nil {
				log.Printf("[ScaleUp] start error for %s: %v", id, err)
				aioRegistry.MarkInactive(id)
				pcancel()
				return
			}
			<-c.Done()
			ww.Stop()
			aioRegistry.MarkInactive(id)
		}(nw, workerID, pc)
	}
	aioRegistry.Register(workerID, coordinator.WorkerGoroutine, wcancel, startFn, w)
	go func(ww *worker.Worker, id string, c context.Context) {
		if err := ww.Start(c); err != nil {
			log.Printf("[ScaleUp] start error for %s: %v", id, err)
			aioRegistry.MarkInactive(id)
			return
		}
		<-c.Done()
		ww.Stop()
		aioRegistry.MarkInactive(id)
	}(w, workerID, wctx)

	return true
}

// nextWorkerIndex scans registered goroutines for worker-* IDs and returns max+1.
func nextWorkerIndex(registry *coordinator.GoroutineRegistry) int {
	maxIdx := -1
	for _, g := range registry.GetAll() {
		if g.Type != coordinator.WorkerGoroutine {
			continue
		}
		if strings.HasPrefix(g.ID, "worker-") {
			var n int
			if _, err := fmt.Sscanf(g.ID, "worker-%d", &n); err == nil {
				if n > maxIdx {
					maxIdx = n
				}
			}
		}
	}

	return maxIdx + 1
}

// handleWorkerGoroutineScaleDown cancels multiple worker goroutines.
func handleWorkerGoroutineScaleDown(
	count int,
	registry *coordinator.GoroutineRegistry,
	metricsCollector *metrics.Collector,
	checkpointMgr *coordinator.CheckpointManager,
) {
	workers := registry.GetByType(coordinator.WorkerGoroutine)
	if len(workers) == 0 {
		log.Println("[Chaos] No active worker goroutines to scale down")
		return
	}

	// Don't scale down more than available
	if count > len(workers) {
		count = len(workers)
	}

	log.Printf("[Chaos] Scaling down: stopping %d worker goroutines", count)

	for i := 0; i < count; i++ {
		target := workers[i]
		log.Printf("[Chaos] Stopping worker goroutine: %s", target.ID)
		target.Cancel()
		registry.MarkInactive(target.ID)
		// Notify coordinator so its audit does not keep attributing partitions
		// to this permanently stopped worker. Skipped for crash/restart paths
		// that re-spawn the same ID because a fresh OnAssignmentChanged will
		// re-populate the snapshot on restart.
		if aioCoord != nil {
			aioCoord.NotifyWorkerStopped(target.ID)
		}
	}

	// Update metrics
	if metricsCollector != nil {
		newCount := registry.GetActiveCount(coordinator.WorkerGoroutine)
		metricsCollector.SetWorkersActive(newCount)
		log.Printf("[Chaos] Updated worker count after scale down: %d", newCount)
	}

	// Update checkpoint manager
	if checkpointMgr != nil {
		newCount := registry.GetActiveCount(coordinator.WorkerGoroutine)
		checkpointMgr.SetActiveWorkers(newCount)
	}
}

// handleCrashEvent kills a random worker or producer process.
func handleCrashEvent(event coordinator.ChaosEvent, processMgr *coordinator.ProcessManager, metricsCollector *metrics.Collector) {
	var processType coordinator.ProcessType
	if event == coordinator.WorkerCrashEvent {
		processType = coordinator.WorkerProcess
	} else {
		processType = coordinator.ProducerProcess
	}

	// Get all running processes of the specified type
	allProcesses := processMgr.ListProcesses()
	var targets []*coordinator.ProcessInfo
	for _, info := range allProcesses {
		if info.Type == processType && info.Status == coordinator.StatusRunning {
			targets = append(targets, info)
		}
	}

	if len(targets) == 0 {
		log.Printf("[Chaos] No running %s processes to crash", processType)
		return
	}

	// Select random target
	target := targets[time.Now().UnixNano()%int64(len(targets))]

	log.Printf("[Chaos] Crashing %s: %s", processType, target.ID)
	if err := processMgr.KillProcess(target.ID); err != nil {
		log.Printf("[Chaos] Failed to crash %s: %v", target.ID, err)
		return
	}

	// Update worker count metric
	if processType == coordinator.WorkerProcess && metricsCollector != nil {
		workerCount := processMgr.GetWorkerCount()
		metricsCollector.SetWorkersActive(workerCount)
		log.Printf("Updated worker count after crash: %d", workerCount)
	}
}

// handleScaleUpEvent adds new worker processes.
func handleScaleUpEvent(
	ctx context.Context,
	count int,
	processMgr *coordinator.ProcessManager,
	metricsCollector *metrics.Collector,
) {
	log.Printf("[Chaos] Scaling up: adding %d workers", count)

	currentCount := processMgr.GetWorkerCount()

	for i := range count {
		workerID := fmt.Sprintf("worker-%d", currentCount+i)
		log.Printf("Try to start worker %s", workerID)
		if err := processMgr.StartWorker(ctx, workerID); err != nil {
			log.Printf("[Chaos] Failed to start worker %s: %v", workerID, err)
		}
	}

	// Update worker count metric
	if metricsCollector != nil {
		workerCount := processMgr.GetWorkerCount()
		metricsCollector.SetWorkersActive(workerCount)
		log.Printf("Updated worker count after scale up: %d", workerCount)
	}
}

// handleScaleDownEvent removes worker processes.
func handleScaleDownEvent(count int, processMgr *coordinator.ProcessManager, metricsCollector *metrics.Collector) {
	log.Printf("[Chaos] Scaling down: removing %d workers", count)

	// Get all running workers
	allProcesses := processMgr.ListProcesses()
	var workers []*coordinator.ProcessInfo
	for _, info := range allProcesses {
		if info.Type == coordinator.WorkerProcess && info.Status == coordinator.StatusRunning {
			workers = append(workers, info)
		}
	}

	if len(workers) == 0 {
		log.Println("[Chaos] No running workers to scale down")
		return
	}

	// Stop up to 'count' workers
	stopCount := min(count, len(workers))

	for i := range stopCount {
		if err := processMgr.StopProcess(workers[i].ID, 10*time.Second); err != nil {
			log.Printf("[Chaos] Failed to stop worker %s: %v", workers[i].ID, err)
		}
	}

	// Update worker count metric
	if metricsCollector != nil {
		workerCount := processMgr.GetWorkerCount()
		metricsCollector.SetWorkersActive(workerCount)
		log.Printf("Updated worker count after scale down: %d", workerCount)
	}
}

// handleLeaderFailure kills the current leader worker (first running worker).
func handleLeaderFailure(processMgr *coordinator.ProcessManager, metricsCollector *metrics.Collector) {
	log.Println("[Chaos] Simulating leader failure")

	// Get first running worker (assume it's the leader)
	allProcesses := processMgr.ListProcesses()
	for _, info := range allProcesses {
		if info.Type == coordinator.WorkerProcess && info.Status == coordinator.StatusRunning {
			log.Printf("[Chaos] Killing leader worker: %s", info.ID)
			if err := processMgr.KillProcess(info.ID); err != nil {
				log.Printf("[Chaos] Failed to kill leader %s: %v", info.ID, err)
				return
			}

			// Update worker count metric
			if metricsCollector != nil {
				workerCount := processMgr.GetWorkerCount()
				metricsCollector.SetWorkersActive(workerCount)
				log.Printf("Updated worker count after leader failure: %d", workerCount)
			}

			return
		}
	}

	log.Println("[Chaos] No running workers found for leader failure")
}

// handleNetworkDisconnect simulates network disconnection by stopping and restarting a worker.
func handleNetworkDisconnect(duration time.Duration, processMgr *coordinator.ProcessManager, metricsCollector *metrics.Collector) {
	log.Printf("[Chaos] Simulating network disconnect for %v", duration)

	targetID := processMgr.SelectRandomWorker()
	if targetID == "" {
		log.Println("[Chaos] No running workers for network disconnect")
		return
	}

	log.Printf("[Chaos] Disconnecting worker: %s for %v (SIGSTOP)", targetID, duration)

	// Use SIGSTOP to simulate network isolation (process hangs, heartbeats fail)
	if err := processMgr.SignalProcess(targetID, syscall.SIGSTOP); err != nil {
		log.Printf("[Chaos] Failed to disconnect worker %s: %v", targetID, err)
		return
	}

	// Update worker count metric to reflect the loss of an active worker
	if metricsCollector != nil {
		count := processMgr.GetWorkerCount() - 1
		metricsCollector.SetWorkersActive(count)
		log.Printf("Updated worker count after network disconnect: %d", count)
	}

	time.AfterFunc(duration, func() {
		log.Printf("[Chaos] Reconnecting worker %s (SIGCONT)", targetID)
		if err := processMgr.SignalProcess(targetID, syscall.SIGCONT); err != nil {
			log.Printf("[Chaos] Failed to reconnect worker %s: %v", targetID, err)
		}
		// Restore metric
		if metricsCollector != nil {
			count := processMgr.GetWorkerCount()
			metricsCollector.SetWorkersActive(count)
		}
	})
}

// handleWorkerPause pauses a random worker process using SIGSTOP/SIGCONT.
func handleWorkerPause(duration time.Duration, processMgr *coordinator.ProcessManager) {
	log.Printf("[Chaos] Simulating worker pause for %v", duration)

	targetID := processMgr.SelectRandomWorker()
	if targetID == "" {
		log.Println("[Chaos] No running workers to pause")
		return
	}

	log.Printf("[Chaos] Pausing worker %s (SIGSTOP)", targetID)
	if err := processMgr.SignalProcess(targetID, syscall.SIGSTOP); err != nil {
		log.Printf("[Chaos] Failed to pause worker %s: %v", targetID, err)
		return
	}

	time.AfterFunc(duration, func() {
		log.Printf("[Chaos] Resuming worker %s (SIGCONT)", targetID)
		if err := processMgr.SignalProcess(targetID, syscall.SIGCONT); err != nil {
			log.Printf("[Chaos] Failed to resume worker %s: %v", targetID, err)
		}
	})
}

// handleLeaderGoroutineNetworkDisconnect implements the "Split Brain"
// chaos variant: pick the current leader and isolate it from NATS for
// the configured duration. Mirrors handleLeaderGoroutineFailure's
// leader-selection pattern but uses Disconnect() instead of Cancel().
func handleLeaderGoroutineNetworkDisconnect(
	registry *coordinator.GoroutineRegistry,
	params map[string]any,
) {
	workers := registry.GetByType(coordinator.WorkerGoroutine)
	if len(workers) == 0 {
		log.Println("[Chaos] No active workers to disconnect (leader variant)")
		return
	}
	var leader *coordinator.GoroutineInfo
	for _, w := range workers {
		if wobj, ok := w.Obj.(*worker.Worker); ok && wobj.IsLeader() {
			leader = w
			break
		}
	}
	if leader == nil {
		log.Println("[Chaos] No leader worker found to disconnect (network_disconnect_leader)")
		return
	}
	dur, ok := params["duration"].(time.Duration)
	if !ok || dur <= 0 {
		dur = 10 * time.Second
	}
	wobj, ok := leader.Obj.(*worker.Worker)
	if !ok {
		log.Printf("[Chaos] Leader worker %s missing underlying object; cannot disconnect", leader.ID)
		return
	}
	log.Printf("[Chaos] Disconnecting LEADER worker %s for %v", leader.ID, dur)
	wobj.Disconnect()
	time.AfterFunc(dur, func() {
		log.Printf("[Chaos] Reconnecting LEADER worker %s", leader.ID)
		wobj.Reconnect()
	})
}

// handleLongGoroutineNetworkDisconnect implements the long-disconnect chaos
// variant (Gap 12): pick a random worker, register a reassignment expectation,
// suppress its slow-start assertion, disconnect it for 60–180s, then reconnect.
//
// Sequence (must be respected — advisor note, point 5):
//  1. Register slow-start suppression BEFORE disconnect (coord.SuspendSlowStartAssertions).
//  2. Register reassignment expectation BEFORE disconnect
//     (coord.RegisterLongDisconnectExpectation) so the partition snapshot is
//     captured before any reassignment has started.
//  3. Call Worker.Disconnect().
//  4. After dur elapses, call Worker.Reconnect().
func handleLongGoroutineNetworkDisconnect(
	registry *coordinator.GoroutineRegistry,
	params map[string]any,
) {
	workers := registry.GetByType(coordinator.WorkerGoroutine)
	if len(workers) == 0 {
		log.Println("[Chaos] No active workers to long-disconnect")
		return
	}
	// Phase 4b: allow scenario to pin an explicit target_worker (e.g.
	// "worker-0") so the disconnect lands on the worker with the tiny-
	// pool stable-ID. Empty / "random" falls back to random selection.
	var target *coordinator.GoroutineInfo
	if explicit, _ := params["target_worker"].(string); explicit != "" && explicit != "random" {
		for i := range workers {
			if workers[i].ID == explicit {
				target = workers[i]
				break
			}
		}
		if target == nil {
			log.Printf("[Chaos] long_disconnect: target_worker=%s not found; falling back to random", explicit)
		}
	}
	if target == nil {
		target = workers[time.Now().UnixNano()%int64(len(workers))]
	}
	dur, ok := params["duration"].(time.Duration)
	if !ok || dur <= 0 {
		dur = 60 * time.Second
	}
	wobj, ok := target.Obj.(*worker.Worker)
	if !ok {
		log.Printf("[Chaos] Worker %s missing underlying object; cannot long-disconnect", target.ID)
		return
	}

	// T_reassign = ElectionTimeout (default 10s) + OperationTimeout (default 5s) + buffer.
	// Use dur + 30s as the reassignment deadline: the partitions must migrate
	// before the disconnect window closes (dur) plus a generous scheduling buffer.
	tReassign := dur + 30*time.Second

	// 1. Suppress slow-start assertion for this worker (per-worker scope).
	// Window = dur + recovery headroom.
	suppressWindow := dur + 60*time.Second
	if aioCoord != nil {
		aioCoord.SuspendSlowStartAssertions(target.ID, suppressWindow)
	}

	// 2. Register reassignment expectation BEFORE disconnect so the partition
	// snapshot is taken before any rebalance.
	if aioCoord != nil {
		aioCoord.RegisterLongDisconnectExpectation(target.ID, tReassign)
	}

	// 3. Suppress conditional claim-lost for the target worker.
	// A long disconnect CAN cause the stable-ID key to expire server-side if:
	//   time_since_last_renewal + disconnect_duration > WorkerIDTTL (75s)
	// With renewal_interval = TTL/3 = 25s, a 60s disconnect has a non-zero
	// probability of triggering claim-lost when the last renewal happened
	// ≥15s before the disconnect. This is the Gap 8a mechanism deferred to
	// Phase 4b/7b; here we tolerate it silently rather than assert it.
	// SuppressClaimLostForWorker is optional (does NOT require claim-lost to
	// fire) — unlike ExpectAfter which would penalize us if claim-lost doesn't
	// fire (incrementing expectedDegradedMissing on expiry).
	// Phase 4b scenarios EXPECT claim-loss to fire (the whole point of
	// chaos_stableid_maxage_expiry); they pass expect_claim_lost=true to
	// disable the suppression AND register a positive expectation so the
	// claim-lost is counted as observed (not just tolerated).
	expectClaimLost, _ := params["expect_claim_lost"].(bool)
	if aioCoord != nil && expectClaimLost {
		if o := aioCoord.DegradedReasonOracle(); o != nil {
			stableID := wobj.StableWorkerID()
			if stableID != "" {
				// Window = disconnect + claimLostShutdown stop timeout.
				window := dur + 30*time.Second
				o.ExpectAfter("network_disconnect_long:claim_lost:"+stableID, nil, window, stableID, true)
				log.Printf("[Chaos] Expecting claim-lost for %s (stable=%s window=%v)", target.ID, stableID, window)
			}
		}
	}
	if aioCoord != nil && !expectClaimLost {
		if o := aioCoord.DegradedReasonOracle(); o != nil {
			stableID := wobj.StableWorkerID()
			if stableID != "" {
				// Window = disconnect + reconnect + claimLostShutdown stop timeout (30s).
				suppressWindow := dur + 60*time.Second
				o.SuppressClaimLostForWorker(stableID, suppressWindow)
				log.Printf("[Chaos] Suppressed claim-lost for %s (stable=%s window=%v)", target.ID, stableID, suppressWindow)
			}
		}
	}

	// 4. Disconnect.
	log.Printf("[Chaos] Long-disconnecting worker %s for %v (reassign_deadline=%v)", target.ID, dur, tReassign)
	wobj.Disconnect()

	// 5. Reconnect after dur.
	time.AfterFunc(dur, func() {
		log.Printf("[Chaos] Reconnecting worker %s after long-disconnect (%v elapsed)", target.ID, dur)
		wobj.Reconnect()
	})
}

// handleLeaderGoroutineFailure finds the leader worker, crashes it, and restarts it.
func handleLeaderGoroutineFailure(
	ctx context.Context,
	registry *coordinator.GoroutineRegistry,
	metricsCollector *metrics.Collector,
	checkpointMgr *coordinator.CheckpointManager,
) {
	workers := registry.GetByType(coordinator.WorkerGoroutine)
	if len(workers) == 0 {
		log.Println("[Chaos] No active worker goroutines to crash")
		return
	}

	var leader *coordinator.GoroutineInfo
	for _, w := range workers {
		if wobj, ok := w.Obj.(*worker.Worker); ok {
			if wobj.IsLeader() {
				leader = w
				break
			}
		}
	}

	if leader == nil {
		log.Println("[Chaos] No leader worker found to crash")
		return
	}

	log.Printf("[Chaos] Crashing LEADER worker goroutine: %s", leader.ID)

	// Cancel the worker's context
	leader.Cancel()
	registry.MarkInactive(leader.ID)

	// Restart the worker immediately (simulating a crash-loop or supervisor restart)
	// In a real scenario, there might be a delay, but for leader election testing,
	// we want to see if a NEW leader is elected while this one is down or coming back.
	log.Printf("[Chaos] Restarting LEADER worker goroutine: %s", leader.ID)
	registry.Restart(ctx, leader.ID)

	// Update metrics
	if metricsCollector != nil {
		newCount := registry.GetActiveCount(coordinator.WorkerGoroutine)
		metricsCollector.SetWorkersActive(newCount)
		log.Printf("[Chaos] Updated worker count after leader crash/restart: %d", newCount)
	}
	if checkpointMgr != nil {
		newCount := registry.GetActiveCount(coordinator.WorkerGoroutine)
		checkpointMgr.SetActiveWorkers(newCount)
	}
}
