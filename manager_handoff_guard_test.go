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

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// recordingRemovalMetrics embeds NopMetrics and counts RecordHandoffRemovalPending.
type recordingRemovalMetrics struct {
	*metrics.NopMetrics
	mu      sync.Mutex
	pending []string
}

func newRecordingRemovalMetrics() *recordingRemovalMetrics {
	return &recordingRemovalMetrics{NopMetrics: metrics.NewNop()}
}

func (r *recordingRemovalMetrics) RecordHandoffRemovalPending(workerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending = append(r.pending, workerID)
}

func (r *recordingRemovalMetrics) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

// fakeClaimStore is an in-memory ClaimStore for guard tests. Get returns a
// seeded claim or, when getErr is set, that error.
type fakeClaimStore struct {
	claims map[string]struct {
		claim handoff.Claim
		rev   uint64
	}
	getErr error
}

func newFakeClaimStore() *fakeClaimStore {
	return &fakeClaimStore{claims: map[string]struct {
		claim handoff.Claim
		rev   uint64
	}{}}
}

func (f *fakeClaimStore) seed(pid string, claim handoff.Claim, rev uint64) {
	f.claims[pid] = struct {
		claim handoff.Claim
		rev   uint64
	}{claim, rev}
}

func (f *fakeClaimStore) Get(_ context.Context, partitionID string) (handoff.Claim, uint64, error) {
	if f.getErr != nil {
		return handoff.Claim{}, 0, f.getErr
	}
	if c, ok := f.claims[partitionID]; ok {
		return c.claim, c.rev, nil
	}
	// Missing claim: rev==0.
	return handoff.Claim{}, 0, nil
}

func (f *fakeClaimStore) PutIfEpoch(_ context.Context, _ string, _ int64, _ handoff.Claim) (uint64, error) {
	return 0, nil
}

func (f *fakeClaimStore) ListKeys(_ context.Context) ([]string, error) {
	return nil, nil
}

func (f *fakeClaimStore) Delete(context.Context, string, uint64) error { return nil }

func part(key string) types.Partition {
	return types.Partition{Keys: []string{key}}
}

// newGuardManager builds a Manager wired for guardHandoffRemoval testing: a
// recording metrics double, a nop logger, the supplied claim store, and a
// commit-batch test hook that returns batchKeys as the current commit's
// partition set for version commitVersion.
func newGuardManager(store handoff.ClaimStore, commitVersion int64, batchKeys ...string) (*Manager, *recordingRemovalMetrics) {
	rm := newRecordingRemovalMetrics()
	m := &Manager{
		logger:  logging.NewNop(),
		metrics: rm,
	}
	m.handoffStore = store

	batch := make(map[string]struct{}, len(batchKeys))
	for _, k := range batchKeys {
		batch[k] = struct{}{}
	}
	// Commit read returns a commit at commitVersion (one synthetic payload ref
	// per key is unnecessary because the batch hook supplies the set directly).
	m.testHookCommitRead = func(_ context.Context) (*types.AssignmentCommit, uint64, error) {
		return &types.AssignmentCommit{Version: commitVersion}, 1, nil
	}
	m.testHookCommitBatch = func(_ context.Context, _ *types.AssignmentCommit) (map[string]struct{}, error) {
		return batch, nil
	}

	return m, rm
}

