package consumer_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/internal/ratelimit"
	partitesting "github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
)

// recordingLimiter wraps a real token-bucket limiter and counts every Wait call.
// It is used to verify that the Dynamic consumer consults the limiter exactly once
// per partition create attempt.
type recordingLimiter struct {
	mu    sync.Mutex
	n     int
	inner ratelimit.Limiter
}

func (r *recordingLimiter) Wait(ctx context.Context) error {
	r.mu.Lock()
	r.n++
	r.mu.Unlock()

	return r.inner.Wait(ctx)
}

// Calls returns the total number of Wait invocations observed so far.
func (r *recordingLimiter) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.n
}

// TestDynamic_ConsumerCreatePacing proves that:
//  1. The injected limiter's Wait is called at least once per partition create.
//  2. The paced apply respects the token-bucket floor: with N=30 partitions, a
//     burst of 5, and a rate of 20/s, the minimum elapsed time is
//     (N - burst) / rate = (30 - 5) / 20 = 1.25s.  We assert >= 1s as a
//     conservative, flake-safe lower bound (any pacing can only add delay, never
//     remove it).
func TestDynamic_ConsumerCreatePacing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	const streamName = "PACE"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"pace.*"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	// Build a recording limiter backed by a real token bucket: 20/s, burst 5.
	rl := &recordingLimiter{inner: ratelimit.New(20, 5, nil)}

	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		return nil
	})

	dc, err := consumer.NewDynamic(js, streamName, "pace-worker", "pace.{{.PartitionID}}", handler,
		consumer.WithConsumerCreateLimiter(rl),
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dc.Stop(ctx) })

	const n = 30
	partitions := make([]types.Partition, n)
	for i := range n {
		partitions[i] = types.Partition{Keys: []string{"pace", strconv.Itoa(i)}}
	}

	start := time.Now()
	err = dc.Update(t.Context(), "pace-worker-1", partitions)
	elapsed := time.Since(start)

	// All partitions must be created (Update returns nil only on full success).
	require.NoError(t, err)

	// The token-bucket guarantees a minimum floor of (N - burst) / rate = 1.25s
	// from the pacing alone.  We assert a conservative 1s lower bound to remain
	// flake-safe: pacing can only add delay, never remove it, so a floor based
	// on the bucket math is always a guaranteed lower bound.
	require.GreaterOrEqual(t, elapsed, 1*time.Second,
		"paced apply must take at least 1s (bucket floor for N=%d, burst=5, rate=20/s); got %v", n, elapsed)

	// The limiter must have been consulted at least once per partition create.
	// We use >= N to tolerate any transient retry that triggers an extra Wait.
	require.GreaterOrEqual(t, rl.Calls(), n,
		"limiter must be called at least once per partition; got %d calls for %d partitions", rl.Calls(), n)

	t.Logf("paced apply elapsed=%v, limiter.Calls=%d (N=%d)", elapsed, rl.Calls(), n)
}
