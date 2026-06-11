package durable

import (
	"context"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestSanitizeConsumerName_ReplacesInvalidRunes(t *testing.T) {
	in := "prefix s/$ub ject!*"
	out := sanitizeConsumerName(in)
	// ensure no spaces or symbols remain
	require.NotContains(t, out, " ")
	require.NotContains(t, out, "/")
	require.NotContains(t, out, "$")
	require.NotContains(t, out, "!")
	// only allowed charset: [A-Za-z0-9-_]
	for _, r := range out {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		t.Fatalf("unexpected rune in output: %q", r)
	}
}

func TestPerSubjectDurableName_StableAndDistinct(t *testing.T) {
	wc := &WorkerConsumer{}
	prefix := "dur"
	// same subject -> same durable
	d1 := wc.perSubjectDurableName(prefix, "work.a.b")
	d2 := wc.perSubjectDurableName(prefix, "work.a.b")
	require.Equal(t, d1, d2)
	// different subjects -> different durable
	d3 := wc.perSubjectDurableName(prefix, "work.a.c")
	require.NotEqual(t, d1, d3)
	// format: prefix_sanitizedID_hash
	// Since extractPartitionID returns empty in this test (no template), it uses full subject as ID
	// "work.a.b" -> "work_a_b"
	// prefix "dur"
	// expected start: "dur_work_a_b_"
	require.True(t, strings.HasPrefix(d1, prefix+"_work_a_b_"))
	// allowed charset only
	for _, r := range d1 {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		t.Fatalf("unexpected rune in durable: %q", r)
	}
}

func TestEnsureGateResolver_Disabled_NoResolver(t *testing.T) {
	wc := &WorkerConsumer{config: WorkerConsumerConfig{ProcessingGate: &ProcessingGateConfig{Enabled: false}}}
	require.NoError(t, wc.ensureGateResolver(context.Background()))
	require.Nil(t, wc.gateResolver)
}

func TestEnsureGateResolver_UsesProvidedResolver(t *testing.T) {
	fr := &fakeResolver{owner: "w1", state: types.HandoffStateStable, ok: true}
	wc := &WorkerConsumer{config: WorkerConsumerConfig{ProcessingGate: &ProcessingGateConfig{Enabled: true}, Resolver: ResolverConfig{OwnershipResolver: fr}}}
	require.NoError(t, wc.ensureGateResolver(context.Background()))
	require.Equal(t, fr, wc.gateResolver)
}

func TestBuildSubjects_DedupeAndSort(t *testing.T) {
	// prepare worker consumer config with parsed template
	cfg := WorkerConsumerConfig{SubjectTemplate: "events.{{.PartitionID}}"}
	// parse template similarly to constructor
	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)
	wc := &WorkerConsumer{config: cfg, subjectTemplate: tmpl}

	parts := []types.Partition{
		{Keys: []string{"a", "1"}},
		{Keys: []string{"b", "2"}},
		{Keys: []string{"a", "1"}}, // duplicate
	}
	subs, err := wc.buildSubjects(parts)
	require.NoError(t, err)
	require.Equal(t, []string{"events.a.1", "events.b.2"}, subs)
}

func TestWorkerConsumer_WorkerIDMutationGuard_EarlyReturn(t *testing.T) {
	// Build minimal wc with no js and parsed template to avoid any NATS calls
	cfg := WorkerConsumerConfig{SubjectTemplate: "x.{{.PartitionID}}"}
	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)
	wc := &WorkerConsumer{
		config:          cfg,
		subjects:        make(map[string]*partitionConsumer),
		subjectTemplate: tmpl,
	}
	wc.workerID = "w1"
	wc.config.AllowWorkerIDChange = false

	h := messageHandlerFunc(func(ctx context.Context, _ jetstream.Msg) error { return nil })

	// Call Update with a different workerID and no partitions; expect early ErrWorkerIDMutation
	wc.handler = h
	err = wc.UpdateWorkerConsumer(context.Background(), "w2", nil)
	require.ErrorIs(t, err, ErrWorkerIDMutation)
}

// TestUpdateWorkerConsumer_OverCap_ErrorsBeforeMutation pins the
// MaxConcurrentSubjects contract: a deduped subject set larger than the cap
// must return ErrMaxSubjectsExceeded BEFORE any mutation — no workerID
// store, no removals, no new loops. The pre-fix behavior (silently skipping
// excess subjects inside the add loop and returning nil) let the two-phase
// handoff commit ownership of a partition no loop was started for,
// stranding it unowned while the worker reported the assignment applied.
func TestUpdateWorkerConsumer_OverCap_ErrorsBeforeMutation(t *testing.T) {
	wc := &WorkerConsumer{
		logger: logging.NewNop(),
		config: WorkerConsumerConfig{
			SubjectTemplate:       "orders.{{.PartitionID}}.events",
			MaxConcurrentSubjects: 2,
		},
		handler: messageHandlerFunc(func(context.Context, jetstream.Msg) error { return nil }),
		subjects: map[string]*partitionConsumer{
			// Pre-existing subject NOT in the new set: over-cap must not remove it.
			"orders.p9.events": nil,
		},
	}

	// types.Partition is keyed by Keys; PartitionID in the subject template
	// is the dot-joined HashID, so a single key "p0" yields "orders.p0.events".
	parts := []types.Partition{
		{Keys: []string{"p0"}}, {Keys: []string{"p1"}}, {Keys: []string{"p2"}},
	}
	err := wc.UpdateWorkerConsumer(context.Background(), "worker-1", parts)

	require.ErrorIs(t, err, ErrMaxSubjectsExceeded,
		"3 deduped subjects over cap 2 must surface the documented sentinel")
	require.Contains(t, wc.subjects, "orders.p9.events",
		"over-cap update must not perform removals — error must precede all mutation")
	require.Len(t, wc.subjects, 1,
		"over-cap update must not start any new subject loops")
	wc.mu.RLock()
	gotWorkerID := wc.workerID
	wc.mu.RUnlock()
	require.Empty(t, gotWorkerID,
		"over-cap update must not store the workerID — the check runs before setWorkerIDAndSnapshot")
}

