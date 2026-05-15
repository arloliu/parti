package parti

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/heartbeat"
	"github.com/arloliu/parti/v2/internal/hooks"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// recordingMetrics captures the Phase 4 commit-path metrics needed by the
// state-machine tests. Embeds NopMetrics so the full MetricsCollector
// interface is satisfied automatically.
type recordingMetrics struct {
	*metrics.NopMetrics

	staleLeaderRejected    atomic.Int64
	commitPayloadMissing   atomic.Int64
	payloadHashMismatch    atomic.Int64
	setDigestMismatch      atomic.Int64
	payloadFetchError      atomic.Int64
	payloadDecompressError atomic.Int64
	payloadDecodeError     atomic.Int64
	assignmentChangeCalls  atomic.Int64
}

func newRecordingMetrics() *recordingMetrics {
	return &recordingMetrics{NopMetrics: metrics.NewNop()}
}

func (r *recordingMetrics) RecordStaleLeaderRejected()    { r.staleLeaderRejected.Add(1) }
func (r *recordingMetrics) RecordCommitPayloadMissing()   { r.commitPayloadMissing.Add(1) }
func (r *recordingMetrics) RecordPayloadHashMismatch()    { r.payloadHashMismatch.Add(1) }
func (r *recordingMetrics) RecordSetDigestMismatch()      { r.setDigestMismatch.Add(1) }
func (r *recordingMetrics) RecordPayloadFetchError()      { r.payloadFetchError.Add(1) }
func (r *recordingMetrics) RecordPayloadDecompressError() { r.payloadDecompressError.Add(1) }
func (r *recordingMetrics) RecordPayloadDecodeError()     { r.payloadDecodeError.Add(1) }
func (r *recordingMetrics) RecordAssignmentChange(_, _ int, _ int64) {
	r.assignmentChangeCalls.Add(1)
}

// recordingHandoff is a barrier-coordinated mock Coordinator. Apply records
// the version in callOrder. When blockFirstApply is true the first Apply
// call blocks on releaseFirst until the test releases it; subsequent Apply
// calls run synchronously so the case (e) drain can complete deterministically.
type recordingHandoff struct {
	mu         sync.Mutex
	callOrder  []int64
	applyCount atomic.Int64

	blockFirstApply atomic.Bool
	releaseFirst    chan struct{}
	firstApplyReady chan struct{}

	errOnce atomic.Pointer[errBox]
}

type errBox struct{ err error }

func newRecordingHandoff() *recordingHandoff {
	return &recordingHandoff{
		releaseFirst:    make(chan struct{}),
		firstApplyReady: make(chan struct{}),
	}
}

func (h *recordingHandoff) Start(_ context.Context) {}

func (h *recordingHandoff) Apply(_ context.Context, _ string, _ /* prev */, next types.Assignment) error {
	// If blockFirstApply is set, the FIRST Apply call signals readiness then
	// waits for the test to release it. Subsequent applies fall through.
	if h.blockFirstApply.CompareAndSwap(true, false) {
		close(h.firstApplyReady)
		<-h.releaseFirst
	}
	h.applyCount.Add(1)
	h.mu.Lock()
	h.callOrder = append(h.callOrder, next.Version)
	h.mu.Unlock()
	if box := h.errOnce.Swap(nil); box != nil {
		return box.err
	}

	return nil
}

// recordingHeartbeat captures SetAppliedAssignment + PublishNow calls so
// tests can assert ack monotonicity.
type recordingHeartbeat struct {
	mu      sync.Mutex
	snaps   []heartbeat.AppliedAssignment
	pubNows atomic.Int64
}

func (r *recordingHeartbeat) Start(_ context.Context) error { return nil }
func (r *recordingHeartbeat) Stop() error                   { return nil }
func (r *recordingHeartbeat) SetAppliedAssignment(snap heartbeat.AppliedAssignment) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.snaps) > 0 {
		// Monotone — mimic Publisher's invariant in the recorder so the
		// tests reflect real publisher behavior.
		if snap.AppliedVersion < r.snaps[len(r.snaps)-1].AppliedVersion {
			return
		}
	}
	r.snaps = append(r.snaps, snap)
}
func (r *recordingHeartbeat) PublishNow(_ context.Context) error {
	r.pubNows.Add(1)
	return nil
}
func (r *recordingHeartbeat) latestSnap() (heartbeat.AppliedAssignment, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.snaps) == 0 {
		return heartbeat.AppliedAssignment{}, false
	}
	return r.snaps[len(r.snaps)-1], true
}

