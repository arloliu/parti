package parti

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestLabelGuard_Matrix exercises the spec §9 guard matrix directly against
// labelIncarnationMismatch. Both the worker's labels and the payload's
// labels-of-record are normalized (sorted+deduped) upstream, so slice
// equality is set equality.
func TestLabelGuard_Matrix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		workerLabels []string
		asg          Assignment
		rejected     bool
	}{
		{"known+equal applies", []string{"vip"},
			Assignment{WorkerLabels: []string{"vip"}, WorkerLabelsKnown: true}, false},
		{"known+mismatch rejects", nil,
			Assignment{WorkerLabels: []string{"vip"}, WorkerLabelsKnown: true}, true},
		{"known+empty vs labeled worker rejects", []string{"vip"},
			Assignment{WorkerLabelsKnown: true}, true},
		{"unknown (pre-label payload) applies", []string{"vip"},
			Assignment{}, false},
		{"known+empty vs unlabeled worker applies", nil,
			Assignment{WorkerLabelsKnown: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := &Manager{workerLabels: tc.workerLabels}
			require.Equal(t, tc.rejected, m.labelIncarnationMismatch(tc.asg))
		})
	}
}

// TestLabelGuard_RejectIsTerminalNoRetryNoAck feeds a mismatched assignment
// through applyAssignmentWithPrev (the fresh-version entrypoint) and asserts
// the reject is terminal: the sentinel surfaces, the handoff coordinator's
// Apply never runs, the applied-snapshot ack does not advance, and no
// apply-retry is stashed.
func TestLabelGuard_RejectIsTerminalNoRetryNoAck(t *testing.T) {
	t.Parallel()
	m, rh, hb, _ := newTestManager(t)
	m.workerLabels = []string{"batch"} // this worker's configured labels

	err := m.applyAssignmentWithPrev(Assignment{}, Assignment{
		Version:           7,
		WorkerLabels:      []string{"vip"}, // computed for a different incarnation
		WorkerLabelsKnown: true,
	})

	require.ErrorIs(t, err, errLabelIncarnationRejected)
	require.Zero(t, rh.applyCount.Load(), "handoff Apply must not run on a stale-incarnation reject")
	_, ok := hb.latestSnap()
	require.False(t, ok, "applied ack must not advance on reject")
	require.Nil(t, m.stashedApplyRetry.Load(), "no apply retry may be stashed on reject")
}

// TestLabelGuard_RetryEntrypointAlsoGuarded feeds a mismatched assignment
// through applyAssignmentWithPrevSkipJitter (the scheduleApplyRetry
// entrypoint) and asserts the same terminal reject. Defense in depth: labels
// are immutable per process, so a stashed retry that matched at stash time
// still matches — but the guard here makes the coverage claim true by
// construction.
func TestLabelGuard_RetryEntrypointAlsoGuarded(t *testing.T) {
	t.Parallel()
	m, rh, hb, _ := newTestManager(t)
	m.workerLabels = []string{"batch"}

	err := m.applyAssignmentWithPrevSkipJitter(Assignment{}, Assignment{
		Version:           7,
		WorkerLabels:      []string{"vip"},
		WorkerLabelsKnown: true,
	})

	require.ErrorIs(t, err, errLabelIncarnationRejected)
	require.Zero(t, rh.applyCount.Load(), "handoff Apply must not run on the retry entrypoint reject")
	_, ok := hb.latestSnap()
	require.False(t, ok, "applied ack must not advance on the retry entrypoint reject")
	require.Nil(t, m.stashedApplyRetry.Load(), "no apply retry may be re-stashed on reject")
}

// TestLabelGuard_BuildAssignmentCopiesLabels proves buildAssignmentFromCommit
// round-trips the payload's labels-of-record onto the returned Assignment so
// the guard can compare them.
func TestLabelGuard_BuildAssignmentCopiesLabels(t *testing.T) {
	t.Parallel()
	_, nc := partitest.StartEmbeddedNATS(t)
	akv := partitest.CreateJetStreamKV(t, nc, "label-guard-build-asgn")

	m, _, _, _ := newTestManager(t)
	m.assignmentKV = akv
	wid := m.WorkerID()

	parts := []types.Partition{{Keys: []string{"p1"}}}
	ref := publishLabeledCommitPayload(t, akv, parts, []string{"vip"}, true)

	commit := &types.AssignmentCommit{
		Version:        9,
		LeaderRevision: 3,
		Workers:        []string{wid},
		Payloads:       map[string]types.AssignmentPayloadRef{wid: ref},
	}

	asg, ok := m.buildAssignmentFromCommit(commit, wid)
	require.True(t, ok)
	require.Equal(t, []string{"vip"}, asg.WorkerLabels)
	require.True(t, asg.WorkerLabelsKnown)
}

