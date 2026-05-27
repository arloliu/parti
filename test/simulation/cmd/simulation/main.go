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

	// procCfg holds the active config in process-orchestrator mode so chaos
	// handlers can resolve per-worker StableID overrides without threading
	// the config pointer through every dispatch surface.
	procCfg *config.Config
	// procNextWorkerIdx tracks the next sim-side worker ID to allocate for
	// process-mode respawns (Phase 7b stableid_tiny_pool_respawn). The
	// orchestrator initial spawn uses 0..count-1; the respawn picks count+.
	procNextWorkerIdx int
	// procLastReplacementWorkerID records the sim-side ID of the most
	// recently spawned stableid_tiny_pool_respawn replacement so the
	// worker_resume wait-for-condition handler can target its assignment
	// snapshot before SIGCONT. Empty if no replacement has spawned this
	// run. Process-mode only.
	procLastReplacementWorkerID string
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
	// Phase 7a: --mode overrides the YAML simulation.mode so the parent
	// orchestrator (mode: process) can spawn the same binary with --mode
	// worker / --mode producer without separate YAML files. --id sets the
	// WORKER_ID/PRODUCER_ID env override the spawned process uses to
	// identify itself.
	modeOverride := flag.String("mode", "", "Override simulation.mode from config (e.g. worker, producer)")
	idOverride := flag.String("id", "", "Set WORKER_ID/PRODUCER_ID for spawned worker/producer modes")
	flag.Parse()

	// Load config
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	applyCLIOverrides(cfg, workersOverride, durationOverride, stopOnFailure, modeOverride, idOverride)

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
		errCh <- dispatchMode(ctx, cfg, *configPath, effectiveCooldown)
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

// applyCLIOverrides folds CLI flag overrides into the loaded config. Extracted
// from main() so the top-level trunk stays under the cyclop threshold.
func applyCLIOverrides(cfg *config.Config, workersOverride *int, durationOverride *time.Duration, stopOnFailure *bool, modeOverride, idOverride *string) {
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
	if stopOnFailure != nil && *stopOnFailure {
		cfg.Coordinator.StopOnFailure = true
		log.Println("Overriding stop-on-failure from CLI flag: true")
	}

	// Apply --mode override (Phase 7a: parent orchestrator spawns children
	// with --mode worker / --mode producer). Validation already ran on
	// LoadConfig; mode strings beyond the allowed set fall through to the
	// dispatchMode default-case error.
	if modeOverride != nil && *modeOverride != "" {
		cfg.Simulation.Mode = *modeOverride
	}
	// --id maps to WORKER_ID / PRODUCER_ID. The downstream runWorker /
	// runProducer already prefer the env var; setting it here keeps the
	// child-spawn path uniform with the env-only path used by chaos
	// dispatchers in all-in-one mode.
	if idOverride != nil && *idOverride != "" {
		switch cfg.Simulation.Mode {
		case "worker":
			_ = os.Setenv("WORKER_ID", *idOverride)
		case "producer":
			_ = os.Setenv("PRODUCER_ID", *idOverride)
		}
	}
}