// newTestManager builds a Manager wired with stub dependencies, sufficient
// for handleCommitValue / handleAssignmentEntry tests without the full
// startup sequence. ctx is the manager context (used by applyAssignment).
//
// The 4-tuple return is preferred over a struct here because every caller
// destructures the result inline; bundling into a struct would only force
// boilerplate field access at the call sites.
//
//nolint:revive // function-result-limit: 4 returns keeps test setup terse.
func newTestManager(t *testing.T) (*Manager, *recordingHandoff, *recordingHeartbeat, *recordingMetrics) {
	t.Helper()
	rh := newRecordingHandoff()
	hb := &recordingHeartbeat{}
	rm := newRecordingMetrics()
	nopHooks := hooks.NewNop()
	m := &Manager{
		hooks:              &nopHooks,
		metrics:            rm,
		logger:             logging.NewNop(),
		handoffCoordinator: rh,
		heartbeat:          hb,
	}
	m.workerID.Store("worker-test")
	m.assignment.Store(Assignment{})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	m.ctx = ctx
	m.cancel = cancel

	return m, rh, hb, rm
}

// ============================================================================
// State-machine tests
// ============================================================================

func TestCommitStateMachine_Case_A_VersionAtOrBelowCurrent_NoOp(t *testing.T) {
	t.Parallel()
	m, rh, _, rm := newTestManager(t)
	m.assignment.Store(Assignment{Version: 5, LeaderRevision: 30})
	m.lastSeenLeaderRevision.Store(30)

	commit := &types.AssignmentCommit{Version: 5, LeaderRevision: 40}
	m.handleCommitValue(commit)

	require.Equal(t, Assignment{Version: 5, LeaderRevision: 30}, m.CurrentAssignment())
	require.Equal(t, int64(0), rh.applyCount.Load(), "case (a) must not call Apply")
	require.Equal(t, uint64(40), m.lastSeenLeaderRevision.Load(), "case (a) updates LSR")
	require.Equal(t, int64(0), rm.staleLeaderRejected.Load())
}

func TestCommitStateMachine_Case_B_StaleLeaderRevision_RejectsAndEmitsMetric(t *testing.T) {
	t.Parallel()
	m, rh, _, rm := newTestManager(t)
	m.lastSeenLeaderRevision.Store(100)

	commit := &types.AssignmentCommit{Version: 10, LeaderRevision: 50}
	m.handleCommitValue(commit)

	require.Equal(t, Assignment{}, m.CurrentAssignment())
	require.Equal(t, int64(0), rh.applyCount.Load())
	require.Equal(t, int64(1), rm.staleLeaderRejected.Load())
	require.Equal(t, uint64(100), m.lastSeenLeaderRevision.Load(), "case (b) MUST NOT advance LSR")
}

func TestCommitStateMachine_Case_C_PayloadRefNil_ClassifiesMalformed(t *testing.T) {
	t.Parallel()
	m, rh, _, rm := newTestManager(t)
	wid := m.WorkerID()

	commit := &types.AssignmentCommit{
		Version:        10,
		LeaderRevision: 20,
		Workers:        []string{wid},
		Payloads:       map[string]types.AssignmentPayloadRef{}, // missing
	}
	m.handleCommitValue(commit)

	require.Equal(t, int64(0), rh.applyCount.Load())
	require.Equal(t, int64(1), rm.commitPayloadMissing.Load())
	require.Equal(t, uint64(0), m.lastSeenLeaderRevision.Load(), "case (c) malformed leaves LSR unchanged")
}

func TestCommitStateMachine_Case_D_WorkerNotInBatch_AppliesEmptyAssignment(t *testing.T) {
	t.Parallel()
	m, rh, hb, _ := newTestManager(t)
	// wid is not in commit.Workers — case (d) synthesizes empty.

	commit := &types.AssignmentCommit{
		Version:        10,
		LeaderRevision: 20,
		Workers:        []string{"other-worker"},
		Payloads:       map[string]types.AssignmentPayloadRef{},
	}
	m.handleCommitValue(commit)

	require.Equal(t, int64(1), rh.applyCount.Load(), "case (d) MUST call Apply with empty assignment")
	cur := m.CurrentAssignment()
	require.Equal(t, int64(10), cur.Version)
	require.Equal(t, uint64(20), cur.LeaderRevision)
	require.Empty(t, cur.Partitions)
	require.Equal(t, uint64(20), m.lastSeenLeaderRevision.Load())
	snap, ok := hb.latestSnap()
	require.True(t, ok)
	require.Equal(t, int64(10), snap.AppliedVersion)
	require.Equal(t, uint64(0), snap.AppliedDigest, "case (d) digest is 0 for empty partition set")
}