func TestGuardHandoffRemoval(t *testing.T) {
	const (
		me    = "worker-self"
		other = "worker-other"
		ver   = int64(7)
	)
	prev := Assignment{Version: ver, Partitions: []types.Partition{part("p1")}}
	next := Assignment{Version: ver, Partitions: nil} // p1 removed

	allowCases := []struct {
		name  string
		claim handoff.Claim
		rev   uint64
	}{
		{"different-owner commit", handoff.Claim{Owner: other, State: handoff.ClaimStateCommit}, 5},
		{"different-owner stable", handoff.Claim{Owner: other, State: handoff.ClaimStateStable}, 5},
	}
	for _, tc := range allowCases {
		t.Run("ALLOW/"+tc.name, func(t *testing.T) {
			store := newFakeClaimStore()
			store.seed("p1", tc.claim, tc.rev)
			m, rm := newGuardManager(store, ver, "p1")

			err := m.guardHandoffRemoval(context.Background(), me, prev, next)
			require.NoError(t, err)
			require.Equal(t, 0, rm.count())
		})
	}

	blockCases := []struct {
		name  string
		seed  bool
		claim handoff.Claim
		rev   uint64
	}{
		{"self-owner commit", true, handoff.Claim{Owner: me, State: handoff.ClaimStateCommit}, 5},
		{"self-owner stable", true, handoff.Claim{Owner: me, State: handoff.ClaimStateStable}, 5},
		{"self-owner prepare", true, handoff.Claim{Owner: me, State: handoff.ClaimStatePrepare}, 5},
		{"different-owner prepare", true, handoff.Claim{Owner: other, State: handoff.ClaimStatePrepare}, 5},
		{"different-owner abort", true, handoff.Claim{Owner: other, State: handoff.ClaimStateAbort}, 5},
		{"different-owner unknown", true, handoff.Claim{Owner: other, State: handoff.ClaimStateUnknown}, 5},
		{"empty owner", true, handoff.Claim{Owner: "", State: handoff.ClaimStateCommit}, 5},
		{"missing claim rev0", false, handoff.Claim{}, 0},
	}
	for _, tc := range blockCases {
		t.Run("BLOCK/"+tc.name, func(t *testing.T) {
			store := newFakeClaimStore()
			if tc.seed {
				store.seed("p1", tc.claim, tc.rev)
			}
			m, rm := newGuardManager(store, ver, "p1")

			err := m.guardHandoffRemoval(context.Background(), me, prev, next)
			require.ErrorIs(t, err, handoff.ErrRemovalPending)
			require.Equal(t, 1, rm.count())
		})
	}

	t.Run("ALLOW/not-a-transfer (source deletion)", func(t *testing.T) {
		// p1 removed but absent from current commit batch -> not a transfer.
		store := newFakeClaimStore()
		// The current commit batch is {p-other}; the removed p1 is NOT a
		// transfer (it left the batch entirely -> source deletion). A claim on
		// the in-batch partition exists but is never consulted for p1.
		store.seed("p-other", handoff.Claim{Owner: me, State: handoff.ClaimStatePrepare}, 5)
		m, rm := newGuardManager(store, ver, "p-other")

		err := m.guardHandoffRemoval(context.Background(), me, prev, next)
		require.NoError(t, err)
		require.Equal(t, 0, rm.count())
	})

	t.Run("ALLOW/no removed partitions", func(t *testing.T) {
		store := newFakeClaimStore()
		m, rm := newGuardManager(store, ver, "p1")
		samePrev := Assignment{Version: ver, Partitions: []types.Partition{part("p1")}}
		err := m.guardHandoffRemoval(context.Background(), me, samePrev, samePrev)
		require.NoError(t, err)
		require.Equal(t, 0, rm.count())
	})

	t.Run("RETRYABLE/claim read error", func(t *testing.T) {
		store := newFakeClaimStore()
		store.getErr = errors.New("kv unavailable")
		m, rm := newGuardManager(store, ver, "p1")

		err := m.guardHandoffRemoval(context.Background(), me, prev, next)
		require.Error(t, err)
		require.NotErrorIs(t, err, handoff.ErrRemovalPending)
		require.Contains(t, err.Error(), "claim read")
		// Read error is not a "pending" deferral.
		require.Equal(t, 0, rm.count())
	})

	t.Run("RETRYABLE/commit read error", func(t *testing.T) {
		store := newFakeClaimStore()
		m, _ := newGuardManager(store, ver, "p1")
		m.testHookCommitRead = func(_ context.Context) (*types.AssignmentCommit, uint64, error) {
			return nil, 0, errors.New("commit kv unavailable")
		}

		err := m.guardHandoffRemoval(context.Background(), me, prev, next)
		require.Error(t, err)
		require.NotErrorIs(t, err, handoff.ErrRemovalPending)
		require.Contains(t, err.Error(), "commit read")
	})

	t.Run("ALLOW/commit version mismatch -> empty batch", func(t *testing.T) {
		store := newFakeClaimStore()
		// Seed a self-owned prepare that WOULD block if it were a transfer.
		store.seed("p1", handoff.Claim{Owner: me, State: handoff.ClaimStatePrepare}, 5)
		m, rm := newGuardManager(store, ver+1, "p1") // commit at a different version
		err := m.guardHandoffRemoval(context.Background(), me, prev, next)
		require.NoError(t, err)
		require.Equal(t, 0, rm.count())
	})

	t.Run("ALLOW/nil store", func(t *testing.T) {
		m, rm := newGuardManager(nil, ver, "p1")
		m.handoffStore = nil
		err := m.guardHandoffRemoval(context.Background(), me, prev, next)
		require.NoError(t, err)
		require.Equal(t, 0, rm.count())
	})

	t.Run("ALLOW/no commit present", func(t *testing.T) {
		// readCommitEntry maps a missing _commit (ErrKeyNotFound) to (nil,0,nil);
		// currentCommitPartitionSet then returns an empty batch -> no block. This
		// is the alias-only rebalance shape where no commit exists.
		store := newFakeClaimStore()
		store.seed("p1", handoff.Claim{Owner: me, State: handoff.ClaimStatePrepare}, 5)
		m, rm := newGuardManager(store, ver, "p1")
		m.testHookCommitRead = func(_ context.Context) (*types.AssignmentCommit, uint64, error) {
			return nil, 0, nil
		}
		err := m.guardHandoffRemoval(context.Background(), me, prev, next)
		require.NoError(t, err)
		require.Equal(t, 0, rm.count())
	})
}

