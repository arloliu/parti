package coordinator

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// ProcessType represents the type of process being managed.
type ProcessType string

const (
	// WorkerProcess represents a worker process.
	WorkerProcess ProcessType = "worker"

	// ProducerProcess represents a producer process.
	ProducerProcess ProcessType = "producer"
)

// ProcessInfo contains information about a managed process.
type ProcessInfo struct {
	ID      string
	Type    ProcessType
	Cmd     *exec.Cmd
	Started time.Time
	Stopped time.Time
	Status  ProcessStatus

	// Stdout is the captured stdout pipe (set when the process was started
	// with IPC capture). The IPC reader goroutine drains it to EOF; the
	// monitor goroutine waits on readerDone before calling Cmd.Wait so the
	// "Wait before all reads complete" race documented in os/exec cannot
	// fire.
	Stdout io.ReadCloser
	// readerDone is closed by the IPC reader goroutine after the stdout
	// pipe has been drained to EOF. Nil when IPC capture is disabled.
	readerDone chan struct{}
	// exited is closed by monitorProcess after Cmd.Wait returns and the
	// process status has been finalised. StopProcess waits on this rather
	// than calling Wait itself, so the two paths cannot double-Wait.
	exited chan struct{}
	// Observer is the per-worker WorkerObserver shim registered into the
	// GoroutineRegistry so the leader/state oracles work in process mode.
	// Nil for non-worker processes or when the orchestrator did not request
	// IPC ingest.
	Observer *ProcessWorkerObserver
}

// ProcessStatus represents the status of a process.
type ProcessStatus string

const (
	// StatusRunning indicates the process is currently running.
	StatusRunning ProcessStatus = "running"

	// StatusStopped indicates the process has stopped gracefully.
	StatusStopped ProcessStatus = "stopped"

	// StatusCrashed indicates the process crashed unexpectedly.
	StatusCrashed ProcessStatus = "crashed"

	// StatusKilled indicates the process was forcibly killed.
	StatusKilled ProcessStatus = "killed"
)

// ProcessIPCSinks bundles the coordinator-side channels and oracles fed by
// the per-worker IPC reader goroutine. Zero-value sinks are valid (each
// non-nil field is independently honored), so a caller can opt in to only
// the surfaces they care about.
//
// The registry is used to register a ProcessWorkerObserver under the worker
// sim ID so LeaderUniquenessWatcher and StateReconcileWatcher can poll it
// exactly like an all-in-one *worker.Worker.
type ProcessIPCSinks struct {
	Registry    *GoroutineRegistry
	Received    chan<- ReceivedMessage
	Assignments chan<- AssignmentReport
	StartLat    chan<- StartLatencyReport
	Degraded    chan<- WorkerDegradedReport
	Smoke       *IPCSmokeOracle
}

// ProcessManager manages worker and producer processes.
type ProcessManager struct {
	mu         sync.RWMutex
	processes  map[string]*ProcessInfo
	binary     string // Path to the simulation binary
	configPath string // Path to config file

	// ipcSinks, when non-nil, causes StartWorker to capture the child's
	// stdout, decode IPC frames, and fan them into the supplied channels /
	// oracle. Process-mode harness setup wires this once at startup; all
	// subsequent StartWorker calls (including chaos-driven respawns) reuse
	// it.
	ipcSinks *ProcessIPCSinks
}

// NewProcessManager creates a new process manager.
//
// Parameters:
//   - binary: Path to the simulation binary
//   - configPath: Path to configuration file
//
// Returns:
//   - *ProcessManager: Initialized process manager
func NewProcessManager(binary, configPath string) *ProcessManager {
	return &ProcessManager{
		processes:  make(map[string]*ProcessInfo),
		binary:     binary,
		configPath: configPath,
	}
}

// StableIDOverrides carries per-worker StableID pool config from the
// chaos dispatcher into a process-mode worker spawn. The zero value
// means "no overrides" and ProcessManager.StartWorker leaves the spawned
// process to inherit cluster defaults from the YAML config.
//
// Non-zero fields are propagated via environment variables (STABLEID_PREFIX,
// STABLEID_MIN, STABLEID_MAX, STABLEID_TTL); the spawned process's
// runWorker parses them back into worker.Config fields, where
// override-if-set semantics in worker.NewWorker preserve sim defaults for
// the zero-value entries (test/simulation/internal/worker/worker.go).
type StableIDOverrides struct {
	Prefix string
	Min    int
	Max    int
	TTL    time.Duration
}