func TestCommitStateMachine_Case_E_CoalescesInFlightApply_HighestVersionWins(t *testing.T) {
	t.Parallel()
	m, rh, _, _ := newTestManager(t)
	rh.blockFirstApply.Store(true)

	// Build three commits at increasing version. Case (d) shape (no partitions)
	// keeps the test focused on coalescing.
	build := func(v int64, lr uint64) *types.AssignmentCommit {
		return &types.AssignmentCommit{
			Version:        v,
			LeaderRevision: lr,
			Workers:        []string{"other-worker"}, // case (d) shape; wid not in commit
			Payloads:       map[string]types.AssignmentPayloadRef{},
		}
	}

	// Start v=10 — first Apply blocks until released.
	done := make(chan struct{})
	go func() {
		m.handleCommitValue(build(10, 20))
		close(done)
	}()

	// Wait until the first Apply is actually parked inside the barrier.
	select {
	case <-rh.firstApplyReady:
	case <-time.After(time.Second):
		t.Fatal("first Apply did not enter barrier")
	}
	require.True(t, m.pendingApplyInFlight.Load())

	// Send v=11, v=12 while v=10 is blocked. They must coalesce in
	// stashedCommit — only the highest survives.
	m.handleCommitValue(build(11, 21))
	m.handleCommitValue(build(12, 22))

	// Release barrier so v=10 completes; drain runs v=12 (skipping v=11)
	// synchronously without the barrier.
	close(rh.releaseFirst)
	<-done

	require.Equal(t, int64(2), rh.applyCount.Load(), "expected v=10 + v=12 to apply")
	cur := m.CurrentAssignment()
	require.Equal(t, int64(12), cur.Version, "highest-version commit wins after coalesce")
	rh.mu.Lock()
	defer rh.mu.Unlock()
	require.Equal(t, []int64{10, 12}, rh.callOrder)
}

// ============================================================================
// applyAssignment apply pipeline tests (step 8 invariant coverage)
// ============================================================================

func TestApplyAssignment_AckPublishedAfterApply(t *testing.T) {
	t.Parallel()
	m, rh, hb, _ := newTestManager(t)

	next := Assignment{
		Version:             7,
		LeaderRevision:      30,
		SourceRevision:      3,
		SourceRevisionKnown: true,
		Partitions:          []Partition{{Keys: []string{"p1"}}},
	}
	require.NoError(t, m.applyAssignment(next))
	require.Equal(t, next, m.CurrentAssignment())
	require.Equal(t, int64(1), rh.applyCount.Load())

	snap, ok := hb.latestSnap()
	require.True(t, ok)
	require.Equal(t, int64(7), snap.AppliedVersion)
	require.Equal(t, uint64(30), snap.LeaderRevision)
	require.True(t, snap.AppliedSourceRevKnown)
	require.Equal(t, uint64(3), snap.AppliedSourceRevision)
	require.Equal(t, types.PartitionSetDigest([]types.Partition{{Keys: []string{"p1"}}}), snap.AppliedDigest)
	require.Equal(t, int64(1), hb.pubNows.Load())
}

func TestApplyAssignment_AckMonotonic_NoVersionRegression(t *testing.T) {
	t.Parallel()
	m, _, hb, _ := newTestManager(t)

	// Apply v=2 succeeds.
	require.NoError(t, m.applyAssignment(Assignment{Version: 2, LeaderRevision: 10, Partitions: []Partition{{Keys: []string{"p1"}}}}))
	require.Equal(t, int64(2), m.CurrentAssignment().Version)

	// Stale lower-version retry: monotonicity gate must no-op silently.
	require.NoError(t, m.applyAssignment(Assignment{Version: 1, LeaderRevision: 10, Partitions: []Partition{{Keys: []string{"p1"}}}}))
	require.Equal(t, int64(2), m.CurrentAssignment().Version, "monotonicity gate must reject stale retry")

	snap, ok := hb.latestSnap()
	require.True(t, ok)
	require.Equal(t, int64(2), snap.AppliedVersion)
}

// TestCommitStateMachine_Case_C_FullChainSucceeds_AppliesAndAcks covers the
// happy path of §3.6 case (c): a well-formed commit referencing a
// content-addressable payload key that hash- and digest-matches.
func TestCommitStateMachine_Case_C_FullChainSucceeds_AppliesAndAcks(t *testing.T) {
	t.Parallel()
	_, nc := partitest.StartEmbeddedNATS(t)
	akv := partitest.CreateJetStreamKV(t, nc, "case-c-ok-asgn")

	m, rh, hb, _ := newTestManager(t)
	m.assignmentKV = akv
	wid := m.WorkerID()

	parts := []types.Partition{{Keys: []string{"alpha"}}, {Keys: []string{"beta"}}}
	payload := types.AssignmentPayload{
		SchemaVersion: types.AssignmentSchemaVersion,
		Partitions:    parts,
	}
	canonical, jerr := json.Marshal(payload)
	require.NoError(t, jerr)
	hash := sha256.Sum256(canonical)
	hashHex := hex.EncodeToString(hash[:])
	key := "assignment._payload." + hashHex

	var gzBuf bytes.Buffer
	gzw, _ := gzip.NewWriterLevel(&gzBuf, gzip.BestCompression)
	_, err := gzw.Write(canonical)
	require.NoError(t, err)
	require.NoError(t, gzw.Close())
	_, err = akv.Create(t.Context(), key, gzBuf.Bytes())
	require.NoError(t, err)

	ref := types.AssignmentPayloadRef{
		Key:         key,
		PayloadHash: hashHex,
		SetDigest:   types.PartitionSetDigest(parts),
	}
	commit := &types.AssignmentCommit{
		Version:             7,
		LeaderRevision:      31,
		SourceRevision:      9,
		SourceRevisionKnown: true,
		Workers:             []string{wid},
		Payloads:            map[string]types.AssignmentPayloadRef{wid: ref},
	}
	m.handleCommitValue(commit)

	require.Equal(t, int64(1), rh.applyCount.Load())
	cur := m.CurrentAssignment()
	require.Equal(t, int64(7), cur.Version)
	require.True(t, cur.SourceRevisionKnown)
	require.Equal(t, uint64(9), cur.SourceRevision)
	require.Equal(t, parts, cur.Partitions)

	snap, ok := hb.latestSnap()
	require.True(t, ok)
	require.Equal(t, int64(7), snap.AppliedVersion)
	require.Equal(t, types.PartitionSetDigest(parts), snap.AppliedDigest)
}

