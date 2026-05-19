package election

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
)

func TestNATSElection_RequestLeadership(t *testing.T) {
	t.Run("acquires leadership when no leader exists", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-1")

		election := NewNATSElection(kv, "leader")

		isLeader, err := election.RequestLeadership(ctx, "worker-1", 30)
		require.NoError(t, err)
		require.True(t, isLeader)
		require.Equal(t, "worker-1", election.WorkerID())
	})

	t.Run("fails when another worker is leader", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-2")

		// First worker becomes leader
		election1 := NewNATSElection(kv, "leader")
		isLeader, err := election1.RequestLeadership(ctx, "worker-1", 30)
		require.NoError(t, err)
		require.True(t, isLeader)

		// Second worker tries to become leader
		election2 := NewNATSElection(kv, "leader")
		isLeader, err = election2.RequestLeadership(ctx, "worker-2", 30)
		require.NoError(t, err)
		require.False(t, isLeader)
	})

	t.Run("renews leadership if already leader", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-3")

		election := NewNATSElection(kv, "leader")

		// Acquire leadership
		isLeader, err := election.RequestLeadership(ctx, "worker-1", 30)
		require.NoError(t, err)
		require.True(t, isLeader)

		// Request again (should renew)
		isLeader, err = election.RequestLeadership(ctx, "worker-1", 30)
		require.NoError(t, err)
		require.True(t, isLeader)
	})

	t.Run("returns error for invalid lease duration", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-4")

		election := NewNATSElection(kv, "leader")

		isLeader, err := election.RequestLeadership(ctx, "worker-1", 0)
		require.ErrorIs(t, err, ErrInvalidDuration)
		require.False(t, isLeader)
	})
}

func TestNATSElection_RenewLeadership(t *testing.T) {
	t.Run("renews leadership successfully", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-renew-1")

		election := NewNATSElection(kv, "leader")

		// Acquire leadership
		isLeader, err := election.RequestLeadership(ctx, "worker-1", 30)
		require.NoError(t, err)
		require.True(t, isLeader)

		// Renew leadership
		err = election.RenewLeadership(ctx)
		require.NoError(t, err)
	})

	t.Run("fails if not the leader", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-renew-2")

		election := NewNATSElection(kv, "leader")

		err := election.RenewLeadership(ctx)
		require.ErrorIs(t, err, ErrNotLeader)
	})

	t.Run("fails if leadership was lost", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-renew-3")

		election := NewNATSElection(kv, "leader")

		// Acquire leadership
		isLeader, err := election.RequestLeadership(ctx, "worker-1", 30)
		require.NoError(t, err)
		require.True(t, isLeader)

		// Another process takes over (simulate by deleting and recreating)
		err = kv.Delete(ctx, "leader")
		require.NoError(t, err)

		// Try to renew - should fail
		err = election.RenewLeadership(ctx)
		require.ErrorIs(t, err, ErrLeadershipLost)
	})
}

func TestNATSElection_ReleaseLeadership(t *testing.T) {
	t.Run("releases leadership successfully", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-release-1")

		election := NewNATSElection(kv, "leader")

		// Acquire leadership
		isLeader, err := election.RequestLeadership(ctx, "worker-1", 30)
		require.NoError(t, err)
		require.True(t, isLeader)

		// Release leadership
		err = election.ReleaseLeadership(ctx)
		require.NoError(t, err)
		require.Empty(t, election.WorkerID())

		// Verify key is deleted
		_, err = kv.Get(ctx, "leader")
		require.Error(t, err)
		require.ErrorIs(t, err, jetstream.ErrKeyNotFound)
	})

	t.Run("fails if not the leader", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-release-2")

		election := NewNATSElection(kv, "leader")

		err := election.ReleaseLeadership(ctx)
		require.ErrorIs(t, err, ErrNotLeader)
	})

	t.Run("allows another worker to become leader", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-release-3")

		// First worker becomes leader
		election1 := NewNATSElection(kv, "leader")
		isLeader, err := election1.RequestLeadership(ctx, "worker-1", 30)
		require.NoError(t, err)
		require.True(t, isLeader)

		// Release leadership
		err = election1.ReleaseLeadership(ctx)
		require.NoError(t, err)

		// Second worker can now become leader
		election2 := NewNATSElection(kv, "leader")
		isLeader, err = election2.RequestLeadership(ctx, "worker-2", 30)
		require.NoError(t, err)
		require.True(t, isLeader)
	})
}

