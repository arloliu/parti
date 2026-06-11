package parti

import (
	"errors"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// armDegradedWithJS builds a Degraded manager wired to a live JetStream and a
// committed==snapshot assignment (commitment guard passes, refresh succeeds), so
// recovery reaches the heartbeat-bucket backstop. heartbeatBucket names the
// configured heartbeat bucket; create it for the reachable case, leave it absent
// for the unavailable case. reason is stamped as the degrade reason.
func armDegradedWithJS(t *testing.T, reason, heartbeatBucket string, createHeartbeat bool) *Manager {
	t.Helper()
	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	m, _, _, _ := newTestManager(t)
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	m.js = js
	m.cfg.OperationTimeout = 2 * time.Second
	m.cfg.KVBuckets.HeartbeatBucket = heartbeatBucket
	if createHeartbeat {
		_ = partitest.CreateJetStreamKV(t, nc, heartbeatBucket)
	}
	m.assignmentKV = partitest.CreateJetStreamKV(t, nc, "selfheal-hbbackstop-asgn")

	committed := snap
	m.committedAssignment.Store(&committed)
	m.assignment.Store(snap)
	m.state.Store(int32(StateDegraded))
	m.markDegraded(time.Now().UnixNano(), reason)
	plantAssignment(t, m, snap)

	return m
}

// These tests give the two integration-only recovery-exit conjuncts a fast unit
// characterization: the reason-scoped kv-unavailable heartbeat-after-degrade gate
// (manager_degraded.go:478-486) and the GLOBAL heartbeat-bucket reachability
// backstop (manager_degraded.go:503-506). Both assert the observable State() so
// they survive the recovery-guard-pipeline refactor.

// TestAttemptRecovery_KVUnavailable_StaleHeartbeat_StaysDegraded covers the
// stay-Degraded direction of the kv-unavailable gate: a kv-unavailable degrade
// must NOT exit on the (unaffected) assignment read until a heartbeat Put
// succeeds AFTER the degrade. A zero or pre-degrade ("stale") heartbeat stamp
// holds the worker Degraded.
func TestAttemptRecovery_KVUnavailable_StaleHeartbeat_StaysDegraded(t *testing.T) {
	t.Parallel()
	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	sinceNano := time.Now().UnixNano()

	t.Run("never-stamped", func(t *testing.T) {
		t.Parallel()
		m, _ := armDegraded(t, &snap, snap)
		plantAssignment(t, m, snap)
		m.markDegraded(sinceNano, DegradeReasonKVUnavailable)
		m.lastHeartbeatSuccessAt.Store(0)

		m.attemptRecoveryFromDegraded()

		require.Equal(t, StateDegraded, m.State(),
			"kv-unavailable with no post-degrade heartbeat must stay Degraded")
	})

	t.Run("stale-stamp", func(t *testing.T) {
		t.Parallel()
		m, _ := armDegraded(t, &snap, snap)
		plantAssignment(t, m, snap)
		m.markDegraded(sinceNano, DegradeReasonKVUnavailable)
		// Heartbeat succeeded BEFORE the degrade — does not prove the op recovered.
		m.lastHeartbeatSuccessAt.Store(sinceNano - 1000)

		m.attemptRecoveryFromDegraded()

		require.Equal(t, StateDegraded, m.State(),
			"kv-unavailable with a pre-degrade (stale) heartbeat must stay Degraded")
	})

	t.Run("equal-stamp-boundary", func(t *testing.T) {
		t.Parallel()
		m, _ := armDegraded(t, &snap, snap)
		plantAssignment(t, m, snap)
		m.markDegraded(sinceNano, DegradeReasonKVUnavailable)
		// Heartbeat stamped at EXACTLY the degrade instant. The gate's boundary is
		// `<=`: a success AT the degrade instant (rec.since) does not prove the op recovered AFTER we
		// degraded, so the worker stays Degraded. This pins the <= (vs <) boundary
		// that the stale/fresh cases straddle but never land on.
		m.lastHeartbeatSuccessAt.Store(sinceNano)

		m.attemptRecoveryFromDegraded()

		require.Equal(t, StateDegraded, m.State(),
			"a heartbeat stamped AT the degrade instant (hbAt == rec.since) must stay Degraded")
	})
}

// TestAttemptRecovery_KVUnavailable_FreshHeartbeat_Exits covers the exit
// direction: a heartbeat Put stamped AFTER the degrade proves the failing op
// recovered, so recovery exits to Stable (the other gates pass: commitment is
// satisfied and the js==nil global backstops are inert in this unit harness).
func TestAttemptRecovery_KVUnavailable_FreshHeartbeat_Exits(t *testing.T) {
	t.Parallel()
	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	m, _ := armDegraded(t, &snap, snap)
	plantAssignment(t, m, snap)
	sinceNano := time.Now().UnixNano()
	m.markDegraded(sinceNano, DegradeReasonKVUnavailable)
	// Heartbeat succeeded AFTER the degrade — the failing op recovered.
	m.lastHeartbeatSuccessAt.Store(sinceNano + 1000)

	m.attemptRecoveryFromDegraded()
	m.wg.Wait()

	require.Equal(t, StateStable, m.State(),
		"kv-unavailable with a post-degrade heartbeat must exit to Stable")
}

// TestAttemptRecovery_HeartbeatBucketUnavailable_StaysDegraded_AnyReason locks
// the GLOBAL nature of the heartbeat-bucket backstop: it must block the recovery
// exit for ANY degrade reason while the heartbeat bucket is unreachable, not just
// for kv-unavailable. It backstops the "KV error threshold exceeded" whole-bucket
// reason that Family B (kv-unavailable) and Family A (recreate-only) do not cover.
// Using non-kv-unavailable reasons proves it is not accidentally reason-scoped —
// the T1 cross-reason defense a naive guard registry would silently drop.
func TestAttemptRecovery_HeartbeatBucketUnavailable_StaysDegraded_AnyReason(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping embedded-NATS test in short mode")
	}
	// All non-kv-unavailable: Family B is silent for these, so the heartbeat-bucket
	// backstop is the ONLY conjunct that can hold them Degraded.
	reasons := []string{"KV error threshold exceeded", "NATS connection down", "startup-timeout"}
	for _, reason := range reasons {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			m := armDegradedWithJS(t, reason, "hb-missing-bucket", false)

			m.attemptRecoveryFromDegraded()

			require.Equal(t, StateDegraded, m.State(),
				"an unreachable heartbeat bucket must keep the worker Degraded for reason %q", reason)
		})
	}
}

