package assignment

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/types"
)

// ============================================================================
// Reproducers: coherent publisher reseed after commit CAS loss.
// ============================================================================
//
// THE DEFECT
//
// Publish proposes currentVersion+1 and advances local state only on a
// successful commit CAS. The CAS-failure branch used to refresh ONLY
// lastCommitRev from the live _commit entry — never currentVersion (nor the
// lastCommit cache). After an external winner landed V+1, the recovered
// publisher's next Publish proposed the SAME V+1 and its CAS now SUCCEEDED
// against the winner's refreshed revision — silently overwriting the winner
// at an already-published Version. Workers never see the overwrite: dispatch
// drops Version <= cur.Version deliveries before payload fetch.
//
// THE FIX
//
// The CAS-failure branch reseeds {lastCommitRev, currentVersion, lastCommit,
// lastCommitObservedAtMono} together from the live winner entry — all four
// or none. If the winner is unreadable or malformed, the publisher latches
// reseed-pending and every subsequent Publish fails closed BEFORE any side
// effect (no payload writes, no aliases, no CAS) until a reseed succeeds.

var errInjectedCommitGet = errors.New("injected commit get failure")

// keyGetFailKV decorates a real KV bucket, failing Get on one key while
// armed. All other operations pass through.
type keyGetFailKV struct {
	jetstream.KeyValue
	mu      sync.Mutex
	failKey string
}

func (k *keyGetFailKV) setFailKey(key string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.failKey = key
}

func (k *keyGetFailKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	k.mu.Lock()
	armed := k.failKey != "" && k.failKey == key
	k.mu.Unlock()
	if armed {
		return nil, errInjectedCommitGet
	}
	return k.KeyValue.Get(ctx, key)
}

// payloadKeyCount counts live "assignment._payload.*" keys — the side-effect
// probe for the fail-closed assertions (a publish that got past the entry
// gate would have created the new input's payload key).
func payloadKeyCount(t *testing.T, ctx context.Context, kv jetstream.KeyValue) int {
	t.Helper()
	lister, err := kv.ListKeys(ctx)
	require.NoError(t, err)
	n := 0
	for key := range lister.Keys() {
		if strings.HasPrefix(key, "assignment._payload.") {
			n++
		}
	}

	return n
}

// forgeWinner CAS-races the fixture's publisher: a direct Put of a valid
// commit at version cur+1 advances the live _commit revision past the
// publisher's cached lastCommitRev. Returns the winner and its revision.
func forgeWinner(t *testing.T, ctx context.Context, f *publisherFixture) (types.AssignmentCommit, uint64) {
	t.Helper()
	cur := f.readCommit(t, ctx)
	require.NotNil(t, cur)
	winner := types.AssignmentCommit{
		Version:        cur.Version + 1,
		LeaderRevision: cur.LeaderRevision,
		Workers:        cur.Workers,
		Payloads:       cur.Payloads,
	}
	winnerBytes, err := json.Marshal(winner)
	require.NoError(t, err)
	rev, err := f.assignmentKV.Put(ctx, "assignment._commit", winnerBytes)
	require.NoError(t, err)

	return winner, rev
}

func reseedTestInput(parts ...string) PublishInput {
	ps2 := make([]types.Partition, 0, len(parts))
	for _, p := range parts {
		ps2 = append(ps2, ps(p))
	}
	return PublishInput{
		Workers:          []string{"w1"},
		Assignments:      map[string][]types.Partition{"w1": ps2},
		SourcePartitions: ps2,
		LeaderRevision:   1,
	}
}

// TestPublisher_CASLossReseed_NeverReusesWinnerVersion: after a lost CAS the
// recovered publisher must commit at winner.Version+1 — never overwrite the
// winner at its own Version.
func TestPublisher_CASLossReseed_NeverReusesWinnerVersion(t *testing.T) {
	f := newPublisherFixture(t, "reseed-never-reuse")
	ctx := t.Context()
	f.putV1Heartbeat(t, ctx, "w1")
	input := reseedTestInput("p1", "p2")

	require.NoError(t, f.pub.Publish(ctx, input)) // commits V1
	winner, winnerRev := forgeWinner(t, ctx, f)   // external winner at V2

	err := f.pub.Publish(ctx, input)
	require.ErrorIs(t, err, types.ErrCommitCASFailed, "stale lastCommitRev must lose the CAS")

	require.NoError(t, f.pub.Publish(ctx, input), "publisher must recover after the reseed")

	live := f.readCommit(t, ctx)
	require.NotNil(t, live)
	require.Equal(t, winner.Version+1, live.Version,
		"recovered publish must commit at winner.Version+1; committing at the winner's own "+
			"Version silently overwrites an already-published assignment")
	require.Equal(t, winnerRev, live.PrevCommitRev,
		"recovered commit must chain from the winner's revision")
}

