package coordinator

import (
	"context"
	"fmt"
	"log"
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

// ProcessManager manages worker and producer processes.
type ProcessManager struct {
	mu         sync.RWMutex
	processes  map[string]*ProcessInfo
	binary     string // Path to the simulation binary
	configPath string // Path to config file
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

// StartWorker starts a new worker process.
//
// Parameters:
//   - ctx: Context for the command
//   - id: Worker ID
//
// Returns:
//   - error: Error if start fails
func (pm *ProcessManager) StartWorker(ctx context.Context, id string) error {
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

	// Set environment variables
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("WORKER_ID=%s", id),
	)

	// Start the process
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start worker %s: %w", id, err)
	}

	// Track the process
	info := &ProcessInfo{
		ID:      id,
		Type:    WorkerProcess,
		Cmd:     cmd,
		Started: time.Now(),
		Status:  StatusRunning,
	}
	pm.processes[id] = info

	// Monitor process in background
	go pm.monitorProcess(id)

	log.Printf("[ProcessManager] Started worker %s (PID: %d)", id, cmd.Process.Pid)

	return nil
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

	// Wait for process to exit with timeout
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
		// Force kill if timeout exceeded
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
	for k, v := range pm.processes {
		result[k] = v
	}

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
	pm.mu.RUnlock()

	if !exists {
		return
	}

	// Wait for process to exit
	err := info.Cmd.Wait()

	pm.mu.Lock()
	defer pm.mu.Unlock()

	info.Stopped = time.Now()
	if err != nil {
		info.Status = StatusCrashed
		log.Printf("[ProcessManager] %s %s crashed: %v", info.Type, id, err)
	} else {
		info.Status = StatusStopped
		log.Printf("[ProcessManager] %s %s exited normally", info.Type, id)
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