func TestNATSElection_IsLeader(t *testing.T) {
	t.Run("returns true when leader", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-isleader-1")

		election := NewNATSElection(kv, "leader")

		// Acquire leadership
		isLeader, err := election.RequestLeadership(ctx, "worker-1", 30)
		require.NoError(t, err)
		require.True(t, isLeader)

		// Check leadership
		isLeader, err = election.IsLeader(ctx)
		require.NoError(t, err)
		require.True(t, isLeader)
	})

	t.Run("returns false when not leader", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-isleader-2")

		election := NewNATSElection(kv, "leader")

		isLeader, err := election.IsLeader(ctx)
		require.NoError(t, err)
		require.False(t, isLeader)
	})

	t.Run("returns false when key was deleted", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-isleader-3")

		election := NewNATSElection(kv, "leader")

		// Acquire leadership
		isLeader, err := election.RequestLeadership(ctx, "worker-1", 30)
		require.NoError(t, err)
		require.True(t, isLeader)

		// Delete the key (simulate another process taking over)
		err = kv.Delete(ctx, "leader")
		require.NoError(t, err)

		// Check leadership - should detect we lost it
		isLeader, err = election.IsLeader(ctx)
		require.NoError(t, err)
		require.False(t, isLeader)
	})

	t.Run("returns false when revision changed", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-isleader-4")

		election := NewNATSElection(kv, "leader")

		// Acquire leadership
		isLeader, err := election.RequestLeadership(ctx, "worker-1", 30)
		require.NoError(t, err)
		require.True(t, isLeader)

		// Another process takes over
		err = kv.Delete(ctx, "leader")
		require.NoError(t, err)
		_, err = kv.Create(ctx, "leader", []byte("worker-2"))
		require.NoError(t, err)

		// Check leadership - should detect revision changed
		isLeader, err = election.IsLeader(ctx)
		require.NoError(t, err)
		require.False(t, isLeader)
	})
}

func TestNATSElection_LeadershipFailover(t *testing.T) {
	t.Run("automatic failover on TTL expiry", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)

		// Create KV with short TTL for testing
		js, err := jetstream.New(nc)
		require.NoError(t, err)

		kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:  "test-election-failover",
			TTL:     1 * time.Second, // Reduced from 2s to 1s for faster test
			Storage: jetstream.MemoryStorage,
		})
		require.NoError(t, err)

		// Worker 1 becomes leader
		election1 := NewNATSElection(kv, "leader")
		isLeader, err := election1.RequestLeadership(ctx, "worker-1", 2)
		require.NoError(t, err)
		require.True(t, isLeader)

		// Wait for TTL to expire (1s + margin)
		time.Sleep(1200 * time.Millisecond)

		// Worker 2 can now become leader
		election2 := NewNATSElection(kv, "leader")
		isLeader, err = election2.RequestLeadership(ctx, "worker-2", 2)
		require.NoError(t, err)
		require.True(t, isLeader)
	})
}

func TestNATSElection_ConcurrentLeadership(t *testing.T) {
	t.Run("only one worker becomes leader", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-concurrent")

		numWorkers := 5
		results := make(chan bool, numWorkers)
		errs := make(chan error, numWorkers)

		// Start multiple workers trying to become leader
		for i := range numWorkers {
			go func(workerNum int) {
				election := NewNATSElection(kv, "leader")
				workerID := "worker-" + string(rune('0'+workerNum))
				isLeader, err := election.RequestLeadership(ctx, workerID, 30)
				if err != nil {
					errs <- err
					return
				}
				results <- isLeader
			}(i)
		}

		// Collect results
		leaderCount := 0
		for range numWorkers {
			select {
			case isLeader := <-results:
				if isLeader {
					leaderCount++
				}
			case err := <-errs:
				t.Fatalf("Request leadership failed: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("Timeout waiting for leadership requests")
			}
		}

		// Exactly one worker should be leader
		require.Equal(t, 1, leaderCount, "Expected exactly one leader")
	})
}

