package failure_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/durable"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestClaimResolver_RecoversAfterNATSRestart exercises the end-to-end production
// trigger for the resolver-recovery fix in commit 30e93ce.
//
// The unit reproducer (internal/durable/claim_resolver_watcher_freeze_test.go)
// closes the watcher via the cooperative r.watcher.Stop() path. This integration
// test instead does a real NATS server restart (shutdown + bring up a new server
// on the same client port + same JetStream StoreDir), which is the actual
// production failure trigger.
//
// Empirical finding from this test: with the JetStream client version Parti
// pins, a NATS server restart does NOT reliably surface as an Updates() channel
// close on the existing KV watcher — the client transparently rebinds, the
// channel stays "open", but the watcher silently stops delivering events.
// So the assertion below is recovered by the OTHER half of the fix: the
// periodic reconciler (which re-walks the bucket and applies missed state via
// the same revision-aware path). Both halves of the fix are necessary; this
// test pins down the reconciler half end-to-end.
//
// Assertions:
//  1. Cache convergence: after NATS restart, a fresh claim KV write is observed
//     by every worker's ClaimBasedResolver within a bounded window. Without the
//     fix the resolver has no recovery path at all (no supervisor restart, no
//     periodic reconcile) and the cache stays frozen at the pre-restart state.
//  2. Message flow continues: a post-restart burst across the partitions whose
//     ownership is unchanged is consumed within 15s, proving no permanent
//     not_owner pull suppression survived the restart.
//
// The injected ClaimBasedResolver (via ResolverConfig.OwnershipResolver) gives
// the test a direct handle on each worker's resolver; the fix lives in the
// resolver itself, so we still exercise the same supervisor / reconciler code
// path that production uses via ensureGateResolver.
//
// To make assertion (1) tractable inside the test's 20s convergence bound we
// override the default 30s reconcile cadence to 1s via WithReconcileInterval.
// The 20s bound (vs. the ~1-2s the reconciler typically needs to detect drift)
// is headroom for post-restart JetStream client re-stabilization on slow CI
// runners: the watcher used internally by KV.Keys() can take several seconds
// to rebind after a server restart, and we want multiple reconcile attempts
// to land inside the bound.
// This option is part of the fix; the parent base 5a9102f does not have it
// (and does not have either recovery path), so the test fails there.
func TestClaimResolver_RecoversAfterNATSRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Start embedded NATS with a restart helper. The helper preserves the
	// client port and StoreDir so JetStream stream + KV state survives.
	// Cleanup is registered inside the helper so the live (possibly
	// restarted) server is the one that gets shut down.
	_, nc, restart := startEmbeddedNATSWithRestart(t)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Stream with file storage so it survives the server restart.
	const streamName = "EVENTS"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{"events.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.FileStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	// Build the partition set. 4 partitions is enough to spread across 3 workers
	// and exercise real ownership without making the test slow.
	const numPartitions = 4
	parts := make([]parti.Partition, numPartitions)
	pids := make([]string, numPartitions)
	for i := range numPartitions {
		pid := fmt.Sprintf("p%d", i)
		parts[i] = parti.Partition{Keys: []string{pid}}
		pids[i] = pid
	}
	src := source.NewStatic(parts)
	curStrategy := strategy.NewConsistentHash()

	// Unique handoff bucket so this test is isolated from any others. The
	// manager will open this bucket on Start (FileStorage) since we use the
	// same name. The bucket is shared by all workers — that is the production
	// shape.
	const handoffBucket = "parti-handoff"

	// Bring up N=3 workers, each with:
	//   - its own injected ClaimBasedResolver pointing at the shared claim KV;
	//   - a WorkerConsumer with ProcessingGate enabled and the resolver wired
	//     in via ResolverConfig.OwnershipResolver (which overrides the
	//     auto-creation path but uses the same code under test);
	//   - a Manager with two-phase handoff enabled, wired to the WorkerConsumer
	//     via WithWorkerConsumerUpdater so partition transitions flow through
	//     prepare/commit/stable claim writes.
	const numWorkers = 3

	// Eager-create the handoff KV so we can wire each worker's resolver at it
	// before the manager has run its setup. The manager will reopen the bucket
	// idempotently via kvutil.EnsureKVBucket.
	handoffKV, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  handoffBucket,
		Storage: jetstream.FileStorage,
		History: 1,
		TTL:     2 * time.Minute,
	})
	require.NoError(t, err)

	wsParams := workerStackParams{
		js:            js,
		src:           src,
		curStrategy:   curStrategy,
		streamName:    streamName,
		handoffBucket: handoffBucket,
		handoffKV:     handoffKV,
	}
	stacks := make([]*workerStack, numWorkers)
	for i := range numWorkers {
		stacks[i] = buildWorkerStack(t, ctx, wsParams, i)
	}

	// Wait until every partition is owned by exactly one worker via the
	// Manager assignment, AND each owner's resolver cache reports itself
	// as the owner per the claims KV. Two-phase handoff writes claims as
	// part of the initial bootstrap, so both views converge.
	waitForInitialConvergence(t, stacks, numPartitions)

	// Baseline message flow: each owner consumes its own partition's messages
	// while non-owners' processing gates suppress pulls.
	const baselineBurst = 2
	publishBurstToPartitions(t, ctx, js, pids, baselineBurst, "baseline-")
	waitForConsumption(t, stacks, pids, nil, baselineBurst, 10*time.Second,
		"baseline messages did not flow before NATS restart")

	// Snapshot per-worker resolver state for every PID. We use this only for
	// diagnostics if the post-restart convergence assertion fails.
	preRestart := make(map[string]map[string]string, numWorkers) // worker -> pid -> owner
	for _, s := range stacks {
		m := make(map[string]string, numPartitions)
		for _, pid := range pids {
			owner, _, _, _ := s.resolver.GetOwner(pid)
			m[pid] = owner
		}
		preRestart[s.mgr.WorkerID()] = m
	}

	// --- The production failure trigger: a real NATS server restart. ---
	restartStart := time.Now()
	restart(t)
	restartElapsed := time.Since(restartStart)
	require.Less(t, restartElapsed, 5*time.Second,
		"NATS restart-to-reconnect window exceeded the 5s bound")
	t.Logf("NATS restart + client reconnect completed in %v", restartElapsed)

	// Read back the post-restart claim KV (FileStorage means it survives).
	// We expect the claim set to match the pre-restart assignment because
	// the manager-driven handoff coordinator wrote stable claims during
	// initial bootstrap.
	postRestartKV, err := js.KeyValue(ctx, handoffBucket)
	require.NoError(t, err)

	// Discriminating step: directly write a fresh claim for ONE partition,
	// changing its owner to a synthetic value. The cache MUST converge
	// within the bound. Without the fix the resolver has no recovery path
	// at all and the cache will stay frozen at the pre-restart owner forever.
	//
	// We pick a partition whose ORIGINAL owner is not "synthetic-mutator" so
	// the change is real for every worker's cache.
	targetPID := pids[0]
	const mutatedOwner = "synthetic-mutator"
	mutateClaimOwner(t, ctx, postRestartKV, targetPID, mutatedOwner)

	// (1) Cache convergence assertion. This is THE discriminating check.
	waitForCacheConvergence(t, ctx, stacks, targetPID, mutatedOwner, preRestart)

	// (2) Message-flow assertion. The mutated claim's owner ("synthetic-mutator")
	// does not actually exist in the cluster, so consumption of targetPID would
	// be (correctly) suppressed by every worker's processing gate. We exclude
	// targetPID from this check and verify that the OTHER partitions still
	// flow end-to-end — this proves no permanent not_owner suppression
	// survived the restart for partitions whose ownership is unchanged.
	flowPids := pids[1:]
	const postBurst = 3
	baseCounts := snapshotConsumed(stacks, flowPids)
	publishBurstToPartitions(t, ctx, js, flowPids, postBurst, "post-")
	waitForConsumption(t, stacks, flowPids, baseCounts, postBurst, 15*time.Second,
		"post-restart messages were not consumed within the 15s bound — "+
			"this indicates permanent not_owner suppression survived the restart")
}

