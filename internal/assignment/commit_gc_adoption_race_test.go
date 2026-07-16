package assignment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Payload-GC-vs-adoption race (see createOrAdoptPayload in
// assignment_publisher.go and the conditioned delete in commit_gc.go
// RunOnce).
//
// The bug this closes: GC's RunOnce can classify a payload key K as an
// orphan from a stale snapshot (computeLiveSet ran before an adopter reused
// K), and a plain kv.Delete(K) races an adopter's Create→ErrKeyExists→
// verify-back with NOTHING for either side to condition on. Whichever
// interleaving wins, the loser gets no signal: GC can delete a payload a
// fresh commit now references, or (pre-fix) an adopter can silently commit a
// reference to a payload GC is about to delete.
//
// The two tests below use a KV wrapper that synchronously injects the
// "other side" at the exact instant one side's write hits the server —
// deterministically reproducing both orderings without relying on real
// goroutine scheduling.
// ============================================================================

// raceInjectingKV wraps a jetstream.KeyValue and lets a test synchronously
// run a callback the first time Delete or Update targets a specific key,
// immediately BEFORE forwarding the call to the real KV. This is how the
// tests below pin an exact interleaving between GC's delete arm and the
// publisher's adoption touch inside a single goroutine: whichever
// intercepted call is made first "happens", then the intercepted call
// itself proceeds against server state the injected callback just changed.
type raceInjectingKV struct {
	jetstream.KeyValue
	targetKey string

	mu       sync.Mutex
	fired    bool
	onDelete func()
	onUpdate func()
}

func (k *raceInjectingKV) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	k.fireOnce(key, k.onDelete)
	return k.KeyValue.Delete(ctx, key, opts...)
}

func (k *raceInjectingKV) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	k.fireOnce(key, k.onUpdate)
	return k.KeyValue.Update(ctx, key, value, revision)
}

func (k *raceInjectingKV) fireOnce(key string, fn func()) {
	if fn == nil || key != k.targetKey {
		return
	}
	k.mu.Lock()
	if k.fired {
		k.mu.Unlock()
		return
	}
	k.fired = true
	k.mu.Unlock()
	fn()
}

// racePayloadFixture wires a real JetStream-backed publisher whose
// AssignmentKV is a raceInjectingKV, plus a v1-heartbeat worker "w1" and the
// precomputed key/bytes for the payload {partition "k"} — the exact content
// a Publish({"w1": {"k"}}) call produces, byte-for-byte, via
// mustMarshalCanonicalPayload.
type racePayloadFixture struct {
	pub        *AssignmentPublisher
	realKV     jetstream.KeyValue // unwrapped — for planting/reading state directly
	wrapped    *raceInjectingKV
	metrics    *countingMetrics
	payloadKey string
	canonical  []byte
}

func newRacePayloadFixture(t *testing.T, name string) *racePayloadFixture {
	t.Helper()
	ctx := t.Context()
	_, nc := partitest.StartEmbeddedNATS(t)
	realKV := partitest.CreateJetStreamKV(t, nc, "race-asgn-"+name)
	hkv := partitest.CreateJetStreamKV(t, nc, "race-hb-"+name)

	hb := types.Heartbeat{
		WorkerID:      "w1",
		SchemaVersion: 1,
		Capabilities:  types.CapAckV1,
		Timestamp:     time.Now().UTC(),
	}
	hbData, err := json.Marshal(hb)
	require.NoError(t, err)
	_, err = hkv.Put(ctx, "heartbeat.w1", hbData)
	require.NoError(t, err)

	payload := types.AssignmentPayload{
		SchemaVersion:     types.AssignmentSchemaVersion,
		Partitions:        []types.Partition{ps("k")},
		WorkerLabelsKnown: true,
	}
	canonical := mustMarshalCanonicalPayload(t, payload)
	hash := sha256.Sum256(canonical)
	payloadKey := "assignment._payload." + hex.EncodeToString(hash[:])
	gz, err := gzipCompress(canonical)
	require.NoError(t, err)
	// Plant K as a pre-existing payload — models an old, possibly
	// cross-process payload that predates this test's publisher instance,
	// exactly like the reused-K scenario in the review reports.
	_, err = realKV.Create(ctx, payloadKey, gz)
	require.NoError(t, err)

	wrapped := &raceInjectingKV{KeyValue: realKV, targetKey: payloadKey}
	m := newCountingMetrics()
	pub := NewAssignmentPublisher(PublisherConfig{
		AssignmentKV:    wrapped,
		HeartbeatKV:     hkv,
		Prefix:          "assignment",
		HeartbeatPrefix: "heartbeat",
		LeaderCheckFn:   func(context.Context, uint64) error { return nil },
		Logger:          logging.NewNop(),
		Metrics:         m,
	})

	return &racePayloadFixture{
		pub:        pub,
		realKV:     realKV,
		wrapped:    wrapped,
		metrics:    m,
		payloadKey: payloadKey,
		canonical:  canonical,
	}
}