// dispatchMode routes execution to the per-mode runner. Extracted from main()
// so the top-level trunk stays under the cyclop threshold.
func dispatchMode(ctx context.Context, cfg *config.Config, configPath string, cooldown time.Duration) error {
	switch cfg.Simulation.Mode {
	case "all-in-one":
		return runAllInOne(ctx, cfg, configPath, cooldown)
	case "producer":
		return runProducer(ctx, cfg)
	case "worker":
		return runWorker(ctx, cfg)
	case "coordinator":
		return runCoordinator(ctx, cfg)
	case "process":
		return runProcessOrchestrator(ctx, cfg, configPath)
	default:
		return fmt.Errorf("unknown mode: %s", cfg.Simulation.Mode)
	}
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
	//
	// Phase 8 / Gap 9: when chaos.storage.skip_bucket_precreate is true,
	// the manager-owned coordination buckets (election, heartbeat,
	// assignment, handoff) are NOT pre-created here. The manager's own
	// Start path is then the only code that creates them, exercising its
	// storage-type and TTL choices end-to-end. The StableID bucket is
	// always pre-created because it is not manager-owned.
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

		skipPrecreate := cfg.Chaos.Storage.SkipBucketPrecreate
		if skipPrecreate {
			log.Printf("[Phase8] skip_bucket_precreate=true: manager-owned coordination buckets " +
				"(election, heartbeat, assignment, handoff) will be created by the manager's " +
				"own Start path. Only the StableID bucket is pre-created.")
		}

		// StableID bucket is always pre-created — not manager-owned.
		{
			bctx, bcancel := context.WithTimeout(ctx, 5*time.Second)
			_, kerr := kvutil.EnsureKVBucket(bctx, js, pc.KVBuckets.StableIDBucket, pc.WorkerIDTTL)
			bcancel()
			if kerr != nil {
				return fmt.Errorf("failed to ensure KV bucket %s: %w", pc.KVBuckets.StableIDBucket, kerr)
			}
		}

		if !skipPrecreate {
			// Phase 8 / Gap 9b: when election_replicas_override > 0,
			// pre-create the election bucket with the configured replica
			// count so the manager's get-first open preserves it.
			electionReplicasOverride := cfg.Chaos.Storage.ElectionReplicasOverride
			if electionReplicasOverride > 0 {
				pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
				actualReplicas, perr := precreateElectionBucketWithReplicas(pctx, js, electionReplicasOverride)
				pcancel()
				if perr != nil {
					return fmt.Errorf("failed to pre-create election bucket with replicas=%d: %w", electionReplicasOverride, perr)
				}
				// Store the accepted replica count so the post-start oracle
				// asserts against the actual value (not the requested one,
				// which may have been clamped by a single-server NATS).
				storageElectionReplicasWant.Store(int64(actualReplicas))
				log.Printf("[Phase8/INV2] election bucket pre-created with Replicas=%d; oracle will assert %d",
					electionReplicasOverride, actualReplicas)
			}

			// Use short, independent timeouts per bucket to avoid blocking startup
			timeBuckets := []struct {
				name string
				ttl  time.Duration
			}{
				{pc.KVBuckets.ElectionBucket, pc.ElectionTimeout},
				{pc.KVBuckets.HeartbeatBucket, pc.HeartbeatTTL},
				{pc.KVBuckets.AssignmentBucket, pc.KVBuckets.AssignmentTTL},
				{pc.KVBuckets.HandoffBucket, handoffTTL},
			}
			for _, b := range timeBuckets {
				// Skip election if already pre-created with specific replicas.
				if electionReplicasOverride > 0 && b.name == pc.KVBuckets.ElectionBucket {
					continue
				}
				bctx, bcancel := context.WithTimeout(ctx, 5*time.Second)
				_, kerr := kvutil.EnsureKVBucket(bctx, js, b.name, b.ttl)
				bcancel()
				if kerr != nil {
					return fmt.Errorf("failed to ensure KV bucket %s: %w", b.name, kerr)
				}
			}
		}

		// Phase 6 / Gap 5: after seeding the handoff bucket with the
		// scenario's PrecreateMaxAge, confirm the value actually stuck
		// on the backing stream before any manager starts. If the
		// inherited MaxAge is NOT what we requested, the downstream
		// oracle is meaningless — log loudly so the run can be
		// diagnosed. We use the same Status() → KeyValueBucketStatus
		// surface the manager uses at manager_setup.go:kvStreamConfig.
		if !skipPrecreate && handoffPrecreateMaxAge > 0 {
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
			RevocationReportDropFn:      coord.IncRevocationReportDropped,
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

	// Phase 8 / Gap 9: fire the storage-type oracle once any worker
	// reaches StateStable (skip_bucket_precreate mode only).
	if cfg.Chaos.Storage.SkipBucketPrecreate {
		startStorageTypeOracle(ctx, goroutineRegistry)
	}

	// Phase 8 / Gap 9b: fire the election-replicas oracle once all
	// workers reach StateStable (election_replicas_override mode only).
	if cfg.Chaos.Storage.ElectionReplicasOverride > 0 {
		startElectionReplicasOracle(ctx, goroutineRegistry, cfg.Workers.Count)
	}

	// Phase 8 / Gap 13: start the burst-after-quiet KV-op rate sampler
	// when the scenario configures a quiet baseline duration. The sampler
	// is started here (before producers/workers emit traffic) so the quiet
	// window measurement begins at the same time as the simulation.
	// The burst window samples the 30s after the scale_up event fires.
	if cfg.Simulation.BurstAfterQuiet.QuietDuration > 0 {
		multiplier := cfg.Chaos.Storage.KvOpRateCeilingMultiplier
		burstAt := cfg.Simulation.BurstAfterQuiet.BurstAt
		burstWindow := cfg.Simulation.BurstAfterQuiet.BurstWindow
		if burstAt <= 0 {
			burstAt = cfg.Simulation.BurstAfterQuiet.QuietDuration
		}
		if burstWindow <= 0 {
			burstWindow = 30 * time.Second
		}
		go burstAfterQuietSampler(
			ctx,
			embeddedServer,
			cfg.Simulation.BurstAfterQuiet.QuietDuration,
			burstAt,
			burstWindow,
			multiplier,
		)
	}

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
				ldReassignInconclusive := coord.GetLongDisconnectReassignmentInconclusive()
				if ldReassignInconclusive > 0 {
					log.Printf("[Phase3] long_disconnect_reassignment_inconclusive=%d (target owned zero partitions at expectation time; audit-only)",
						ldReassignInconclusive)
				}
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
				// Phase 8 / Gap 9 + Gap 13 counters.
				storageTypeViol := storageTypeViolations.Load()
				electionReplicasViol := electionReplicasViolations.Load()
				kvOpRateCeilingViol := kvOpRateCeilingViolations.Load()
				revocationDropped := coord.GetRevocationReportDropped()
				// Phase 3 strict long-disconnect gate: only enforced when the
				// scenario actually configures network_disconnect_long (via
				// scheduled_events or chaos.events). Otherwise observed=0 is
				// normal and must not fail the run.
				ldSkipped := coord.GetLongDisconnectSkipped()
				var ldStrictViol int
				if scenarioHasLongDisconnect(cfg) {
					if ldReassignObserved == 0 {
						log.Print("[Phase3] STRICT GATE FAIL: long_disconnect_reassignment_observed=0 — scenario enables network_disconnect_long but no firing produced a real reassignment observation")
						ldStrictViol++
					}
					if ldReassignInconclusive > 0 {
						log.Printf("[Phase3] STRICT GATE FAIL: long_disconnect_reassignment_inconclusive=%d — target owned zero partitions at expectation time; the chaos firing proved nothing",
							ldReassignInconclusive)
						ldStrictViol++
					}
					if ldSkipped > 0 {
						log.Printf("[Phase3] STRICT GATE FAIL: long_disconnect_skipped=%d — chaos handler skipped firing because no worker held non-empty owners; the scenario expected the chaos to fire",
							ldSkipped)
						ldStrictViol++
					}
				}
				claimLossGate := scenarioHasClaimLossChaos(cfg)
				if failures > 0 || late > 0 || lost > 0 || initialExc > 0 || takeoverExc > 0 || violations > 0 || inconclusive > 0 || unobservedPost > 0 || snapshotOverlaps > 0 || doubleLeaders > 0 || stateReconcileViol > 0 || expDegMissing > 0 || (claimLossGate && unexpClaimLost > 0) || srcConvMissing > 0 || spuriousUnavail > 0 || ldReassignMissing > 0 || (claimLossGate && claimLossOrderViol > 0) || producerResumeMissing > 0 || casStormPanics > 0 || handoffMaxAgeViol > 0 || orphanClaimDrift > 0 || storageTypeViol > 0 || electionReplicasViol > 0 || kvOpRateCeilingViol > 0 || revocationDropped > 0 || ldStrictViol > 0 {
					// Record error but proceed to ordered shutdown
					invariantsErr = fmt.Errorf("stability invariants failed: audit_failures=%d late=%d lost=%d slow_start_initial=%d slow_start_takeover=%d ownership_violations=%d concurrent_owners=%d inconclusive=%d unobserved_post_chaos=%d snapshot_overlaps=%d double_leaders=%d state_reconcile_violations=%d expected_degraded_observed=%d expected_degraded_missing=%d unexpected_claim_lost_shutdown=%d source_convergence_observed=%d source_convergence_missing=%d spurious_source_unavailable=%d long_disconnect_reassignment_observed=%d long_disconnect_reassignment_missing=%d long_disconnect_reassignment_inconclusive=%d long_disconnect_skipped=%d claim_loss_stop_ordering_violations=%d producer_resume_missing=%d assignment_cas_storm_unexpected_panic=%d handoff_bucket_maxage_violation=%d orphan_stable_claim_drift=%d storage_type_violation=%d election_replicas_violation=%d kv_op_rate_ceiling_violation=%d revocation_report_dropped=%d",
						failures, late, lost, initialExc, takeoverExc, violations, concurrent, inconclusive, unobservedPost, snapshotOverlaps, doubleLeaders, stateReconcileViol, expDegObserved, expDegMissing, unexpClaimLost, srcConvObserved, srcConvMissing, spuriousUnavail, ldReassignObserved, ldReassignMissing, ldReassignInconclusive, ldSkipped, claimLossOrderViol, producerResumeMissing, casStormPanics, handoffMaxAgeViol, orphanClaimDrift, storageTypeViol, electionReplicasViol, kvOpRateCeilingViol, revocationDropped)
				}
				if invariantsErr == nil {
					log.Printf("Stability invariants passed: audit_failures=%d late=%d lost=%d slow_start_initial=%d slow_start_takeover=%d ownership_violations=%d concurrent_owners=%d inconclusive=%d unobserved_post_chaos=%d snapshot_overlaps=%d double_leaders=%d state_reconcile_violations=%d expected_degraded_observed=%d expected_degraded_missing=%d unexpected_claim_lost_shutdown=%d source_convergence_observed=%d source_convergence_missing=%d spurious_source_unavailable=%d long_disconnect_reassignment_observed=%d long_disconnect_reassignment_missing=%d long_disconnect_reassignment_inconclusive=%d long_disconnect_skipped=%d claim_loss_stop_ordering_violations=%d producer_resume_missing=%d assignment_cas_storm_unexpected_panic=%d handoff_bucket_maxage_violation=%d orphan_stable_claim_drift=%d storage_type_violation=%d election_replicas_violation=%d kv_op_rate_ceiling_violation=%d revocation_report_dropped=%d",
						failures, late, lost, initialExc, takeoverExc, violations, concurrent, inconclusive, unobservedPost, snapshotOverlaps, doubleLeaders, stateReconcileViol, expDegObserved, expDegMissing, unexpClaimLost, srcConvObserved, srcConvMissing, spuriousUnavail, ldReassignObserved, ldReassignMissing, ldReassignInconclusive, ldSkipped, claimLossOrderViol, producerResumeMissing, casStormPanics, handoffMaxAgeViol, orphanClaimDrift, storageTypeViol, electionReplicasViol, kvOpRateCeilingViol, revocationDropped)
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
				ldReassignInconclusive := coord.GetLongDisconnectReassignmentInconclusive()
				if ldReassignInconclusive > 0 {
					log.Printf("[Phase3] long_disconnect_reassignment_inconclusive=%d (target owned zero partitions at expectation time; audit-only)",
						ldReassignInconclusive)
				}
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
				// Phase 8 / Gap 9 + Gap 13 counters.
				storageTypeViol := storageTypeViolations.Load()
				electionReplicasViol := electionReplicasViolations.Load()
				kvOpRateCeilingViol := kvOpRateCeilingViolations.Load()
				revocationDropped := coord.GetRevocationReportDropped()
				// Phase 3 strict long-disconnect gate: only enforced when the
				// scenario actually configures network_disconnect_long (via
				// scheduled_events or chaos.events). Otherwise observed=0 is
				// normal and must not fail the run.
				ldSkipped := coord.GetLongDisconnectSkipped()
				var ldStrictViol int
				if scenarioHasLongDisconnect(cfg) {
					if ldReassignObserved == 0 {
						log.Print("[Phase3] STRICT GATE FAIL: long_disconnect_reassignment_observed=0 — scenario enables network_disconnect_long but no firing produced a real reassignment observation")
						ldStrictViol++
					}
					if ldReassignInconclusive > 0 {
						log.Printf("[Phase3] STRICT GATE FAIL: long_disconnect_reassignment_inconclusive=%d — target owned zero partitions at expectation time; the chaos firing proved nothing",
							ldReassignInconclusive)
						ldStrictViol++
					}
					if ldSkipped > 0 {
						log.Printf("[Phase3] STRICT GATE FAIL: long_disconnect_skipped=%d — chaos handler skipped firing because no worker held non-empty owners; the scenario expected the chaos to fire",
							ldSkipped)
						ldStrictViol++
					}
				}
				claimLossGate := scenarioHasClaimLossChaos(cfg)
				if failures > 0 || late > 0 || lost > 0 || initialExc > 0 || takeoverExc > 0 || violations > 0 || inconclusive > 0 || unobservedPost > 0 || snapshotOverlaps > 0 || doubleLeaders > 0 || stateReconcileViol > 0 || expDegMissing > 0 || (claimLossGate && unexpClaimLost > 0) || srcConvMissing > 0 || spuriousUnavail > 0 || ldReassignMissing > 0 || (claimLossGate && claimLossOrderViol > 0) || producerResumeMissing > 0 || casStormPanics > 0 || handoffMaxAgeViol > 0 || orphanClaimDrift > 0 || storageTypeViol > 0 || electionReplicasViol > 0 || kvOpRateCeilingViol > 0 || revocationDropped > 0 || ldStrictViol > 0 {
					invariantsErr := fmt.Errorf("stability invariants failed: audit_failures=%d late=%d lost=%d slow_start_initial=%d slow_start_takeover=%d ownership_violations=%d concurrent_owners=%d inconclusive=%d unobserved_post_chaos=%d snapshot_overlaps=%d double_leaders=%d state_reconcile_violations=%d expected_degraded_observed=%d expected_degraded_missing=%d unexpected_claim_lost_shutdown=%d source_convergence_observed=%d source_convergence_missing=%d spurious_source_unavailable=%d long_disconnect_reassignment_observed=%d long_disconnect_reassignment_missing=%d long_disconnect_reassignment_inconclusive=%d long_disconnect_skipped=%d claim_loss_stop_ordering_violations=%d producer_resume_missing=%d assignment_cas_storm_unexpected_panic=%d handoff_bucket_maxage_violation=%d orphan_stable_claim_drift=%d storage_type_violation=%d election_replicas_violation=%d kv_op_rate_ceiling_violation=%d revocation_report_dropped=%d",
						failures, late, lost, initialExc, takeoverExc, violations, concurrent, inconclusive, unobservedPost, snapshotOverlaps, doubleLeaders, stateReconcileViol, expDegObserved, expDegMissing, unexpClaimLost, srcConvObserved, srcConvMissing, spuriousUnavail, ldReassignObserved, ldReassignMissing, ldReassignInconclusive, ldSkipped, claimLossOrderViol, producerResumeMissing, casStormPanics, handoffMaxAgeViol, orphanClaimDrift, storageTypeViol, electionReplicasViol, kvOpRateCeilingViol, revocationDropped)
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
	// IMPORTANT: stdout is reserved as the IPC transport in process mode
	// (the orchestrator's ProcessManager.ipcReader parses NDJSON frames
	// from each child's stdout). Force the standard library logger onto
	// stderr — its default is stderr too, but make it explicit so any
	// future caller cannot inadvertently drown the IPC stream.
	log.SetOutput(os.Stderr)

	// Spawned children (mode: process orchestrator → --mode worker) read
	// the parent's embedded-NATS ClientURL from SIM_NATS_URL. Fall back
	// to cfg.NATS.URL for the legacy distributed standalone path.
	natsURL := os.Getenv("SIM_NATS_URL")
	if natsURL == "" {
		natsURL = cfg.NATS.URL
	}
	ns, err := nats.Connect(natsURL)
	if err != nil {
		return fmt.Errorf("failed to connect to NATS at %s: %w", natsURL, err)
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

	workerID := os.Getenv("WORKER_ID")
	if workerID == "" {
		workerID = "worker-0"
	}

	// Phase 7a: route the four worker report channels through local
	// emitter sinks that serialise each event as an IPC NDJSON frame on
	// stdout. The orchestrator's ProcessManager.ipcReader fans the frames
	// back into the coordinator's channels and per-worker observer.
	//
	// Buffers mirror the all-in-one path so chaos bursts (large
	// assignment snapshots, post-takeover replay) cannot drop frames at
	// the producing edge.
	receivedCh := make(chan coordinator.ReceivedMessage, 100000)
	assignmentsCh := make(chan coordinator.AssignmentReport, 1000)
	startLatCh := make(chan coordinator.StartLatencyReport, 16)
	degradedCh := make(chan coordinator.WorkerDegradedReport, 256)

	workerCfg := worker.Config{
		ID:                  workerID,
		NC:                  ns,
		JS:                  js,
		PartitionCount:      cfg.Partitions.Count,
		PartitionWeights:    weights,
		AssignmentStrategy:  cfg.Workers.AssignmentStrategy,
		ProcessingDelayMin:  cfg.Workers.ProcessingDelay.Min,
		ProcessingDelayMax:  cfg.Workers.ProcessingDelay.Max,
		CoordinatorReportCh: receivedCh,
		AssignmentReportCh:  assignmentsCh,
		StartLatencyCh:      startLatCh,
		DegradedReportCh:    degradedCh,
		MetricsCollector:    nil, // No metrics in standalone worker mode
		ConsumerBatchSize:   cfg.Workers.ConsumerBatchSize,
		HandlerConcurrency:  cfg.Workers.HandlerConcurrency,
		AckWait:             cfg.Workers.AckWait,
		MaxSubjects:         cfg.Workers.MaxSubjects,
	}
	// Apply cluster-wide WorkerIDTTL + any matching per_worker entry from
	// the scenario config so process-mode worker children see the same
	// pool semantics as all-in-one workers. Process-mode chaos
	// (stableid_tiny_pool_respawn) additionally layers STABLEID_* env vars
	// on top via applyStableIDEnv below — those win over the config (last
	// write wins under override-if-set semantics).
	applyWorkerOverrides(&workerCfg, cfg, workerID)

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

	// Wire IPC emitters BEFORE Start so any pre-stable events (initial
	// assignment, start_latency) reach stdout. The emitters return when
	// emitterCtx is cancelled below.
	emitterCtx, emitterCancel := context.WithCancel(ctx)
	defer emitterCancel()
	startEmitters(emitterCtx, workerID, w, receivedCh, assignmentsCh, startLatCh, degradedCh)

	if err := w.Start(ctx); err != nil {
		return err
	}

	// Long-lived loop. Exit conditions:
	//  - ctx cancelled (parent orchestrator shutdown, SIGKILL chaos path
	//    surfaces here via context's deadline / parent close);
	//  - the manager's state reaches StateShutdown (claimLostShutdown
	//    self-stop path — Phase 7b's Gap 8a invariant).
	//
	// The state poll piggybacks on the same ticker the emitter uses for
	// state/leader frames (250ms cadence). Bandwidth is trivial; the
	// poll-vs-hook tradeoff is documented in the Phase 7a brief.
	const exitPollInterval = 250 * time.Millisecond
	ticker := time.NewTicker(exitPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			w.Stop()
			return nil
		case <-ticker.C:
			// types.StateShutdown == 9 (see types/state.go). Use the
			// same numeric value the coordinator's
			// workerStateShutdown const carries so a future enum
			// renumbering surfaces in both packages at once.
			if w.WorkerStateInt() == int(types.StateShutdown) {
				log.Printf("[%s] manager reached StateShutdown; runWorker exiting", workerID)
				w.Stop()
				return nil
			}
		}
	}
}

// startEmitters launches the four channel-drain goroutines plus the periodic
// state/leader ticker that together write NDJSON IPC frames to stdout. All
// goroutines stop when ctx is cancelled.
//
// Each frame's stable_id field is filled lazily from w.StableWorkerID() so
// the coordinator can correlate sim-side worker IDs with parti stable IDs
// for the Phase 4 claim-loss ordering oracle.
func startEmitters(ctx context.Context, workerID string, w *worker.Worker,
	receivedCh <-chan coordinator.ReceivedMessage,
	assignmentsCh <-chan coordinator.AssignmentReport,
	startLatCh <-chan coordinator.StartLatencyReport,
	degradedCh <-chan coordinator.WorkerDegradedReport,
) {
	emit := func(kind string, payload any) {
		line, err := coordinator.EncodeIPCFrame(kind, workerID, w.StableWorkerID(), time.Now().UnixMilli(), payload)
		if err != nil {
			log.Printf("[%s] ipc encode kind=%s: %v", workerID, kind, err)
			return
		}
		// Write directly to stdout; Stdout is line-buffered by the OS
		// pipe so the orchestrator's bufio.Scanner sees each frame as
		// soon as we finish the write.
		if _, err := os.Stdout.WriteString(line); err != nil {
			log.Printf("[%s] ipc stdout write kind=%s: %v", workerID, kind, err)
		}
	}

	// received_message frames
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case rm, ok := <-receivedCh:
				if !ok {
					return
				}
				emit(coordinator.IPCKindReceivedMessage, coordinator.IPCPayloadReceivedMessage{
					PartitionID:       rm.PartitionID,
					PartitionSequence: rm.PartitionSequence,
				})
			}
		}
	}()

	// assignment_report frames
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ar, ok := <-assignmentsCh:
				if !ok {
					return
				}
				emit(coordinator.IPCKindAssignmentReport, coordinator.IPCPayloadAssignmentReport{
					Partitions: ar.Partitions,
				})
			}
		}
	}()

	// start_latency frames
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case sl, ok := <-startLatCh:
				if !ok {
					return
				}
				emit(coordinator.IPCKindStartLatency, coordinator.IPCPayloadStartLatency{
					LatencyMS:       sl.Latency.Milliseconds(),
					IsInitialCohort: sl.IsInitialCohort,
				})
			}
		}
	}()

	// degraded frames
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case dr, ok := <-degradedCh:
				if !ok {
					return
				}
				emit(coordinator.IPCKindDegraded, coordinator.IPCPayloadDegraded{
					Reason: dr.Reason,
				})
			}
		}
	}()

	// state + leader ticker. Emits every tick (even when the value has
	// not changed) so the coordinator-side smoke oracle observes at
	// least one of each kind within T_smoke even for workers that never
	// transition past their initial state (the empty-cluster edge case).
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				emit(coordinator.IPCKindState, coordinator.IPCPayloadState{State: w.WorkerStateInt()})
				emit(coordinator.IPCKindLeader, coordinator.IPCPayloadLeader{IsLeader: w.IsLeader()})
			}
		}
	}()
}

