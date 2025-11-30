package subscription

import (
	"context"
	"sync"
	"testing"
	"text/template"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	partitesting "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
)

// TestWorkerConsumer_ConcurrentAddRemove exercises repeated add/remove of subject loops
// from multiple goroutines to validate thread-safety, absence of deadlocks, and clean shutdown.
func TestWorkerConsumer_ConcurrentAddRemove(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "CONC",
		Subjects:  []string{"conc.*.*"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	cfg := WorkerConsumerConfig{
		StreamName:      "CONC",
		ConsumerPrefix:  "wc2",
		SubjectTemplate: "conc.{{.PartitionID}}",
		BatchSize:       1,
	}
	require.NoError(t, cfg.SetDefaults())

	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)

	wc := &WorkerConsumer{
		js:              js,
		config:          cfg,
		logger:          cfg.Logger,
		handler:         MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil }),
		subjects:        make(map[string]*subjectLoop),
		iterFactory:     defaultIterFactory,
		subjectTemplate: tmpl,
	}
	t.Cleanup(func() { _ = wc.Close(context.Background()) })

	// Two partitions we will rapidly add/remove
	pA := []types.Partition{{Keys: []string{"a", "1"}}}
	pB := []types.Partition{{Keys: []string{"b", "1"}}}

	var wg sync.WaitGroup

	// Goroutine 1 rapidly toggles partition A
	wg.Go(func() {
		for range 10 {
			require.NoError(t, wc.UpdateWorkerConsumer(ctx, "w1", pA))
			time.Sleep(10 * time.Millisecond)
			require.NoError(t, wc.UpdateWorkerConsumer(ctx, "w1", nil))
		}
	})

	// Goroutine 2 rapidly toggles partition B
	wg.Go(func() {
		for range 10 {
			require.NoError(t, wc.UpdateWorkerConsumer(ctx, "w1", pB))
			time.Sleep(10 * time.Millisecond)
			require.NoError(t, wc.UpdateWorkerConsumer(ctx, "w1", nil))
		}
	})

	// Wait for completion and ensure Close succeeds quickly
	wg.Wait()
	require.NoError(t, wc.Close(ctx))
}

// TestWorkerConsumer_FlipSetsWithClose flips between multiple subject sets while
// invoking Close() mid-flip to stress lifecycle edges and ensure no deadlocks or races.
func TestWorkerConsumer_FlipSetsWithClose(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "FLIP",
		Subjects:  []string{"flip.*.*"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	cfg := WorkerConsumerConfig{
		StreamName:      "FLIP",
		ConsumerPrefix:  "wc2",
		SubjectTemplate: "flip.{{.PartitionID}}",
		BatchSize:       1,
	}
	require.NoError(t, cfg.SetDefaults())

	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)

	wc := &WorkerConsumer{
		js:              js,
		config:          cfg,
		logger:          cfg.Logger,
		handler:         MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error { return nil }),
		subjects:        make(map[string]*subjectLoop),
		iterFactory:     defaultIterFactory,
		subjectTemplate: tmpl,
	}
	// Ensure cleanup even if the test fails earlier
	t.Cleanup(func() { _ = wc.Close(context.Background()) })

	// handler is stored on wc; no local handler needed

	// Define several subject sets to flip between
	set1 := []types.Partition{{Keys: []string{"a", "1"}}, {Keys: []string{"b", "1"}}}
	set2 := []types.Partition{{Keys: []string{"c", "1"}}, {Keys: []string{"d", "1"}}}
	set3 := []types.Partition{{Keys: []string{"a", "1"}}, {Keys: []string{"c", "1"}}}
	sets := [][]types.Partition{set1, set2, set3}

	flipCtx, flipCancel := context.WithCancel(ctx)
	t.Cleanup(flipCancel)

	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		idx := 0
		for i := 0; i < 20; i++ {
			select {
			case <-flipCtx.Done():
				return
			default:
			}
			if err := wc.UpdateWorkerConsumer(flipCtx, "w1", sets[idx]); err != nil {
				// During Close() races, transient errors (e.g., context canceled) are acceptable
				t.Logf("update error during flip: %v", err)
			}
			idx = (idx + 1) % len(sets)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// Allow flipper to start and be mid-flight
	time.Sleep(50 * time.Millisecond)

	// Call Close while flipper is likely mid-update
	closeCtx, closeCancel := context.WithTimeout(ctx, 2*time.Second)
	require.NoError(t, wc.Close(closeCtx))
	closeCancel()

	// Stop flipper and wait; it may have attempted updates after Close but should not deadlock
	flipCancel()
	<-doneCh

	// Idempotent Close should still succeed
	closeCtx2, closeCancel2 := context.WithTimeout(ctx, 2*time.Second)
	require.NoError(t, wc.Close(closeCtx2))
	closeCancel2()

	// Verify internal map is empty
	wc.mu.RLock()
	defer wc.mu.RUnlock()
	require.Equal(t, 0, len(wc.subjects))
}
