package assignment

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// keysTimeoutKV wraps a real jetstream.KeyValue and, when armed, makes Keys()
// return context.DeadlineExceeded while every other operation — notably the
// single-key Put/Get a worker uses for its own heartbeat — passes through to
// the real bucket unchanged. This is NP-10's defining asymmetry: the
// stream-wide heartbeat-enumeration scan times out, single-key ops do not.
//
// The wrapper returns the deadline directly rather than blocking on ctx so the
// calculator-level proof is deterministic and timing-free; the resulting error
// value is identical to what a real Keys scan returns when WorkerMonitor's
// bounded op-context (hbTTL/2) expires mid-scan.
type keysTimeoutKV struct {
	jetstream.KeyValue
	armed atomic.Bool
}

func (k *keysTimeoutKV) Keys(ctx context.Context, opts ...jetstream.WatchOpt) ([]string, error) {
	if k.armed.Load() {
		return nil, context.DeadlineExceeded
	}

	return k.KeyValue.Keys(ctx, opts...)
}

// TestNP10_EnumerationDeadline_EvadesClassifiers is the empirical linchpin of
// NP-10. It confirms — by running the real classifiers, not by reading them —
// that a heartbeat Keys-scan context.DeadlineExceeded is classified as NEITHER
// a connectivity error NOR a degrading-JetStream error, both bare AND wrapped
// (worker_monitor wraps it as "failed to list heartbeat keys: %w"). That dual
// miss is precisely why getActiveWorkers returns the deadline raw and no caller
// routes it to the manager's degraded circuit.
//
// PASSES on the parent: it characterizes the current classification gap.
func TestNP10_EnumerationDeadline_EvadesClassifiers(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
	}{
		{"bare", context.DeadlineExceeded},
		{"wrapped", fmt.Errorf("failed to list heartbeat keys: %w", context.DeadlineExceeded)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.False(t, natsutil.IsConnectivityError(tc.err),
				"%s context.DeadlineExceeded must NOT be a connectivity error; if it were, getActiveWorkers would take the cache-fallback/ErrDegraded branch and NP-10 would not exist", tc.name)
			require.False(t, natsutil.IsDegradingJetStreamError(tc.err),
				"%s context.DeadlineExceeded must NOT be a degrading-JetStream error", tc.name)
		})
	}

	// The fix will need to classify the WRAPPED deadline, so confirm the
	// errors.Is chain that a fix would key on is intact today.
	require.ErrorIs(t, cases[1].err, context.DeadlineExceeded)
}

// TestNP10_GetActiveWorkers_EnumerationDeadline_IsSwallowed proves the NP-10
// gap at the calculator level: a sustained heartbeat Keys-scan deadline is
// surfaced RAW by getActiveWorkers — it does NOT take the connectivity
// cache-fallback branch and is NOT wrapped in types.ErrDegraded, so nothing in
// the assignment layer routes it to a degrade signal. The worker's own
// single-key heartbeat Put is untouched (the wrapper faults Keys only), modeling
// "own heartbeat healthy, enumeration blind".
//
// PASSES on the parent: it characterizes the swallow. It does NOT — and cannot —
// assert the manager stays falsely StateStable; that is a different state
// machine and is proven end-to-end in the integration proof
// (test/integration/failure/np10_enumeration_stall_test.go).
func TestNP10_GetActiveWorkers_EnumerationDeadline_IsSwallowed(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "np10-calc-assign")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "np10-calc-hb")

	// Seed a live worker heartbeat so a non-faulted scan succeeds and sees it.
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	fault := &keysTimeoutKV{KeyValue: heartbeatKV}
	calc, err := NewCalculator(&Config{
		AssignmentKV:     assignmentKV,
		HeartbeatKV:      fault,
		AssignmentPrefix: "assignment",
		Source:           &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}}},
		Strategy:         &mockStrategy{},
		HeartbeatPrefix:  "worker-hb",
		HeartbeatTTL:     6 * time.Second,
	})
	require.NoError(t, err)

	// Baseline (fault disarmed): enumeration succeeds and observes the worker.
	workers, fresh, err := calc.getActiveWorkers(ctx)
	require.NoError(t, err)
	require.True(t, fresh)
	require.Contains(t, workers, "worker-1")

	// Arm the Keys-only stall. The scan now times out; single-key Puts (the
	// worker's own heartbeat) would still succeed against the real bucket.
	fault.armed.Store(true)

	workers, fresh, err = calc.getActiveWorkers(ctx)

	// THE GAP: the deadline surfaces raw. Not a connectivity error (so no cache
	// fallback and no types.ErrDegraded wrap), not classified as degrading —
	// nothing routes it to the degraded circuit.
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotErrorIs(t, err, types.ErrDegraded,
		"parent gap: a sustained enumeration deadline is swallowed (returned raw), NOT surfaced as ErrDegraded")
	require.False(t, fresh)
	require.Nil(t, workers)
}
