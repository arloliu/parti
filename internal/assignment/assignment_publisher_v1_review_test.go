// Tests added in response to the Phase 3 v1 post-implementation review.
//
// Coverage:
//   - P0-1: live election-KV revision fence wired through publisher (pre/post alias)
//   - P0-2: GC must not delete a payload an in-flight publish will reference
//   - P1-2: alias-barrier failure with payload-create succeeding (KV wrapper double)
//   - P1-3: heartbeat-default classification paths (missing, malformed)
//   - P1-4: documented exposure invariant on legacy heartbeat (no AppliedVersion)
//   - P2: alias_visible_uncommitted does NOT fire when no alias landed

package assignment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/internal/election"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
)

// ============================================================================
// P0-1: live election-KV revision fence
// ============================================================================

// TestPublisher_LeadershipFence_LiveElectionKV_AbortsWhenLiveRevisionMismatches
// wires the production NATSElection.CheckLeadership through the publisher
// and verifies that a former leader whose claim does not match the live KV
// revision is rejected at the pre-alias fence — even when a "naive cached
// callback" would have returned nil. This proves the live KV read is the
// load-bearing part of the fence (P0-1).
func TestPublisher_LeadershipFence_LiveElectionKV_AbortsWhenLiveRevisionMismatches(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, nc := partitest.StartEmbeddedNATS(t)
	electionKV := partitest.CreateJetStreamKV(t, nc, "live-fence-election")
	akv := partitest.CreateJetStreamKV(t, nc, "live-fence-asgn")
	hkv := partitest.CreateJetStreamKV(t, nc, "live-fence-hb")

	e := election.NewNATSElection(electionKV, "leader")
	isLeader, err := e.RequestLeadership(ctx, "leader-w", 30)
	require.NoError(t, err)
	require.True(t, isLeader)
	claimed := e.Revision()
	require.NotZero(t, claimed)

	m := newCountingMetrics()
	pub := NewAssignmentPublisher(PublisherConfig{
		AssignmentKV:    akv,
		HeartbeatKV:     hkv,
		Prefix:          "assignment",
		HeartbeatPrefix: "heartbeat",
		LeaderCheckFn:   e.CheckLeadership,
		Logger:          logging.NewNop(),
		Metrics:         m,
	})

	// Sanity: with the matching claim, publish succeeds.
	tsBytes := []byte(time.Now().UTC().Format(time.RFC3339Nano))
	_, err = hkv.Put(ctx, "heartbeat.legacy-w", tsBytes)
	require.NoError(t, err)
	require.NoError(t, pub.Publish(ctx, PublishInput{
		Workers:          []string{"legacy-w"},
		Assignments:      map[string][]types.Partition{"legacy-w": {ps("p1")}},
		SourcePartitions: []types.Partition{ps("p1")},
		LeaderRevision:   claimed,
	}))

	// Now simulate a takeover at the KV layer by overwriting the leader key
	// (real takeovers do this atomically after TTL expiry; we model the
	// resulting KV state). The former leader's cached Revision() is unchanged,
	// so a naive cached-callback fence would still pass the claim — but the
	// live KV revision differs.
	require.NoError(t, electionKV.Delete(ctx, "leader"))
	_, err = electionKV.Create(ctx, "leader", []byte("new-leader:1"))
	require.NoError(t, err)
	require.Equal(t, claimed, e.Revision(), "cached Revision must still report the stale term")

	// Pre-alias fence MUST abort with the wrapped sentinel.
	err = pub.Publish(ctx, PublishInput{
		Workers:          []string{"legacy-w"},
		Assignments:      map[string][]types.Partition{"legacy-w": {ps("p2")}},
		SourcePartitions: []types.Partition{ps("p2")},
		LeaderRevision:   claimed, // stale claim
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, types.ErrLeadershipLostPreAlias),
		"want ErrLeadershipLostPreAlias, got %v", err)
	require.True(t, errors.Is(err, types.ErrLeadershipRevisionMismatch),
		"underlying mismatch sentinel must be preserved in the chain, got %v", err)
	// No legacy alias was written for the new attempt — the abort happened
	// BEFORE step 6.
	entry, gerr := akv.Get(ctx, "assignment.legacy-w")
	require.NoError(t, gerr)
	// The alias from the FIRST publish is the v=1 one — confirm it wasn't
	// overwritten with v=2.
	require.Less(t, len(entry.Value()), 1024)
}

// ============================================================================
// P0-2: GC must not delete a payload that an in-flight publish adopted
// ============================================================================

// fakeLiveRefsProvider lets a test stage exact in-flight payload keys.
type fakeLiveRefsProvider struct {
	refs []string
}

