package parti

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// kvUnavailManager builds a minimal Manager parked in StateStable with a
// degrade-reason capture hook. The returned pointer receives the reason string
// passed to enterDegraded (via the OnDegraded hook, the only surface that
// observes it).
//
// The threshold is fixed at 3: low enough that the degrade tests trip it with a
// short loop, high enough that the isolated-below-threshold negative-space test
// (which clears the window after each error) never reaches it.
func kvUnavailManager(t *testing.T) (*Manager, *string, context.CancelFunc) {
	t.Helper()
	const threshold = 3
	cfg := Config{
		DegradedBehavior: DegradedBehaviorConfig{
			KVErrorThreshold: threshold,
			KVErrorWindow:    10 * time.Second,
		},
		DegradedAlert: DegradedAlertConfig{
			AlertInterval: 1 * time.Minute,
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	reason := new(string)
	m := &Manager{
		logger:  logging.NewNop(),
		cfg:     cfg,
		hooks:   &Hooks{},
		metrics: metrics.NewNop(),
		ctx:     ctx,
		cancel:  cancel,
	}
	m.hooks.OnDegraded = func(_ context.Context, r string) error {
		*reason = r
		return nil
	}
	m.state.Store(int32(StateStable))

	return m, reason, cancel
}

// TestMarkKVUnavailable is the F-D1 classifier/routing table. It pins the
// "existing classifiers win first" construction that makes it impossible for
// the new path to steal the whole-bucket-loss route (ErrNoStreamResponse stays
// connectivity) or the bucket-missing route (ErrBucketNotFound stays degrading
// JetStream). Only an otherwise-unclassified deadline / no-responders timeout
// is marked.
func TestMarkKVUnavailable(t *testing.T) {
	t.Run("nil passes through", func(t *testing.T) {
		require.NoError(t, markKVUnavailable(nil))
	})

	t.Run("bare deadline is marked", func(t *testing.T) {
		got := markKVUnavailable(context.DeadlineExceeded)
		require.ErrorIs(t, got, ErrKVUnavailable)
		require.ErrorIs(t, got, context.DeadlineExceeded,
			"the original cause must remain inspectable")
	})

	t.Run("no-responders is marked", func(t *testing.T) {
		got := markKVUnavailable(nats.ErrNoResponders)
		require.ErrorIs(t, got, ErrKVUnavailable)
		require.ErrorIs(t, got, nats.ErrNoResponders)
	})

	t.Run("wrapped deadline is marked", func(t *testing.T) {
		// The election renew path wraps the ctx deadline as
		// "leadership was lost: context deadline exceeded"; the wrapper must
		// still see the deadline through the wrap.
		got := markKVUnavailable(
			fmt.Errorf("leadership was lost: %w", context.DeadlineExceeded))
		require.ErrorIs(t, got, ErrKVUnavailable)
	})

	t.Run("ErrNoStreamResponse is NOT marked (stays whole-bucket route)", func(t *testing.T) {
		got := markKVUnavailable(jetstream.ErrNoStreamResponse)
		require.NotErrorIs(t, got, ErrKVUnavailable,
			"ErrNoStreamResponse must remain on the connectivity / whole-bucket route")
		require.True(t, natsutil.IsConnectivityError(got),
			"must still classify as connectivity after the wrapper")
	})

	t.Run("ErrBucketNotFound is NOT marked (stays degrading route)", func(t *testing.T) {
		got := markKVUnavailable(jetstream.ErrBucketNotFound)
		require.NotErrorIs(t, got, ErrKVUnavailable)
		require.True(t, natsutil.IsDegradingJetStreamError(got),
			"must still classify as degrading JetStream after the wrapper")
	})

	t.Run("unrelated error is NOT marked", func(t *testing.T) {
		got := markKVUnavailable(nats.ErrInvalidArg)
		require.NotErrorIs(t, got, ErrKVUnavailable)
		require.ErrorIs(t, got, nats.ErrInvalidArg, "passes through unchanged")
	})
}

// TestRecordKVError_ReadUnavailable_Degrades pins that a marked
// KV-unavailable error, sustained past the threshold, drives the manager into
// Degraded with the distinct reason kv-unavailable — the F-D1 observability
// value (the operator sees Degraded instead of a silent stall).
func TestRecordKVError_ReadUnavailable_Degrades(t *testing.T) {
	for _, tc := range []struct {
		name string
		base error
	}{
		{"deadline", context.DeadlineExceeded},
		{"no-responders", nats.ErrNoResponders},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, reason, cancel := kvUnavailManager(t)
			defer cancel()

			for range 3 {
				m.recordKVError(markKVUnavailable(tc.base))
			}
			cancel()
			m.wg.Wait()

			require.Equal(t, StateDegraded, m.State())
			require.Equal(t, DegradeReasonKVUnavailable, *reason,
				"a sustained KV-unavailable condition must use the distinct reason")
		})
	}
}