// publishK runs the adopter's Publish call that reuses payloadKey for
// worker "w1".
func (f *racePayloadFixture) publishK(ctx context.Context) error {
	return f.pub.Publish(ctx, PublishInput{
		Workers:          []string{"w1"},
		Assignments:      map[string][]types.Partition{"w1": {ps("k")}},
		SourcePartitions: []types.Partition{ps("k")},
		LeaderRevision:   1,
	})
}

// readCommit fetches and decodes assignment._commit via the unwrapped KV.
func (f *racePayloadFixture) readCommit(t *testing.T, ctx context.Context) *types.AssignmentCommit {
	t.Helper()
	entry, err := f.realKV.Get(ctx, "assignment._commit")
	require.NoError(t, err)
	var c types.AssignmentCommit
	require.NoError(t, json.Unmarshal(entry.Value(), &c))

	return &c
}

// TestCommitGC_PayloadAdoptionRace_AdopterTouchWins_KSurvives reproduces the
// P0 from the review reports: GC's live-set snapshot (computeLiveSet) is
// taken BEFORE an adopter reuses K, and K is old enough / outside the log
// window to be classified as an orphan. Between GC's per-key Get (the
// age-gate read whose revision the fixed delete conditions on) and GC's
// Delete call, a full adopter Publish — Create→ErrKeyExists→verify-back→
// touch→commit C2 — lands, referencing K.
//
// On parent (pre-fix) code, GC's kv.Delete(K) is unconditioned: it deletes K
// regardless of the adopter's touch/commit, so C2 ends up referencing a
// payload that no longer exists. This test's "K must survive" assertion
// FAILS on parent code (RED).
//
// After the fix, GC's delete is conditioned on the (now stale) revision it
// observed before the adoption landed; the conditioned delete is rejected
// and K survives (GREEN).
func TestCommitGC_PayloadAdoptionRace_AdopterTouchWins_KSurvives(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newRacePayloadFixture(t, "adopter-wins")

	// Inject: run the FULL adopter publish (which lands commit C2
	// referencing K) at the exact instant GC's delete call reaches the
	// server — i.e., strictly between GC's per-key Get and its Delete.
	f.wrapped.onDelete = func() {
		require.NoError(t, f.publishK(ctx), "adopter publish (C2) must succeed")
	}

	gc := NewCommitGC(CommitGCConfig{
		Publisher:   f.pub,
		Interval:    time.Hour,
		Retention:   time.Second,
		KeepCommits: 10,
		// K's real Created() timestamp is "now" (planted moments ago); skew
		// GC's clock far enough forward to clear the retention gate so K is
		// classified as an eligible orphan at computeLiveSet time (no
		// commit exists yet, so the live set is empty).
		Now:     func() time.Time { return time.Now().Add(48 * time.Hour) },
		Metrics: f.metrics,
	})
	require.NotNil(t, gc)
	require.NoError(t, gc.RunOnce(ctx))

	// K must survive: the adopter's touch (part of publishK, above) must
	// have raced ahead of GC's conditioned delete.
	_, err := f.realKV.Get(ctx, f.payloadKey)
	require.NoError(t, err, "K must survive: adopter's touch must beat GC's conditioned delete")

	// The committed C2 must actually reference the surviving K — proving
	// this isn't a case where the commit failed silently.
	commit := f.readCommit(t, ctx)
	ref, ok := commit.Payloads["w1"]
	require.True(t, ok, "C2 must reference worker w1's payload")
	require.Equal(t, f.payloadKey, ref.Key, "C2 must reference K")

	// Losing the conditioned delete is an EXPECTED-loser skip, not a delete
	// failure: the GC must not count it toward payload delete errors.
	require.EqualValues(t, 0, f.metrics.gcDeleteErrors.Load(),
		"conditioned-delete loss must not count as a payload delete error")
}

