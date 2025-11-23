package assignment

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestEmergency_BasicScenarios tests basic emergency detection scenarios using table-driven approach.
// Uses shorter grace periods (100ms) and parallel execution for faster tests.
func TestEmergency_BasicScenarios(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		gracePeriod     time.Duration
		prevWorkers     map[string]bool
		currWorkers     map[string]bool
		waitAfterGrace  time.Duration
		expectEmergency bool
		expectedWorkers []string
		scenario        string
	}{
		{
			name:            "single worker disappears",
			gracePeriod:     100 * time.Millisecond,
			prevWorkers:     map[string]bool{"worker-1": true},
			currWorkers:     map[string]bool{},
			waitAfterGrace:  110 * time.Millisecond,
			expectEmergency: true,
			expectedWorkers: []string{"worker-1"},
			scenario:        "Single-node deployment crashes",
		},
		{
			name:            "single worker remains stable",
			gracePeriod:     100 * time.Millisecond,
			prevWorkers:     map[string]bool{"worker-1": true},
			currWorkers:     map[string]bool{"worker-1": true},
			waitAfterGrace:  110 * time.Millisecond,
			expectEmergency: false,
			expectedWorkers: []string{},
			scenario:        "Single-node deployment running normally",
		},
		{
			name:            "two workers one disappears",
			gracePeriod:     100 * time.Millisecond,
			prevWorkers:     map[string]bool{"worker-1": true, "worker-2": true},
			currWorkers:     map[string]bool{"worker-1": true},
			waitAfterGrace:  110 * time.Millisecond,
			expectEmergency: true,
			expectedWorkers: []string{"worker-2"},
			scenario:        "50% capacity loss in two-node cluster",
		},
		{
			name:            "two workers both disappear",
			gracePeriod:     100 * time.Millisecond,
			prevWorkers:     map[string]bool{"worker-1": true, "worker-2": true},
			currWorkers:     map[string]bool{},
			waitAfterGrace:  110 * time.Millisecond,
			expectEmergency: true,
			expectedWorkers: []string{"worker-1", "worker-2"},
			scenario:        "Total cluster failure",
		},
		{
			name:            "zero to one worker (cold start)",
			gracePeriod:     100 * time.Millisecond,
			prevWorkers:     map[string]bool{},
			currWorkers:     map[string]bool{"worker-1": true},
			waitAfterGrace:  110 * time.Millisecond,
			expectEmergency: false,
			expectedWorkers: []string{},
			scenario:        "First deployment, cluster starts",
		},
		{
			name:        "all workers disappear simultaneously",
			gracePeriod: 100 * time.Millisecond,
			prevWorkers: map[string]bool{
				"worker-1": true, "worker-2": true, "worker-3": true,
				"worker-4": true, "worker-5": true,
			},
			currWorkers:     map[string]bool{},
			waitAfterGrace:  110 * time.Millisecond,
			expectEmergency: true,
			expectedWorkers: []string{"worker-1", "worker-2", "worker-3", "worker-4", "worker-5"},
			scenario:        "Data center outage",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			detector := NewEmergencyDetector(tt.gracePeriod)

			// Initial check - should not be emergency yet
			emergency, workers := detector.CheckEmergency(tt.prevWorkers, tt.currWorkers)
			require.False(t, emergency, "Should not be emergency within grace period")
			require.Empty(t, workers)

			// Wait for grace period to expire
			time.Sleep(tt.waitAfterGrace)

			// Check again after grace period
			emergency, workers = detector.CheckEmergency(tt.prevWorkers, tt.currWorkers)
			require.Equal(t, tt.expectEmergency, emergency, "Emergency detection mismatch for: %s", tt.scenario)

			if tt.expectEmergency {
				require.Len(t, workers, len(tt.expectedWorkers), "Wrong number of disappeared workers")
				for _, expectedWorker := range tt.expectedWorkers {
					require.Contains(t, workers, expectedWorker, "Expected worker not in disappeared list")
				}
			} else {
				require.Empty(t, workers, "No workers should be in disappeared list")
			}
		})
	}
}

