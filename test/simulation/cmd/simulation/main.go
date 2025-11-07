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
	"syscall"
	"time"

	"github.com/arloliu/parti/test/simulation/internal/config"
	"github.com/arloliu/parti/test/simulation/internal/coordinator"
	"github.com/arloliu/parti/test/simulation/internal/metrics"
	"github.com/arloliu/parti/test/simulation/internal/natsutil"
	"github.com/arloliu/parti/test/simulation/internal/producer"
	"github.com/arloliu/parti/test/simulation/internal/worker"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

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
	flag.Parse()

	// Load config
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
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

	// Start components based on mode
	errCh := make(chan error, 1)
	go func() {
		var runErr error
		switch cfg.Simulation.Mode {
		case "all-in-one":
			runErr = runAllInOne(ctx, cfg, *configPath)
		case "producer":
			runErr = runProducer(ctx, cfg)
		case "worker":
			runErr = runWorker(ctx, cfg)
		case "coordinator":
			runErr = runCoordinator(ctx)
		default:
			runErr = fmt.Errorf("unknown mode: %s", cfg.Simulation.Mode)
		}
		errCh <- runErr
	}()

	// Wait for either completion, timeout, or signal
	select {
	case runErr := <-errCh:
		if runErr != nil {
			log.Fatalf("Simulation failed: %v", runErr)
		}
		log.Println("Simulation completed successfully")
	case <-ctx.Done():
		if cfg.Simulation.Duration > 0 {
			log.Printf("Simulation duration (%v) reached, shutting down...", cfg.Simulation.Duration)
		} else {
			log.Println("Context cancelled, shutting down...")
		}
	case sig := <-sigCh:
		log.Printf("Received signal %v, shutting down...", sig)
		cancel()
	}

	// Give components time to shut down gracefully
	time.Sleep(2 * time.Second)
	log.Println("Shutdown complete")
}

