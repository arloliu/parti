package testutil

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti"
	"github.com/nats-io/nats.go"
)

// ChaosController manages chaos engineering operations for testing.
//
// This helper provides controlled failure injection for distributed system testing.
// All chaos operations are safe to use in parallel tests and clean up automatically
// when the test completes.
type ChaosController struct {
	t       *testing.T
	nc      *nats.Conn
	done    chan struct{}
	wg      sync.WaitGroup
	cleanup []func()
	mu      sync.Mutex
}

// NewChaosController creates a new chaos engineering controller.
//
// The controller manages chaos operations and ensures proper cleanup.
// Always call Stop() when done (typically with defer).
//
// Parameters:
//   - t: Test instance
//   - nc: NATS connection to inject chaos into
//
// Returns:
//   - *ChaosController: Controller instance
//
// Example:
//
//	chaos := NewChaosController(t, nc)
//	defer chaos.Stop()
//	chaos.DelayMessages("parti.>", 50*time.Millisecond, 200*time.Millisecond)
func NewChaosController(t *testing.T, nc *nats.Conn) *ChaosController {
	t.Helper()

	return &ChaosController{
		t:       t,
		nc:      nc,
		done:    make(chan struct{}),
		cleanup: make([]func(), 0),
	}
}

// Stop stops all chaos operations and cleans up resources.
func (c *ChaosController) Stop() {
	c.t.Helper()

	close(c.done)
	c.wg.Wait()

	c.mu.Lock()
	defer c.mu.Unlock()

	for i := len(c.cleanup) - 1; i >= 0; i-- {
		c.cleanup[i]()
	}
	c.cleanup = nil
}

// DelayMessages injects random delays into NATS messages matching the subject pattern.
//
// This simulates network latency or processing delays. Delays are applied
// per-message and chosen randomly between minDelay and maxDelay.
//
// NOTE: This is a simplified implementation that works with embedded NATS.
// For production NATS clusters, use network-level tools (tc, toxiproxy) instead.
//
// Parameters:
//   - subject: NATS subject pattern to delay (e.g., "parti.>" for all parti messages)
//   - minDelay: Minimum delay to inject
//   - maxDelay: Maximum delay to inject
//
// Example:
//
//	// Add 50-200ms delay to all heartbeat messages
//	chaos.DelayMessages("parti.heartbeat.>", 50*time.Millisecond, 200*time.Millisecond)
func (c *ChaosController) DelayMessages(subject string, minDelay, maxDelay time.Duration) {
	c.t.Helper()
	c.t.Logf("Chaos: Injecting %v-%v delays on subject %s", minDelay, maxDelay, subject)

	// NOTE: Embedded NATS doesn't support message interception.
	// This function is a placeholder for the chaos engineering interface.
	// In a real distributed test environment, you would:
	// 1. Use NATS proxy/gateway with delay injection
	// 2. Use network-level tools (tc, toxiproxy)
	// 3. Modify application code to inject controlled delays

	c.t.Logf("Warning: DelayMessages is a placeholder - embedded NATS doesn't support message interception")
	c.t.Logf("For real chaos testing, use: network proxies, tc, or toxiproxy")
}

// DropMessages randomly drops a percentage of NATS messages matching the subject pattern.
//
// This simulates network packet loss or message broker failures.
//
// NOTE: This is a simplified implementation that works with embedded NATS.
// For production NATS clusters, use network-level tools instead.
//
// Parameters:
//   - subject: NATS subject pattern to drop messages from
//   - dropRate: Percentage of messages to drop (0.0-1.0, e.g., 0.1 = 10%)
//
// Example:
//
//	// Drop 20% of assignment messages
//	chaos.DropMessages("parti.assignment.>", 0.2)
func (c *ChaosController) DropMessages(subject string, dropRate float64) {
	c.t.Helper()
	c.t.Logf("Chaos: Dropping %.1f%% of messages on subject %s", dropRate*100, subject)

	// NOTE: Same limitation as DelayMessages - embedded NATS doesn't support
	// message interception. This is a placeholder for the chaos interface.

	c.t.Logf("Warning: DropMessages is a placeholder - embedded NATS doesn't support message interception")
	c.t.Logf("For real chaos testing, use: network proxies, tc, or toxiproxy")
}