// IsZero returns true when no fields are set; used by callers to decide
// whether to append the four STABLEID_* env vars to the spawned cmd.Env.
func (o StableIDOverrides) IsZero() bool {
	return o.Prefix == "" && o.Min == 0 && o.Max == 0 && o.TTL == 0
}

// SetIPCSinks installs the process-mode IPC ingest sinks. Subsequent
// StartWorker calls capture the child's stdout, parse NDJSON IPC frames, and
// fan them out into sinks.Received / sinks.Assignments / etc.; non-IPC lines
// are forwarded to the orchestrator's log so debugging visibility is
// preserved.
//
// Idempotent: passing nil clears the previously installed sinks. Safe to call
// before any StartWorker; calling after a worker has already been spawned
// does NOT retro-fit IPC capture onto that worker.
func (pm *ProcessManager) SetIPCSinks(sinks *ProcessIPCSinks) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.ipcSinks = sinks
}

// StartWorker starts a new worker process.
//
// Parameters:
//   - ctx: Context for the command
//   - id: Worker ID
//   - overrides: Optional StableID pool overrides (Phase 4c). Zero value =
//     no overrides; the spawned process inherits scenario defaults.
//
// Returns:
//   - error: Error if start fails
func (pm *ProcessManager) StartWorker(ctx context.Context, id string, overrides ...StableIDOverrides) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if _, exists := pm.processes[id]; exists {
		return fmt.Errorf("worker %s already exists", id)
	}

	// Create command
	//nolint:gosec // Subprocess inputs are controlled by simulation configuration
	cmd := exec.CommandContext(ctx, pm.binary,
		"--config", pm.configPath,
		"--mode", "worker",
		"--id", id,
	)

	// Build environment.
	cmd.Env = buildWorkerEnv(id, overrides...)

	// Inherit stderr so the child's log output reaches the orchestrator
	// terminal. (stdout is reserved for IPC frames below.) Without this,
	// crashed children die silently and Phase 7 debugging is brutal.
	cmd.Stderr = os.Stderr

	// Wire stdout capture for IPC if sinks are configured. The pipe MUST be
	// opened before cmd.Start; per os/exec, "it is incorrect to call Wait
	// before all reads from the pipe have completed", so monitorProcess
	// gates Cmd.Wait on the reader goroutine's readerDone channel.
	var stdoutPipe io.ReadCloser
	var observer *ProcessWorkerObserver
	if pm.ipcSinks != nil {
		var err error
		stdoutPipe, err = cmd.StdoutPipe()
		if err != nil {
			return fmt.Errorf("failed to open stdout pipe for worker %s: %w", id, err)
		}
		observer = NewProcessWorkerObserver()
		if pm.ipcSinks.Registry != nil {
			// Register with a no-op cancel so Chaos handlers that call
			// info.Cancel on workers do not panic on the shim. The
			// observer's Active flag is toggled in monitorProcess on
			// process exit.
			pm.ipcSinks.Registry.Register(id, WorkerGoroutine, func() {}, nil, observer)
		}
	}

	// Start the process
	if err := cmd.Start(); err != nil {
		if stdoutPipe != nil {
			_ = stdoutPipe.Close()
		}
		if observer != nil && pm.ipcSinks != nil && pm.ipcSinks.Registry != nil {
			pm.ipcSinks.Registry.Unregister(id)
		}
		return fmt.Errorf("failed to start worker %s: %w", id, err)
	}

	// Track the process
	info := &ProcessInfo{
		ID:       id,
		Type:     WorkerProcess,
		Cmd:      cmd,
		Started:  time.Now(),
		Status:   StatusRunning,
		Stdout:   stdoutPipe,
		Observer: observer,
		exited:   make(chan struct{}),
	}
	if stdoutPipe != nil {
		info.readerDone = make(chan struct{})
	}
	pm.processes[id] = info

	// Launch the IPC reader BEFORE the monitor so monitorProcess can wait
	// on readerDone safely (reader goroutine always closes the channel
	// even on early errors).
	if stdoutPipe != nil {
		sinks := pm.ipcSinks
		go pm.ipcReader(id, stdoutPipe, info.readerDone, sinks, observer)
	}

	// Monitor process in background
	go pm.monitorProcess(id)

	log.Printf("[ProcessManager] Started worker %s (PID: %d)", id, cmd.Process.Pid)

	return nil
}