// TestCurrentCommitPartitionSet_CacheFetchOnce asserts the per-payload fan-out
// runs once per (version, _commit rev), not once per call.
func TestCurrentCommitPartitionSet_CacheFetchOnce(t *testing.T) {
	m := &Manager{logger: logging.NewNop(), metrics: metrics.NewNop()}

	var commitRev uint64 = 1
	m.testHookCommitRead = func(_ context.Context) (*types.AssignmentCommit, uint64, error) {
		return &types.AssignmentCommit{Version: 9}, atomic.LoadUint64(&commitRev), nil
	}
	var fanouts int32
	m.testHookCommitBatch = func(_ context.Context, _ *types.AssignmentCommit) (map[string]struct{}, error) {
		atomic.AddInt32(&fanouts, 1)
		return map[string]struct{}{"p1": {}}, nil
	}

	for range 5 {
		batch, err := m.currentCommitPartitionSet(context.Background(), 9)
		require.NoError(t, err)
		require.Len(t, batch, 1)
	}
	require.Equal(t, int32(1), atomic.LoadInt32(&fanouts), "fan-out should run once for a stable (version, rev)")

	// A new _commit revision for the same version invalidates the cache.
	atomic.StoreUint64(&commitRev, 2)
	_, err := m.currentCommitPartitionSet(context.Background(), 9)
	require.NoError(t, err)
	require.Equal(t, int32(2), atomic.LoadInt32(&fanouts), "fan-out should re-run when _commit revision changes")

	// Version mismatch returns an empty batch and does NOT fan out.
	batch, err := m.currentCommitPartitionSet(context.Background(), 10)
	require.NoError(t, err)
	require.Empty(t, batch)
	require.Equal(t, int32(2), atomic.LoadInt32(&fanouts), "version mismatch must not fan out")
}