func TestCommitStateMachine_Case_C_PayloadFetchError_NoApply(t *testing.T) {
	t.Parallel()
	_, nc := partitest.StartEmbeddedNATS(t)
	akv := partitest.CreateJetStreamKV(t, nc, "case-c-fetch-err-asgn")

	m, rh, _, rm := newTestManager(t)
	m.assignmentKV = akv
	wid := m.WorkerID()

	// Ref points at a key that does not exist.
	commit := &types.AssignmentCommit{
		Version:        7,
		LeaderRevision: 31,
		Workers:        []string{wid},
		Payloads: map[string]types.AssignmentPayloadRef{
			wid: {
				Key:         "assignment._payload.nonexistent",
				PayloadHash: "deadbeef",
				SetDigest:   1,
			},
		},
	}
	m.handleCommitValue(commit)
	require.Equal(t, int64(0), rh.applyCount.Load(), "fetch error MUST NOT call Apply")
	require.Equal(t, int64(1), rm.payloadFetchError.Load())
	require.Equal(t, uint64(0), m.lastSeenLeaderRevision.Load(), "case (c) failure leaves LSR unchanged")
}

func TestCommitStateMachine_Case_C_PayloadHashMismatch_RejectsPayload(t *testing.T) {
	t.Parallel()
	_, nc := partitest.StartEmbeddedNATS(t)
	akv := partitest.CreateJetStreamKV(t, nc, "case-c-hash-mismatch-asgn")

	m, rh, _, rm := newTestManager(t)
	m.assignmentKV = akv
	wid := m.WorkerID()

	parts := []types.Partition{{Keys: []string{"p1"}}}
	payload := types.AssignmentPayload{
		SchemaVersion: types.AssignmentSchemaVersion,
		Partitions:    parts,
	}
	canonical, _ := json.Marshal(payload)
	hash := sha256.Sum256(canonical)
	hashHex := hex.EncodeToString(hash[:])
	key := "assignment._payload." + hashHex

	var gzBuf bytes.Buffer
	gzw, _ := gzip.NewWriterLevel(&gzBuf, gzip.BestCompression)
	_, err := gzw.Write(canonical)
	require.NoError(t, err)
	require.NoError(t, gzw.Close())
	_, err = akv.Create(t.Context(), key, gzBuf.Bytes())
	require.NoError(t, err)

	// Ref's PayloadHash deliberately does not match the planted bytes.
	commit := &types.AssignmentCommit{
		Version:        7,
		LeaderRevision: 31,
		Workers:        []string{wid},
		Payloads: map[string]types.AssignmentPayloadRef{
			wid: {Key: key, PayloadHash: "deadbeef-wrong", SetDigest: types.PartitionSetDigest(parts)},
		},
	}
	m.handleCommitValue(commit)
	require.Equal(t, int64(0), rh.applyCount.Load())
	require.Equal(t, int64(1), rm.payloadHashMismatch.Load())
}

