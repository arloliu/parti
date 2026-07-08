package parti

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestClassifyKVError table-tests the pure KV-error router extracted from
// recordKVError, locking the precedence and the transient/whole-bucket split
// (the latter selects DegradeReasonKVUnavailable vs DegradeReasonKVErrorThreshold).
func TestClassifyKVError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		err       error
		route     kvErrorRoute
		transient bool
	}{
		{"nil drops", nil, kvRouteDrop, false},
		{"stream-missing is observer-owned", types.ErrStreamMissing, kvRouteStreamMissing, false},
		{"wrapped stream-missing is observer-owned", fmt.Errorf("recovery exhausted: %w", types.ErrStreamMissing), kvRouteStreamMissing, false},
		{"connectivity is whole-bucket window", nats.ErrTimeout, kvRouteWindow, false},
		{"degrading-jetstream is whole-bucket window", jetstream.ErrBucketNotFound, kvRouteWindow, false},
		{"kv-unavailable-marked timeout is transient window", errors.Join(ErrKVUnavailable, context.DeadlineExceeded), kvRouteWindow, true},
		{"unclassified non-timeout drops", errors.New("some unrelated failure"), kvRouteDrop, false},
		{"bare deadline (unmarked) drops", context.DeadlineExceeded, kvRouteDrop, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyKVError(tc.err)
			require.Equal(t, tc.route, got.route, "route for %v", tc.err)
			require.Equal(t, tc.transient, got.transient, "transient for %v", tc.err)
		})
	}
}

// TestRecordLabelReadFailure_Routing pins the manager-side adapter that routes a
// broad label-read failure from the calculator into the degraded circuit. A
// classed cause (connectivity / degrading JetStream) passes through as a
// non-transient whole-bucket-loss window entry; a bare count-based error is
// wrapped ErrKVUnavailable so it enters as the transient (F-D1) class and, when
// sustained, degrades with DegradeReasonKVUnavailable; nil is a no-op.
func TestRecordLabelReadFailure_Routing(t *testing.T) {
	logger := logging.NewNop()
	nopMetrics := metrics.NewNop()
	cfg := Config{
		DegradedBehavior: DegradedBehaviorConfig{
			KVErrorThreshold: 3,
			KVErrorWindow:    10 * time.Second,
		},
		DegradedAlert: DegradedAlertConfig{
			AlertInterval: 1 * time.Minute,
		},
	}

	t.Run("connectivity-classed cause passes through non-transient", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}

		m.recordLabelReadFailure(nats.ErrTimeout)

		require.Equal(t, int32(1), m.kvErrorCount.Load())
		require.Len(t, m.kvErrorWindow, 1)
		require.False(t, m.kvErrorWindow[0].transient,
			"connectivity cause must be admitted as a whole-bucket-loss (non-transient) entry")
	})

	t.Run("bare count-based error is wrapped transient", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}

		m.recordLabelReadFailure(errors.New("2 of 3 worker label reads failed"))

		require.Equal(t, int32(1), m.kvErrorCount.Load())
		require.Len(t, m.kvErrorWindow, 1)
		require.True(t, m.kvErrorWindow[0].transient,
			"a bare count-based failure must be wrapped ErrKVUnavailable and enter as the transient class")
	})

	t.Run("wrapped shape classifies as transient window entry", func(t *testing.T) {
		// Lock the exact wrapping shape the adapter uses so classifyKVError keeps
		// routing it to the transient (F-D1) window class.
		wrapped := fmt.Errorf("%w: broad worker label read failure: %w", ErrKVUnavailable, errors.New("boom"))
		got := classifyKVError(wrapped)
		require.Equal(t, kvRouteWindow, got.route)
		require.True(t, got.transient)
	})

	t.Run("sustained bare failures trip degraded with kv-unavailable reason", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}
		m.state.Store(int32(StateStable)) // must be a state that allows Degraded entry

		for range 3 {
			m.recordLabelReadFailure(errors.New("broad worker label read failure"))
		}
		// Stop the monitorDegradedAlerts goroutine started by enterDegraded.
		cancel()
		m.wg.Wait()

		require.Equal(t, StateDegraded, m.State())
		rec := m.degraded.Load()
		require.NotNil(t, rec)
		require.Equal(t, DegradeReasonKVUnavailable, rec.reason)
	})

	t.Run("nil is a no-op", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}

		m.recordLabelReadFailure(nil)

		require.Equal(t, int32(0), m.kvErrorCount.Load())
		require.Empty(t, m.kvErrorWindow)
	})
}