// TestCommitGC_PayloadAdoptionRace_GCDeleteWins_AdopterRecreatesAndCommits
// covers the opposite ordering named in the review: GC's delete lands
// FIRST, strictly between the adopter's verify-back Get and its touch
// (Update) call. The touch must lose (K is gone), the whole
// create-or-adopt step must retry, the retry's Create must recreate K (it
// no longer exists), and the publish must still succeed, committing a
// reference to the recreated K.
//
// This exercises the real production GC delete path (via gc.RunOnce) as the
// "GC wins" side of the race, not a hand-rolled substitute.
func TestCommitGC_PayloadAdoptionRace_GCDeleteWins_AdopterRecreatesAndCommits(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newRacePayloadFixture(t, "gc-wins")

	gc := NewCommitGC(CommitGCConfig{
		Publisher:   f.pub,
		Interval:    time.Hour,
		Retention:   time.Second,
		KeepCommits: 10,
		Now:         func() time.Time { return time.Now().Add(48 * time.Hour) },
		Metrics:     f.metrics,
	})
	require.NotNil(t, gc)

	// Inject: run a REAL GC sweep at the exact instant the adopter's touch
	// (Update) reaches the server — i.e., strictly between the adopter's
	// verify-back Get and its touch. At this point in the flow the
	// publisher has not yet registered K in inflightRefs (writePayloads has
	// not returned), so GC's LiveRefs fast path does not see it either —
	// this is precisely the "verify-back-to-registration window" the
	// review reports named as unclosed by LiveRefs alone.
	f.wrapped.onUpdate = func() {
		require.NoError(t, gc.RunOnce(ctx), "simulated concurrent GC sweep")
	}

	err := f.publishK(ctx)
	require.NoError(t, err, "adopter must recover by re-creating K after losing the touch race")

	// K must be present again (recreated by the retry, not the original
	// planted key — the original was really deleted by gc.RunOnce above).
	entry, err := f.realKV.Get(ctx, f.payloadKey)
	require.NoError(t, err, "K must be recreated after the touch lost to GC's delete")
	plain, derr := gzipDecompress(entry.Value())
	require.NoError(t, derr)
	require.Equal(t, f.canonical, plain, "recreated K must carry the same canonical content")

	// The committed C2 must reference K.
	commit := f.readCommit(t, ctx)
	ref, ok := commit.Payloads["w1"]
	require.True(t, ok)
	require.Equal(t, f.payloadKey, ref.Key)

	// Pin that the recovery path was a re-CREATE, not a reuse: the only
	// successful Create the publisher's metrics observed is the retry's
	// (the original plant used realKV.Create directly, bypassing metrics).
	require.EqualValues(t, 1, f.metrics.payloadsCreated.Load(),
		"exactly one successful Create: the retry after the touch lost to GC")
	require.EqualValues(t, 0, f.metrics.payloadsReused.Load(),
		"the touch-loses-to-delete path must NOT count as a reuse")
}