// TestPublisher_CASLossReseed_WinnerUnreadable_FailsClosed: when the winner
// entry cannot be read back after a lost CAS, the publisher must not publish
// at all (no payload writes, no commit) until a reseed succeeds — a blind
// retry would propose a Version the winner may already own.
func TestPublisher_CASLossReseed_WinnerUnreadable_FailsClosed(t *testing.T) {
	var fkv *keyGetFailKV
	f := newPublisherFixtureWrapKV(t, "reseed-get-fail", func(kv jetstream.KeyValue) jetstream.KeyValue {
		fkv = &keyGetFailKV{KeyValue: kv}
		return fkv
	})
	ctx := t.Context()
	f.putV1Heartbeat(t, ctx, "w1")
	input := reseedTestInput("p1", "p2")

	require.NoError(t, f.pub.Publish(ctx, input)) // commits V1
	winner, _ := forgeWinner(t, ctx, f)           // external winner at V2

	// The reseed inside the CAS-failure branch must hit the injected error.
	fkv.setFailKey("assignment._commit")
	err := f.pub.Publish(ctx, input)
	require.ErrorIs(t, err, types.ErrCommitCASFailed)

	// While the winner stays unreadable, a publish with NEW content must fail
	// closed before any side effect.
	winnerView := f.readCommit(t, ctx)
	keysBefore := payloadKeyCount(t, ctx, f.assignmentKV)
	err = f.pub.Publish(ctx, reseedTestInput("p9"))
	require.ErrorIs(t, err, types.ErrCommitCASFailed,
		"reseed-pending abort must keep the surrendered-batch error class")
	require.EqualValues(t, 1, f.metrics.batchAbortedCount("commit_reseed_pending"),
		"the fail-closed abort must be observable under its own reason")
	require.Equal(t, keysBefore, payloadKeyCount(t, ctx, f.assignmentKV),
		"a fail-closed publish must not write payload keys")
	require.Equal(t, winnerView, f.readCommit(t, ctx),
		"a fail-closed publish must not touch the live commit")

	// Winner readable again: the next publish reseeds and commits past it.
	fkv.setFailKey("")
	require.NoError(t, f.pub.Publish(ctx, input))
	live := f.readCommit(t, ctx)
	require.NotNil(t, live)
	require.Equal(t, winner.Version+1, live.Version,
		"post-recovery commit must be winner.Version+1, never a reused Version")
}

// TestPublisher_CASLossReseed_MalformedWinner_FailsClosed: a winner entry
// that exists but does not unmarshal gives the publisher a revision it could
// CAS against but no Version to order after — it must fail closed rather
// than advance the revision alone and overwrite at a reused Version.
func TestPublisher_CASLossReseed_MalformedWinner_FailsClosed(t *testing.T) {
	f := newPublisherFixture(t, "reseed-malformed")
	ctx := t.Context()
	f.putV1Heartbeat(t, ctx, "w1")
	input := reseedTestInput("p1", "p2")

	require.NoError(t, f.pub.Publish(ctx, input)) // commits V1
	v1 := f.readCommit(t, ctx)
	require.NotNil(t, v1)

	// External writer lands garbage at _commit (advancing the revision).
	_, err := f.assignmentKV.Put(ctx, "assignment._commit", []byte("{malformed winner"))
	require.NoError(t, err)

	err = f.pub.Publish(ctx, input)
	require.ErrorIs(t, err, types.ErrCommitCASFailed)

	// The malformed winner must not be CAS-overwritten by a blind retry.
	keysBefore := payloadKeyCount(t, ctx, f.assignmentKV)
	err = f.pub.Publish(ctx, reseedTestInput("p9"))
	require.ErrorIs(t, err, types.ErrCommitCASFailed,
		"a malformed winner must fail closed, not be overwritten at a reused Version")
	require.EqualValues(t, 1, f.metrics.batchAbortedCount("commit_reseed_pending"))
	require.Equal(t, keysBefore, payloadKeyCount(t, ctx, f.assignmentKV),
		"a fail-closed publish must not write payload keys")

	// A valid winner replaces the garbage; the publisher reseeds past it.
	valid := *v1
	valid.Version = v1.Version + 4 // arbitrary jump; reseed must adopt it as-is
	validBytes, merr := json.Marshal(valid)
	require.NoError(t, merr)
	_, err = f.assignmentKV.Put(ctx, "assignment._commit", validBytes)
	require.NoError(t, err)

	require.NoError(t, f.pub.Publish(ctx, input))
	live := f.readCommit(t, ctx)
	require.NotNil(t, live)
	require.Equal(t, valid.Version+1, live.Version,
		"recovered commit must order after the eventually-readable winner")
}