func runAllInOne(ctx context.Context, cfg *config.Config, cfgPath string) error {
	// Start embedded NATS if configured
	var ns *nats.Conn
	var err error
	if cfg.NATS.Mode == "embedded" {
		server, nc, err := natsutil.StartEmbeddedNATS()
		if err != nil {
			return fmt.Errorf("failed to start embedded NATS: %w", err)
		}
		defer server.Shutdown()
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
	defer ns.Close()

	js, err := jetstream.New(ns)
	if err != nil {
		return fmt.Errorf("failed to get JetStream: %w", err)
	}

	// Create metrics collector and Prometheus server if enabled
	var metricsCollector *metrics.Collector
	var prometheusServer *metrics.PrometheusServer
	if cfg.Metrics.Prometheus.Enabled {
		metricsCollector = metrics.NewCollector()
		addr := fmt.Sprintf(":%d", cfg.Metrics.Prometheus.Port)
		prometheusServer = metrics.NewPrometheusServer(addr, metricsCollector)

		go func() {
			if err := prometheusServer.Start(ctx); err != nil {
				log.Printf("Prometheus server error: %v", err)
			}
		}()

		// Start system metrics updater
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

		log.Printf("Prometheus metrics server started on %s", addr)
		log.Printf("Access metrics at: http://localhost%s/metrics", addr)
	}

	// Create coordinator
	coord := coordinator.NewCoordinator(metricsCollector)
	go coord.Start(ctx)

	// Create goroutine registry for all-in-one mode chaos
	goroutineRegistry := coordinator.NewGoroutineRegistry()

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

	// Create chaos controller if enabled
	var chaosCtrl *coordinator.ChaosController
	if cfg.Chaos.Enabled {
		minInterval, maxInterval, err := parseChaosInterval(cfg.Chaos.Interval)
		if err != nil {
			return fmt.Errorf("failed to parse chaos interval: %w", err)
		}

		chaosConfig := coordinator.ChaosConfig{
			Enabled:     cfg.Chaos.Enabled,
			Events:      cfg.Chaos.Events,
			MinInterval: minInterval,
			MaxInterval: maxInterval,
			EventCallback: func(event coordinator.ChaosEvent, params map[string]any) {
				handleChaosEvent(ctx, event, params, processMgr, metricsCollector, checkpointMgr, goroutineRegistry)
			},
		}

		chaosCtrl = coordinator.NewChaosController(chaosConfig)
		go chaosCtrl.Start(ctx)
		log.Printf("Chaos controller started (events: %v, interval: %v-%v)",
			cfg.Chaos.Events, minInterval, maxInterval)
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

	// Create producers
	for i := 0; i < cfg.Producers.Count; i++ {
		// Calculate partition range for this producer
		startPartition := i * partitionsPerProducer
		endPartition := (i + 1) * partitionsPerProducer
		if endPartition > cfg.Partitions.Count {
			endPartition = cfg.Partitions.Count
		}

		partitionIDs := make([]int, 0, endPartition-startPartition)
		partitionWeights := make([]int64, 0, endPartition-startPartition)
		for j := startPartition; j < endPartition; j++ {
			partitionIDs = append(partitionIDs, j)
			partitionWeights = append(partitionWeights, weights[j])
		}

		producerID := fmt.Sprintf("producer-%d", i)
		prod := producer.NewProducer(
			producerID,
			js,
			partitionIDs,
			partitionWeights,
			cfg.Partitions.MessageRatePerPartition,
			coord.GetSentChannel(),
			metricsCollector,
		)

		// Create cancelable context for this producer
		producerCtx, producerCancel := context.WithCancel(ctx)
		goroutineRegistry.Register(producerID, coordinator.ProducerGoroutine, producerCancel)

		go func(p *producer.Producer, id string, prodCtx context.Context) {
			p.Start(prodCtx)
			// Producer stopped (context cancelled), mark as inactive
			goroutineRegistry.MarkInactive(id)
		}(prod, producerID, producerCtx)
	}

	// Create workers (start concurrently for faster stable ID claiming)
	for i := 0; i < cfg.Workers.Count; i++ {
		workerID := fmt.Sprintf("worker-%d", i)
		workerCfg := worker.Config{
			ID:                  workerID,
			NC:                  ns,
			JS:                  js,
			PartitionCount:      cfg.Partitions.Count,
			PartitionWeights:    weights,
			AssignmentStrategy:  cfg.Workers.AssignmentStrategy,
			ProcessingDelayMin:  cfg.Workers.ProcessingDelay.Min,
			ProcessingDelayMax:  cfg.Workers.ProcessingDelay.Max,
			CoordinatorReportCh: coord.GetReceivedChannel(),
			MetricsCollector:    metricsCollector,
		}

		w, err := worker.NewWorker(workerCfg)
		if err != nil {
			return fmt.Errorf("failed to create worker %d: %w", i, err)
		}

		// Create cancelable context for this worker
		workerCtx, workerCancel := context.WithCancel(ctx)
		goroutineRegistry.Register(workerID, coordinator.WorkerGoroutine, workerCancel)

		// Start worker in goroutine to allow concurrent stable ID claiming
		go func(worker *worker.Worker, id string, workerCtx context.Context) {
			if err := worker.Start(workerCtx); err != nil {
				log.Printf("Failed to start worker %s: %v", id, err)
			}
			// Worker stopped (context cancelled or error), mark as inactive
			goroutineRegistry.MarkInactive(id)
		}(w, workerID, workerCtx)
	}

	// Record initial worker count
	if metricsCollector != nil {
		metricsCollector.SetWorkersActive(cfg.Workers.Count)
		log.Printf("Recorded initial worker count: %d", cfg.Workers.Count)
	}

	// Print report periodically
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

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
	reportCh := make(chan producer.ReportMessage, 1000)
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
	endPartition := (producerIndex + 1) * partitionsPerProducer
	if endPartition > cfg.Partitions.Count {
		endPartition = cfg.Partitions.Count
	}

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
	reportCh := make(chan coordinator.ReceivedMessage, 1000)
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
	}

	w, err := worker.NewWorker(workerCfg)
	if err != nil {
		return fmt.Errorf("failed to create worker: %w", err)
	}

	return w.Start(ctx)
}

func runCoordinator(ctx context.Context) error {
	coord := coordinator.NewCoordinator(nil) // No metrics in standalone mode

	// Print report periodically
	ticker := time.NewTicker(30 * time.Second)
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
func handleChaosEvent(
	ctx context.Context,
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
		// Gracefully restart random worker
		handleRestartEvent(ctx, processMgr, metricsCollector)

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
		// Kill leader worker (just kill first running worker for now)
		handleLeaderFailure(processMgr, metricsCollector)

	case coordinator.NetworkDisconnectEvent:
		// Simulate network disconnect
		duration, ok := params["duration"].(time.Duration)
		if !ok {
			log.Println("[Chaos] Invalid duration parameter for network_disconnect event")
			return
		}
		handleNetworkDisconnect(duration, processMgr, metricsCollector)

	default:
		log.Printf("[Chaos] Unknown event type: %s", event)
	}
}

// handleGoroutineChaos handles chaos events for goroutine-level (all-in-one mode).
func handleGoroutineChaos(
	ctx context.Context,
	event coordinator.ChaosEvent,
	params map[string]any,
	registry *coordinator.GoroutineRegistry,
	metricsCollector *metrics.Collector,
	checkpointMgr *coordinator.CheckpointManager,
) {
	switch event {
	case coordinator.WorkerCrashEvent:
		handleWorkerGoroutineCrash(registry, metricsCollector, checkpointMgr)

	case coordinator.ProducerCrashEvent:
		handleProducerGoroutineCrash(registry)

	case coordinator.WorkerRestartEvent:
		// Restart is effectively the same as crash in goroutine mode (cancel context)
		handleWorkerGoroutineCrash(registry, metricsCollector, checkpointMgr)

	case coordinator.ScaleDownEvent:
		count, ok := params["count"].(int)
		if !ok {
			log.Println("[Chaos] Invalid count parameter for scale_down event")
			return
		}
		handleWorkerGoroutineScaleDown(count, registry, metricsCollector, checkpointMgr)

	case coordinator.LeaderFailureEvent:
		// Kill the first active worker (assume it's the leader)
		handleWorkerGoroutineCrash(registry, metricsCollector, checkpointMgr)

	case coordinator.ScaleUpEvent:
		// Scale up not implemented for goroutine mode (requires creating new workers dynamically)
		log.Println("[Chaos] Scale up not supported in all-in-one mode")

	case coordinator.NetworkDisconnectEvent:
		// Network disconnect not applicable in all-in-one mode
		log.Println("[Chaos] Network disconnect not applicable in all-in-one mode")

	default:
		log.Printf("[Chaos] Unknown event type: %s", event)
	}
}

// handleWorkerGoroutineCrash cancels a random worker goroutine.
func handleWorkerGoroutineCrash(
	registry *coordinator.GoroutineRegistry,
	metricsCollector *metrics.Collector,
	checkpointMgr *coordinator.CheckpointManager,
) {
	workers := registry.GetByType(coordinator.WorkerGoroutine)
	if len(workers) == 0 {
		log.Println("[Chaos] No active worker goroutines to crash")
		return
	}

	// Select random worker
	target := workers[time.Now().UnixNano()%int64(len(workers))]

	log.Printf("[Chaos] Crashing worker goroutine: %s", target.ID)

	// Cancel the worker's context
	target.Cancel()
	registry.MarkInactive(target.ID)

	// Update metrics
	if metricsCollector != nil {
		newCount := registry.GetActiveCount(coordinator.WorkerGoroutine)
		metricsCollector.SetWorkersActive(newCount)
		log.Printf("[Chaos] Updated worker count after crash: %d", newCount)
	}

	// Update checkpoint manager
	if checkpointMgr != nil {
		newCount := registry.GetActiveCount(coordinator.WorkerGoroutine)
		checkpointMgr.SetActiveWorkers(newCount)
	}
}

// handleProducerGoroutineCrash cancels a random producer goroutine.
func handleProducerGoroutineCrash(registry *coordinator.GoroutineRegistry) {
	producers := registry.GetByType(coordinator.ProducerGoroutine)
	if len(producers) == 0 {
		log.Println("[Chaos] No active producer goroutines to crash")
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

// handleRestartEvent gracefully restarts a random worker.
func handleRestartEvent(
	ctx context.Context,
	processMgr *coordinator.ProcessManager,
	metricsCollector *metrics.Collector,
) {
	// Get all running workers
	allProcesses := processMgr.ListProcesses()
	var workers []*coordinator.ProcessInfo
	for _, info := range allProcesses {
		if info.Type == coordinator.WorkerProcess && info.Status == coordinator.StatusRunning {
			workers = append(workers, info)
		}
	}

	if len(workers) == 0 {
		log.Println("[Chaos] No running workers to restart")
		return
	}

	// Select random worker
	target := workers[time.Now().UnixNano()%int64(len(workers))]

	log.Printf("[Chaos] Restarting worker: %s", target.ID)

	// Stop gracefully
	if err := processMgr.StopProcess(target.ID, 10*time.Second); err != nil {
		log.Printf("[Chaos] Failed to stop worker %s: %v", target.ID, err)
		return
	}

	// Update worker count after stop
	if metricsCollector != nil {
		workerCount := processMgr.GetWorkerCount()
		metricsCollector.SetWorkersActive(workerCount)
		log.Printf("Updated worker count after stop: %d", workerCount)
	}

	// Wait a bit
	time.Sleep(2 * time.Second)

	// Restart with same ID
	if err := processMgr.StartWorker(ctx, target.ID); err != nil {
		log.Printf("[Chaos] Failed to restart worker %s: %v", target.ID, err)
		return
	}

	// Update worker count after restart
	if metricsCollector != nil {
		workerCount := processMgr.GetWorkerCount()
		metricsCollector.SetWorkersActive(workerCount)
		log.Printf("Updated worker count after restart: %d", workerCount)
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

	for i := 0; i < count; i++ {
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
	stopCount := count
	if stopCount > len(workers) {
		stopCount = len(workers)
	}

	for i := 0; i < stopCount; i++ {
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

	// Get all running workers
	allProcesses := processMgr.ListProcesses()
	var workers []*coordinator.ProcessInfo
	for _, info := range allProcesses {
		if info.Type == coordinator.WorkerProcess && info.Status == coordinator.StatusRunning {
			workers = append(workers, info)
		}
	}

	if len(workers) == 0 {
		log.Println("[Chaos] No running workers for network disconnect")
		return
	}

	// Select random worker
	target := workers[time.Now().UnixNano()%int64(len(workers))]

	log.Printf("[Chaos] Disconnecting worker: %s for %v", target.ID, duration)

	// Kill the process (simulating network disconnect)
	if err := processMgr.KillProcess(target.ID); err != nil {
		log.Printf("[Chaos] Failed to disconnect worker %s: %v", target.ID, err)
		return
	}

	// Update worker count metric
	if metricsCollector != nil {
		workerCount := processMgr.GetWorkerCount()
		metricsCollector.SetWorkersActive(workerCount)
		log.Printf("Updated worker count after network disconnect: %d", workerCount)
	}

	// TODO: In a real implementation, we would reconnect after duration
	// For now, the worker stays disconnected
}
