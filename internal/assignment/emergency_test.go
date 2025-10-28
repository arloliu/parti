package assignment

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestEmergencyDetector_Hysteresis verifies grace period prevents false positives.
func TestEmergencyDetector_Hysteresis(t *testing.T) {
	gracePeriod := 2 * time.Second
	detector := NewEmergencyDetector(gracePeriod)

	prev := map[string]bool{"worker-1": true, "worker-2": true, "worker-3": true}

	// Worker-2 disappears
	curr := map[string]bool{"worker-1": true, "worker-3": true}

	// Immediate check - should NOT be emergency (grace period not expired)
	emergency, workers := detector.CheckEmergency(prev, curr)
	require.False(t, emergency)
	require.Empty(t, workers)

	// Wait half grace period
	time.Sleep(1 * time.Second)
	emergency, workers = detector.CheckEmergency(prev, curr)
	require.False(t, emergency)
	require.Empty(t, workers)

	// Wait full grace period
	time.Sleep(1100 * time.Millisecond) // Total: 2.1s
	emergency, workers = detector.CheckEmergency(prev, curr)
	require.True(t, emergency)
	require.Len(t, workers, 1)
	require.Contains(t, workers, "worker-2")
}

// TestEmergencyDetector_WorkerReappears verifies tracking cleared when worker returns.
func TestEmergencyDetector_WorkerReappears(t *testing.T) {
	gracePeriod := 2 * time.Second
	detector := NewEmergencyDetector(gracePeriod)

	prev := map[string]bool{"worker-1": true, "worker-2": true}

	// Worker-2 disappears
	curr := map[string]bool{"worker-1": true}
	emergency, _ := detector.CheckEmergency(prev, curr)
	require.False(t, emergency)

	// Wait 1 second
	time.Sleep(1 * time.Second)

	// Worker-2 reappears
	currReappeared := map[string]bool{"worker-1": true, "worker-2": true}
	emergency, workers := detector.CheckEmergency(prev, currReappeared)
	require.False(t, emergency)
	require.Empty(t, workers)

	// Wait another 2 seconds - should still not be emergency (tracking cleared)
	time.Sleep(2 * time.Second)
	emergency, workers = detector.CheckEmergency(prev, currReappeared)
	require.False(t, emergency)
	require.Empty(t, workers)
}

// TestEmergencyDetector_MultipleWorkers verifies tracking multiple disappeared workers.
func TestEmergencyDetector_MultipleWorkers(t *testing.T) {
	gracePeriod := 1 * time.Second
	detector := NewEmergencyDetector(gracePeriod)

	prev := map[string]bool{
		"worker-1": true,
		"worker-2": true,
		"worker-3": true,
		"worker-4": true,
	}

	// Two workers disappear
	curr := map[string]bool{"worker-1": true, "worker-2": true}

	// Immediate check - no emergency yet
	emergency, workers := detector.CheckEmergency(prev, curr)
	require.False(t, emergency)
	require.Empty(t, workers)

	// Wait past grace period
	time.Sleep(1100 * time.Millisecond)

	emergency, workers = detector.CheckEmergency(prev, curr)
	require.True(t, emergency)
	require.Len(t, workers, 2)
	require.Contains(t, workers, "worker-3")
	require.Contains(t, workers, "worker-4")
}

// TestEmergencyDetector_Reset verifies Reset() clears all tracking.
func TestEmergencyDetector_Reset(t *testing.T) {
	gracePeriod := 1 * time.Second
	detector := NewEmergencyDetector(gracePeriod)

	prev := map[string]bool{"worker-1": true, "worker-2": true}
	curr := map[string]bool{"worker-1": true}

	// Worker-2 disappears
	emergency, _ := detector.CheckEmergency(prev, curr)
	require.False(t, emergency)

	// Wait past grace period
	time.Sleep(1100 * time.Millisecond)
	emergency, workers := detector.CheckEmergency(prev, curr)
	require.True(t, emergency)
	require.Contains(t, workers, "worker-2")

	// Reset tracking
	detector.Reset()

	// Check immediately after reset - should be back to tracking fresh
	emergency, workers = detector.CheckEmergency(prev, curr)
	require.False(t, emergency) // New tracking period started
	require.Empty(t, workers)
}

// TestEmergencyDetector_PartialReappearance verifies partial worker recovery.
func TestEmergencyDetector_PartialReappearance(t *testing.T) {
	gracePeriod := 1 * time.Second
	detector := NewEmergencyDetector(gracePeriod)

	prev := map[string]bool{
		"worker-1": true,
		"worker-2": true,
		"worker-3": true,
	}

	// Two workers disappear
	curr := map[string]bool{"worker-1": true}

	// Start tracking
	emergency, _ := detector.CheckEmergency(prev, curr)
	require.False(t, emergency)

	// Wait 500ms
	time.Sleep(500 * time.Millisecond)

	// Worker-2 reappears, worker-3 still missing
	currPartial := map[string]bool{"worker-1": true, "worker-2": true}
	emergency, workers := detector.CheckEmergency(prev, currPartial)
	require.False(t, emergency) // worker-3 not past grace period yet
	require.Empty(t, workers)

	// Wait another 700ms (total 1.2s for worker-3)
	time.Sleep(700 * time.Millisecond)

	emergency, workers = detector.CheckEmergency(prev, currPartial)
	require.True(t, emergency)
	require.Len(t, workers, 1)
	require.Contains(t, workers, "worker-3")
	require.NotContains(t, workers, "worker-2") // worker-2 tracking was cleared
}

// TestEmergencyDetector_ZeroGracePeriod verifies immediate emergency with zero grace period.
func TestEmergencyDetector_ZeroGracePeriod(t *testing.T) {
	detector := NewEmergencyDetector(0 * time.Second)

	prev := map[string]bool{"worker-1": true, "worker-2": true}
	curr := map[string]bool{"worker-1": true}

	// With zero grace period, should be immediate emergency
	emergency, workers := detector.CheckEmergency(prev, curr)
	require.True(t, emergency)
	require.Len(t, workers, 1)
	require.Contains(t, workers, "worker-2")
}

// TestEmergencyDetector_NoWorkerChange verifies no emergency when workers stable.
func TestEmergencyDetector_NoWorkerChange(t *testing.T) {
	gracePeriod := 1 * time.Second
	detector := NewEmergencyDetector(gracePeriod)

	workers := map[string]bool{"worker-1": true, "worker-2": true}

	// No changes
	emergency, disappeared := detector.CheckEmergency(workers, workers)
	require.False(t, emergency)
	require.Empty(t, disappeared)

	// Wait past grace period - still no emergency (no changes)
	time.Sleep(1100 * time.Millisecond)
	emergency, disappeared = detector.CheckEmergency(workers, workers)
	require.False(t, emergency)
	require.Empty(t, disappeared)
}