// ExportedBuildWorkerEnv is a test-facing wrapper around the unexported
// buildWorkerEnv. Used by cross-package tests (e.g. cmd/simulation
// round-trip) to validate the env transport without forcing
// buildWorkerEnv itself to be exported.
func ExportedBuildWorkerEnv(id string, overrides ...StableIDOverrides) []string {
	return buildWorkerEnv(id, overrides...)
}

// buildWorkerEnv composes the env slice passed to a spawned worker process.
// Always sets WORKER_ID. When the first overrides entry is non-zero, also
// sets STABLEID_PREFIX, STABLEID_MIN, STABLEID_MAX, STABLEID_TTL so the
// spawned process's runWorker can plumb them into worker.Config.
//
// Extracted to a free function so a unit test can validate the env shape
// without spawning a real process.
func buildWorkerEnv(id string, overrides ...StableIDOverrides) []string {
	env := append(os.Environ(),
		fmt.Sprintf("WORKER_ID=%s", id),
	)
	if len(overrides) == 0 || overrides[0].IsZero() {
		return env
	}
	o := overrides[0]
	// Each field is only set when non-zero so the receiving worker.Config
	// override-if-set semantics restore the appropriate sim default for
	// any field the caller left unspecified.
	if o.Prefix != "" {
		env = append(env, fmt.Sprintf("STABLEID_PREFIX=%s", o.Prefix))
	}
	if o.Min != 0 {
		env = append(env, fmt.Sprintf("STABLEID_MIN=%d", o.Min))
	}
	if o.Max != 0 {
		env = append(env, fmt.Sprintf("STABLEID_MAX=%d", o.Max))
	}
	if o.TTL != 0 {
		env = append(env, fmt.Sprintf("STABLEID_TTL=%s", o.TTL.String()))
	}

	return env
}

// StartProducer starts a new producer process.
//
// Parameters:
//   - ctx: Context for the command
//   - id: Producer ID
//
// Returns:
//   - error: Error if start fails
func (pm *ProcessManager) StartProducer(ctx context.Context, id string) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if _, exists := pm.processes[id]; exists {
		return fmt.Errorf("producer %s already exists", id)
	}

	// Create command
	//nolint:gosec // Subprocess inputs are controlled by simulation configuration
	cmd := exec.CommandContext(ctx, pm.binary,
		"--config", pm.configPath,
		"--mode", "producer",
		"--id", id,
	)

	// Set environment variables
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("PRODUCER_ID=%s", id),
	)

	// Start the process
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start producer %s: %w", id, err)
	}

	// Track the process
	info := &ProcessInfo{
		ID:      id,
		Type:    ProducerProcess,
		Cmd:     cmd,
		Started: time.Now(),
		Status:  StatusRunning,
		exited:  make(chan struct{}),
	}
	pm.processes[id] = info

	// Monitor process in background
	go pm.monitorProcess(id)

	log.Printf("[ProcessManager] Started producer %s (PID: %d)", id, cmd.Process.Pid)

	return nil
}

// StopProcess gracefully stops a process.
//
// Parameters:
//   - id: Process ID
//   - timeout: Timeout for graceful shutdown
//
// Returns:
//   - error: Error if stop fails
func (pm *ProcessManager) StopProcess(id string, timeout time.Duration) error {
	pm.mu.RLock()
	info, exists := pm.processes[id]
	pm.mu.RUnlock()

	if !exists {
		return fmt.Errorf("process %s not found", id)
	}

	if info.Status != StatusRunning {
		return fmt.Errorf("process %s is not running (status: %s)", id, info.Status)
	}

	log.Printf("[ProcessManager] Stopping %s %s gracefully", info.Type, id)

	// Send SIGTERM for graceful shutdown
	if err := info.Cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("failed to send SIGTERM to %s: %w", id, err)
	}

	// Wait on monitorProcess's exit channel rather than calling Cmd.Wait
	// directly. Only one goroutine may call Wait per Cmd; monitorProcess
	// (started in Start{Worker,Producer}) is that goroutine.
	if info.exited != nil {
		select {
		case <-info.exited:
			log.Printf("[ProcessManager] %s %s stopped", info.Type, id)
			return nil
		case <-time.After(timeout):
			log.Printf("[ProcessManager] Timeout waiting for %s %s, force killing", info.Type, id)
			return pm.KillProcess(id)
		}
	}
	// Backward compatibility: pre-IPC code paths constructed ProcessInfo
	// without info.exited. Fall back to the original Wait() pattern so
	// any legacy caller still drains.
	done := make(chan error, 1)
	go func() {
		done <- info.Cmd.Wait()
	}()
	select {
	case err := <-done:
		pm.mu.Lock()
		info.Stopped = time.Now()
		if err != nil {
			info.Status = StatusCrashed
		} else {
			info.Status = StatusStopped
		}
		pm.mu.Unlock()
		log.Printf("[ProcessManager] %s %s stopped", info.Type, id)
		return nil
	case <-time.After(timeout):
		log.Printf("[ProcessManager] Timeout waiting for %s %s, force killing", info.Type, id)
		return pm.KillProcess(id)
	}
}