func TestCommitStateMachine_Case_C_SetDigestMismatch_RejectsPayload(t *testing.T) {
	t.Parallel()
	_, nc := partitest.StartEmbeddedNATS(t)
	akv := partitest.CreateJetStreamKV(t, nc, "case-c-set-digest-mismatch-asgn")

	m, rh, _, rm := newTestManager(t)
	m.assignmentKV = akv
	wid := m.WorkerID()

	parts := []types.Partition{{Keys: []string{"alpha"}}}
	payload := types.AssignmentPayload{
		SchemaVersion: types.AssignmentSchemaVersion,
		Partitions:    parts,
	}
	canonical, _ := json.Marshal(payload)
	hash := sha256.Sum256(canonical)
	hashHex := hex.EncodeToString(hash[:])
	key := "assignment._payload." + hashHex

	var gzBuf bytes.Buffer
	gzw, _ := gzip.NewWriterLevel(&gzBuf, gzip.BestCompression)
	_, err := gzw.Write(canonical)
	require.NoError(t, err)
	require.NoError(t, gzw.Close())
	_, err = akv.Create(t.Context(), key, gzBuf.Bytes())
	require.NoError(t, err)

	// The PayloadHash is correct (so the bytes verify) but the SetDigest
	// in the ref does NOT match the decoded payload's partitions. This
	// is the internal-consistency failure surfaced by the
	// ErrCommitPayloadDigestMismatch sentinel.
	commit := &types.AssignmentCommit{
		Version:        7,
		LeaderRevision: 31,
		Workers:        []string{wid},
		Payloads: map[string]types.AssignmentPayloadRef{
			wid: {Key: key, PayloadHash: hashHex, SetDigest: 0xDEADBEEF},
		},
	}
	m.handleCommitValue(commit)
	require.Equal(t, int64(0), rh.applyCount.Load(), "set-digest mismatch must NOT call Apply")
	require.Equal(t, int64(1), rm.setDigestMismatch.Load())
	require.Equal(t, uint64(0), m.lastSeenLeaderRevision.Load(), "case (c) failure leaves LSR unchanged")
}

// TestCommitStateMachine_DeleteEventIgnored verifies that a watcher delete
// event passes through handleCommitEntry as a no-op (no panic, no apply).
func TestCommitStateMachine_DeleteEventIgnored(t *testing.T) {
	t.Parallel()
	m, rh, _, _ := newTestManager(t)
	m.handleCommitEntry(deletedEntry{})
	require.Equal(t, int64(0), rh.applyCount.Load())
}

// deletedEntry is a minimal jetstream.KeyValueEntry stub that reports
// itself as a KeyValueDelete operation. Implements the methods the manager
// calls (Operation, Value, Key).
type deletedEntry struct{}

func (deletedEntry) Bucket() string                  { return "" }
func (deletedEntry) Key() string                     { return "assignment._commit" }
func (deletedEntry) Value() []byte                   { return nil }
func (deletedEntry) Revision() uint64                { return 0 }
func (deletedEntry) Created() time.Time              { return time.Time{} }
func (deletedEntry) Delta() uint64                   { return 0 }
func (deletedEntry) Operation() jetstream.KeyValueOp { return jetstream.KeyValueDelete }

// TestCommitStateMachine_ApplyError_LSRUnchanged verifies the P0 LSR
// ordering invariant: when applyAssignment fails on the commit path, the
// stale-leader fence (lastSeenLeaderRevision) MUST NOT accept the leader
// revision the failed commit carried. Otherwise a subsequent legitimate
// alias from a fresher leader could be rejected as "stale".
func TestCommitStateMachine_ApplyError_LSRUnchanged(t *testing.T) {
	t.Parallel()
	_, nc := partitest.StartEmbeddedNATS(t)
	akv := partitest.CreateJetStreamKV(t, nc, "lsr-apply-err-asgn")

	m, rh, _, _ := newTestManager(t)
	m.assignmentKV = akv
	wid := m.WorkerID()

	// Plant a fully valid payload so the case-(c) chain succeeds up to
	// applyAssignment, then inject a one-shot apply error.
	parts := []types.Partition{{Keys: []string{"alpha"}}}
	payload := types.AssignmentPayload{
		SchemaVersion: types.AssignmentSchemaVersion,
		Partitions:    parts,
	}
	canonical, jerr := json.Marshal(payload)
	require.NoError(t, jerr)
	hash := sha256.Sum256(canonical)
	hashHex := hex.EncodeToString(hash[:])
	key := "assignment._payload." + hashHex

	var gzBuf bytes.Buffer
	gzw, _ := gzip.NewWriterLevel(&gzBuf, gzip.BestCompression)
	_, err := gzw.Write(canonical)
	require.NoError(t, err)
	require.NoError(t, gzw.Close())
	_, err = akv.Create(t.Context(), key, gzBuf.Bytes())
	require.NoError(t, err)

	rh.errOnce.Store(&errBox{err: errors.New("synthetic apply failure")})

	commit := &types.AssignmentCommit{
		Version:        7,
		LeaderRevision: 100,
		Workers:        []string{wid},
		Payloads: map[string]types.AssignmentPayloadRef{
			wid: {Key: key, PayloadHash: hashHex, SetDigest: types.PartitionSetDigest(parts)},
		},
	}
	m.handleCommitValue(commit)

	require.Equal(t, int64(1), rh.applyCount.Load(), "Apply was attempted exactly once")
	require.Equal(t, uint64(0), m.lastSeenLeaderRevision.Load(),
		"LSR MUST remain 0 when Apply failed — stale-leader fence must not advance")
	require.Equal(t, Assignment{}, m.CurrentAssignment(),
		"snapshot MUST remain empty when Apply failed (Store skipped)")
}

