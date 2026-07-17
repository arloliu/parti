package parti

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/types"
)

// ============================================================================
// Equal-version divergence counter (AssignmentDivergenceMetricsRecorder).
// ============================================================================
//
// Dispatch drops equal-version authority deliveries before payload fetch
// (commit case (a), alias version gate), so a delivery carrying DIFFERENT
// content at the worker's current version is silently ignored — the
// documented-open hazard family deferred to the claim-level commit-identity
// fence project (issue #74). The counter makes the precondition observable:
// it must fire on genuine content divergence at an equal version and stay
// silent for every healthy redelivery shape.

// Pin the documented promise that collectors embedding types.NopMetrics
// satisfy the optional capability automatically.
var _ types.AssignmentDivergenceMetricsRecorder = types.NopMetrics{}

// recordingDivergence records IncEqualVersionDivergence calls by source.
type recordingDivergence struct {
	mu     sync.Mutex
	counts map[string]int
}

func newRecordingDivergence() *recordingDivergence {
	return &recordingDivergence{counts: make(map[string]int)}
}

func (r *recordingDivergence) IncEqualVersionDivergence(source string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counts[source]++
}

func (r *recordingDivergence) count(source string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.counts[source]
}

func divergenceParts(keys ...string) []types.Partition {
	parts := make([]types.Partition, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, types.Partition{Keys: []string{k}})
	}

	return parts
}

func equalVersionCommit(version int64, workerID string, parts []types.Partition) *types.AssignmentCommit {
	return &types.AssignmentCommit{
		Version:        version,
		LeaderRevision: 30,
		Workers:        []string{workerID},
		Payloads: map[string]types.AssignmentPayloadRef{
			workerID: {Key: "assignment._payload.test", SetDigest: types.PartitionSetDigest(parts)},
		},
	}
}

func TestEqualVersionDivergence_Commit(t *testing.T) {
	t.Parallel()
	m, rh, _, _ := newTestManager(t)
	rec := newRecordingDivergence()
	m.divergenceMetrics = rec

	partsX := divergenceParts("alpha")
	partsY := divergenceParts("beta")
	m.assignment.Store(Assignment{Version: 5, LeaderRevision: 30, Partitions: partsX})
	m.lastSeenLeaderRevision.Store(30)

	// Healthy redelivery: same version, same content for this worker.
	m.handleCommitValue(equalVersionCommit(5, m.WorkerID(), partsX))
	require.Equal(t, 0, rec.count("commit"), "same-content redelivery must not count")

	// Divergence: same version, different content for this worker.
	m.handleCommitValue(equalVersionCommit(5, m.WorkerID(), partsY))
	require.Equal(t, 1, rec.count("commit"), "equal-version different-content commit must count")

	// Stale lower version with different content: not an equal-version event.
	m.handleCommitValue(equalVersionCommit(4, m.WorkerID(), partsY))
	require.Equal(t, 1, rec.count("commit"), "stale lower-version deliveries must not count")

	require.Equal(t, int64(0), rh.applyCount.Load(), "counting must not change dispatch: case (a) never applies")
	require.Equal(t, Assignment{Version: 5, LeaderRevision: 30, Partitions: partsX}, m.CurrentAssignment())
}

func TestEqualVersionDivergence_CommitWorkerAbsent(t *testing.T) {
	t.Parallel()
	m, _, _, _ := newTestManager(t)
	rec := newRecordingDivergence()
	m.divergenceMetrics = rec

	// Holding partitions while an equal-version commit omits this worker:
	// the commit says "you own nothing at V5" — divergent.
	m.assignment.Store(Assignment{Version: 5, Partitions: divergenceParts("alpha")})
	m.handleCommitValue(&types.AssignmentCommit{Version: 5, LeaderRevision: 30, Workers: []string{"other"}})
	require.Equal(t, 1, rec.count("commit"))

	// Holding nothing while absent: consistent, not divergent.
	m.assignment.Store(Assignment{Version: 5})
	m.handleCommitValue(&types.AssignmentCommit{Version: 5, LeaderRevision: 31, Workers: []string{"other"}})
	require.Equal(t, 1, rec.count("commit"))

	// Membership is decided by Workers, not Payloads: a commit that omits
	// this worker from Workers but leaves a stale matching payload ref is
	// still a revocation at the worker's own version — divergent.
	held := divergenceParts("alpha")
	m.assignment.Store(Assignment{Version: 5, Partitions: held})
	m.handleCommitValue(&types.AssignmentCommit{
		Version: 5, LeaderRevision: 32, Workers: []string{"other"},
		Payloads: map[string]types.AssignmentPayloadRef{
			m.WorkerID(): {Key: "assignment._payload.stale", SetDigest: types.PartitionSetDigest(held)},
		},
	})
	require.Equal(t, 2, rec.count("commit"),
		"a stale payload ref must not mask a revocation at an equal version")

	// A member without a payload ref is a malformed commit — dispatch
	// classifies that separately; the divergence counter stays silent.
	m.handleCommitValue(&types.AssignmentCommit{Version: 5, LeaderRevision: 33, Workers: []string{m.WorkerID()}})
	require.Equal(t, 2, rec.count("commit"), "malformed member-without-payload must not count")
}

func TestEqualVersionDivergence_Alias(t *testing.T) {
	t.Parallel()
	m, rh, _, _ := newTestManager(t)
	rec := newRecordingDivergence()
	m.divergenceMetrics = rec

	partsX := divergenceParts("alpha")
	partsY := divergenceParts("beta")
	m.assignment.Store(Assignment{Version: 5, LeaderRevision: 30, Partitions: partsX})
	m.lastSeenLeaderRevision.Store(30)

	deliverAlias := func(parts []types.Partition) {
		encoded, err := json.Marshal(Assignment{Version: 5, LeaderRevision: 30, Partitions: parts})
		require.NoError(t, err)
		m.handleAssignmentEntry(m.WorkerID(), aliasEntry{value: encoded})
	}

	// Healthy redelivery (e.g. post-commit compat alias): same content.
	deliverAlias(partsX)
	require.Equal(t, 0, rec.count("alias"), "same-content alias redelivery must not count")

	// Divergence: same version, different partitions.
	deliverAlias(partsY)
	require.Equal(t, 1, rec.count("alias"), "equal-version different-content alias must count")

	require.Equal(t, int64(0), rh.applyCount.Load(), "counting must not change dispatch: the version gate never applies")
}

func TestEqualVersionDivergence_NoRecorder_NoPanic(t *testing.T) {
	t.Parallel()
	m, _, _, _ := newTestManager(t)
	require.Nil(t, m.divergenceMetrics, "newTestManager must not wire the capability by default")

	m.assignment.Store(Assignment{Version: 5, Partitions: divergenceParts("alpha")})
	m.handleCommitValue(equalVersionCommit(5, m.WorkerID(), divergenceParts("beta")))

	encoded, err := json.Marshal(Assignment{Version: 5, Partitions: divergenceParts("beta")})
	require.NoError(t, err)
	m.handleAssignmentEntry(m.WorkerID(), aliasEntry{value: encoded})
}