// KillProcess forcibly kills a process (SIGKILL).
//
// Parameters:
//   - id: Process ID
//
// Returns:
//   - error: Error if kill fails
func (pm *ProcessManager) KillProcess(id string) error {
	pm.mu.RLock()
	info, exists := pm.processes[id]
	pm.mu.RUnlock()

	if !exists {
		return fmt.Errorf("process %s not found", id)
	}

	if info.Status != StatusRunning {
		return fmt.Errorf("process %s is not running (status: %s)", id, info.Status)
	}

	log.Printf("[ProcessManager] Killing %s %s with SIGKILL", info.Type, id)

	// Send SIGKILL
	if err := info.Cmd.Process.Kill(); err != nil {
		return fmt.Errorf("failed to kill %s: %w", id, err)
	}

	pm.mu.Lock()
	info.Stopped = time.Now()
	info.Status = StatusKilled
	pm.mu.Unlock()

	log.Printf("[ProcessManager] %s %s killed", info.Type, id)

	return nil
}

// SignalProcess sends a signal to a process.
//
// Parameters:
//   - id: Process ID
//   - sig: Signal to send
//
// Returns:
//   - error: Error if signal fails
func (pm *ProcessManager) SignalProcess(id string, sig syscall.Signal) error {
	pm.mu.RLock()
	info, exists := pm.processes[id]
	pm.mu.RUnlock()

	if !exists {
		return fmt.Errorf("process %s not found", id)
	}

	if info.Status != StatusRunning {
		return fmt.Errorf("process %s is not running (status: %s)", id, info.Status)
	}

	log.Printf("[ProcessManager] Sending signal %s to %s %s", sig, info.Type, id)

	if err := info.Cmd.Process.Signal(sig); err != nil {
		return fmt.Errorf("failed to send signal %s to %s: %w", sig, id, err)
	}

	return nil
}

// GetProcessInfo returns information about a process.
//
// Parameters:
//   - id: Process ID
//
// Returns:
//   - *ProcessInfo: Process information
//   - bool: true if process exists
func (pm *ProcessManager) GetProcessInfo(id string) (*ProcessInfo, bool) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	info, exists := pm.processes[id]

	return info, exists
}

// ListProcesses returns all managed processes.
//
// Returns:
//   - map[string]*ProcessInfo: Map of process ID to process info
func (pm *ProcessManager) ListProcesses() map[string]*ProcessInfo {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	// Create a copy to avoid race conditions
	result := make(map[string]*ProcessInfo, len(pm.processes))
	maps.Copy(result, pm.processes)

	return result
}

// GetRunningProcesses returns all currently running processes.
//
// Returns:
//   - []*ProcessInfo: List of running processes
func (pm *ProcessManager) GetRunningProcesses() []*ProcessInfo {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	var running []*ProcessInfo
	for _, info := range pm.processes {
		if info.Status == StatusRunning {
			running = append(running, info)
		}
	}

	return running
}

// StopAll stops all managed processes.
//
// Parameters:
//   - timeout: Timeout for each process
//
// Returns:
//   - error: Error if any stop fails
func (pm *ProcessManager) StopAll(timeout time.Duration) error {
	log.Println("[ProcessManager] Stopping all processes")

	processes := pm.GetRunningProcesses()

	var wg sync.WaitGroup
	errCh := make(chan error, len(processes))

	for _, info := range processes {
		wg.Go(func() {
			if err := pm.StopProcess(info.ID, timeout); err != nil {
				errCh <- fmt.Errorf("failed to stop %s: %w", info.ID, err)
			}
		})
	}

	wg.Wait()
	close(errCh)

	// Collect any errors
	errs := make([]error, 0, len(processes))
	for err := range errCh {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("errors stopping processes: %v", errs)
	}

	log.Println("[ProcessManager] All processes stopped")

	return nil
}

