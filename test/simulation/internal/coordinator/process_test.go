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

// TestProcessManager_KillProcess_PreservesStatusKilled is the regression
// test for the P2 post-impl-review finding: KillProcess set StatusKilled,
// but monitorProcess later called Cmd.Wait, observed the non-nil
// "signal: killed" error, and unconditionally overwrote the status to
// StatusCrashed. The killRequested flag now distinguishes the intentional
// kill so the final terminal status remains StatusKilled.
func TestProcessManager_KillProcess_PreservesStatusKilled(t *testing.T) {
	binary, err := exec.LookPath("sleep")
	require.NoError(t, err)

	pm := NewProcessManager(binary, "dummy_config")
	ctx := t.Context()

	cmd := exec.CommandContext(ctx, "sleep", "30")
	err = cmd.Start()
	require.NoError(t, err)

	id := "test-kill-preserve"
	exited := make(chan struct{})
	pm.mu.Lock()
	pm.processes[id] = &ProcessInfo{
		ID:      id,
		Type:    WorkerProcess,
		Cmd:     cmd,
		Started: time.Now(),
		Status:  StatusRunning,
		exited:  exited,
		// readerDone left nil — no IPC capture in this test.
	}
	pm.mu.Unlock()

	// Start the monitor goroutine that races KillProcess: it will call
	// Cmd.Wait, observe "signal: killed", and (with the fix) preserve
	// StatusKilled.
	go pm.monitorProcess(id)

	err = pm.KillProcess(id)
	require.NoError(t, err)

	// Wait for the monitor goroutine to finalise the status.
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("monitorProcess did not finalise status within 5s")
	}

	info, exists := pm.GetProcessInfo(id)
	require.True(t, exists)
	require.Equal(t, StatusKilled, info.Status,
		"intentional SIGKILL must preserve StatusKilled even after Cmd.Wait observes the signal-killed error")
}