// TestEmergency_SingleWorkerDisappears_ThresholdCalculation verifies that emergency
// detection works correctly for the edge case of a single worker disappearing.
//
// This test documents that there's no explicit threshold check - ANY worker disappearing
// beyond grace period is an emergency, regardless of percentage.
//
// Production consideration: In a 100-node cluster, losing 1 node (1%) still triggers
// emergency because it means some partitions need immediate reassignment.
func TestEmergency_SingleWorkerDisappears_ThresholdCalculation(t *testing.T) {
	t.Parallel()

	gracePeriod := 100 * time.Millisecond
	detector := NewEmergencyDetector(gracePeriod)

	// Large cluster: 10 workers
	prev := map[string]bool{
		"worker-1": true, "worker-2": true, "worker-3": true, "worker-4": true, "worker-5": true,
		"worker-6": true, "worker-7": true, "worker-8": true, "worker-9": true, "worker-10": true,
	}

	// Current state: only 1 worker disappeared (10%)
	curr := map[string]bool{
		"worker-1": true, "worker-2": true, "worker-3": true, "worker-4": true, "worker-5": true,
		"worker-6": true, "worker-7": true, "worker-8": true, "worker-9": true,
	}

	// Initial check
	emergency, _ := detector.CheckEmergency(prev, curr)
	require.False(t, emergency)

	// After grace period - should still be emergency even though only 10% lost
	time.Sleep(110 * time.Millisecond)
	emergency, workers := detector.CheckEmergency(prev, curr)
	require.True(t, emergency, "Even 1 worker disappearing (10%) should be emergency")
	require.Len(t, workers, 1)
	require.Contains(t, workers, "worker-10")
}

// TestEmergency_CascadingFailures tests cascading worker failures with independent tracking.
//
// Production scenario: First one node fails, then others follow with different timing.
func TestEmergency_CascadingFailures(t *testing.T) {
	t.Parallel()

	gracePeriod := 200 * time.Millisecond
	detector := NewEmergencyDetector(gracePeriod)

	// Initial state: 4 workers
	prev := map[string]bool{
		"worker-1": true,
		"worker-2": true,
		"worker-3": true,
		"worker-4": true,
	}

	// First failure: worker-4 disappears
	curr1 := map[string]bool{
		"worker-1": true,
		"worker-2": true,
		"worker-3": true,
	}

	emergency, workers := detector.CheckEmergency(prev, curr1)
	require.False(t, emergency, "Should be in grace period for worker-4")
	require.Empty(t, workers)

	// Wait 100ms (half grace period)
	time.Sleep(100 * time.Millisecond)

	// Second failure: worker-3 also disappears (cascading failure)
	curr2 := map[string]bool{
		"worker-1": true,
		"worker-2": true,
	}

	emergency, workers = detector.CheckEmergency(prev, curr2)
	require.False(t, emergency, "Should still be in grace period for both")
	require.Empty(t, workers)

	// Wait another 120ms (total 220ms for worker-4, 120ms for worker-3)
	time.Sleep(120 * time.Millisecond)

	emergency, workers = detector.CheckEmergency(prev, curr2)
	require.True(t, emergency, "worker-4 exceeded grace period")
	require.Len(t, workers, 1, "Only worker-4 should be past grace period")
	require.Contains(t, workers, "worker-4")
	require.NotContains(t, workers, "worker-3", "worker-3 still in grace period")

	// Wait another 100ms (total 320ms for worker-4, 220ms for worker-3)
	time.Sleep(100 * time.Millisecond)

	emergency, workers = detector.CheckEmergency(prev, curr2)
	require.True(t, emergency, "Both workers exceeded grace period")
	require.Len(t, workers, 2, "Both workers should be past grace period now")
	require.Contains(t, workers, "worker-4")
	require.Contains(t, workers, "worker-3")
}