// runProcessOrchestrator is the parent process for Phase 7a "process" mode.
//
// Responsibilities:
//   - bring up an embedded NATS server (or attach to the configured external
//     one) and pre-create the parti coordination KV buckets, exactly like
//     runAllInOne, so spawned worker children can claim their stable IDs
//     and ensure assignment / handoff buckets without thundering the bucket
//     creation path;
//   - construct a Coordinator + GoroutineRegistry + the Phase 0 oracles
//     (leader-uniqueness + state-reconcile poll the registry, which we
//     populate with ProcessWorkerObserver shims via ProcessManager);
//   - install ProcessIPCSinks on the ProcessManager so each child's stdout
//     IPC stream fans into the coordinator;
//   - spawn cfg.Workers.Count worker processes and block until the parent
//     ctx ends. No producer / chaos integration in this scope; the smoke
//     scenario asserts on lifecycle frames only (assignment_report,
//     start_latency, state, leader) which fire independently of message
//     traffic.
//
// Phase 7b (process-mode chaos dispatch) IS in scope: scheduled chaos
// events (worker_pause / worker_resume / stableid_tiny_pool_respawn)
// are dispatched below via handleChaosEvent with goroutineRegistry=nil
// so the process-mode branch runs instead of the all-in-one fast-path.
// Producer-mode integration remains out of scope (see chaos_process_sigstop_long.yaml
// LIMITATION note).
func runProcessOrchestrator(ctx context.Context, cfg *config.Config, cfgPath string) error { //nolint:cyclop,funlen,revive,gocyclo
	// Bring up NATS exactly as runAllInOne does. The pre-create logic
	// mirrors runAllInOne's bucket-readying block, minus the Phase 6
	// handoff-MaxAge knob (the smoke scenario has no Phase 6 chaos).
	var ns *nats.Conn
	var embeddedServer *server.Server
	var err error
	if cfg.NATS.Mode == "embedded" {
		srv, nc, eerr := natsutil.StartEmbeddedNATS()
		if eerr != nil {
			return fmt.Errorf("failed to start embedded NATS: %w", eerr)
		}
		embeddedServer = srv
		ns = nc
		if err := natsutil.CreateStream(nc, cfg.Partitions.Count); err != nil {
			return fmt.Errorf("failed to create stream: %w", err)
		}
		// Publish the random-port ClientURL to env so spawned worker
		// children connect to the parent's embedded server instead of
		// trying to stand up their own (would race the port + double-
		// init the JetStream stream / KV buckets).
		_ = os.Setenv("SIM_NATS_URL", srv.ClientURL())
	} else {
		ns, err = nats.Connect(cfg.NATS.URL)
		if err != nil {
			return fmt.Errorf("failed to connect to NATS: %w", err)
		}
		_ = os.Setenv("SIM_NATS_URL", cfg.NATS.URL)
	}
	defer func() {
		if ns != nil {
			ns.Close()
		}
		if embeddedServer != nil {
			embeddedServer.Shutdown()
		}
	}()

	js, err := jetstream.New(ns)
	if err != nil {
		return fmt.Errorf("failed to get JetStream: %w", err)
	}

	// Pre-create coordination KV buckets so the concurrent worker-process
	// startups don't thunder the create path. Same set runAllInOne uses.
	{
		pc := parti.DefaultConfig()
		pc.KVBuckets.HandoffTTL = 0
		timeBuckets := []struct {
			name string
			ttl  time.Duration
		}{
			{pc.KVBuckets.StableIDBucket, pc.WorkerIDTTL},
			{pc.KVBuckets.ElectionBucket, pc.ElectionTimeout},
			{pc.KVBuckets.HeartbeatBucket, pc.HeartbeatTTL},
			{pc.KVBuckets.AssignmentBucket, pc.KVBuckets.AssignmentTTL},
			{pc.KVBuckets.HandoffBucket, pc.KVBuckets.HandoffTTL},
		}
		for _, b := range timeBuckets {
			bctx, bcancel := context.WithTimeout(ctx, 5*time.Second)
			_, kerr := kvutil.EnsureKVBucket(bctx, js, b.name, b.ttl)
			bcancel()
			if kerr != nil {
				return fmt.Errorf("failed to ensure KV bucket %s: %w", b.name, kerr)
			}
		}
	}

	// Coordinator + registry + oracles.
	coord := coordinator.NewCoordinator(cfg.Partitions.Count, nil, coordinator.DupTraceSettings{}, cfg.Coordinator.StopOnFailure, cfg.Coordinator.FailureReportPath)
	if cfg.Workers.Count > 0 {
		coord.SetExpectedWorkers(cfg.Workers.Count)
	}
	registry := coordinator.NewGoroutineRegistry()
	coord.EnableShutdownOracles(registry)
	go coord.Start(ctx)

	// IPC smoke oracle (Phase 7a gating; Phase 7b reads its counter).
	// Scenario-tunable via process.smoke_window; default 30s preserves the
	// historical behavior. Cold-NATS / race-build runs that legitimately
	// take >30s to emit a first assignment_report can bump this to 60s+
	// without weakening the lifecycle assertion — the window is per-worker
	// and counts from each worker's first observed frame.
	smokeWindow := cfg.Process.SmokeWindow
	if smokeWindow <= 0 {
		smokeWindow = 30 * time.Second
	}
	log.Printf("[process] IPC smoke window: %v (default 30s; set process.smoke_window to override)", smokeWindow)
	smoke := coordinator.NewIPCSmokeOracle(smokeWindow, nil)
	smoke.Run(ctx, 2*time.Second)

	// ProcessManager + IPC sinks. The sinks share buffered channels with
	// the coordinator so its existing processAssignments /
	// receivedCh-drain goroutines pick up worker reports transparently.
	processMgr := coordinator.NewProcessManager(os.Args[0], cfgPath)
	processMgr.SetIPCSinks(&coordinator.ProcessIPCSinks{
		Registry:    registry,
		Received:    coord.GetReceivedChannel(),
		Assignments: coord.GetAssignmentsChannel(),
		StartLat:    coord.GetStartLatencyChannel(),
		Degraded:    coord.GetDegradedReportsChannel(),
		Smoke:       smoke,
	})

	// Expose the active config to chaos handlers (process-mode equivalent
	// of aioCfg) so handleProcessStableIDTinyPoolRespawn can resolve
	// cfg.Workers.PerWorker[target_worker] → StableIDOverrides.
	procCfg = cfg
	defer func() { procCfg = nil }()
	procNextWorkerIdx = cfg.Workers.Count - 1

	// Spawn the worker children. Each carries WORKER_ID=worker-i via env;
	// PerWorker overrides (when configured) are forwarded as STABLEID_* env
	// vars so per_worker tiny-pool scenarios are reproducible in process
	// mode (Phase 4c plumbing).
	for i := 0; i < cfg.Workers.Count; i++ {
		workerID := fmt.Sprintf("worker-%d", i)
		var startErr error
		if pw, ok := cfg.Workers.PerWorker[workerID]; ok {
			startErr = processMgr.StartWorker(ctx, workerID, coordinator.StableIDOverrides{
				Prefix: pw.WorkerIDPrefix,
				Min:    pw.WorkerIDMin,
				Max:    pw.WorkerIDMax,
				TTL:    pw.WorkerIDTTL,
			})
		} else {
			startErr = processMgr.StartWorker(ctx, workerID)
		}
		if startErr != nil {
			log.Printf("[process] failed to start %s: %v", workerID, startErr)
		}
	}

	// Phase 7b scheduled-events dispatcher: fire each scenario-scheduled
	// event at its configured offset from the orchestrator's start. Mirrors
	// the all-in-one dispatcher (runAllInOne above); routes through
	// handleChaosEvent with goroutineRegistry=nil so the process-mode
	// branch (worker_pause / worker_resume / stableid_tiny_pool_respawn)
	// runs instead of the all-in-one fast-path.
	if cfg.Chaos.Enabled || len(cfg.Chaos.ScheduledEvents) > 0 {
		for _, sev := range cfg.Chaos.ScheduledEvents {
			go func() {
				select {
				case <-ctx.Done():
					return
				case <-time.After(sev.At):
				}
				log.Printf("[ScheduledChaos] firing %s at +%v params=%v", sev.Event, sev.At, sev.Params)
				// MarkChaosStarted BEFORE dispatch so any
				// transient post-event observations
				// (double-leader during SIGSTOP/SIGCONT, etc.)
				// land in the informational post-chaos bucket
				// rather than the fail-run counter. Mirrors the
				// all-in-one ChaosController hook at main.go:534.
				if coord != nil {
					coord.MarkChaosStarted()
				}
				handleChaosEvent(ctx, coord, coordinator.ChaosEvent(sev.Event), sev.Params, processMgr, nil, nil, nil)
			}()
		}
	}

	// Block until parent ctx ends, then stop children with a bounded
	// SIGTERM-then-SIGKILL window. The defer above closes NATS last.
	<-ctx.Done()
	log.Println("[process] orchestrator stopping; killing worker children")
	if err := processMgr.StopAll(5 * time.Second); err != nil {
		log.Printf("[process] StopAll: %v", err)
	}

	// Final smoke evaluation before reporting counters.
	smoke.Check()
	seen := smoke.WorkersSeen()
	counts := smoke.FrameCounts()
	log.Printf("[process] smoke summary: workers_seen=%d expected=%d frame_counts=%v",
		seen, cfg.Workers.Count, counts)
	// Phase 7b: surface Phase 0 / Phase 4 oracle counters for the
	// long-SIGSTOP scenario. The oracles fire through the existing IPC →
	// coordinator channels so process mode gets them for free; the
	// dedicated log line + exit-code gating wires them into the scenario
	// pass / fail signal.
	claimLossOrderViol := coord.GetClaimLossOrderingViolations()
	leaderViol := coord.GetDoubleLeaderObservations()
	snapOverlap := coord.GetSnapshotOverlapCount()
	revocationDropped := coord.GetRevocationReportDropped()
	replacementAssignObs := coord.GetProcessSIGSTOPReplacementAssignmentObserved()
	returningShutdownObs := coord.GetProcessSIGSTOPReturningShutdownObserved()
	log.Printf("[process] oracle counters: claim_loss_stop_ordering=%d leader_uniqueness=%d snapshot_overlap=%d "+
		"revocation_report_dropped=%d process_sigstop_replacement_assignment_observed=%d "+
		"process_sigstop_returning_shutdown_observed=%d",
		claimLossOrderViol, leaderViol, snapOverlap, revocationDropped, replacementAssignObs, returningShutdownObs)
	if v := smoke.Violations(); v > 0 {
		return fmt.Errorf("ipc_smoke_violation=%d (workers_seen=%d frame_counts=%v)", v, seen, counts)
	}
	if seen < cfg.Workers.Count {
		return fmt.Errorf("ipc_smoke_violation: only %d of %d workers ever emitted a frame", seen, cfg.Workers.Count)
	}
	if scenarioHasClaimLossChaos(cfg) && claimLossOrderViol > 0 {
		return fmt.Errorf("claim_loss_stop_ordering_violations=%d", claimLossOrderViol)
	}
	if leaderViol > 0 {
		return fmt.Errorf("double_leader_observations=%d", leaderViol)
	}
	if snapOverlap > 0 {
		return fmt.Errorf("snapshot_overlap_violations=%d", snapOverlap)
	}
	if revocationDropped > 0 {
		return fmt.Errorf("revocation_report_dropped=%d (Phase 4 ordering oracle lost its only negative signal)", revocationDropped)
	}
	// Phase 3 strict long-disconnect gate (mirrors all-in-one): only
	// enforced when the scenario configures network_disconnect_long.
	// Process mode does not currently dispatch network_disconnect_long
	// (see chaos.go switch above) so observed will always be 0 — the
	// gate exists for symmetry / future support.
	if scenarioHasLongDisconnect(cfg) {
		coord.CheckLongDisconnectExpectations()
		ldObs := coord.GetLongDisconnectReassignmentObserved()
		ldInc := coord.GetLongDisconnectReassignmentInconclusive()
		ldSkip := coord.GetLongDisconnectSkipped()
		if ldObs == 0 {
			return fmt.Errorf("long_disconnect_reassignment_observed=0 — scenario enables network_disconnect_long but no firing produced a real reassignment observation (inconclusive=%d skipped=%d)", ldInc, ldSkip)
		}
		if ldInc > 0 {
			return fmt.Errorf("long_disconnect_reassignment_inconclusive=%d — target owned zero partitions at expectation time", ldInc)
		}
		if ldSkip > 0 {
			return fmt.Errorf("long_disconnect_skipped=%d — chaos handler skipped firing despite scenario enabling it", ldSkip)
		}
	}
	// Phase 7b positive gates: only required when the scenario actually
	// scheduled a worker_resume event (the only path that increments these
	// counters). Detect by scanning cfg.Chaos.ScheduledEvents for
	// WorkerResumeEvent; if present, both counters MUST be > 0.
	if scenarioHasScheduledEvent(cfg, coordinator.WorkerResumeEvent) {
		if replacementAssignObs == 0 {
			return errors.New("process_sigstop_replacement_assignment_observed=0 — replacement worker never reported non-empty assignment before SIGCONT")
		}
		if returningShutdownObs == 0 {
			return errors.New("process_sigstop_returning_shutdown_observed=0 — returning worker never reached StateShutdown after SIGCONT")
		}
	}
	log.Printf("[process] smoke check passed (workers=%d)", cfg.Workers.Count)

	return nil
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
		// Simulate worker pause. duration<=0 means "open-ended; driven by
		// later worker_resume" (Phase 7b long-SIGSTOP scenario).
		duration := durationFromParams(params, "duration", 0)
		targetWorker, _ := params["target_worker"].(string)
		handleWorkerPause(duration, processMgr, targetWorker)

	case coordinator.WorkerResumeEvent:
		// Phase 7b: drive SIGCONT for a previously open-ended worker_pause.
		// Wait-for-condition variant: before SIGCONT, block until the
		// replacement worker has emitted a non-empty assignment snapshot
		// (proof the claim-loss invariant can actually fire on resume).
		// On timeout the handler does NOT SIGCONT — leaves the target
		// frozen so the failure is diagnosable.
		targetWorker, _ := params["target_worker"].(string)
		handleWorkerResume(ctx, coord, processMgr, params, targetWorker)

	case coordinator.StableIDTinyPoolRespawnEvent:
		// Phase 7b: process-mode same-pool replacement. Look up the target
		// worker's StableID overrides from cfg.Workers.PerWorker and spawn a
		// fresh child process pinned to the same tiny pool so its Claim
		// performs the revision-bumping Update that moves the stable-ID key.
		handleProcessStableIDTinyPoolRespawn(ctx, params, processMgr)

	case coordinator.SlowConsumerEvent:
		// SlowConsumer only works in all-in-one (goroutine) mode - skip in process mode
		log.Println("[Chaos] slow_consumer event only supported in all-in-one mode")

	case coordinator.BucketDeleteEvent, coordinator.BucketRecreateEvent, coordinator.BucketPeerTakeoverEvent,
		coordinator.WatcherStallEvent, coordinator.StableIDClaimStealEvent,
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
	case coordinator.ScaleUpEvent, coordinator.ProducerCrashEvent, coordinator.BucketDeleteEvent, coordinator.BucketRecreateEvent, coordinator.WatcherStallEvent, coordinator.StableIDTinyPoolRespawnEvent, coordinator.ProducerDisconnectEvent, coordinator.AssignmentCASStormEvent, coordinator.AssignmentBucketDeleteEvent, coordinator.HandoffOrphanClaimWriteEvent, coordinator.WorkerResumeEvent:
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

	case coordinator.WorkerResumeEvent:
		// Phase 7b: process-mode only. All-in-one mode has no frozen-process
		// equivalent (workers are goroutines), so this is a no-op here.
		log.Println("[Chaos] worker_resume is process-mode only; skipping in all-in-one")

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
		RevocationReportDropFn:      aioCoord.IncRevocationReportDropped,
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

// handleWorkerPause pauses a worker process using SIGSTOP. When targetWorker
// is non-empty, that specific worker is paused; otherwise a random running
// worker is chosen. When duration > 0, a SIGCONT is scheduled via
// time.AfterFunc; when duration <= 0, the pause is open-ended and the caller
// is expected to drive a SIGCONT via a later worker_resume event (Phase 7b
// long-SIGSTOP scenario).
func handleWorkerPause(duration time.Duration, processMgr *coordinator.ProcessManager, targetWorker string) {
	log.Printf("[Chaos] Simulating worker pause for %v target=%q", duration, targetWorker)

	targetID := targetWorker
	if targetID == "" {
		targetID = processMgr.SelectRandomWorker()
	}
	if targetID == "" {
		log.Println("[Chaos] No running workers to pause")
		return
	}

	log.Printf("[Chaos] Pausing worker %s (SIGSTOP)", targetID)
	if err := processMgr.SignalProcess(targetID, syscall.SIGSTOP); err != nil {
		log.Printf("[Chaos] Failed to pause worker %s: %v", targetID, err)
		return
	}

	if duration <= 0 {
		log.Printf("[Chaos] Open-ended pause on %s; resume must be driven by worker_resume event", targetID)
		return
	}

	time.AfterFunc(duration, func() {
		log.Printf("[Chaos] Resuming worker %s (SIGCONT)", targetID)
		if err := processMgr.SignalProcess(targetID, syscall.SIGCONT); err != nil {
			log.Printf("[Chaos] Failed to resume worker %s: %v", targetID, err)
		}
	})
}

// handleProcessStableIDTinyPoolRespawn spawns a fresh worker process pinned
// to the same tiny stable-ID pool as a previously-frozen target. Phase 7b
// driver for the same-pool replacement step of the OS-signal Gap 8a chain:
//
//  1. Resolve cfg.Workers.PerWorker[target_worker] → StableIDOverrides.
//  2. Allocate a new sim-side worker ID (procNextWorkerIdx++).
//  3. ProcessManager.StartWorker(ctx, newID, overrides) — the spawned child
//     process inherits SIM_NATS_URL from the orchestrator, runs through the
//     production stableid.Claimer.Claim path, and wins the same stable ID
//     via the revision-checked stale-takeover Update
//     (internal/stableid/claimer.go:219-243).
//
// Must run strictly after staleThreshold = 3 * max(WorkerIDTTL/3, 100ms);
// otherwise the replacement's Claim returns ErrNoAvailableID.
func handleProcessStableIDTinyPoolRespawn(ctx context.Context, params map[string]any, processMgr *coordinator.ProcessManager) {
	if procCfg == nil {
		log.Println("[Chaos] stableid_tiny_pool_respawn: procCfg not initialized (not in process mode?)")
		return
	}
	target, _ := params["target_worker"].(string)
	if target == "" {
		// Backward-compat alias used by the all-in-one scenarios.
		target, _ = params["target_role"].(string)
	}
	if target == "" {
		log.Println("[Chaos] stableid_tiny_pool_respawn: target_worker (or target_role) required; skipping")
		return
	}
	override, ok := procCfg.Workers.PerWorker[target]
	if !ok {
		log.Printf("[Chaos] stableid_tiny_pool_respawn: no per_worker override for %q; skipping", target)
		return
	}
	overrides := coordinator.StableIDOverrides{
		Prefix: override.WorkerIDPrefix,
		Min:    override.WorkerIDMin,
		Max:    override.WorkerIDMax,
		TTL:    override.WorkerIDTTL,
	}
	procNextWorkerIdx++
	newID := fmt.Sprintf("worker-%d", procNextWorkerIdx)
	log.Printf("[Chaos] stableid_tiny_pool_respawn: spawning %s with overrides=%+v (target=%s) at t=%v",
		newID, overrides, target, time.Now().Format(time.RFC3339Nano))
	if err := processMgr.StartWorker(ctx, newID, overrides); err != nil {
		log.Printf("[Chaos] stableid_tiny_pool_respawn: failed to spawn %s: %v", newID, err)
		return
	}
	procLastReplacementWorkerID = newID
}

// handleWorkerResume drives the Phase 7b wait-for-condition SIGCONT.
//
// The Phase 7b long-SIGSTOP scenario requires three positive observations
// before the original target is unfrozen, otherwise the run silently
// passes on absence-of-failure rather than presence-of-correct-behavior:
//
//  1. The replacement worker (spawned by stableid_tiny_pool_respawn) has
//     a non-empty assignment snapshot — proves the production
//     stale-takeover claim path actually moved the stable-ID key and
//     the new manager won the election. Without this gate, SIGCONT
//     might fire before the replacement claims anything, in which case
//     the returning worker's claim-loss invariant is vacuously
//     satisfied.
//  2. SIGCONT is sent only after (1). On (1) timeout the handler logs
//     loudly and DOES NOT SIGCONT — leaves the target frozen so the
//     failure is diagnosable in the live process tree.
//  3. After SIGCONT the handler polls the original target's observer
//     until WorkerStateInt() == workerStateShutdown (the claim-loss
//     onClaimerError → claimLostShutdown path landed). On success it
//     bumps process_sigstop_returning_shutdown_observed; the
//     orchestrator gates that counter > 0.
//
// All deadlines are bounded so a wedged run cannot hang past
// cfg.Simulation.Duration.
//
// params optionally carries scenario knobs:
//   - replacement_wait: max time to wait for replacement assignment
//     snapshot (default 30s).
//   - shutdown_wait: max time to wait for returning worker shutdown
//     after SIGCONT (default 60s — the claim-loss path has a 30s stop
//     timeout per claimLostShutdown).
func handleWorkerResume(
	ctx context.Context,
	coord *coordinator.Coordinator,
	processMgr *coordinator.ProcessManager,
	params map[string]any,
	targetWorker string,
) {
	if targetWorker == "" {
		log.Println("[Chaos] worker_resume requires target_worker; skipping")
		return
	}
	if processMgr == nil {
		log.Println("[Chaos] worker_resume requires process manager; skipping")
		return
	}

	replacementWait := durationFromParams(params, "replacement_wait", 30*time.Second)
	shutdownWait := durationFromParams(params, "shutdown_wait", 60*time.Second)

	replacement := procLastReplacementWorkerID
	if replacement == "" || coord == nil {
		log.Printf("[Chaos] worker_resume: no replacement worker recorded (replacement=%q coord_nil=%v); "+
			"SIGCONT will fire WITHOUT positive replacement-claim gate; "+
			"scenario invariants cannot be proven", replacement, coord == nil)
	} else {
		log.Printf("[Chaos] worker_resume: waiting up to %v for replacement worker %s to report non-empty assignment "+
			"before SIGCONT on %s", replacementWait, replacement, targetWorker)
		waitCtx, cancel := context.WithTimeout(ctx, replacementWait)
		gotAssignment := waitForNonEmptyAssignment(waitCtx, coord, replacement)
		cancel()
		if !gotAssignment {
			log.Printf("[Chaos] worker_resume: TIMEOUT waiting %v for replacement %s to claim partitions; "+
				"NOT sending SIGCONT to %s (leaves the target frozen for live diagnosis). "+
				"Phase 7b positive gate will fail (process_sigstop_replacement_assignment_observed=0).",
				replacementWait, replacement, targetWorker)
			return
		}
		coord.IncProcessSIGSTOPReplacementAssignmentObserved()
		log.Printf("[Chaos] worker_resume: replacement %s observed with non-empty assignment; proceeding to SIGCONT %s",
			replacement, targetWorker)
	}

	log.Printf("[Chaos] Resuming worker %s (SIGCONT)", targetWorker)
	if err := processMgr.SignalProcess(targetWorker, syscall.SIGCONT); err != nil {
		log.Printf("[Chaos] Failed to resume worker %s: %v", targetWorker, err)
		return
	}

	// Phase 7b positive returning-worker gate: poll the target observer
	// until WorkerStateInt() == workerStateShutdown. The claim-loss path
	// (manager_election.go:onClaimerError → claimLostShutdown) drives the
	// state machine to Shutdown after the next renew tick fails with
	// jetstream.ErrKeyExists. shutdownWait bounds the wait.
	if coord == nil {
		return
	}
	go func() {
		waitCtx, cancel := context.WithTimeout(ctx, shutdownWait)
		defer cancel()
		if observeStateEquals(waitCtx, processMgr, targetWorker, workerStateShutdownInt) {
			coord.IncProcessSIGSTOPReturningShutdownObserved()
			log.Printf("[Chaos] worker_resume: returning worker %s observed in StateShutdown (Phase 7b positive gate satisfied)",
				targetWorker)
			return
		}
		log.Printf("[Chaos] worker_resume: TIMEOUT after %v waiting for %s to reach StateShutdown; "+
			"Phase 7b returning-shutdown gate will FAIL", shutdownWait, targetWorker)
	}()
}

// scenarioHasScheduledEvent reports whether the configured scenario
// explicitly schedules an event of the given kind. Used by
// runProcessOrchestrator's final gate to make the Phase 7b positive
// counters required only on scenarios that drive worker_resume — other
// process-mode scenarios (e.g. process_smoke) intentionally never
// increment them and must not be failed by this gate.
func scenarioHasScheduledEvent(cfg *config.Config, want coordinator.ChaosEvent) bool {
	if cfg == nil {
		return false
	}
	for _, sev := range cfg.Chaos.ScheduledEvents {
		if coordinator.ChaosEvent(sev.Event) == want {
			return true
		}
	}

	return false
}

// scenarioHasLongDisconnect reports whether the configured scenario
// could fire a network_disconnect_long event — either via the random
// chaos ticker (cfg.Chaos.Events listing "network_disconnect_long")
// or via an explicit scheduled_events entry. Used by both final
// gates: scenarios that deliberately enable long-disconnect MUST end
// with long_disconnect_reassignment_observed > 0,
// long_disconnect_reassignment_inconclusive == 0, and
// long_disconnect_skipped == 0. Scenarios that never schedule one
// are exempt from this gate.
// scenarioHasClaimLossChaos reports whether the configured scenario
// deliberately drives a claim-loss event — one where a worker loses
// its stable-ID claim to a peer (stableid_claim_steal /
// bucket_peer_takeover), is respawned out of a tiny pool
// (stableid_tiny_pool_respawn), is explicitly resumed from a long
// SIGSTOP (worker_resume), or suffers a long network disconnect
// (network_disconnect_long). Only these scenarios may enforce the
// DegradedReasonOracle's unexpected_claim_lost_shutdown and
// ClaimLossOrderingOracle's claim_loss_stop_ordering_violations as
// hard shutdown gates; diffuse chaos runs (e.g. chaos_comprehensive,
// which schedules worker_pause without a paired resume) still log the
// counters but cannot fail on them.
//
// worker_pause is deliberately NOT in the trigger set: it is used
// diffusely by chaos_comprehensive / chaos_roundrobin to drive
// transient stalls, and the resulting claim-loss is a legitimate
// side-effect rather than the scenario's targeted invariant.
func scenarioHasClaimLossChaos(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	// Deliberately a partial trigger set; events not listed
	// (worker_pause, worker_crash, etc.) are exempt from the claim-loss
	// failure gate by design.
	triggers := map[coordinator.ChaosEvent]struct{}{ //nolint:exhaustive // partial trigger set is intentional
		coordinator.StableIDClaimStealEvent:      {},
		coordinator.BucketPeerTakeoverEvent:      {},
		coordinator.StableIDTinyPoolRespawnEvent: {},
		coordinator.WorkerResumeEvent:            {},
		coordinator.NetworkDisconnectLongEvent:   {},
	}
	for _, sev := range cfg.Chaos.ScheduledEvents {
		if _, ok := triggers[coordinator.ChaosEvent(sev.Event)]; ok {
			return true
		}
	}
	if cfg.Chaos.Enabled {
		for _, ev := range cfg.Chaos.Events {
			if _, ok := triggers[coordinator.ChaosEvent(ev)]; ok {
				return true
			}
		}
	}

	return false
}

func scenarioHasLongDisconnect(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	if scenarioHasScheduledEvent(cfg, coordinator.NetworkDisconnectLongEvent) {
		return true
	}
	if cfg.Chaos.Enabled {
		for _, ev := range cfg.Chaos.Events {
			if coordinator.ChaosEvent(ev) == coordinator.NetworkDisconnectLongEvent {
				return true
			}
		}
	}

	return false
}

// workerStateShutdownInt mirrors coordinator.workerStateShutdown (the int
// value of parti.StateShutdown). Hardcoded here to avoid importing an
// internal const; kept in sync via the same types/state.go reference.
const workerStateShutdownInt = 9

// waitForNonEmptyAssignment polls the coordinator's owner snapshot until
// the worker workerID owns at least one partition, or ctx is done.
// Returns true if a non-empty snapshot was observed.
func waitForNonEmptyAssignment(ctx context.Context, coord *coordinator.Coordinator, workerID string) bool {
	if coord == nil {
		return false
	}
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if owned := coord.PartitionsOwnedBy(workerID); len(owned) > 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-tick.C:
		}
	}
}

