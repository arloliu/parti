package coordinator

import (
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProcessManager_SignalProcess(t *testing.T) {
	// Create a dummy process that sleeps
	// We use "sleep" command which is available on Linux
	binary, err := exec.LookPath("sleep")
	require.NoError(t, err)

	pm := NewProcessManager(binary, "dummy_config")

	// Manually start a process using the internal logic style
	// Since StartWorker expects specific flags for the simulation binary,
	// we'll bypass it and use a custom command for testing,
	// or we can mock the binary.
	// Actually, ProcessManager.StartWorker constructs specific args.
	// So we can't easily use "sleep" with StartWorker.
	// We should probably add a generic StartProcess or just manually populate the map for testing.

	ctx := t.Context()

	cmd := exec.CommandContext(ctx, "sleep", "10")
	err = cmd.Start()
	require.NoError(t, err)

	id := "test-process-1"
	pm.mu.Lock()
	pm.processes[id] = &ProcessInfo{
		ID:      id,
		Type:    WorkerProcess,
		Cmd:     cmd,
		Started: time.Now(),
		Status:  StatusRunning,
	}
	pm.mu.Unlock()

	// Test sending SIGSTOP (Pause)
	err = pm.SignalProcess(id, syscall.SIGSTOP)
	require.NoError(t, err)

	// Verify process is still running (but stopped)
	// In Linux, a stopped process is still "running" in terms of existence,
	// but its state in /proc/[pid]/stat would be 'T'.
	// We can't easily check that without platform specific code,
	// but we can check that SignalProcess didn't return error.

	// Test sending SIGCONT (Resume)
	err = pm.SignalProcess(id, syscall.SIGCONT)
	require.NoError(t, err)

	// Test sending SIGTERM (Stop)
	err = pm.StopProcess(id, 1*time.Second)
	require.NoError(t, err)

	// Verify status updated
	info, exists := pm.GetProcessInfo(id)
	require.True(t, exists)
	// sleep command returns error when terminated by signal, so it might be marked as crashed
	require.Contains(t, []ProcessStatus{StatusStopped, StatusCrashed}, info.Status)
}