// waitForInitialConvergence waits until every partition has an owner per the
// manager assignment and every worker's resolver cache agrees on that owner.
func waitForInitialConvergence(t *testing.T, stacks []*workerStack, numPartitions int) {
	t.Helper()
	require.Eventually(t, func() bool {
		ownerByPID := make(map[string]string)
		for _, s := range stacks {
			for _, p := range s.mgr.CurrentAssignment().Partitions {
				ownerByPID[p.ID()] = s.mgr.WorkerID()
			}
		}
		if len(ownerByPID) != numPartitions {
			return false
		}
		for pid, owner := range ownerByPID {
			for _, s := range stacks {
				got, _, _, ok := s.resolver.GetOwner(pid)
				if !ok || got != owner {
					return false
				}
			}
		}

		return true
	}, 20*time.Second, 100*time.Millisecond,
		"initial assignment+claim convergence did not complete")
}

// publishBurstToPartitions publishes count messages to "events.<pid>" for each
// pid. The payload is purely diagnostic — counters key only on PID.
func publishBurstToPartitions(t *testing.T, ctx context.Context, js jetstream.JetStream, pids []string, count int, prefix string) {
	t.Helper()
	for _, pid := range pids {
		for j := range count {
			_, err := js.Publish(ctx, "events."+pid, fmt.Appendf(nil, "%s%s-%d", prefix, pid, j))
			require.NoError(t, err)
		}
	}
}