// TestEmergency_WorkerFlapping tests worker flapping scenarios with table-driven approach.
func TestEmergency_WorkerFlapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		scenario    string
		setupAndRun func(t *testing.T, detector *EmergencyDetector)
	}{
		{
			name:     "flapping within grace period",
			scenario: "Network hiccup causes brief heartbeat loss, worker reconnects quickly",
			setupAndRun: func(t *testing.T, detector *EmergencyDetector) {
				prev := map[string]bool{"worker-1": true, "worker-2": true}

				// Worker-2 disappears
				currDisappeared := map[string]bool{"worker-1": true}
				emergency, workers := detector.CheckEmergency(prev, currDisappeared)
				require.False(t, emergency)
				require.Empty(t, workers)

				// Wait 50ms (half grace period)
				time.Sleep(50 * time.Millisecond)

				// Worker-2 reappears (flap)
				currReappeared := map[string]bool{"worker-1": true, "worker-2": true}
				emergency, workers = detector.CheckEmergency(prev, currReappeared)
				require.False(t, emergency, "Worker reappearing within grace period should not be emergency")
				require.Empty(t, workers)

				// Wait another 70ms (total 120ms from initial disappearance)
				time.Sleep(70 * time.Millisecond)

				// Worker still present - should still not be emergency (tracking was cleared)
				emergency, workers = detector.CheckEmergency(prev, currReappeared)
				require.False(t, emergency, "Tracking should be cleared, no emergency")
				require.Empty(t, workers)
			},
		},
		{
			name:     "flapping beyond grace period",
			scenario: "Worker has network issues, eventually crashes permanently",
			setupAndRun: func(t *testing.T, detector *EmergencyDetector) {
				prev := map[string]bool{"worker-1": true, "worker-2": true}

				// Worker-2 disappears
				currDisappeared := map[string]bool{"worker-1": true}
				emergency, _ := detector.CheckEmergency(prev, currDisappeared)
				require.False(t, emergency)

				// Wait 50ms
				time.Sleep(50 * time.Millisecond)

				// Worker-2 reappears briefly (flap)
				currReappeared := map[string]bool{"worker-1": true, "worker-2": true}
				emergency, _ = detector.CheckEmergency(prev, currReappeared)
				require.False(t, emergency)

				// Wait 20ms
				time.Sleep(20 * time.Millisecond)

				// Worker-2 disappears again (permanent failure)
				emergency, _ = detector.CheckEmergency(prev, currDisappeared)
				require.False(t, emergency, "Should restart grace period tracking")

				// Wait full grace period from this disappearance
				time.Sleep(110 * time.Millisecond)

				// Now should be emergency
				emergency, workers := detector.CheckEmergency(prev, currDisappeared)
				require.True(t, emergency, "Worker disappeared beyond grace period after flapping")
				require.Len(t, workers, 1)
				require.Contains(t, workers, "worker-2")
			},
		},
		{
			name:     "worker rejoins before emergency detected",
			scenario: "Pod restart completes within grace period",
			setupAndRun: func(t *testing.T, detector *EmergencyDetector) {
				prev := map[string]bool{"worker-1": true, "worker-2": true, "worker-3": true}

				// Worker-2 disappears
				currDisappeared := map[string]bool{"worker-1": true, "worker-3": true}
				emergency, _ := detector.CheckEmergency(prev, currDisappeared)
				require.False(t, emergency)

				// Wait 80ms (almost at grace period)
				time.Sleep(80 * time.Millisecond)

				// Worker-2 rejoins just in time
				currRejoined := map[string]bool{"worker-1": true, "worker-2": true, "worker-3": true}
				emergency, workers := detector.CheckEmergency(prev, currRejoined)
				require.False(t, emergency, "Worker rejoining before grace period should cancel emergency")
				require.Empty(t, workers)

				// Wait another 50ms (would have been past grace period)
				time.Sleep(50 * time.Millisecond)

				// Still should not be emergency
				emergency, workers = detector.CheckEmergency(prev, currRejoined)
				require.False(t, emergency, "Rejoined worker should not trigger emergency later")
				require.Empty(t, workers)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			detector := NewEmergencyDetector(100 * time.Millisecond)
			tt.setupAndRun(t, detector)
		})
	}
}