// TestHandleAssignmentEntry_LegacyAliasApplyError_LSRUnchanged verifies the
// legacy-alias path also leaves LSR unchanged on apply failure.
func TestHandleAssignmentEntry_LegacyAliasApplyError_LSRUnchanged(t *testing.T) {
	t.Parallel()
	m, rh, _, _ := newTestManager(t)

	rh.errOnce.Store(&errBox{err: errors.New("synthetic apply failure")})

	alias := Assignment{
		Version:        9,
		LeaderRevision: 200,
		Partitions:     []Partition{{Keys: []string{"alpha"}}},
	}
	encoded, err := json.Marshal(alias)
	require.NoError(t, err)
	m.handleAssignmentEntry(m.WorkerID(), aliasEntry{value: encoded})

	require.Equal(t, int64(1), rh.applyCount.Load(), "Apply was attempted once")
	require.Equal(t, uint64(0), m.lastSeenLeaderRevision.Load(),
		"LSR MUST remain 0 when alias Apply failed")
	require.Equal(t, Assignment{}, m.CurrentAssignment(),
		"snapshot MUST remain empty when alias Apply failed")
}

// aliasEntry is a minimal jetstream.KeyValueEntry stub for the legacy
// alias path (Put operation, value = encoded assignment).
type aliasEntry struct {
	value []byte
}

func (aliasEntry) Bucket() string                  { return "" }
func (aliasEntry) Key() string                     { return "assignment.worker-test" }
func (e aliasEntry) Value() []byte                 { return e.value }
func (aliasEntry) Revision() uint64                { return 1 }
func (aliasEntry) Created() time.Time              { return time.Time{} }
func (aliasEntry) Delta() uint64                   { return 0 }
func (aliasEntry) Operation() jetstream.KeyValueOp { return jetstream.KeyValuePut }

// TestCommitStateMachine_Case_C_PayloadDecompressError_NoApply verifies the
// §3.6 case (c) state-table requirement that gzip-decompression failure
// drops the transition (no Apply, no LSR advance, no snapshot mutation)
// and records RecordPayloadDecompressError. The strict policy means the
// worker MUST NOT silently accept uncompressed payloads.
func TestCommitStateMachine_Case_C_PayloadDecompressError_NoApply(t *testing.T) {
	t.Parallel()
	_, nc := partitest.StartEmbeddedNATS(t)
	akv := partitest.CreateJetStreamKV(t, nc, "case-c-decompress-err-asgn")

	m, rh, _, rm := newTestManager(t)
	m.assignmentKV = akv
	wid := m.WorkerID()

	// Write the canonical JSON UNCOMPRESSED. PayloadHash matches the raw
	// bytes, so the pre-strict implementation would have happily accepted
	// this payload. With strict gzip, decompression fails first and the
	// drop transition fires.
	parts := []types.Partition{{Keys: []string{"alpha"}}}
	payload := types.AssignmentPayload{
		SchemaVersion: types.AssignmentSchemaVersion,
		Partitions:    parts,
	}
	canonical, jerr := json.Marshal(payload)
	require.NoError(t, jerr)
	hash := sha256.Sum256(canonical)
	hashHex := hex.EncodeToString(hash[:])
	key := "assignment._payload." + hashHex
	_, err := akv.Create(t.Context(), key, canonical) // intentionally not gzipped
	require.NoError(t, err)

	commit := &types.AssignmentCommit{
		Version:        7,
		LeaderRevision: 31,
		Workers:        []string{wid},
		Payloads: map[string]types.AssignmentPayloadRef{
			wid: {Key: key, PayloadHash: hashHex, SetDigest: types.PartitionSetDigest(parts)},
		},
	}
	m.handleCommitValue(commit)

	require.Equal(t, int64(0), rh.applyCount.Load(),
		"decompress failure MUST drop transition — no Apply")
	require.Equal(t, int64(1), rm.payloadDecompressError.Load(),
		"RecordPayloadDecompressError MUST fire on gzip decode failure")
	require.Equal(t, uint64(0), m.lastSeenLeaderRevision.Load(),
		"case (c) drop leaves LSR unchanged")
	require.Equal(t, Assignment{}, m.CurrentAssignment(),
		"case (c) drop leaves snapshot unchanged")
}

