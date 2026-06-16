package manager_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	partitesting "github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
)

// claimWriteThrottleSpy implements types.HandoffMetricsRecorder plus the
// optional claim-write throttle sidecar (matched structurally by the Manager),
// so the test can observe that a configured claim-write rate limiter actually
// paced the startup hygiene writes end-to-end through real NATS.
type claimWriteThrottleSpy struct {
	types.NopHandoffMetricsRecorder
	mu        sync.Mutex
	throttled int
}

func (s *claimWriteThrottleSpy) IncClaimWriteThrottled() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.throttled++
}

func (s *claimWriteThrottleSpy) ObserveClaimWriteThrottleWait(float64) {}

func (s *claimWriteThrottleSpy) Throttled() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.throttled
}

// TestClaimWriteRateLimit_StartupHygienePaced is the end-to-end proof that the
// HandoffConfig.ClaimWrite{PerSec,Burst} settings flow through the Manager build
// into the startup hygiene loop and actually pace the physical claim-writes.
//
// It pre-seeds the handoff bucket with several expired non-stable claims, then
// starts a Manager with a low claim-write rate (burst=1). The startup hygiene
// pass resets each stale claim to stable; because the rate forces a wait on
// every write after the first burst token, the throttle sidecar fires. The test
// asserts (a) the manager reaches Stable, (b) the throttle metric incremented
// (the limiter was on the path), and (c) the seeded claims were reset to stable.
func TestClaimWriteRateLimit_StartupHygienePaced(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	const bucket = "parti-handoff"
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)

	// Pre-seed N expired (TTL 1s, stamped 1h ago) prepare-state claims so the
	// startup hygiene pass has real work to pace.
	const n = 8
	store := handoff.NewNATSClaimStore(kv, "claims/")
	staleStamp := time.Now().UTC().Add(-time.Hour)
	for i := range n {
		pid := "seed-" + strconv.Itoa(i)
		claim := handoff.Claim{
			PartitionID: pid,
			Owner:       "dead-worker",
			State:       handoff.ClaimStatePrepare,
			Epoch:       1,
			TTLSeconds:  1,
			LastUpdated: staleStamp,
		}
		_, perr := store.PutIfEpoch(ctx, pid, 0, claim)
		require.NoError(t, perr)
	}

	cfg := parti.TestConfig()
	cfg.WorkerIDPrefix = "cwrl-w"
	cfg.WorkerIDMax = 10
	cfg.StartupTimeout = 30 * time.Second
	cfg.OperationTimeout = 5 * time.Second // generous: paced hygiene must finish within it
	cfg.EnableTwoPhaseHandoff = true
	cfg.KVBuckets.HandoffBucket = bucket
	cfg.KVBuckets.HandoffTTL = 2 * time.Minute
	// Low rate, burst 1: the first write draws the token immediately, every
	// subsequent write waits (~50ms at 20/s) → the throttle sidecar fires.
	cfg.Handoff.ClaimWritePerSec = 20
	cfg.Handoff.ClaimWriteBurst = 1

	partitions := []parti.Partition{{Keys: []string{"a"}}, {Keys: []string{"b"}}}
	src := source.NewStatic(partitions)
	chStrat := strategy.NewConsistentHash()
	spy := &claimWriteThrottleSpy{}

	mgr, err := parti.NewManager(&cfg, js, src, chStrat,
		parti.WithHandoffMetricsRecorder(spy),
	)
	require.NoError(t, err)

	startCtx, startCancel := context.WithTimeout(ctx, 10*time.Second)
	defer startCancel()
	require.NoError(t, mgr.Start(startCtx))
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = mgr.Stop(stopCtx)
	})

	require.NoError(t, <-mgr.WaitState(parti.StateStable, 30*time.Second),
		"manager must reach Stable after paced startup hygiene")

	// The limiter must have been on the write path (burst=1 forces waits).
	assert.Positive(t, spy.Throttled(),
		"paced startup hygiene must increment the claim-write throttle metric")

	// All seeded stale claims (owner "dead-worker") must have been reset to
	// stable by the paced hygiene pass. The manager's own initial apply may add
	// further claims for partitions a/b, so filter to the seeded owner.
	claims, err := mgr.InspectHandoffClaims(ctx)
	require.NoError(t, err)
	seeded := 0
	for _, c := range claims {
		if c.Owner != "dead-worker" {
			continue
		}
		seeded++
		assert.Equalf(t, parti.HandoffClaimStable, c.State,
			"seeded claim %q must be reset to stable by paced hygiene", c.PartitionID)
	}
	require.Equal(t, n, seeded, "all seeded claims should still be present and inspected")
}