// publishLabeledCommitPayload mirrors publishCommitPayload but stamps the
// labels-of-record so buildAssignmentFromCommit's copy can be asserted.
func publishLabeledCommitPayload(t *testing.T, kv jetstream.KeyValue, parts []types.Partition, labels []string, known bool) types.AssignmentPayloadRef {
	t.Helper()
	payload := types.AssignmentPayload{
		SchemaVersion:     types.AssignmentSchemaVersion,
		Partitions:        parts,
		WorkerLabels:      labels,
		WorkerLabelsKnown: known,
	}
	canonical, err := json.Marshal(payload)
	require.NoError(t, err)
	hash := sha256.Sum256(canonical)
	hashHex := hex.EncodeToString(hash[:])
	key := "assignment._payload." + hashHex

	var gzBuf bytes.Buffer
	gzw, _ := gzip.NewWriterLevel(&gzBuf, gzip.BestCompression)
	_, err = gzw.Write(canonical)
	require.NoError(t, err)
	require.NoError(t, gzw.Close())
	_, err = kv.Create(t.Context(), key, gzBuf.Bytes())
	require.NoError(t, err)

	return types.AssignmentPayloadRef{
		Key:         key,
		PayloadHash: hashHex,
		SetDigest:   types.PartitionSetDigest(parts),
	}
}

// TestLabelGuard_StartupReject_ClearsExposedAssignment pins the CurrentAssignment
// exposure fix: waitForAssignment raw-stores the fetched assignment BEFORE the
// startup apply runs the stale-incarnation guard, so a guard reject used to
// leave the fetched-but-never-applied assignment visible through the public
// CurrentAssignment() until the leader republished a label-correct commit.
// After a startup reject the store must be empty. The reject itself stays
// benign (returns nil; the Stable transition stands as adjudicated) — only the
// exposure changes. Covers both applyInitialAssignment branches.
func TestLabelGuard_StartupReject_ClearsExposedAssignment(t *testing.T) {
	t.Parallel()

	t.Run("commit branch", func(t *testing.T) {
		t.Parallel()
		_, nc := partitest.StartEmbeddedNATS(t)
		akv := partitest.CreateJetStreamKV(t, nc, "label-guard-startup-commit")

		m, rh, _, _ := newTestManager(t)
		m.assignmentKV = akv
		m.workerLabels = []string{"batch"} // this incarnation's labels
		wid := m.WorkerID()

		parts := []types.Partition{{Keys: []string{"p1"}}}
		ref := publishLabeledCommitPayload(t, akv, parts, []string{"vip"}, true)
		commit := types.AssignmentCommit{
			Version:        3,
			LeaderRevision: 2,
			Workers:        []string{wid},
			Payloads:       map[string]types.AssignmentPayloadRef{wid: ref},
		}
		commitBytes, err := json.Marshal(commit)
		require.NoError(t, err)
		_, err = akv.Put(t.Context(), "assignment._commit", commitBytes)
		require.NoError(t, err)

		// Mirror waitForAssignment's raw store of the fetched assignment.
		m.assignment.Store(Assignment{
			Version:           3,
			Partitions:        parts,
			WorkerLabels:      []string{"vip"},
			WorkerLabelsKnown: true,
		})

		require.NoError(t, m.applyInitialAssignment(t.Context(), akv),
			"startup reject is benign (adjudicated): startup proceeds")
		require.Zero(t, rh.applyCount.Load(), "reject must not run Apply")
		require.Equal(t, Assignment{}, m.CurrentAssignment(),
			"the fetched-but-rejected assignment must not leak through CurrentAssignment")
	})

	t.Run("alias branch", func(t *testing.T) {
		t.Parallel()
		_, nc := partitest.StartEmbeddedNATS(t)
		akv := partitest.CreateJetStreamKV(t, nc, "label-guard-startup-alias")

		m, rh, _, _ := newTestManager(t)
		m.assignmentKV = akv
		m.workerLabels = []string{"batch"}

		// No _commit key exists: applyInitialAssignment falls through to the
		// legacy-alias branch, applying what waitForAssignment surfaced.
		m.assignment.Store(Assignment{
			Version:           2,
			Partitions:        []types.Partition{{Keys: []string{"p1"}}},
			WorkerLabels:      []string{"vip"},
			WorkerLabelsKnown: true,
		})

		require.NoError(t, m.applyInitialAssignment(t.Context(), akv),
			"startup reject is benign (adjudicated): startup proceeds")
		require.Zero(t, rh.applyCount.Load(), "reject must not run Apply")
		require.Equal(t, Assignment{}, m.CurrentAssignment(),
			"the alias-branch reject must clear the raw store too")
	})
}