// TestNATSElection_ClearLeadership_ResetsRevision verifies that involuntary
// leadership loss (via clearLeadership) zeros the revision so that Revision()
// does not return a stale value after the election key is gone.
func TestNATSElection_ClearLeadership_ResetsRevision(t *testing.T) {
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "test-election-clear")

	e := NewNATSElection(kv, "leader")

	// Acquire leadership so revision is set to a non-zero value.
	isLeader, err := e.RequestLeadership(ctx, "worker-1", 30)
	require.NoError(t, err)
	require.True(t, isLeader)
	require.NotZero(t, e.Revision(), "revision must be non-zero after acquiring leadership")

	// Simulate involuntary leadership loss.
	e.clearLeadership()

	require.Zero(t, e.Revision(), "Revision must be 0 after clearLeadership")
	require.Empty(t, e.WorkerID(), "WorkerID must be empty after clearLeadership")
}

// TestNATSElection_Revision_StableAcrossRenewals verifies that Revision() returns
// the term-epoch revision (set at initial acquisition) and does not advance on
// subsequent renewals. A stale-assignment check based on LeaderRevision would
// incorrectly discard valid assignments if this value advanced on every renewal.
func TestNATSElection_Revision_StableAcrossRenewals(t *testing.T) {
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "test-election-term-rev")

	e := NewNATSElection(kv, "leader")

	isLeader, err := e.RequestLeadership(ctx, "worker-1", 30)
	require.NoError(t, err)
	require.True(t, isLeader)

	termRev := e.Revision()
	require.NotZero(t, termRev, "term revision must be non-zero after acquisition")

	// Renew several times; the term revision must remain unchanged.
	for i := range 3 {
		err = e.RenewLeadership(ctx)
		require.NoError(t, err, "renewal %d failed", i+1)
		require.Equal(t, termRev, e.Revision(),
			"Revision() must not advance on renewal %d (was %d, got %d)", i+1, termRev, e.Revision())
	}

	// Simulate a leader change: first leader releases, second worker acquires.
	err = e.ReleaseLeadership(ctx)
	require.NoError(t, err)

	e2 := NewNATSElection(kv, "leader")
	isLeader, err = e2.RequestLeadership(ctx, "worker-2", 30)
	require.NoError(t, err)
	require.True(t, isLeader)

	// The new leader's term revision must be strictly greater than the old one.
	require.Greater(t, e2.Revision(), termRev,
		"new leader's term revision must exceed previous leader's term revision")
}

// TestNATSElection_CheckLeadership_LiveKVRevision verifies that CheckLeadership
// reads the live election KV key and returns ErrLeadershipRevisionMismatch
// whenever the claimed revision does not match the live one — including the
// case where leadership has been taken over by another worker but the former
// leader's local cached termRevision is still stable. This is the live fence
// the assignment publisher relies on for its pre-alias and post-alias
// leadership rechecks (P0-1).
func TestNATSElection_CheckLeadership_LiveKVRevision(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "test-election-checkleadership")

	e := NewNATSElection(kv, "leader")
	isLeader, err := e.RequestLeadership(ctx, "worker-1", 30)
	require.NoError(t, err)
	require.True(t, isLeader)
	claimed := e.Revision()
	require.NotZero(t, claimed)

	// Happy path: claimed matches live.
	require.NoError(t, e.CheckLeadership(ctx, claimed))

	// Wrong revision claim: returns ErrLeadershipRevisionMismatch.
	err = e.CheckLeadership(ctx, claimed+1)
	require.Error(t, err)
	require.True(t, errors.Is(err, types.ErrLeadershipRevisionMismatch),
		"want ErrLeadershipRevisionMismatch, got %v", err)

	// Zero claim: also a mismatch.
	err = e.CheckLeadership(ctx, 0)
	require.True(t, errors.Is(err, types.ErrLeadershipRevisionMismatch))

	// Simulate takeover: delete the leader key out from under e (a real takeover
	// would atomically replace it; for this unit test we model the absence
	// path explicitly). e.Revision() still returns the stable termRevision —
	// CheckLeadership must read the LIVE state and detect the mismatch.
	require.NoError(t, kv.Delete(ctx, "leader"))
	require.Equal(t, claimed, e.Revision(), "cached Revision must still report the stable term")
	err = e.CheckLeadership(ctx, claimed)
	require.True(t, errors.Is(err, types.ErrLeadershipRevisionMismatch),
		"missing leader key must surface as ErrLeadershipRevisionMismatch, got %v", err)

	// Now another worker takes over at a fresh revision; the former leader's
	// cached termRevision is unchanged but CheckLeadership must reject the
	// stale claim on a LIVE basis.
	e2 := NewNATSElection(kv, "leader")
	isLeader, err = e2.RequestLeadership(ctx, "worker-2", 30)
	require.NoError(t, err)
	require.True(t, isLeader)
	require.NotEqual(t, claimed, e2.Revision())
	err = e.CheckLeadership(ctx, claimed)
	require.True(t, errors.Is(err, types.ErrLeadershipRevisionMismatch),
		"stale leader claim against new term must be rejected, got %v", err)
}