// snapshotConsumed returns the current consumed-count totals across all workers
// keyed by PID, for use as a base to count messages from a subsequent burst.
func snapshotConsumed(stacks []*workerStack, pids []string) map[string]int32 {
	out := make(map[string]int32, len(pids))
	for _, pid := range pids {
		out[pid] = totalConsumed(stacks, pid)
	}

	return out
}

// waitForConsumption polls until each PID's consumed-count delta over base
// reaches expected. base may be nil to count from zero.
func waitForConsumption(t *testing.T, stacks []*workerStack, pids []string, base map[string]int32, expected int, timeout time.Duration, msg string) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, pid := range pids {
			cur := totalConsumed(stacks, pid)
			if base != nil {
				cur -= base[pid]
			}
			if int(cur) < expected {
				return false
			}
		}

		return true
	}, timeout, 100*time.Millisecond, msg)
}

// mutateClaimOwner writes a fresh claim to KV that changes the owner of
// partitionID to newOwner with a bumped epoch. The bumped epoch is required
// because the resolver's revision-aware apply path rejects state at older
// revisions.
func mutateClaimOwner(t *testing.T, ctx context.Context, kv jetstream.KeyValue, partitionID, newOwner string) {
	t.Helper()
	currentEntry, err := kv.Get(ctx, "claims/"+partitionID)
	require.NoError(t, err, "claim for target partition should exist")
	currentClaim, err := handoff.UnmarshalClaim(currentEntry.Value())
	require.NoError(t, err)
	mutated := handoff.Claim{
		PartitionID: partitionID,
		Owner:       newOwner,
		State:       handoff.ClaimStateStable,
		Epoch:       currentClaim.Epoch + 100, // generous monotonic bump
		LastUpdated: time.Now().UTC(),
		TTLSeconds:  currentClaim.TTLSeconds,
	}
	mutatedBytes, err := mutated.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/"+partitionID, mutatedBytes)
	require.NoError(t, err)
}

// waitForCacheConvergence asserts that every worker's resolver cache reports
// expectedOwner for partitionID within 20s. preRestartSnapshot is included in
// the failure message for diagnostics.
func waitForCacheConvergence(
	t *testing.T,
	ctx context.Context,
	stacks []*workerStack,
	partitionID, expectedOwner string,
	preRestartSnapshot map[string]map[string]string,
) {
	t.Helper()
	convergenceCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	require.Eventually(t, func() bool {
		for _, s := range stacks {
			owner, _, _, ok := s.resolver.GetOwner(partitionID)
			if !ok || owner != expectedOwner {
				return false
			}
		}

		return true
	}, time.Until(deadline(convergenceCtx)), 100*time.Millisecond,
		"resolver cache did not converge to mutated owner after NATS restart — "+
			"this is the production bug: the watcher silently stops delivering "+
			"events on server restart and, without the periodic-reconcile + "+
			"watcher-restart fix in commit 30e93ce, the cache stays frozen at "+
			"the pre-restart state forever. pre-restart snapshot=%v",
		preRestartSnapshot)
}