// monitorProcess monitors a process and updates its status.
func (pm *ProcessManager) monitorProcess(id string) {
	pm.mu.RLock()
	info, exists := pm.processes[id]
	sinks := pm.ipcSinks
	pm.mu.RUnlock()

	if !exists {
		return
	}

	// If IPC capture is enabled, the os/exec contract requires the stdout
	// pipe to be drained to EOF before Cmd.Wait returns. ipcReader closes
	// readerDone once it finishes; we block here so we cannot beat it to
	// Wait.
	if info.readerDone != nil {
		<-info.readerDone
	}

	// Wait for process to exit. monitorProcess is the ONLY caller of
	// Cmd.Wait (StopProcess waits on info.exited instead) so the
	// pre-existing double-Wait race is closed.
	err := info.Cmd.Wait()

	pm.mu.Lock()
	info.Stopped = time.Now()
	if err != nil {
		info.Status = StatusCrashed
		log.Printf("[ProcessManager] %s %s crashed: %v", info.Type, id, err)
	} else {
		info.Status = StatusStopped
		log.Printf("[ProcessManager] %s %s exited normally", info.Type, id)
	}
	if info.exited != nil {
		close(info.exited)
	}
	pm.mu.Unlock()

	// Mark the observer entry inactive in the registry so registry-polling
	// oracles (LeaderUniquenessWatcher, StateReconcileWatcher) drop the
	// dead worker. Unregister entirely so a respawn of the same sim ID
	// (Phase 7b: stableid_tiny_pool_respawn) can re-register a fresh
	// observer without colliding.
	if sinks != nil && sinks.Registry != nil && info.Observer != nil {
		sinks.Registry.Unregister(id)
	}
}

// ipcReader drains the worker's stdout pipe, decodes IPC frames, and fans
// them into the coordinator-side sinks. Non-IPC lines are forwarded to the
// orchestrator's log so debugging output is preserved. The function returns
// (closing readerDone) when the pipe hits EOF or any unrecoverable read
// error — Cmd.Wait then runs in monitorProcess.
func (pm *ProcessManager) ipcReader(workerID string, pipe io.ReadCloser, readerDone chan<- struct{}, sinks *ProcessIPCSinks, observer *ProcessWorkerObserver) {
	defer close(readerDone)
	defer func() { _ = pipe.Close() }()
	scanner := bufio.NewScanner(pipe)
	// The default 64KB scanner buffer can drop long lines under bursts of
	// received_message frames; bump to 1MB so a multi-K-element assignment
	// snapshot never silently truncates.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		frame, err := DecodeIPCLine(line)
		if err != nil {
			if errors.Is(err, ErrNotIPC) {
				// Forward plain log lines so existing debugging
				// flows still surface in the orchestrator log.
				log.Printf("[worker:%s] %s", workerID, line)
				continue
			}
			log.Printf("[ProcessManager] worker=%s ipc decode error: %v line=%q", workerID, err, line)
			continue
		}
		pm.dispatchIPCFrame(workerID, frame, sinks, observer)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		log.Printf("[ProcessManager] worker=%s ipc reader exited: %v", workerID, err)
	}
}

