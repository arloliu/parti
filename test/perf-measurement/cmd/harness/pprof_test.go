package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLeaderTracker_NoLeaderInitially locks the zero-value contract: a
// freshly constructed tracker reports no leader until some worker claims
// it.
func TestLeaderTracker_NoLeaderInitially(t *testing.T) {
	lt := NewLeaderTracker()
	_, ok := lt.Current()
	require.False(t, ok, "a fresh tracker must report no leader")
}

// TestLeaderTracker_ClaimAndRelease covers the ordinary single-worker
// claim/release cycle.
func TestLeaderTracker_ClaimAndRelease(t *testing.T) {
	lt := NewLeaderTracker()

	lt.Record(2, true)
	idx, ok := lt.Current()
	require.True(t, ok)
	require.Equal(t, 2, idx)

	lt.Record(2, false)
	_, ok = lt.Current()
	require.False(t, ok, "the recorded leader releasing leadership must clear the tracker")
}

// TestLeaderTracker_StaleReleaseDoesNotClobberNewerLeader is the load-bearing
// case for the CAS-based clear in Record: parti's OnLeadershipChanged hooks
// run asynchronously per-worker, so a former leader's isLeader=false callback
// can be delivered to the tracker AFTER a different worker has already
// claimed leadership (e.g. a fast handoff during a rebalance burst — exactly
// the scenario this endpoint exists to observe). That stale release must not
// erase the newer leader's claim.
func TestLeaderTracker_StaleReleaseDoesNotClobberNewerLeader(t *testing.T) {
	lt := NewLeaderTracker()

	lt.Record(0, true) // worker 0 becomes leader
	lt.Record(1, true) // worker 1 claims leadership (handoff)

	idx, ok := lt.Current()
	require.True(t, ok)
	require.Equal(t, 1, idx)

	// Worker 0's delayed "I lost leadership" callback arrives after the
	// handoff above. It must be a no-op: worker 0 is no longer the
	// recorded leader.
	lt.Record(0, false)

	idx, ok = lt.Current()
	require.True(t, ok, "a stale release from a former leader must not clear the tracker")
	require.Equal(t, 1, idx, "the current leader must remain worker 1")
}

// TestLeaderHandler_ReportsCurrentLeader exercises the /leader HTTP
// handler's own branching (not net/http/pprof, which is out of scope):
// 404+leader:false with no leader recorded, 200+leader:true+workerIdx with
// one recorded.
func TestLeaderHandler_ReportsCurrentLeader(t *testing.T) {
	lt := NewLeaderTracker()
	h := leaderHandler(lt)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/leader", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
	var resp leaderResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.False(t, resp.Leader)

	lt.Record(3, true)
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/leader", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.True(t, resp.Leader)
	require.Equal(t, 3, resp.WorkerIdx)
}

// TestLeaderHandler_WorkerZeroIsNotOmitted regression-locks the
// leaderResponse JSON encoding: WorkerIdx must not be tagged omitempty,
// because worker 0 is a legitimate leader and Go's encoding/json drops
// zero-value int fields marked omitempty, which would make a leading
// worker 0 indistinguishable on the wire from an absent index.
func TestLeaderHandler_WorkerZeroIsNotOmitted(t *testing.T) {
	lt := NewLeaderTracker()
	lt.Record(0, true)
	h := leaderHandler(lt)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/leader", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"workerIdx":0`, "worker 0 must be explicit in the JSON body, not omitted")

	var resp leaderResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.True(t, resp.Leader)
	require.Equal(t, 0, resp.WorkerIdx)
}
