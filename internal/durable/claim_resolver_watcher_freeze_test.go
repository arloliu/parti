package durable

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClaimResolver_CacheFreezesAfterWatcherClose reproduces the production
// failure mode reported against parti v2.3.0: when the ClaimBasedResolver's
// KV watcher channel closes (NATS reconnect, server-side consumer GC,
// transient error), the resolver's processWatcher goroutine returns
// silently and the cache freezes forever at the pre-close state. The
// processing gate then suppresses pulls with "not_owner(owner=<stale>)"
// for partitions the worker actually owns per KV, and messages pile up
// in the stream.
//
// This is a verify-the-bug-first test. With the current (buggy) code it
// MUST FAIL — proving the bug exists. Once the watcher restart + reconcile
// fix lands, the same test MUST PASS without changes.
func TestClaimResolver_CacheFreezesAfterWatcherClose(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	// Seed an initial claim: partition pX owned by worker-A.
	initial := handoff.Claim{
		PartitionID: "pX",
		Owner:       "worker-A",
		State:       handoff.ClaimStateStable,
		Epoch:       1,
		LastUpdated: time.Now().UTC(),
	}
	bInitial, err := initial.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/pX", bInitial)
	require.NoError(t, err)

	// Start the resolver. warm() seeds the cache; startWatcher subscribes
	// to KV updates.
	r := NewClaimBasedResolver(kv, "claims/", nil)
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	// Sanity: the warm read populated the cache with the initial owner.
	require.Eventually(t, func() bool {
		owner, _, _, ok := r.GetOwner("pX")
		return ok && owner == "worker-A"
	}, 2*time.Second, 10*time.Millisecond, "initial warm did not populate cache with worker-A")

	// Force the watcher to die — this is the production trigger
	// (NATS reconnect, server-side consumer GC, etc.). Stopping the
	// underlying watcher causes its Updates() channel to be closed,
	// which is the exact !ok signal processWatcher returns on without
	// restart.
	require.NotNil(t, r.watcher)
	require.NoError(t, r.watcher.Stop())

	// Now write the new owner directly to KV. In a healthy resolver, the
	// watcher (or a periodic reconcile) would observe this within a
	// bounded time and update the cache. In the buggy resolver, the
	// cache is permanently frozen at worker-A.
	updated := handoff.Claim{
		PartitionID: "pX",
		Owner:       "worker-B",
		State:       handoff.ClaimStateStable,
		Epoch:       2,
		LastUpdated: time.Now().UTC(),
	}
	bUpdated, err := updated.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/pX", bUpdated)
	require.NoError(t, err)

	// The bound here matches the production observation: the cache
	// stayed stale for tens of seconds (the entire reassignment window)
	// despite the KV being authoritative. We give the healthy resolver
	// generous time (5s) to converge; the buggy resolver will never
	// converge.
	if !assert.Eventually(t, func() bool {
		owner, _, _, ok := r.GetOwner("pX")
		return ok && owner == "worker-B"
	}, 5*time.Second, 50*time.Millisecond) {
		owner, _, _, _ := r.GetOwner("pX")
		t.Fatalf("cache failed to converge after watcher channel close: "+
			"GetOwner(pX) still reports owner=%q after 5s, "+
			"but KV has owner=worker-B at revision 2. "+
			"This is the production bug: a closed watcher freezes "+
			"the cache permanently. Fix: restart watcher on channel "+
			"close (mirror source/nats_kv.go Pillar 2 pattern) and "+
			"add a periodic reconcile as a safety net.", owner)
	}
}