// TestEmergency_K8sRollingUpdate simulates Kubernetes rolling update where pods restart
// in quick succession but each completes within grace period.
//
// Production scenario: Kubernetes rolling update with 3 pods, each pod restarts in sequence,
// each restart takes ~60ms but grace period is 200ms. Should NOT trigger emergency.
func TestEmergency_K8sRollingUpdate(t *testing.T) {
	t.Parallel()

	gracePeriod := 200 * time.Millisecond
	detector := NewEmergencyDetector(gracePeriod)

	// Initial state: 3 workers
	workers := map[string]bool{
		"worker-1": true,
		"worker-2": true,
		"worker-3": true,
	}

	// Pod 1 restarts (disappears briefly)
	workers1Restarting := map[string]bool{
		"worker-2": true,
		"worker-3": true,
	}
	emergency, disappeared := detector.CheckEmergency(workers, workers1Restarting)
	require.False(t, emergency)
	require.Empty(t, disappeared)

	// Wait 50ms (quarter of grace period)
	time.Sleep(50 * time.Millisecond)

	// Pod 1 completes restart
	workersAllUp := map[string]bool{
		"worker-1": true,
		"worker-2": true,
		"worker-3": true,
	}
	emergency, disappeared = detector.CheckEmergency(workers, workersAllUp)
	require.False(t, emergency, "Pod 1 restarted within grace period")
	require.Empty(t, disappeared)

	// Pod 2 starts restart
	workers2Restarting := map[string]bool{
		"worker-1": true,
		"worker-3": true,
	}
	emergency, disappeared = detector.CheckEmergency(workers, workers2Restarting)
	require.False(t, emergency)
	require.Empty(t, disappeared)

	// Wait 50ms
	time.Sleep(50 * time.Millisecond)

	// Pod 2 completes restart
	emergency, disappeared = detector.CheckEmergency(workers, workersAllUp)
	require.False(t, emergency, "Pod 2 restarted within grace period")
	require.Empty(t, disappeared)

	// Pod 3 starts restart
	workers3Restarting := map[string]bool{
		"worker-1": true,
		"worker-2": true,
	}
	emergency, disappeared = detector.CheckEmergency(workers, workers3Restarting)
	require.False(t, emergency)
	require.Empty(t, disappeared)

	// Wait 50ms
	time.Sleep(50 * time.Millisecond)

	// Pod 3 completes restart
	emergency, disappeared = detector.CheckEmergency(workers, workersAllUp)
	require.False(t, emergency, "All pods restarted successfully, no emergency")
	require.Empty(t, disappeared)
}

// TestEmergency_MultipleFlappingWorkers verifies that multiple workers flapping
// simultaneously are tracked independently.
//
// Production scenario: Network instability affecting multiple nodes differently.
func TestEmergency_MultipleFlappingWorkers(t *testing.T) {
	t.Parallel()

	gracePeriod := 200 * time.Millisecond
	detector := NewEmergencyDetector(gracePeriod)

	prev := map[string]bool{
		"worker-1": true,
		"worker-2": true,
		"worker-3": true,
		"worker-4": true,
	}

	// Workers 2 and 3 disappear
	curr1 := map[string]bool{
		"worker-1": true,
		"worker-4": true,
	}
	emergency, _ := detector.CheckEmergency(prev, curr1)
	require.False(t, emergency)

	// Wait 100ms
	time.Sleep(100 * time.Millisecond)

	// Worker-2 reappears, worker-3 still gone
	curr2 := map[string]bool{
		"worker-1": true,
		"worker-2": true,
		"worker-4": true,
	}
	emergency, workers := detector.CheckEmergency(prev, curr2)
	require.False(t, emergency, "Worker-3 still within grace period")
	require.Empty(t, workers)

	// Wait another 120ms (total 220ms for worker-3, 120ms for worker-2's tracking cleared)
	time.Sleep(120 * time.Millisecond)

	// Now worker-3 should be emergency, but worker-2 is fine
	emergency, workers = detector.CheckEmergency(prev, curr2)
	require.True(t, emergency, "Worker-3 exceeded grace period")
	require.Len(t, workers, 1)
	require.Contains(t, workers, "worker-3")
	require.NotContains(t, workers, "worker-2", "Worker-2 reappeared, should not be in emergency list")
}

// TestEmergency_GracePeriodRespected verifies that emergency detection never fires
// before the grace period expires, regardless of how many times we check.
//
// Production scenario: Aggressive monitoring that polls frequently should not cause
// premature emergency detection.
func TestEmergency_GracePeriodRespected(t *testing.T) {
	t.Parallel()

	gracePeriod := 200 * time.Millisecond
	detector := NewEmergencyDetector(gracePeriod)

	prev := map[string]bool{"worker-1": true, "worker-2": true}
	curr := map[string]bool{"worker-1": true}

	// Poll aggressively for 190ms (just under grace period)
	startTime := time.Now()
	for time.Since(startTime) < 190*time.Millisecond {
		emergency, workers := detector.CheckEmergency(prev, curr)
		require.False(t, emergency, "Should never fire before grace period expires")
		require.Empty(t, workers)
		time.Sleep(20 * time.Millisecond) // Poll every 20ms
	}

	// Wait for grace period to definitely expire
	time.Sleep(30 * time.Millisecond) // Total: 220ms

	// Now should be emergency
	emergency, workers := detector.CheckEmergency(prev, curr)
	require.True(t, emergency, "Should fire after grace period expires")
	require.Len(t, workers, 1)
	require.Contains(t, workers, "worker-2")
}