// TestNATSElection_RequestLeadership_CancelledRenew_PreservesLeaderStateForRelease
// is a regression test for the indirect Stop-ordering race path: when
// RequestLeadership's internal RenewLeadership call fails with a cancelled
// context, the local leader state must NOT be cleared. If it were cleared, a
// subsequent ReleaseLeadership would see isLeader=false, return ErrNotLeader,
// and skip kv.Delete — leaving the leader key until TTL expiry.
func TestNATSElection_RequestLeadership_CancelledRenew_PreservesLeaderStateForRelease(t *testing.T) {
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "test-election-request-cancelled")

	e := NewNATSElection(kv, "leader")

	// Acquire leadership so the renew-on-request path is reachable.
	isLeader, err := e.RequestLeadership(ctx, "worker-1", 30)
	require.NoError(t, err)
	require.True(t, isLeader)

	// Simulate Stop: cancel the context before calling RequestLeadership.
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel() // immediately cancelled

	// RequestLeadership with a cancelled context must return an error (not
	// silently succeed), but must NOT clear the local leader state.
	_, err = e.RequestLeadership(cancelledCtx, "worker-1", 30)
	require.Error(t, err, "RequestLeadership must return error on cancelled context during renew")

	// Local leader state must remain intact for ReleaseLeadership to act on.
	isLeaderLocal, _, _ := e.getLeaderState()
	require.True(t, isLeaderLocal,
		"local isLeader must remain true after RequestLeadership with cancelled ctx — "+
			"Stop's ReleaseLeadership must still be able to delete the key")

	// ReleaseLeadership with a fresh context must succeed and delete the key.
	err = e.ReleaseLeadership(ctx)
	require.NoError(t, err, "ReleaseLeadership must succeed after a cancelled-ctx RequestLeadership")

	// Verify the key is actually gone from KV.
	_, err = kv.Get(ctx, "leader")
	require.ErrorIs(t, err, jetstream.ErrKeyNotFound,
		"leader key must be deleted after ReleaseLeadership")
}

// TestNATSElection_RenewWithCancelledCtx_PreservesLeaderStateForRelease is a
// regression test for the Stop-ordering race: when monitorLeadership calls
// RenewLeadership with a context that was already cancelled (because Stop called
// m.cancel()), the local leader state must NOT be cleared. If it were cleared,
// Stop's subsequent ReleaseLeadership call would see isLeader=false, return
// ErrNotLeader, and skip kv.Delete — leaving the leader key until TTL expiry.
func TestNATSElection_RenewWithCancelledCtx_PreservesLeaderStateForRelease(t *testing.T) {
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "test-election-renew-cancelled")

	e := NewNATSElection(kv, "leader")

	// Acquire leadership.
	isLeader, err := e.RequestLeadership(ctx, "worker-1", 30)
	require.NoError(t, err)
	require.True(t, isLeader)

	// Simulate Stop: cancel the renew context (as if m.ctx was cancelled).
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel() // immediately cancelled

	// RenewLeadership with a cancelled context must return an error...
	err = e.RenewLeadership(cancelledCtx)
	require.Error(t, err, "RenewLeadership must return error on cancelled context")

	// ...but must NOT clear local leader state.
	isLeaderLocal, _, _ := e.getLeaderState()
	require.True(t, isLeaderLocal,
		"local isLeader must remain true after RenewLeadership with cancelled ctx — "+
			"Stop's ReleaseLeadership must still be able to delete the key")

	// ReleaseLeadership with a fresh context must succeed and delete the key.
	err = e.ReleaseLeadership(ctx)
	require.NoError(t, err, "ReleaseLeadership must succeed after a cancelled-ctx renewal")

	// Verify the key is actually gone from KV.
	_, err = kv.Get(ctx, "leader")
	require.ErrorIs(t, err, jetstream.ErrKeyNotFound,
		"leader key must be deleted after ReleaseLeadership")
}