// TestApplyRetry_SuccessAdvancesLSR closes the v2-review P0: when a commit
// fails its first Apply, the scheduled retry succeeds, and LSR MUST be
// advanced by the retry's success — otherwise a subsequent lower-LR
// commit from a stale leader can slip past the stale-leader fence
// (case (b) compares against LSR=0).
//
// Sequence:
//  1. Plant a valid commit payload for V=7/LR=100.
//  2. Arm a one-shot apply failure.
//  3. handleCommitValue(V=7/LR=100) — first Apply fails, scheduleApplyRetry
//     stages the retry goroutine.
//  4. Wait for the retry to succeed (CurrentAssignment().Version == 7).
//  5. Assert lastSeenLeaderRevision == 100.
//  6. handleCommitValue(V=8/LR=50) — stale split-brain commit. Case (b)
//     MUST reject (RecordStaleLeaderRejected fires, CurrentAssignment
//     stays at V=7).
//
// Without the fix, step 5 would observe LSR == 0 and step 6 would let the
// stale commit through.
func TestApplyRetry_SuccessAdvancesLSR(t *testing.T) {
	t.Parallel()
	_, nc := partitest.StartEmbeddedNATS(t)
	akv := partitest.CreateJetStreamKV(t, nc, "lsr-retry-success-asgn")

	m, rh, _, rm := newTestManager(t)
	m.assignmentKV = akv
	wid := m.WorkerID()

	// Plant a fully valid gzipped payload so case (c) verification passes.
	parts := []types.Partition{{Keys: []string{"alpha"}}}
	payload := types.AssignmentPayload{
		SchemaVersion: types.AssignmentSchemaVersion,
		Partitions:    parts,
	}
	canonical, jerr := json.Marshal(payload)
	require.NoError(t, jerr)
	hash := sha256.Sum256(canonical)
	hashHex := hex.EncodeToString(hash[:])
	key := "assignment._payload." + hashHex

	var gzBuf bytes.Buffer
	gzw, _ := gzip.NewWriterLevel(&gzBuf, gzip.BestCompression)
	_, err := gzw.Write(canonical)
	require.NoError(t, err)
	require.NoError(t, gzw.Close())
	_, err = akv.Create(t.Context(), key, gzBuf.Bytes())
	require.NoError(t, err)

	// Arm a one-shot apply failure. errOnce is consumed by Swap(nil) on
	// the first call, so the retry will succeed.
	rh.errOnce.Store(&errBox{err: errors.New("synthetic apply failure")})

	v7 := &types.AssignmentCommit{
		Version:        7,
		LeaderRevision: 100,
		Workers:        []string{wid},
		Payloads: map[string]types.AssignmentPayloadRef{
			wid: {Key: key, PayloadHash: hashHex, SetDigest: types.PartitionSetDigest(parts)},
		},
	}
	m.handleCommitValue(v7)

	// First Apply failed: snapshot stays empty, LSR stays 0,
	// scheduleApplyRetry stashed the retry target.
	require.Equal(t, int64(1), rh.applyCount.Load(),
		"first Apply attempted exactly once before failure")
	require.Equal(t, uint64(0), m.lastSeenLeaderRevision.Load(),
		"LSR MUST remain 0 immediately after first Apply failure")
	require.Equal(t, Assignment{}, m.CurrentAssignment())

	// Retry goroutine waits ~1s (initial backoff) before re-attempting.
	// Allow up to 5s for scheduling jitter and CI variance.
	require.Eventually(t, func() bool {
		return m.CurrentAssignment().Version == 7
	}, 5*time.Second, 25*time.Millisecond,
		"scheduleApplyRetry must eventually succeed and store V=7")

	// CORE ASSERTION: LSR was advanced by the retry's success path,
	// even though the original commit handler did not advance it.
	require.Equal(t, uint64(100), m.lastSeenLeaderRevision.Load(),
		"LSR MUST be advanced to commit.LeaderRevision after retry success")

	// Now drop a stale-leader split-brain commit. Case (b) MUST reject:
	// LR=50 < LSR=100.
	staleRejectedBefore := rm.staleLeaderRejected.Load()
	v8Stale := &types.AssignmentCommit{
		Version:        8,
		LeaderRevision: 50,
		Workers:        []string{"someone-else"},
		Payloads:       map[string]types.AssignmentPayloadRef{},
	}
	m.handleCommitValue(v8Stale)

	require.Equal(t, staleRejectedBefore+1, rm.staleLeaderRejected.Load(),
		"stale split-brain commit MUST trip RecordStaleLeaderRejected")
	require.Equal(t, int64(7), m.CurrentAssignment().Version,
		"stale commit MUST NOT regress the snapshot — V=7 stays")
	require.Equal(t, uint64(100), m.lastSeenLeaderRevision.Load(),
		"case (b) MUST NOT advance LSR")
}