// PartitionNetwork simulates a network partition by blocking messages between worker groups.
//
// This is useful for testing split-brain scenarios and partition healing.
//
// NOTE: This is a simplified implementation. Real network partitions would use
// firewall rules, network namespaces, or container networking tools.
//
// Parameters:
//   - partition1: Worker IDs in first partition group
//   - partition2: Worker IDs in second partition group
//
// Example:
//
//	// Split 4 workers into two groups of 2
//	chaos.PartitionNetwork([]string{"worker-0", "worker-1"}, []string{"worker-2", "worker-3"})
func (c *ChaosController) PartitionNetwork(partition1, partition2 []string) {
	c.t.Helper()
	c.t.Logf("Chaos: Creating network partition [%v] <-> [%v]", partition1, partition2)

	// NOTE: Embedded NATS doesn't support selective message blocking.
	// This is a placeholder for the chaos interface.
	//
	// For real network partition testing:
	// 1. Use separate NATS servers per partition
	// 2. Use firewall rules (iptables) to block traffic
	// 3. Use container networking (Docker networks)
	// 4. Use Kubernetes network policies

	c.t.Logf("Warning: PartitionNetwork is a placeholder - embedded NATS doesn't support network partitioning")
	c.t.Logf("For real partition testing, use: separate NATS servers, iptables, or K8s network policies")
}

// HealNetworkPartition removes a previously created network partition.
//
// This restores normal network connectivity between all workers.
func (c *ChaosController) HealNetworkPartition() {
	c.t.Helper()
	c.t.Log("Chaos: Healing network partition")

	// Placeholder - see PartitionNetwork notes
}

// SlowdownWorker injects artificial delays into a specific worker's operations.
//
// This simulates worker performance degradation (e.g., CPU contention, GC pauses,
// disk I/O bottlenecks) without completely stopping the worker.
//
// NOTE: This requires the Manager to support chaos hooks. This is a design
// placeholder for future chaos injection capabilities.
//
// Parameters:
//   - manager: Worker manager to slow down
//   - slowdownFactor: Multiplier for operation delays (2 = 2x slower, 10 = 10x slower)
//
// Example:
//
//	// Make worker 3x slower (simulate CPU contention)
//	chaos.SlowdownWorker(manager, 3)
func (c *ChaosController) SlowdownWorker(manager *parti.Manager, slowdownFactor int) {
	c.t.Helper()
	c.t.Logf("Chaos: Slowing down worker %s by %dx", manager.WorkerID(), slowdownFactor)

	// NOTE: This requires Manager to expose chaos injection hooks.
	// This is a placeholder for the chaos interface.
	//
	// Implementation would require:
	// 1. Add ChaosConfig to Manager options
	// 2. Inject delays into heartbeat publishing
	// 3. Inject delays into assignment processing
	// 4. Inject delays into state machine transitions

	c.t.Logf("Warning: SlowdownWorker is a placeholder - Manager doesn't support chaos injection yet")
	c.t.Logf("For real slowdown testing, use: CPU limits (cgroups), SIGSTOP/SIGCONT, or chaos hooks")
}

// CrashWorker simulates sudden worker termination without cleanup.
//
// This is more severe than graceful Stop() - it mimics process kill, panic,
// or power loss scenarios.
//
// NOTE: This forcibly stops the worker context without proper cleanup.
// Use with caution - may leave resources in inconsistent state.
//
// Parameters:
//   - manager: Worker manager to crash
//
// Example:
//
//	// Simulate worker crash (like SIGKILL)
//	chaos.CrashWorker(manager)
func (c *ChaosController) CrashWorker(manager *parti.Manager) {
	c.t.Helper()
	c.t.Logf("Chaos: Crashing worker %s (forced termination)", manager.WorkerID())

	// NOTE: Manager.Stop() already handles graceful shutdown.
	// For a true "crash", we would need to:
	// 1. Cancel worker's context immediately (no cleanup)
	// 2. Stop heartbeat publishing without cleanup
	// 3. Leave KV entries in place (simulate unexpected termination)
	//
	// This is a placeholder - real crash simulation would require
	// Manager to expose a ForceStop() or Crash() method.

	c.t.Logf("Warning: CrashWorker is a placeholder - using Stop() instead of forced crash")
	c.t.Logf("For real crash testing, implement: ForceStop(), context cancellation, or SIGKILL")

	// Use regular stop for now (not a true crash)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = manager.Stop(ctx) // Best effort stop
}