// TestRecordKVError_RawDeadline_StillDropped is the scoping guarantee: an
// UNWRAPPED deadline (one that did not pass through a manager KV-op call site)
// must still be dropped, exactly as before F-D1. Only call-site-marked errors
// enter the new path.
func TestRecordKVError_RawDeadline_StillDropped(t *testing.T) {
	m, _, cancel := kvUnavailManager(t)
	defer cancel()

	for range 5 {
		m.recordKVError(context.DeadlineExceeded) // raw, never marked
	}

	require.Equal(t, int32(0), m.kvErrorCount.Load(),
		"a raw deadline from outside the wrapped call sites must not count")
	require.NotEqual(t, StateDegraded, m.State())
}

// TestRecordKVError_WholeBucketLoss_KeepsThresholdReason pins cross-feature
// contract 1: whole-bucket loss must still reach
// enterDegraded("KV error threshold exceeded"), NOT the new reason — even when
// the bucket-missing error flows through a marked call site. The wrapper returns
// bucket-missing unchanged (classifiers win first), so the reason is unaffected.
func TestRecordKVError_WholeBucketLoss_KeepsThresholdReason(t *testing.T) {
	m, reason, cancel := kvUnavailManager(t)
	defer cancel()

	for range 3 {
		m.recordKVError(markKVUnavailable(jetstream.ErrBucketNotFound))
	}
	cancel()
	m.wg.Wait()

	require.Equal(t, StateDegraded, m.State())
	require.Equal(t, "KV error threshold exceeded", *reason,
		"whole-bucket loss must keep the threshold reason, not the KV-unavailable reason")
}

// TestRecordKVError_ReadUnavailable_IsolatedBelowThreshold_NoDegrade is the
// negative-space test for the threshold circuit (see the both-directions-of-a-
// boundary discipline): isolated marked failures, each fully recovered, must
// NEVER accumulate to a degrade. Positive-space "N consecutive failures degrade"
// is consistent with both correct and broken counter semantics; this proves the
// window resets on success.
func TestRecordKVError_ReadUnavailable_IsolatedBelowThreshold_NoDegrade(t *testing.T) {
	m, _, cancel := kvUnavailManager(t)
	defer cancel()

	// Ten isolated failures, each cleared by a success before the next.
	for range 10 {
		m.recordKVError(markKVUnavailable(context.DeadlineExceeded))
		m.recordKVSuccess()
	}

	require.NotEqual(t, StateDegraded, m.State(),
		"isolated KV-unavailable blips that each recover must not degrade")
	require.Zero(t, m.degradedSince.Load())
}

// TestOnClaimerError_ReadTimeout_Degrades pins the stableid-renew wrap site
// (onClaimerError's non-ErrClaimLost branch): a renewal read timeout — which
// arrives as a plain (non-ErrClaimLost) error — must be marked and drive the
// degraded circuit, NOT be silently dropped.
func TestOnClaimerError_ReadTimeout_Degrades(t *testing.T) {
	m, reason, cancel := kvUnavailManager(t)
	defer cancel()

	origShutdown := claimLostShutdown
	claimLostShutdown = func(*Manager) { t.Error("a read timeout must not stop the worker") }
	defer func() { claimLostShutdown = origShutdown }()

	for range 3 {
		m.onClaimerError(context.DeadlineExceeded) // renewal default branch: plain deadline
	}
	cancel()
	m.wg.Wait()

	require.Equal(t, StateDegraded, m.State())
	require.Equal(t, DegradeReasonKVUnavailable, *reason)
}