// TestApplyAssignment_LSRAdvancesBeforeSnapshotStore verifies the v3-review
// P0 invariant: applyAssignmentWithPrev MUST advance lastSeenLeaderRevision
// BEFORE storing the new snapshot via m.assignment.Store. Otherwise a
// concurrent handleCommitValueOnce reader could observe (new snapshot,
// old LSR) and accept a stale higher-Version commit that should have been
// rejected by case (b)'s stale-leader fence.
//
// Strategy: install testHookAfterApplyStore — fires synchronously
// immediately after m.assignment.Store returns. Inside the hook, observe
// lastSeenLeaderRevision. If LSR is still below newAssignment.LeaderRevision
// at this point, the ordering invariant is violated. Using a hook makes the
// check deterministic (no goroutine timing); the prior order (Store →
// LSR-advance) would reliably trip this assertion.
//
// A second concurrent-reader assertion runs a tight loop alongside the
// apply, recording (Version, LSR) tuples, and asserts no observed tuple
// matches the dangerous (Version=new, LSR<new.LeaderRevision) shape.
func TestApplyAssignment_LSRAdvancesBeforeSnapshotStore(t *testing.T) {
	t.Parallel()

	t.Run("hook fires after Store and sees LSR already advanced", func(t *testing.T) {
		t.Parallel()
		m, rh, _, _ := newTestManager(t)

		var (
			hookFired       atomic.Bool
			lsrAtHook       uint64
			snapVerAtHook   int64
			snapPartsAtHook int
		)
		newAsgn := Assignment{
			Version:        7,
			LeaderRevision: 100,
			Partitions:     []Partition{{Keys: []string{"alpha"}}},
		}
		m.testHookAfterApplyStore = func(stored Assignment) {
			hookFired.Store(true)
			// Observe BOTH so the test can attribute a failure to either
			// "Store didn't happen yet" or "LSR not advanced yet".
			lsrAtHook = m.lastSeenLeaderRevision.Load()
			cur := m.CurrentAssignment()
			snapVerAtHook = cur.Version
			snapPartsAtHook = len(cur.Partitions)
			require.Equal(t, newAsgn.Version, stored.Version,
				"hook receives the just-stored assignment")
		}

		require.NoError(t, m.applyAssignment(newAsgn))

		require.True(t, hookFired.Load(), "testHookAfterApplyStore MUST have fired")
		require.Equal(t, int64(1), rh.applyCount.Load(), "Apply called once")

		// Core invariant: LSR is at least newAssignment.LeaderRevision at
		// the moment Store returns. The OLD order (Store then LSR-advance)
		// would observe lsrAtHook == 0.
		require.GreaterOrEqual(t, lsrAtHook, newAsgn.LeaderRevision,
			"LSR MUST be advanced BEFORE m.assignment.Store completes (v3 P0 ordering)")
		// Sanity: snapshot is also visible inside the hook (confirms the
		// hook truly fires after Store, not somehow before).
		require.Equal(t, newAsgn.Version, snapVerAtHook,
			"snapshot Version visible inside the post-Store hook")
		require.Equal(t, 1, snapPartsAtHook,
			"snapshot Partitions visible inside the post-Store hook")
	})

	t.Run("concurrent reader never observes (new snapshot, old LSR)", func(t *testing.T) {
		t.Parallel()
		m, _, _, _ := newTestManager(t)

		newAsgn := Assignment{
			Version:        9,
			LeaderRevision: 200,
			Partitions:     []Partition{{Keys: []string{"beta"}}},
		}

		// Launch a tight-loop reader BEFORE applyAssignment. The reader
		// records (Version, LSR) tuples as fast as it can; with the
		// LSR-before-Store fix it should never observe Version == newAsgn.Version
		// with LSR < newAsgn.LeaderRevision. The OLD order had a tiny
		// Store→LSR window that this loop could occasionally catch.
		var (
			readerStop atomic.Bool
			readerWG   sync.WaitGroup
			badObs     atomic.Int64
			ready      = make(chan struct{})
		)
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			close(ready)
			for !readerStop.Load() {
				lsr := m.lastSeenLeaderRevision.Load()
				cur := m.CurrentAssignment()
				if cur.Version == newAsgn.Version && lsr < newAsgn.LeaderRevision {
					badObs.Add(1)
				}
			}
		}()
		<-ready

		// Repeated applies of monotonically increasing versions widen the
		// window the reader has to catch a violation; each apply pair
		// exercises the Store/LSR pair once.
		for i := 0; i < 50; i++ {
			require.NoError(t, m.applyAssignment(newAsgn))
			// Reset for the next iteration: bump LSR/version slightly
			// would require new assignments. Simpler — apply the SAME
			// (Version, LR) again; the monotonicity gate is < not <=,
			// and the LSR update is monotone, so re-apply is a no-op
			// but still exercises the Store + LSR-advance lines.
		}
		// Brief settling time so the reader can sample post-final-Store
		// states. Race detector also benefits.
		time.Sleep(2 * time.Millisecond)
		readerStop.Store(true)
		readerWG.Wait()

		require.Equal(t, int64(0), badObs.Load(),
			"concurrent reader observed (Version=new, LSR<new.LR) — Store happened before LSR advanced")
	})
}