// workerStack bundles one worker's manager, injected resolver, and per-PID
// consumption counters. processedByPID stores *int32 values keyed by
// partition ID.
type workerStack struct {
	mgr      *parti.Manager
	resolver *durable.ClaimBasedResolver
	// processedByPID counts messages consumed on THIS worker, keyed by the
	// partition ID extracted from the message subject.
	processedByPID *sync.Map
}

// workerStackParams bundles the shared deps used to build a worker stack.
type workerStackParams struct {
	js            jetstream.JetStream
	src           parti.PartitionSource
	curStrategy   parti.AssignmentStrategy
	streamName    string
	handoffBucket string
	handoffKV     jetstream.KeyValue
}

// buildWorkerStack constructs and starts one worker's full stack: an injected
// ClaimBasedResolver, a WorkerConsumer with ProcessingGate enabled, and a
// Manager with two-phase handoff. All cleanup is registered on t.
func buildWorkerStack(t *testing.T, ctx context.Context, p workerStackParams, index int) *workerStack {
	t.Helper()
	s := &workerStack{processedByPID: &sync.Map{}}

	// The periodic reconcile is the half of the fix that catches a
	// silently-broken watcher (the NATS client does not always surface a
	// server restart as an Updates() channel close — see the test doc).
	// With a 1s interval we converge inside the 20s bound. Production
	// default is 30s, which would not fit the test window but is otherwise
	// the same code path.
	s.resolver = durable.NewClaimBasedResolver(
		p.handoffKV, "claims/", nil,
		durable.WithReconcileInterval(1*time.Second),
	)
	require.NoError(t, s.resolver.Start(ctx))
	t.Cleanup(s.resolver.Stop)

	wc, err := durable.NewWorkerConsumer(p.js, durable.WorkerConsumerConfig{
		StreamName:      p.streamName,
		ConsumerPrefix:  fmt.Sprintf("w%d", index),
		SubjectTemplate: "events.{{.PartitionID}}",
		BatchSize:       1,
		FetchTimeout:    1 * time.Second,
		AckWait:         2 * time.Second,
		MaxDeliver:      -1,
		ProcessingGate: &durable.ProcessingGateConfig{
			Enabled:  true,
			NakDelay: 50 * time.Millisecond,
			AllowedStates: []types.HandoffState{
				types.HandoffStateStable,
				types.HandoffStateCommit,
			},
		},
		Resolver: durable.ResolverConfig{
			HandoffBucketName:   p.handoffBucket,
			HandoffClaimsPrefix: "claims/",
			OwnershipResolver:   s.resolver,
		},
		PullGatingEnabled: true,
	}, s.makeHandler())
	require.NoError(t, err)
	t.Cleanup(func() { _ = wc.Close(context.Background()) })

	cfg := parti.TestConfig()
	cfg.EnableTwoPhaseHandoff = true
	cfg.KVBuckets.HandoffBucket = p.handoffBucket
	cfg.KVBuckets.HandoffTTL = 2 * time.Minute
	cfg.WorkerIDMax = 8

	mgr, err := parti.NewManager(&cfg, p.js, p.src, p.curStrategy,
		parti.WithWorkerConsumerUpdater(wc),
	)
	require.NoError(t, err)
	s.mgr = mgr
	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	return s
}

// makeHandler returns a message handler that records consumption by partition
// ID. Subject template is "events.{{.PartitionID}}" with single-key partitions,
// so the PID is recovered by stripping the "events." prefix.
func (s *workerStack) makeHandler() func(context.Context, jetstream.Msg) error {
	return func(ctx context.Context, msg jetstream.Msg) error {
		const prefix = "events."
		if subj := msg.Subject(); len(subj) > len(prefix) {
			s.recordConsumed(subj[len(prefix):])
		}

		return msg.Ack()
	}
}

// recordConsumed atomically increments the per-PID consumption counter,
// initializing the int32 storage on first observation.
func (s *workerStack) recordConsumed(pid string) {
	if v, ok := s.processedByPID.Load(pid); ok {
		atomic.AddInt32(v.(*int32), 1) //nolint:errcheck // type guaranteed by Store

		return
	}
	var c int32 = 1
	if actual, loaded := s.processedByPID.LoadOrStore(pid, &c); loaded {
		atomic.AddInt32(actual.(*int32), 1) //nolint:errcheck
	}
}