// dispatchIPCFrame fans a decoded frame out to the configured sinks plus the
// per-worker observer. All sends are non-blocking where the sink is bounded;
// drops are logged so the smoke oracle can still surface a violation.
func (pm *ProcessManager) dispatchIPCFrame(workerID string, frame *IPCFrame, sinks *ProcessIPCSinks, observer *ProcessWorkerObserver) {
	if sinks == nil || frame == nil {
		return
	}
	frameAt := time.UnixMilli(frame.AtMS)
	if frameAt.IsZero() {
		frameAt = time.Now()
	}
	if observer != nil && frame.StableID != "" {
		observer.SetStableID(frame.StableID)
	}
	if sinks.Smoke != nil {
		sinks.Smoke.Ingest(workerID, frame.Kind, frameAt)
	}
	switch frame.Kind {
	case IPCKindReceivedMessage:
		var p IPCPayloadReceivedMessage
		if err := json.Unmarshal(frame.Payload, &p); err != nil {
			log.Printf("[ProcessManager] worker=%s decode received_message: %v", workerID, err)
			return
		}
		if sinks.Received != nil {
			select {
			case sinks.Received <- ReceivedMessage{
				PartitionID:       p.PartitionID,
				PartitionSequence: p.PartitionSequence,
				WorkerID:          workerID,
			}:
			default:
				log.Printf("[ProcessManager] worker=%s received channel full; dropping frame", workerID)
			}
		}
	case IPCKindAssignmentReport:
		var p IPCPayloadAssignmentReport
		if err := json.Unmarshal(frame.Payload, &p); err != nil {
			log.Printf("[ProcessManager] worker=%s decode assignment_report: %v", workerID, err)
			return
		}
		if sinks.Assignments != nil {
			select {
			case sinks.Assignments <- AssignmentReport{
				WorkerID:   workerID,
				Partitions: p.Partitions,
			}:
			default:
				log.Printf("[ProcessManager] worker=%s assignments channel full; dropping frame", workerID)
			}
		}
	case IPCKindStartLatency:
		var p IPCPayloadStartLatency
		if err := json.Unmarshal(frame.Payload, &p); err != nil {
			log.Printf("[ProcessManager] worker=%s decode start_latency: %v", workerID, err)
			return
		}
		if sinks.StartLat != nil {
			select {
			case sinks.StartLat <- StartLatencyReport{
				WorkerID:        workerID,
				Latency:         time.Duration(p.LatencyMS) * time.Millisecond,
				IsInitialCohort: p.IsInitialCohort,
			}:
			default:
				log.Printf("[ProcessManager] worker=%s start_lat channel full; dropping frame", workerID)
			}
		}
	case IPCKindState:
		var p IPCPayloadState
		if err := json.Unmarshal(frame.Payload, &p); err != nil {
			log.Printf("[ProcessManager] worker=%s decode state: %v", workerID, err)
			return
		}
		if observer != nil {
			observer.SetState(p.State)
		}
	case IPCKindLeader:
		var p IPCPayloadLeader
		if err := json.Unmarshal(frame.Payload, &p); err != nil {
			log.Printf("[ProcessManager] worker=%s decode leader: %v", workerID, err)
			return
		}
		if observer != nil {
			observer.SetLeader(p.IsLeader)
		}
	case IPCKindDegraded:
		var p IPCPayloadDegraded
		if err := json.Unmarshal(frame.Payload, &p); err != nil {
			log.Printf("[ProcessManager] worker=%s decode degraded: %v", workerID, err)
			return
		}
		if sinks.Degraded != nil {
			select {
			case sinks.Degraded <- WorkerDegradedReport{
				WorkerID: frame.StableID,
				Reason:   p.Reason,
				At:       frameAt,
			}:
			default:
				log.Printf("[ProcessManager] worker=%s degraded channel full; dropping frame", workerID)
			}
		}
	default:
		log.Printf("[ProcessManager] worker=%s unknown IPC kind=%q", workerID, frame.Kind)
	}
}

// GetWorkerCount returns the number of running workers.
//
// Returns:
//   - int: Number of running workers
func (pm *ProcessManager) GetWorkerCount() int {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	count := 0
	for _, info := range pm.processes {
		if info.Type == WorkerProcess && info.Status == StatusRunning {
			count++
		}
	}

	return count
}

// GetProducerCount returns the number of running producers.
//
// Returns:
//   - int: Number of running producers
func (pm *ProcessManager) GetProducerCount() int {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	count := 0
	for _, info := range pm.processes {
		if info.Type == ProducerProcess && info.Status == StatusRunning {
			count++
		}
	}

	return count
}

// SelectRandomWorker selects a random running worker.
//
// Returns:
//   - string: Worker ID (empty if no workers running)
func (pm *ProcessManager) SelectRandomWorker() string {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	var workers []string
	for id, info := range pm.processes {
		if info.Type == WorkerProcess && info.Status == StatusRunning {
			workers = append(workers, id)
		}
	}

	if len(workers) == 0 {
		return ""
	}

	return workers[time.Now().UnixNano()%int64(len(workers))]
}

// SelectRandomProducer selects a random running producer.
//
// Returns:
//   - string: Producer ID (empty if no producers running)
func (pm *ProcessManager) SelectRandomProducer() string {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	var producers []string
	for id, info := range pm.processes {
		if info.Type == ProducerProcess && info.Status == StatusRunning {
			producers = append(producers, id)
		}
	}

	if len(producers) == 0 {
		return ""
	}

	return producers[time.Now().UnixNano()%int64(len(producers))]
}
