package parti

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestWaitForAssignment_RespectsContextDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create an empty assignment KV bucket (no assignment will ever appear)
	kv, err := js.CreateOrUpdateKeyValue(t.Context(), jetstream.KeyValueConfig{
		Bucket: "test-wait-assignment",
	})
	require.NoError(t, err)

	m := &Manager{
		logger:  logging.NewNop(),
		metrics: metrics.NewNop(),
		hooks:   &Hooks{},
	}
	m.workerID.Store("worker-0")

	// Use a short deadline (200ms) — must timeout within that window,
	// not after any hardcoded 30s.
	shortCtx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = m.waitForAssignment(shortCtx, kv, nil)
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, elapsed, 2*time.Second,
		"waitForAssignment must respect context deadline, not use a hardcoded 30s timeout")
}