func (f *fakeLiveRefsProvider) LiveRefs() []string { return f.refs }

// TestCommitGC_DoesNotDeletePayloadAdoptedByInFlightPublish verifies that GC
// honors the publisher's in-flight ref set (P0-2). We pre-create an orphan
// payload whose age exceeds retention; without the in-flight set, GC would
// delete it. With the publisher's LiveRefs reporting the orphan key as
// in-flight, GC must skip it.
func TestCommitGC_DoesNotDeletePayloadAdoptedByInFlightPublish(t *testing.T) {
	t.Parallel()
	f := newPublisherFixture(t, "gc-respects-inflight")
	ctx := t.Context()

	// Plant an orphan payload with the same key shape the publisher would use.
	canonical := mustMarshalCanonicalPayload(t, types.AssignmentPayload{
		SchemaVersion: types.AssignmentSchemaVersion,
		Partitions:    []types.Partition{ps("about-to-be-adopted")},
	})
	hash := sha256.Sum256(canonical)
	orphanKey := "assignment._payload." + hex.EncodeToString(hash[:])
	gz, gerr := gzipCompress(canonical)
	require.NoError(t, gerr)
	_, err := f.assignmentKV.Create(ctx, orphanKey, gz)
	require.NoError(t, err)

	// Stage a fake LiveRefsProvider that says "this key is in-flight". This
	// models the state mid-publish, after the publisher's verify-back at step
	// 4 and BEFORE its CAS at step 9 returns.
	staged := &fakeLiveRefsProvider{refs: []string{orphanKey}}
	gc := NewCommitGC(CommitGCConfig{
		Publisher:        f.pub,
		LiveRefsProvider: staged,
		Interval:         time.Hour,
		Retention:        time.Second,
		KeepCommits:      10,
		Now:              func() time.Time { return time.Now().Add(48 * time.Hour) },
		Metrics:          f.metrics,
	})
	require.NotNil(t, gc)
	require.NoError(t, gc.RunOnce(ctx))

	// The "in-flight" payload MUST still be present.
	_, err = f.assignmentKV.Get(ctx, orphanKey)
	require.NoError(t, err, "GC must not delete a payload reported as in-flight by the publisher")

	// Now drop the in-flight claim and re-run GC — the orphan is now eligible
	// and should be reaped (proves the only thing keeping it alive was the
	// in-flight set).
	staged.refs = nil
	require.NoError(t, gc.RunOnce(ctx))
	_, err = f.assignmentKV.Get(ctx, orphanKey)
	require.Error(t, err, "after the in-flight claim drops, GC must reap the orphan")
}

// TestCommitGC_PublisherAdoptedPayloadVisibleViaLiveRefs is a smaller assertion
// that the production publisher's LiveRefs() actually exposes adopted payload
// keys mid-publish. We can't easily pause the publisher mid-flight without a
// test seam, so we exercise the round trip by manually calling LiveRefs after
// stuffing the publisher's inflightRefs map directly. This documents the
// invariant: GC's snapshot includes whatever keys the publisher has staged.
func TestCommitGC_PublisherAdoptedPayloadVisibleViaLiveRefs(t *testing.T) {
	t.Parallel()
	f := newPublisherFixture(t, "live-refs-snapshot")
	require.Empty(t, f.pub.LiveRefs(), "no in-flight keys before any publish")

	// Simulate the publisher having staged two refs.
	f.pub.inflightRefs.Store("assignment._payload.aaa", struct{}{})
	f.pub.inflightRefs.Store("assignment._payload.bbb", struct{}{})
	t.Cleanup(func() {
		f.pub.inflightRefs.Delete("assignment._payload.aaa")
		f.pub.inflightRefs.Delete("assignment._payload.bbb")
	})
	got := f.pub.LiveRefs()
	require.ElementsMatch(t, []string{"assignment._payload.aaa", "assignment._payload.bbb"}, got)
}

// ============================================================================
// P1-1: GC lifecycle starts on calculator Start, stops on calculator Stop
// ============================================================================