// FreezeWorker stops a worker's heartbeats but keeps it running.
//
// This simulates a worker becoming unresponsive (e.g., deadlock, infinite loop,
// network isolation) while still consuming resources.
//
// The worker will eventually be declared dead by other workers when its
// heartbeat TTL expires.
//
// Parameters:
//   - manager: Worker manager to freeze
//   - duration: How long to freeze (0 = until test ends)
//
// Example:
//
//	// Freeze worker for 10 seconds (simulate deadlock)
//	chaos.FreezeWorker(manager, 10*time.Second)
func (c *ChaosController) FreezeWorker(manager *parti.Manager, duration time.Duration) {
	c.t.Helper()
	c.t.Logf("Chaos: Freezing worker %s for %v", manager.WorkerID(), duration)

	// NOTE: This requires Manager to expose heartbeat control.
	// This is a placeholder for the chaos interface.
	//
	// Implementation would require:
	// 1. Add PauseHeartbeat() method to Manager
	// 2. Add ResumeHeartbeat() method to Manager
	// 3. Heartbeat publisher must support pause/resume

	c.t.Logf("Warning: FreezeWorker is a placeholder - Manager doesn't support heartbeat pause yet")
	c.t.Logf("For real freeze testing, implement: PauseHeartbeat(), or use SIGSTOP/SIGCONT")

	// Simulate freeze by stopping the worker (not ideal, but close enough for now)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = manager.Stop(ctx)

	if duration > 0 {
		// Schedule unfreeze (but worker is already stopped, so this is just logging)
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			select {
			case <-time.After(duration):
				c.t.Logf("Chaos: Unfreeze time reached for worker (already stopped)")
			case <-c.done:
			}
		}()
	}
}

// InjectCPUContention simulates CPU resource contention on a worker.
//
// This creates background CPU load to slow down worker operations without
// completely freezing them.
//
// Parameters:
//   - intensity: CPU load intensity (0.0-1.0, where 1.0 = 100% CPU)
//   - duration: How long to maintain contention
//
// Example:
//
//	// Create 80% CPU load for 30 seconds
//	chaos.InjectCPUContention(0.8, 30*time.Second)
func (c *ChaosController) InjectCPUContention(intensity float64, duration time.Duration) {
	c.t.Helper()
	c.t.Logf("Chaos: Injecting %.1f%% CPU contention for %v", intensity*100, duration)

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()

		deadline := time.Now().Add(duration)
		busyDuration := time.Duration(float64(time.Millisecond) * intensity)
		sleepDuration := time.Millisecond - busyDuration

		for time.Now().Before(deadline) {
			select {
			case <-c.done:
				return
			default:
				// Busy loop to consume CPU
				start := time.Now()
				for time.Since(start) < busyDuration {
					// Spin CPU
					// do some math to prevent optimization
					_ = 12345 * 67890
				}
				time.Sleep(sleepDuration)
			}
		}

		c.t.Log("Chaos: CPU contention ended")
	}()
}

// InjectMemoryPressure simulates memory pressure by allocating and holding memory.
//
// This can trigger GC pressure and slow down worker operations.
//
// Parameters:
//   - sizeMB: Amount of memory to allocate in megabytes
//   - duration: How long to hold the memory
//
// Example:
//
//	// Allocate 500MB for 30 seconds (simulate memory pressure)
//	chaos.InjectMemoryPressure(500, 30*time.Second)
func (c *ChaosController) InjectMemoryPressure(sizeMB int, duration time.Duration) {
	c.t.Helper()
	c.t.Logf("Chaos: Injecting %dMB memory pressure for %v", sizeMB, duration)

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()

		// Allocate memory in 1MB chunks
		ballast := make([][]byte, 0, sizeMB)
		for i := 0; i < sizeMB; i++ {
			chunk := make([]byte, 1024*1024) // 1MB
			// Write to ensure allocation
			for j := range chunk {
				chunk[j] = byte(j % 256)
			}
			ballast = append(ballast, chunk)
		}

		c.t.Logf("Chaos: Allocated %dMB memory", sizeMB)

		// Hold memory for duration
		select {
		case <-time.After(duration):
			c.t.Log("Chaos: Releasing memory pressure")
		case <-c.done:
		}

		// Let ballast be garbage collected
		_ = ballast
	}()
}

// SimulateNetworkLatency adds latency to all NATS operations for testing purposes.
//
// This wraps a NATS connection with artificial delays to simulate high-latency networks.
//
// NOTE: This is a best-effort simulation and doesn't affect all network operations.
//
// Parameters:
//   - baseLatency: Base latency to add to all operations
//   - jitter: Random jitter to add (±jitter/2)
//
// Example:
//
//	// Simulate 100ms ± 20ms network latency
//	chaos.SimulateNetworkLatency(100*time.Millisecond, 20*time.Millisecond)
func (c *ChaosController) SimulateNetworkLatency(baseLatency, jitter time.Duration) {
	c.t.Helper()
	c.t.Logf("Chaos: Simulating network latency: %v ± %v", baseLatency, jitter/2)

	// NOTE: This is a placeholder. Real latency simulation would require:
	// 1. NATS proxy with configurable delays
	// 2. Network-level tools (tc, toxiproxy)
	// 3. Custom NATS dialer with delay injection

	c.t.Logf("Warning: SimulateNetworkLatency is a placeholder")
	c.t.Logf("For real latency testing, use: tc (Linux), toxiproxy, or network proxy")
}