// totalConsumed sums across all workers the per-PID consumption count.
func totalConsumed(stacks []*workerStack, pid string) int32 {
	var total int32
	for _, s := range stacks {
		if v, ok := s.processedByPID.Load(pid); ok {
			total += atomic.LoadInt32(v.(*int32)) //nolint:errcheck
		}
	}

	return total
}

// startEmbeddedNATSWithRestart starts an embedded NATS server with JetStream
// enabled and returns the server, a connected client, and a helper that
// shuts down the current server and brings up a new one on the same client
// port and StoreDir. JetStream streams and KV buckets backed by FileStorage
// therefore survive the restart.
//
// The client is configured with nats.MaxReconnects(-1) (unlimited) and a
// short ReconnectWait so it transparently rejoins the new server. Without
// these, a single restart can permanently disconnect the client and mask the
// behavior under test.
func startEmbeddedNATSWithRestart(t *testing.T) (*server.Server, *nats.Conn, func(t *testing.T)) {
	t.Helper()

	port, err := getFreePortForRestart()
	require.NoError(t, err)
	storeDir := t.TempDir()

	buildOpts := func() *server.Options {
		return &server.Options{
			Host:      "127.0.0.1",
			Port:      port,
			JetStream: true,
			StoreDir:  storeDir,
			NoLog:     true,
		}
	}

	startServer := func() *server.Server {
		ns, serr := server.NewServer(buildOpts())
		require.NoError(t, serr)
		go ns.Start()
		require.True(t, ns.ReadyForConnections(5*time.Second),
			"embedded NATS not ready for connections")
		return ns
	}

	ns := startServer()
	nsPtr := &serverHolder{srv: ns}

	// Register cleanup that reads the live server pointer at teardown time,
	// not the initial server captured here — restart() may have swapped it.
	t.Cleanup(func() {
		live := nsPtr.srv
		if live != nil {
			live.Shutdown()
			live.WaitForShutdown()
		}
	})

	nc, err := nats.Connect(ns.ClientURL(),
		nats.Timeout(2*time.Second),
		nats.RetryOnFailedConnect(true),
		// Unlimited reconnect attempts: a single restart must not drop the
		// client permanently. ReconnectWait is small so the post-restart
		// reconnect window fits inside the test's 5s bound.
		nats.MaxReconnects(-1),
		nats.ReconnectWait(100*time.Millisecond),
	)
	require.NoError(t, err)
	t.Cleanup(func() { nc.Close() })

	restart := func(t *testing.T) {
		t.Helper()
		nsPtr.srv.Shutdown()
		nsPtr.srv.WaitForShutdown()
		// Bring up a new server on the SAME port and SAME StoreDir.
		nsPtr.srv = startServer()
		// Wait for the client to observe the reconnect. nats.Conn.Status()
		// transitions through RECONNECTING -> CONNECTED once the new server
		// accepts the connection. Bound the wait at 5s (same as the test's
		// restart-window assertion).
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if nc.Status() == nats.CONNECTED {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("client did not reconnect within 5s after NATS restart (status=%v)", nc.Status())
	}

	// Expose the live server pointer to the cleanup hook via closure.
	return nsPtr.srv, nc, restart
}

// serverHolder lets the restart closure swap the live *server.Server while
// keeping a stable reference for the cleanup hook in t.Cleanup.
type serverHolder struct {
	srv *server.Server
}

// getFreePortForRestart returns a TCP port that is currently free. We need a
// stable port number (not -1) so the post-restart server reuses the same
// client URL, allowing the existing nats.Conn to transparently reconnect.
//
// There is an inherent TOCTOU window between freeing the port and the server
// binding it, but for a single in-process test on localhost this is negligible.
func getFreePortForRestart() (int, error) {
	addr, err := net.ResolveTCPAddr("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	tcpAddr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.New("failed to obtain TCP address")
	}

	return tcpAddr.Port, nil
}

// deadline returns the deadline of ctx, or time.Now() if none is set. Used
// to clamp inner Eventually timeouts to a parent context's bound.
func deadline(ctx context.Context) time.Time {
	if d, ok := ctx.Deadline(); ok {
		return d
	}

	return time.Now()
}