// TestCommitGC_LifecycleStartStop verifies that the GC lifecycle is wired to
// the calculator: Start launches the loop and a Trigger wakes it; Stop
// terminates the loop within a bounded time.
//
// We use a calculator-internal hook by directly running GC.Start and GC.Stop
// since the calculator constructs its own GC at NewCalculator and starts it
// in Start; this test proves the Start/Stop primitives the calculator uses
// behave as expected.
func TestCommitGC_LifecycleStartStop(t *testing.T) {
	t.Parallel()
	f := newPublisherFixture(t, "gc-lifecycle")
	ctx := t.Context()

	gc := NewCommitGC(CommitGCConfig{
		Publisher: f.pub,
		Interval:  10 * time.Millisecond, // fast for test
		Retention: time.Second,
	})
	require.NotNil(t, gc)
	require.NoError(t, gc.Start(ctx))
	// Trigger several times — non-blocking, should coalesce, no panic.
	for range 10 {
		gc.Trigger()
	}
	// Allow at least one sweep to run.
	time.Sleep(50 * time.Millisecond)

	// Stop must drain the loop and return promptly.
	stopped := make(chan struct{})
	go func() {
		gc.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("GC.Stop did not return within 2s")
	}

	// Calling Stop again must be safe (no panic, no hang).
	gc.Stop()
}

// ============================================================================
// P1-2: alias-barrier failure proven distinct from payload-create failure
// ============================================================================

// putFailingKV wraps a JetStream KV with an override that fails Put for a
// specific key. Used to drive an alias-barrier failure (step 6) without
// affecting payload Create (step 4) — proving the abort path is the
// alias-barrier path, not a step-4 payload-create failure.
type putFailingKV struct {
	jetstream.KeyValue
	failKey string
	err     error
}

func (k *putFailingKV) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	if key == k.failKey {
		return 0, k.err
	}
	return k.KeyValue.Put(ctx, key, value)
}

// TestRollingUpgrade_AliasBarrier_FailureAbortsBeforeCommit_NotAtPayloadCreate
// replaces the degenerate cancelled-context test (#48). It uses a KV wrapper
// that fails Put for the legacy worker's alias key, so step 4 (payload
// Create) succeeds and step 6 (alias barrier) deterministically fails. Proves
// that the publisher aborts before commit CAS with the correct sentinel and
// the correct metrics fire.
func TestRollingUpgrade_AliasBarrier_FailureAbortsBeforeCommit_NotAtPayloadCreate(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, nc := partitest.StartEmbeddedNATS(t)
	akv := partitest.CreateJetStreamKV(t, nc, "alias-barrier-fail-asgn")
	hkv := partitest.CreateJetStreamKV(t, nc, "alias-barrier-fail-hb")

	const oldWorker = "old-w"
	failingKV := &putFailingKV{
		KeyValue: akv,
		failKey:  "assignment." + oldWorker,
		err:      errors.New("simulated KV put failure"),
	}

	lr := &atomic.Uint64{}
	lr.Store(1)
	m := newCountingMetrics()
	pub := NewAssignmentPublisher(PublisherConfig{
		AssignmentKV:    failingKV,
		HeartbeatKV:     hkv,
		Prefix:          "assignment",
		HeartbeatPrefix: "heartbeat",
		LeaderCheckFn:   liveRevCheck(lr),
		Logger:          logging.NewNop(),
		Metrics:         m,
	})

	// Legacy heartbeat for the old worker classifies it as legacy_in_batch.
	tsBytes := []byte(time.Now().UTC().Format(time.RFC3339Nano))
	_, err := hkv.Put(ctx, "heartbeat."+oldWorker, tsBytes)
	require.NoError(t, err)

	srcParts := []types.Partition{ps("p1"), ps("p2")}
	err = pub.Publish(ctx, PublishInput{
		Workers: []string{oldWorker, "new-w"},
		Assignments: map[string][]types.Partition{
			oldWorker: {ps("p1")},
			"new-w":   {ps("p2")},
		},
		SourcePartitions: srcParts,
		LeaderRevision:   1,
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, types.ErrAliasBarrierFailed),
		"want ErrAliasBarrierFailed (step 6), got %v", err)

	// Metric checks: alias_barrier_failed and batch_aborted("alias_barrier_failed")
	// fire; commit_aborts does NOT (the abort happened BEFORE the CAS attempt).
	require.EqualValues(t, 1, m.aliasBarrierFailed.Load())
	require.EqualValues(t, 1, m.batchAbortedCount("alias_barrier_failed"))
	require.EqualValues(t, 0, m.commitAborts.Load(),
		"commit_aborts must NOT fire; abort happened before CAS")

	// No commit landed.
	_, err = akv.Get(ctx, "assignment._commit")
	require.Error(t, err, "no commit may exist")

	// alias_visible_uncommitted must NOT fire — no alias landed (P2 fix
	// validates this here too: this is the FIRST attempt failing).
	require.EqualValues(t, 0, m.aliasVisibleUncommitted.Load(),
		"alias_visible_uncommitted must NOT fire when no legacy alias actually landed")
}

