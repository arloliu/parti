package coordinator
package coordinator

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestChaosController_GoroutineLeak tests that ChaosController doesn't leak goroutines.
//
// This test verifies:
// 1. Starting the chaos controller creates exactly 1 goroutine
// 2. When context is cancelled, the goroutine exits
// 3. No goroutine leaks after stop
func TestChaosController_GoroutineLeak(t *testing.T) {
	// Get baseline goroutine count
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	// Create chaos controller
	cfg := ChaosConfig{
		Enabled:     true,
		Events:      []string{"worker_crash"},
		MinInterval: 10 * time.Second,
		MaxInterval: 20 * time.Second,
		EventCallback: func(ChaosEvent, map[string]any) {
			// No-op callback for testing
		},
	}

	chaosCtrl := NewChaosController(cfg)
	require.NotNil(t, chaosCtrl)

	// Start chaos controller
	ctx, cancel := context.WithCancel(context.Background())
	chaosCtrl.Start(ctx)

	// Wait for goroutine to start
	time.Sleep(200 * time.Millisecond)

	// Check goroutine increased by 1
	afterStart := runtime.NumGoroutine()
	require.Equal(t, baseline+1, afterStart,
		"Expected exactly 1 new goroutine after Start()")

	// Cancel context to stop chaos controller
	cancel()

	// Wait for goroutine to exit
	time.Sleep(200 * time.Millisecond)
	runtime.GC()
	time.Sleep(100 * time.Millisecond)

	// Verify goroutine count returned to baseline
	afterStop := runtime.NumGoroutine()
	require.Equal(t, baseline, afterStop,
		"Goroutine leak detected: expected %d, got %d", baseline, afterStop)
}

// TestChaosController_MultipleStarts tests that starting chaos controller multiple times
// doesn't create multiple goroutines.
func TestChaosController_MultipleStarts(t *testing.T) {
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	cfg := ChaosConfig{
		Enabled:     true,
		Events:      []string{"worker_crash"},
		MinInterval: 10 * time.Second,
		MaxInterval: 20 * time.Second,
		EventCallback: func(ChaosEvent, map[string]any) {},
	}

	chaosCtrl := NewChaosController(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start multiple times
	chaosCtrl.Start(ctx)
	chaosCtrl.Start(ctx)
	chaosCtrl.Start(ctx)

	time.Sleep(200 * time.Millisecond)

	// Should only have 3 goroutines (one per Start call - THIS IS THE BUG!)
	afterStart := runtime.NumGoroutine()
	t.Logf("Baseline: %d, After 3 starts: %d, Leaked: %d",
		baseline, afterStart, afterStart-baseline)

	// This test will FAIL with current implementation because each Start()
	// creates a new goroutine without checking if one already exists
	require.LessOrEqual(t, afterStart-baseline, 1,
		"Multiple Start() calls should not create multiple goroutines")

	cancel()
	time.Sleep(200 * time.Millisecond)
	runtime.GC()
	time.Sleep(100 * time.Millisecond)

	afterStop := runtime.NumGoroutine()
	require.Equal(t, baseline, afterStop, "Goroutine leak after stop")
}

// TestChaosController_DisabledNoGoroutine tests that disabled chaos controller
// doesn't create any goroutines.
func TestChaosController_DisabledNoGoroutine(t *testing.T) {
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	cfg := ChaosConfig{
		Enabled:     false, // Disabled
		Events:      []string{"worker_crash"},
		MinInterval: 10 * time.Second,
		MaxInterval: 20 * time.Second,
		EventCallback: func(ChaosEvent, map[string]any) {},
	}

	chaosCtrl := NewChaosController(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chaosCtrl.Start(ctx)
	time.Sleep(200 * time.Millisecond)

	afterStart := runtime.NumGoroutine()
	require.Equal(t, baseline, afterStart,
		"Disabled chaos controller should not create goroutines")
}