// TestUpdateWorkerConsumer_AtCap_Succeeds pins the positive direction of the
// boundary: a deduped subject count EQUAL to the cap passes the check. The
// target subjects are pre-populated so the update is a no-op diff and no
// JetStream client is needed.
func TestUpdateWorkerConsumer_AtCap_Succeeds(t *testing.T) {
	wc := &WorkerConsumer{
		logger: logging.NewNop(),
		config: WorkerConsumerConfig{
			SubjectTemplate:       "orders.{{.PartitionID}}.events",
			MaxConcurrentSubjects: 2,
		},
		handler: messageHandlerFunc(func(context.Context, jetstream.Msg) error { return nil }),
		subjects: map[string]*partitionConsumer{
			"orders.p0.events": nil,
			"orders.p1.events": nil,
		},
	}

	parts := []types.Partition{{Keys: []string{"p0"}}, {Keys: []string{"p1"}}}
	err := wc.UpdateWorkerConsumer(context.Background(), "worker-1", parts)

	require.NoError(t, err, "subject count == cap must pass; the boundary is len(subjects) > cap")
	require.Len(t, wc.subjects, 2)
}

// TestUpdateWorkerConsumer_AfterClose_ReturnsErrConsumerStopped pins the
// terminal-Close contract for the per-subject worker consumer. Pre-fix, a
// post-Close UpdateWorkerConsumer recomputed every subject as an add and
// restarted pull loops — and because Close nils the gate resolver (which is
// only initialized in the constructor), the resurrected loops consumed
// WITHOUT the configured processing gate: a silent safety downgrade, not
// just a zombie restart.
func TestUpdateWorkerConsumer_AfterClose_ReturnsErrConsumerStopped(t *testing.T) {
	ctx := context.Background()

	wc := &WorkerConsumer{
		logger: logging.NewNop(),
		config: WorkerConsumerConfig{
			SubjectTemplate: "orders.{{.PartitionID}}.events",
		},
		handler:  messageHandlerFunc(func(context.Context, jetstream.Msg) error { return nil }),
		subjects: make(map[string]*partitionConsumer),
	}

	// Close on a never-started consumer must be nil (idempotency baseline).
	require.NoError(t, wc.Close(ctx), "first Close must be nil")

	// Post-Close UpdateWorkerConsumer must return ErrConsumerStopped — not nil
	// and not panic. Pre-fix the nil-js add path either panicked (js.CreateOrUpdateConsumer
	// on nil) or returned nil after restarting loops without a gate resolver.
	parts := []types.Partition{{Keys: []string{"p0"}}}
	err := wc.UpdateWorkerConsumer(ctx, "worker-1", parts)
	require.ErrorIs(t, err, types.ErrConsumerStopped,
		"UpdateWorkerConsumer after Close must return ErrConsumerStopped, got: %v", err)

	// subjects map must remain empty — no loops were (re)started.
	require.Empty(t, wc.subjects, "no subject loops must be created after Close")

	// Second Close must still be idempotent (nil).
	require.NoError(t, wc.Close(ctx), "second Close must be idempotent")
}

// TestUpdateWorkerConsumer_RemoveTimeout_SurfacesError pins the
// remove-timeout contract: when subject loops fail to stop within
// DrainOnRemoveTimeout, UpdateWorkerConsumer must return an error (so the
// manager's apply fails pre-commit and retries) instead of reporting silent
// success while a loop may still be processing. Map entries are still
// deleted — a retained-but-stopped entry would make a later re-add of the
// same subject a silent no-op (the dead-subject hazard) — so the follow-up
// update converges.
//
// The never-stopping loop is simulated by a partitionConsumer whose done
// channel never closes: Stop() tolerates a never-started consumer (cancel
// is nil-checked) and Wait() blocks forever.
func TestUpdateWorkerConsumer_RemoveTimeout_SurfacesError(t *testing.T) {
	stuck := &partitionConsumer{done: make(chan struct{})}
	wc := &WorkerConsumer{
		logger: logging.NewNop(),
		config: WorkerConsumerConfig{
			SubjectTemplate:      "orders.{{.PartitionID}}.events",
			DrainOnRemoveTimeout: 50 * time.Millisecond,
		},
		handler: messageHandlerFunc(func(context.Context, jetstream.Msg) error { return nil }),
		subjects: map[string]*partitionConsumer{
			"orders.p0.events": stuck,
		},
	}

	// Empty set removes everything; the stuck loop forces the wait to time out.
	err := wc.UpdateWorkerConsumer(context.Background(), "worker-1", nil)
	require.Error(t, err,
		"a remove that times out waiting for loops to stop must fail the update, not report success")
	require.NotErrorIs(t, err, context.DeadlineExceeded,
		"the timeout is the internal wait bound, not the caller context")
	require.Empty(t, wc.subjects,
		"entries must still be deleted on timeout so the retry converges and re-adds are not silent no-ops")

	// Convergence: the retry (same target set) finds nothing to remove.
	err = wc.UpdateWorkerConsumer(context.Background(), "worker-1", nil)
	require.NoError(t, err, "the follow-up update must converge once entries are gone")
}