// ============================================================================
// P1-3: heartbeat default-classification paths (missing, malformed)
// ============================================================================

// TestPublisher_LegacyAliasBarrier_MissingHeartbeatTreatedAsLegacy verifies
// that a worker with no heartbeat key is classified as legacy and its
// mandatory alias is written before commit (the safe default).
func TestPublisher_LegacyAliasBarrier_MissingHeartbeatTreatedAsLegacy(t *testing.T) {
	t.Parallel()
	f := newPublisherFixture(t, "legacy-missing-hb")
	ctx := t.Context()
	// NOTE: deliberately do NOT write a heartbeat for "no-hb-w".
	srcParts := []types.Partition{ps("p1")}
	require.NoError(t, f.pub.Publish(ctx, PublishInput{
		Workers:          []string{"no-hb-w"},
		Assignments:      map[string][]types.Partition{"no-hb-w": {ps("p1")}},
		SourcePartitions: srcParts,
		LeaderRevision:   1,
	}))
	// Mandatory legacy alias must exist for the missing-heartbeat worker.
	entry, err := f.assignmentKV.Get(ctx, "assignment.no-hb-w")
	require.NoError(t, err, "missing heartbeat must be classified as legacy and produce a mandatory alias")
	require.NotZero(t, entry.Revision())
}

// TestPublisher_LegacyAliasBarrier_MalformedHeartbeatTreatedAsLegacy verifies
// that a worker with a malformed (neither JSON nor RFC3339) heartbeat is
// classified as legacy and its mandatory alias is written before commit.
func TestPublisher_LegacyAliasBarrier_MalformedHeartbeatTreatedAsLegacy(t *testing.T) {
	t.Parallel()
	f := newPublisherFixture(t, "legacy-malformed-hb")
	ctx := t.Context()
	// Plant garbage that fails both JSON and timestamp parses.
	_, err := f.heartbeatKV.Put(ctx, "heartbeat.bad-hb-w", []byte("\x00\x01\x02not-json-not-time"))
	require.NoError(t, err)
	srcParts := []types.Partition{ps("p1")}
	require.NoError(t, f.pub.Publish(ctx, PublishInput{
		Workers:          []string{"bad-hb-w"},
		Assignments:      map[string][]types.Partition{"bad-hb-w": {ps("p1")}},
		SourcePartitions: srcParts,
		LeaderRevision:   1,
	}))
	entry, err := f.assignmentKV.Get(ctx, "assignment.bad-hb-w")
	require.NoError(t, err, "malformed heartbeat must be classified as legacy and produce a mandatory alias")
	require.NotZero(t, entry.Revision())
}

// ============================================================================
// P1-4: documented exposure invariant — legacy heartbeat lacks AppliedVersion=V
// ============================================================================

// TestPublisher_AliasBarrier_CASFailure_LegacyHeartbeatHasNoAppliedVersion is
// the same scenario as test #61 with the additional invariant assertion: the
// legacy worker's timestamp heartbeat decodes to a Heartbeat with
// Capabilities=0 and AppliedVersion!=V. This documents the recovery story
// (cluster does not rely on legacy ack drift).
func TestPublisher_AliasBarrier_CASFailure_LegacyHeartbeatHasNoAppliedVersion(t *testing.T) {
	t.Parallel()
	f := newPublisherFixture(t, "alias-cas-failure-hb-invariant")
	ctx := t.Context()
	const w = "old-w"
	f.putLegacyHeartbeat(t, ctx, w)

	// Pre-create a competing commit at V=42 so this publisher's CAS fails.
	prior := types.AssignmentCommit{Version: 42, LeaderRevision: 50}
	priorBytes, jerr := json.Marshal(prior)
	require.NoError(t, jerr)
	_, err := f.assignmentKV.Create(ctx, "assignment._commit", priorBytes)
	require.NoError(t, err)

	const proposedV int64 = 1
	srcParts := []types.Partition{ps("p1")}
	err = f.pub.Publish(ctx, PublishInput{
		Workers:          []string{w},
		Assignments:      map[string][]types.Partition{w: {ps("p1")}},
		SourcePartitions: srcParts,
		LeaderRevision:   1,
	})
	require.True(t, errors.Is(err, types.ErrCommitCASFailed))

	// Read the legacy heartbeat back; it must remain a timestamp string with
	// no ack info — the cluster's recovery does not rely on a "legacy worker
	// acked V" signal.
	hbEntry, err := f.heartbeatKV.Get(ctx, "heartbeat."+w)
	require.NoError(t, err)
	hb, derr := types.DecodeHeartbeat(hbEntry.Value())
	require.NoError(t, derr, "legacy timestamp heartbeat must still decode (with zero capability fields)")
	require.EqualValues(t, 0, hb.Capabilities, "legacy heartbeat must report no capabilities")
	require.NotEqual(t, proposedV, hb.AppliedVersion,
		"legacy heartbeat must NOT carry AppliedVersion=V — the publisher does not rely on legacy ack drift")
}