// TestPayloadAdoptionExhaustion_NoCommitWritten pins the exhaustion contract
// of the adoption CAS loop: when every one of the payloadAdoptMaxAttempts
// touch attempts loses its revision condition, Publish must FAIL — and,
// critically, assignment._commit must never be written. A commit referencing
// a payload whose adoption touch never succeeded is exactly the torn state
// the revision-conditioned protocol exists to prevent, so the abort-before-
// commit-CAS ordering is the load-bearing half of the fix.
func TestPayloadAdoptionExhaustion_NoCommitWritten(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newRacePayloadFixture(t, "adopt-exhaustion")

	// Inject: immediately before EVERY adopter touch (Update) reaches the
	// server, advance K's revision out-of-band via the unwrapped KV — same
	// bytes, current revision — so the adopter's touch, conditioned on the
	// revision from its verify-back Get, always loses. Re-arming the
	// fire-once latch inside the callback makes the injection repeat for
	// every retry attempt, modeling a competing writer winning every round.
	f.wrapped.onUpdate = func() {
		entry, gerr := f.realKV.Get(ctx, f.payloadKey)
		require.NoError(t, gerr, "out-of-band revision bump: read current entry")
		_, uerr := f.realKV.Update(ctx, f.payloadKey, entry.Value(), entry.Revision())
		require.NoError(t, uerr, "out-of-band revision bump: advance revision")
		f.wrapped.mu.Lock()
		f.wrapped.fired = false // re-arm: the NEXT touch attempt must lose too
		f.wrapped.mu.Unlock()
	}

	err := f.publishK(ctx)
	require.Error(t, err, "publish must fail once adoption attempts are exhausted")
	require.ErrorContains(t, err, "payload adoption exhausted")

	// The load-bearing assertion: exhaustion aborted the publish BEFORE the
	// commit CAS — no commit may exist referencing the never-adopted K.
	_, gerr := f.realKV.Get(ctx, "assignment._commit")
	require.ErrorIs(t, gerr, jetstream.ErrKeyNotFound,
		"assignment._commit must not be written after adoption exhaustion")

	// And the failed adoption must not have been counted as a successful
	// create or reuse.
	require.EqualValues(t, 0, f.metrics.payloadsCreated.Load())
	require.EqualValues(t, 0, f.metrics.payloadsReused.Load())
}

// TestPayloadRevisionRace_LoserClassification pins the nats.go/JetStream
// behavior both sides of the fix depend on: a revision-conditioned write
// that loses a race — whether it's GC's LastRevision-conditioned Delete or
// the adopter's revision-conditioned Update (touch) — surfaces as
// jetstream.ErrKeyExists (nats.go maps the underlying JetStream
// "wrong last sequence" API code, JSErrCodeStreamWrongLastSequence, to the
// same sentinel Create uses for "key already exists"). This is NOT the same
// classification as deleting a key that is already fully gone
// (jetstream.ErrKeyNotFound), which both commit_gc.go's delete-arm switch
// and the pre-existing "raced away" handling still rely on staying distinct.
//
// This test does not depend on the fix's application code — it is a
// contract pin against the vendored nats.go behavior the fix's error
// classification switches assume.
func TestPayloadRevisionRace_LoserClassification(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "race-loser-classification")

	key := "assignment._payload.deadbeef"
	rev, err := kv.Create(ctx, key, []byte("v1"))
	require.NoError(t, err)

	// A second writer advances the key's revision — models either side
	// (a GC delete or a concurrent adopter's touch) winning first.
	_, err = kv.Update(ctx, key, []byte("v2"), rev)
	require.NoError(t, err)

	// A conditioned Delete against the now-stale revision (models GC's
	// delete arm losing to an adopter's touch) must classify as
	// ErrKeyExists, NOT ErrKeyNotFound — the key is very much still there.
	derr := kv.Delete(ctx, key, jetstream.LastRevision(rev))
	require.Error(t, derr)
	require.ErrorIs(t, derr, jetstream.ErrKeyExists)
	require.NotErrorIs(t, derr, jetstream.ErrKeyNotFound)

	// A conditioned Update against the now-stale revision (models the
	// adopter's touch losing to a GC delete that landed first) must also
	// classify as ErrKeyExists.
	_, uerr := kv.Update(ctx, key, []byte("v3"), rev)
	require.Error(t, uerr)
	require.ErrorIs(t, uerr, jetstream.ErrKeyExists)
	require.NotErrorIs(t, uerr, jetstream.ErrKeyNotFound)

	// Contrast: deleting an ALREADY-fully-deleted key stays classified as
	// ErrKeyNotFound — the pre-existing "raced away" case GC already
	// handled — so the two loser classifications remain distinguishable.
	require.NoError(t, kv.Delete(ctx, key)) // unconditioned: removes latest.
	_, gerr := kv.Get(ctx, key)
	require.ErrorIs(t, gerr, jetstream.ErrKeyNotFound)
}