// TestAttemptRecovery_HeartbeatBucketReachable_Exits is the opposite direction:
// when the heartbeat bucket IS reachable, the backstop does not block and recovery
// exits to Stable (the other gates pass). This proves the backstop discriminates
// on bucket reachability rather than always blocking.
func TestAttemptRecovery_HeartbeatBucketReachable_Exits(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embedded-NATS test in short mode")
	}
	t.Parallel()
	m := armDegradedWithJS(t, "NATS connection down", "hb-present-bucket", true)

	m.attemptRecoveryFromDegraded()
	m.wg.Wait()

	require.Equal(t, StateStable, m.State(),
		"a reachable heartbeat bucket must let recovery exit to Stable")
}

// TestAttemptRecovery_StreamMissingExhausted_StaysDegraded pins the terminal
// hold for the dynamic-consumer stream-missing route: once a partition
// consumer's recovery envelope has exhausted, its loop has exited and cannot
// restart in-process (the dead subject remains in the worker-consumer's
// subject map, so a re-apply computes an empty diff), and operator stream
// recreation cannot revive it either. Recovery must therefore never exit
// this reason — rotation is the only recovery, matching the
// heartbeat-bucket backstop's terminal contract. Without the gate, the
// connection monitor exits back to Stable within ~one tick (the NATS
// connection never dropped) and the worker reports Stable while assigned
// partitions are silently not consumed.
func TestAttemptRecovery_StreamMissingExhausted_StaysDegraded(t *testing.T) {
	t.Parallel()
	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	m, _ := armDegraded(t, &snap, snap)
	plantAssignment(t, m, snap)
	m.markDegraded(time.Now().UnixNano(), DegradeReasonStreamMissingRecoveryExhausted)

	// Even with every recovery signal healthy (commitment guard satisfied by
	// armDegraded, fresh heartbeat far in the future), the hold is terminal.
	m.lastHeartbeatSuccessAt.Store(time.Now().UnixNano() + int64(time.Hour))

	m.attemptRecoveryFromDegraded()

	require.Equal(t, StateDegraded, m.State(),
		"stream-missing-recovery-exhausted must hold the worker terminally Degraded for rotation")
}

// TestAttemptRecovery_StreamMissingHold_IsReasonScoped proves the new gate
// is NOT accidentally global: a reason with no blocking gate (startup
// timeout) still exits to Stable through the same pipeline. This is the
// negative-space direction the boundary-test discipline requires.
func TestAttemptRecovery_StreamMissingHold_IsReasonScoped(t *testing.T) {
	t.Parallel()
	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	m, _ := armDegraded(t, &snap, snap)
	plantAssignment(t, m, snap)
	m.markDegraded(time.Now().UnixNano(), DegradeReasonStartupTimeout)

	m.attemptRecoveryFromDegraded()
	m.wg.Wait()

	require.Equal(t, StateStable, m.State(),
		"a non-stream-missing reason with healthy signals must still exit; the terminal hold must be reason-scoped")
}

// TestAttemptRecovery_StreamMissingExhausted_LatchSurvivesReasonOverlap pins
// the overlap defense: when stream-missing exhaustion fires while the worker
// is ALREADY Degraded for another reason, enterDegraded's CAS no-ops and the
// reason string never records the exhaustion. The atomic latch must keep the
// recovery exit terminal anyway — otherwise the other reason's recovery
// signal (here: a fresh post-degrade heartbeat satisfying the kv-unavailable
// gate) would exit to Stable with a permanently dead partition loop.
func TestAttemptRecovery_StreamMissingExhausted_LatchSurvivesReasonOverlap(t *testing.T) {
	t.Parallel()
	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	m, _ := armDegraded(t, &snap, snap)
	plantAssignment(t, m, snap)
	sinceNano := time.Now().UnixNano()
	m.markDegraded(sinceNano, DegradeReasonKVUnavailable)

	// Exhaustion fires while already Degraded: enterDegraded CAS no-ops, only
	// the latch records it.
	m.onStreamMissingError("ORDERS", errors.New("recovery exhausted"))

	// The kv-unavailable gate's own exit signal is satisfied (fresh heartbeat
	// after the degrade) — without the latch, recovery would exit to Stable.
	m.lastHeartbeatSuccessAt.Store(sinceNano + int64(time.Minute))

	m.attemptRecoveryFromDegraded()

	require.Equal(t, StateDegraded, m.State(),
		"the exhaustion latch must hold the worker Degraded even when the degrade reason belongs to another (recovered) failure")
}