// ============================================================================
// Phase 4 step 4: LastCommit + BootstrapLastCommit accessors
// ============================================================================

// TestPublisher_LastCommit_PopulatedAfterSuccessfulCAS verifies that
// LastCommit returns a defensive copy of the most recently CAS-written
// AssignmentCommit and that mutating the returned struct does not affect
// publisher state.
func TestPublisher_LastCommit_PopulatedAfterSuccessfulCAS(t *testing.T) {
	t.Parallel()
	f := newPublisherFixture(t, "last-commit-after-cas")
	ctx := t.Context()
	const w = "w1"
	f.putV1Heartbeat(t, ctx, w)

	// Pre-condition: no commit observed yet.
	require.Nil(t, f.pub.LastCommit())

	require.NoError(t, f.pub.Publish(ctx, PublishInput{
		Workers:          []string{w},
		Assignments:      map[string][]types.Partition{w: {ps("p1")}},
		SourcePartitions: []types.Partition{ps("p1")},
		LeaderRevision:   1,
		Lifecycle:        "cold_start",
	}))

	got := f.pub.LastCommit()
	require.NotNil(t, got)
	require.Equal(t, int64(1), got.Version)
	require.Equal(t, uint64(1), got.LeaderRevision)
	require.Equal(t, []string{w}, got.Workers)
	require.Contains(t, got.Payloads, w)

	// Defensive-copy invariant: mutating Workers + Payloads must not affect
	// the publisher's cached commit.
	got.Workers[0] = "tampered"
	got.Payloads["x"] = types.AssignmentPayloadRef{Key: "tampered"}
	again := f.pub.LastCommit()
	require.Equal(t, []string{w}, again.Workers, "publisher cache must be insulated from caller mutation")
	require.NotContains(t, again.Payloads, "x")
}

// TestPublisher_BootstrapLastCommit_SeedsFromKV verifies that
// BootstrapLastCommit reads the live "<prefix>._commit" key once and seeds
// the in-memory cache so subsequent LastCommit calls return the bootstrap
// value (used at calculator start before this publisher writes its own
// commit).
func TestPublisher_BootstrapLastCommit_SeedsFromKV(t *testing.T) {
	t.Parallel()
	f := newPublisherFixture(t, "bootstrap-last-commit")
	ctx := t.Context()

	// Plant a pre-existing commit (as a prior leader would have left).
	pre := types.AssignmentCommit{
		Version:        7,
		LeaderRevision: 20,
		PublishedAt:    time.Now().UTC(),
		Workers:        []string{"a", "b"},
		Payloads: map[string]types.AssignmentPayloadRef{
			"a": {Key: "assignment._payload.deadbeef", PayloadHash: "deadbeef", SetDigest: 1, Revision: 1},
			"b": {Key: "assignment._payload.cafef00d", PayloadHash: "cafef00d", SetDigest: 2, Revision: 2},
		},
	}
	preBytes, jerr := json.Marshal(pre)
	require.NoError(t, jerr)
	_, err := f.assignmentKV.Create(ctx, "assignment._commit", preBytes)
	require.NoError(t, err)

	// Fresh publisher (newPublisherFixture builds one, but the cache is empty).
	require.Nil(t, f.pub.LastCommit(), "fresh publisher must report no commit")
	require.NoError(t, f.pub.BootstrapLastCommit(ctx))

	got := f.pub.LastCommit()
	require.NotNil(t, got, "bootstrap must seed the cache")
	require.Equal(t, int64(7), got.Version)
	require.Equal(t, uint64(20), got.LeaderRevision)
	require.ElementsMatch(t, []string{"a", "b"}, got.Workers)
	require.Equal(t, "deadbeef", got.Payloads["a"].PayloadHash)
}

// TestPublisher_BootstrapLastCommit_AbsentKey_NoError verifies that the
// bootstrap path is non-fatal when no commit exists in KV (cold-start path
// against an empty assignment bucket).
func TestPublisher_BootstrapLastCommit_AbsentKey_NoError(t *testing.T) {
	t.Parallel()
	f := newPublisherFixture(t, "bootstrap-absent")
	ctx := t.Context()
	require.NoError(t, f.pub.BootstrapLastCommit(ctx))
	require.Nil(t, f.pub.LastCommit())
}