// observeStateEquals polls the ProcessWorkerObserver for the given worker
// until its WorkerStateInt() equals wantState, or ctx is done. Returns
// true if the state was observed.
func observeStateEquals(ctx context.Context, processMgr *coordinator.ProcessManager, workerID string, wantState int) bool {
	if processMgr == nil {
		return false
	}
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		info, ok := processMgr.GetProcessInfo(workerID)
		if ok && info.Observer != nil && info.Observer.WorkerStateInt() == wantState {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-tick.C:
		}
	}
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
//
//nolint:cyclop // sequential chaos handler: select target → register expectations → disconnect/reconnect; splitting hurts readability.
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
			log.Printf("[Chaos] long_disconnect: target_worker=%s not found; falling back to non-empty random", explicit)
		}
	}
	if target == nil {
		// Prefer workers with a non-empty current assignment so the
		// reassignment-observed invariant (Phase 3 INV1) can actually
		// fire. Random pick among workers with len(owned)>0; fall back
		// to plain random (with a loud warning) only when no worker
		// owns any partitions, which means the owner snapshot has not
		// initialized yet — proceeding would yield an inconclusive
		// expectation.
		var nonEmpty []*coordinator.GoroutineInfo
		if aioCoord != nil {
			for i := range workers {
				if owned := aioCoord.PartitionsOwnedBy(workers[i].ID); len(owned) > 0 {
					nonEmpty = append(nonEmpty, workers[i])
				}
			}
		}
		switch {
		case len(nonEmpty) > 0:
			target = nonEmpty[time.Now().UnixNano()%int64(len(nonEmpty))]
			log.Printf("[Chaos] long_disconnect: selected target=%s (had non-empty owner snapshot)", target.ID)
		default:
			log.Printf("[Chaos] long_disconnect: WARN no worker currently owns any partitions in the owner snapshot; " +
				"skipping this chaos firing — the reassignment expectation would be vacuous (empty→empty). " +
				"Incrementing long_disconnect_skipped; scenarios that explicitly schedule/enable " +
				"network_disconnect_long will fail the final gate on a non-zero skip count.")
			if aioCoord != nil {
				aioCoord.IncLongDisconnectSkipped()
			}

			return
		}
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