// TestReadCommitEntry_RealKV exercises the REAL readCommitEntry / currentCommitPartitionSet
// code paths (hooks nil) against a live in-process JetStream bucket. The test-hook
// seam in the guard tests bypasses these paths entirely, so the SAFETY-critical
// error mappings — especially ErrKeyNotFound → (nil,0,nil) (a misclassification
// here = premature removal) — had zero coverage until this test.
func TestReadCommitEntry_RealKV(t *testing.T) {
	t.Parallel()

	// Each sub-test gets its own NATS + KV so they can run in parallel without
	// conflicting on the shared "assignment._commit" key.
	newKV := func(t *testing.T, name string) (jetstream.KeyValue, context.Context) {
		t.Helper()
		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, name)
		return kv, t.Context()
	}
	newM := func(kv jetstream.KeyValue) *Manager {
		m := &Manager{logger: logging.NewNop(), metrics: metrics.NewNop()}
		m.assignmentKV = kv
		// Leave hooks nil so the REAL path executes.
		return m
	}

	t.Run("missing key maps to nil commit", func(t *testing.T) {
		t.Parallel()
		// ErrKeyNotFound from a live bucket with no entry must map to (nil,0,nil),
		// not a wrapped error. A wrapped error would cause currentCommitPartitionSet
		// to propagate an error and guardHandoffRemoval to fail closed — but with a
		// generic "commit read" error rather than the expected empty-batch allow.
		// Misclassifying ErrKeyNotFound as an error is the SAFETY bug: the guard
		// would block ALL removals until the key exists.
		kv, ctx := newKV(t, "rce-missing")
		m := newM(kv)
		commit, rev, err := m.readCommitEntry(ctx)
		require.NoError(t, err, "ErrKeyNotFound must map to (nil,0,nil), not an error")
		require.Nil(t, commit)
		require.Zero(t, rev)
	})

	t.Run("missing key -> currentCommitPartitionSet returns empty batch", func(t *testing.T) {
		t.Parallel()
		// Exercises the nil-commit short-circuit in currentCommitPartitionSet.
		kv, ctx := newKV(t, "rce-ccps-empty")
		m := newM(kv)
		batch, err := m.currentCommitPartitionSet(ctx, 5)
		require.NoError(t, err)
		require.Empty(t, batch)
	})

	t.Run("version mismatch returns empty batch", func(t *testing.T) {
		t.Parallel()
		// Seed a commit at version 3; query at version 4 must return empty.
		kv, ctx := newKV(t, "rce-ver-mismatch")
		m := newM(kv)
		c := types.AssignmentCommit{Version: 3}
		raw, marshalErr := json.Marshal(c)
		require.NoError(t, marshalErr)
		_, putErr := kv.Put(ctx, "assignment."+commitKeyName, raw)
		require.NoError(t, putErr)

		batch, err := m.currentCommitPartitionSet(ctx, 4) // wrong version
		require.NoError(t, err)
		require.Empty(t, batch, "version mismatch must yield empty batch (no block)")
	})

	t.Run("corrupt entry returns wrapped error", func(t *testing.T) {
		t.Parallel()
		// An undecodable _commit entry must surface as an error (fail-closed).
		kv, ctx := newKV(t, "rce-corrupt")
		m := newM(kv)
		_, putErr := kv.Put(ctx, "assignment."+commitKeyName, []byte("not-json{{"))
		require.NoError(t, putErr)

		commit, rev, err := m.readCommitEntry(ctx)
		require.Error(t, err, "corrupt JSON must return a non-nil error")
		require.Contains(t, err.Error(), "unmarshal commit entry")
		require.Nil(t, commit)
		require.Zero(t, rev)

		// currentCommitPartitionSet must propagate the unmarshal error.
		_, err2 := m.currentCommitPartitionSet(ctx, 1)
		require.Error(t, err2)
	})

	t.Run("valid commit, no payload refs, version match -> empty batch, no error", func(t *testing.T) {
		t.Parallel()
		// A valid commit at the queried version with no payload refs yields an
		// empty batch (nothing to block), not an error.
		kv, ctx := newKV(t, "rce-valid")
		m := newM(kv)
		c := types.AssignmentCommit{Version: 7}
		raw, marshalErr := json.Marshal(c)
		require.NoError(t, marshalErr)
		_, putErr := kv.Put(ctx, "assignment."+commitKeyName, raw)
		require.NoError(t, putErr)

		// Verify readCommitEntry round-trips the version correctly.
		got, rev, err := m.readCommitEntry(ctx)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.Equal(t, int64(7), got.Version)
		require.NotZero(t, rev)

		// currentCommitPartitionSet: version matches, no payload refs -> empty batch, no error.
		batch, err2 := m.currentCommitPartitionSet(ctx, 7)
		require.NoError(t, err2)
		require.Empty(t, batch, "commit with no payload refs yields empty batch, not an error")
	})

	t.Run("valid commit with real payload -> SubjectKey lands in batch", func(t *testing.T) {
		t.Parallel()
		// Case (b): exercises the real commitPartitionBatch fan-out loop.
		// Seed a real gzip-compressed, sha256-hashed payload blob in the KV, plant
		// a matching AssignmentCommit._commit entry, and confirm the partition's
		// SubjectKey() appears in the returned batch. This is the safety-critical
		// path the guard blocks on — it was dead code in tests before this case.
		kv, ctx := newKV(t, "rce-real-payload")
		m := newM(kv)

		parts := []types.Partition{{Keys: []string{"alpha", "beta"}}}
		payload := types.AssignmentPayload{
			SchemaVersion: types.AssignmentSchemaVersion,
			Partitions:    parts,
		}
		canonical, jerr := json.Marshal(payload)
		require.NoError(t, jerr)
		hash := sha256.Sum256(canonical)
		hashHex := hex.EncodeToString(hash[:])
		payloadKey := "assignment._payload." + hashHex

		var buf bytes.Buffer
		w, werr := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		require.NoError(t, werr)
		_, werr = w.Write(canonical)
		require.NoError(t, werr)
		require.NoError(t, w.Close())
		_, perr := kv.Create(ctx, payloadKey, buf.Bytes())
		require.NoError(t, perr)

		ref := types.AssignmentPayloadRef{
			Key:         payloadKey,
			PayloadHash: hashHex,
			SetDigest:   types.PartitionSetDigest(parts),
		}
		commit := types.AssignmentCommit{
			Version:  11,
			Payloads: map[string]types.AssignmentPayloadRef{"worker-x": ref},
		}
		commitRaw, cerr := json.Marshal(commit)
		require.NoError(t, cerr)
		_, putErr := kv.Put(ctx, "assignment."+commitKeyName, commitRaw)
		require.NoError(t, putErr)

		batch, err := m.currentCommitPartitionSet(ctx, 11)
		require.NoError(t, err)
		wantKey := parts[0].SubjectKey()
		require.Contains(t, batch, wantKey,
			"partition SubjectKey must appear in batch from real fan-out; batch=%v", batch)
	})

	// Confirm the jetstream.ErrKeyNotFound sentinel is what the live bucket actually
	// returns for a missing key, not ErrNoResponders or ErrBucketNotFound (which would
	// silently skip the errors.Is branch in readCommitEntry and treat it as a real error).
	t.Run("live bucket missing key produces ErrKeyNotFound sentinel", func(t *testing.T) {
		t.Parallel()
		kv, ctx := newKV(t, "rce-sentinel")
		_, err := kv.Get(ctx, "no-such-key")
		require.True(t, errors.Is(err, jetstream.ErrKeyNotFound),
			"live bucket Get on missing key must produce ErrKeyNotFound; got %v", err)
	})
}
