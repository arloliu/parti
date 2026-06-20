package durable

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"text/template"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/logging"
	partitesting "github.com/arloliu/parti/v2/partitest"
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

// --- merged from worker_consumer_loop_test.go ---

func TestWorkerConsumer_UpdateAndPullLoop_ProcessesMessages(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	// Start embedded NATS with JetStream
	_, nc := partitesting.StartEmbeddedNATS(t)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create a stream for our subjects
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "TEST",
		Subjects:  []string{"events.*.*"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	// Prepare helper
	cfg := WorkerConsumerConfig{
		StreamName:      "TEST",
		ConsumerPrefix:  "wc2",
		SubjectTemplate: "events.{{.PartitionID}}",
		BatchSize:       2,
	}
	require.NoError(t, cfg.SetDefaults())

	// Parse template for direct construction path (avoid New helper dependencies)
	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)

	var handled int32
	handler := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		atomic.AddInt32(&handled, 1)
		// returning nil causes helper to Ack in default mode
		return nil
	})

	wc := &WorkerConsumer{
		js:              js,
		config:          cfg,
		logger:          cfg.Logger,
		handler:         handler,
		subjects:        make(map[string]*partitionConsumer),
		iterFactory:     defaultIterFactory,
		subjectTemplate: tmpl,
	}

	// Exercise UpdateWorkerConsumer to add one subject and start loop
	parts := []types.Partition{{Keys: []string{"a", "1"}}}
	require.NoError(t, wc.UpdateWorkerConsumer(ctx, "worker-1", parts))
	t.Cleanup(func() { _ = wc.Close(ctx) })

	// Wait for the per-subject durable to exist before publishing to avoid race
	durable := wc.perSubjectDurableName(cfg.ConsumerPrefix, "events.a.1")
	waitDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(waitDeadline) {
		if _, err := js.Consumer(ctx, cfg.StreamName, durable); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Publish a few messages to the subject
	for range 3 {
		require.NoError(t, nc.Publish("events.a.1", []byte("m")))
	}
	_ = nc.Flush()

	// Wait until all messages are handled or timeout
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&handled) >= 3 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	require.GreaterOrEqual(t, atomic.LoadInt32(&handled), int32(3), "expected handler to process at least 3 messages")
}

// errIter is a messages iterator that always returns an error.
type errIter struct{}

func (e *errIter) Next(opts ...jetstream.NextOpt) (jetstream.Msg, error) {
	return nil, errors.New("forced")
}
func (e *errIter) Stop()  {}
func (e *errIter) Drain() {}

func TestWorkerConsumer_PullGating_SuppressesUntilOwnerStable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// stream for subjects
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "PG",
		Subjects:  []string{"pg.*.*"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	// configure with gate + pull gating
	cfg := WorkerConsumerConfig{
		StreamName:        "PG",
		ConsumerPrefix:    "wc2",
		SubjectTemplate:   "pg.{{.PartitionID}}",
		PullGatingEnabled: true,
		ProcessingGate:    &ProcessingGateConfig{Enabled: true},
		Resolver: ResolverConfig{
			HandoffBucketName:   "pg-handoff",
			HandoffClaimsPrefix: "claims/",
			BatchWindow:         5 * time.Millisecond,
			BatchMaxItems:       64,
		},
	}
	require.NoError(t, cfg.SetDefaults())
	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)

	var handled int32
	h := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		atomic.AddInt32(&handled, 1)
		return nil
	})

	wc := &WorkerConsumer{
		js:              js,
		config:          cfg,
		logger:          cfg.Logger,
		handler:         h,
		subjects:        make(map[string]*partitionConsumer),
		iterFactory:     defaultIterFactory,
		subjectTemplate: tmpl,
	}
	// Configure partition prefix/suffix for extraction (since we bypassed constructor)
	wc.partitionPrefix = "pg."
	wc.partitionSuffix = ""

	// init resolver
	require.NoError(t, wc.ensureGateResolver(ctx))

	// add subject with worker w1; gating should suppress since no claim exists
	parts := []types.Partition{{Keys: []string{"a", "1"}}}
	require.NoError(t, wc.UpdateWorkerConsumer(ctx, "w1", parts))
	t.Cleanup(func() { _ = wc.Close(context.Background()) })

	// Wait for the per-subject durable to exist before publishing to avoid race
	durable := wc.perSubjectDurableName(cfg.ConsumerPrefix, "pg.a.1")
	waitDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(waitDeadline) {
		if _, err := js.Consumer(ctx, cfg.StreamName, durable); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Sanity: extraction should produce correct partition id
	require.Equal(t, "a.1", wc.extractPartitionID("pg.a.1"))

	// Publish 3 messages pre-claim; expect none handled yet (gated by processing gate)
	for range 3 {
		require.NoError(t, nc.Publish("pg.a.1", []byte("m")))
	}
	_ = nc.Flush()
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, int32(0), atomic.LoadInt32(&handled))

	// Put claim for owner w1 in Stable; resolver should pick it up and allow pulls
	kv, err := js.KeyValue(ctx, cfg.Resolver.HandoffBucketName)
	require.NoError(t, err)
	cl := handoff.NewInitialClaim("a.1", "w1", time.Now(), 10*time.Minute)
	b, err := cl.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, cfg.Resolver.HandoffClaimsPrefix+cl.PartitionID, b)
	require.NoError(t, err)

	// Publish 3 more messages after claim becomes visible to ensure delivery post-claim
	for range 3 {
		require.NoError(t, nc.Publish("pg.a.1", []byte("m2")))
	}
	_ = nc.Flush()

	// Wait for handling to progress after claim becomes visible
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&handled) >= 3 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	require.GreaterOrEqual(t, atomic.LoadInt32(&handled), int32(3))
}

func TestWorkerConsumer_IteratorEscalation_BurstTriggersMetric(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "ESC", Subjects: []string{"esc.*"}, Retention: jetstream.LimitsPolicy, Storage: jetstream.MemoryStorage, MaxMsgs: -1})
	require.NoError(t, err)

	// Iterator always errors
	iterFactory := func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error) {
		return &errIter{}, nil
	}

	var m captureEscalationMetrics
	cfg := WorkerConsumerConfig{StreamName: "ESC", ConsumerPrefix: "wc2", SubjectTemplate: "esc.{{.PartitionID}}", Retry: RetryConfig{Max: 50 * time.Millisecond}}
	cfg.IteratorEscalationWindow = 200 * time.Millisecond
	cfg.IteratorEscalationThreshold = 3
	cfg.Metrics = &m
	require.NoError(t, cfg.SetDefaults())
	tmpl, _ := template.New("subject").Parse(cfg.SubjectTemplate)
	wc := &WorkerConsumer{js: js, config: cfg, logger: cfg.Logger, handler: messageHandlerFunc(func(context.Context, jetstream.Msg) error { return nil }), subjects: make(map[string]*partitionConsumer), iterFactory: iterFactory, subjectTemplate: tmpl}

	require.NoError(t, wc.UpdateWorkerConsumer(ctx, "w1", []types.Partition{{Keys: []string{"x"}}}))
	t.Cleanup(func() { _ = wc.Close(context.Background()) })

	// Wait for escalation metric to trigger
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&m.escalations) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.GreaterOrEqual(t, atomic.LoadInt32(&m.escalations), int32(1))
}

func TestWorkerConsumer_SubjectRemoval_InactiveGCGarbageCollects(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "GC", Subjects: []string{"gc.*.*"}, Retention: jetstream.LimitsPolicy, Storage: jetstream.MemoryStorage, MaxMsgs: -1})
	require.NoError(t, err)

	cfg := WorkerConsumerConfig{StreamName: "GC", ConsumerPrefix: "wc2", SubjectTemplate: "gc.{{.PartitionID}}"}
	cfg.InactiveThreshold = 300 * time.Millisecond
	require.NoError(t, cfg.SetDefaults())
	tmpl, _ := template.New("subject").Parse(cfg.SubjectTemplate)
	wc := &WorkerConsumer{js: js, config: cfg, logger: cfg.Logger, handler: messageHandlerFunc(func(context.Context, jetstream.Msg) error { return nil }), subjects: make(map[string]*partitionConsumer), iterFactory: defaultIterFactory, subjectTemplate: tmpl}

	subj := "gc.a.1"
	part := []types.Partition{{Keys: []string{"a", "1"}}}
	require.NoError(t, wc.UpdateWorkerConsumer(ctx, "w1", part))
	// Wait for durable creation
	durable := wc.perSubjectDurableName(cfg.ConsumerPrefix, subj)
	waitDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(waitDeadline) {
		if _, err := js.Consumer(ctx, cfg.StreamName, durable); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Remove subject (stop loop)
	require.NoError(t, wc.UpdateWorkerConsumer(ctx, "w1", nil))

	// Wait for InactiveThreshold * ~6 and assert consumer is GC'd (allow headroom)
	time.Sleep(2 * time.Second)
	_, err = js.Consumer(ctx, cfg.StreamName, durable)
	require.Error(t, err, "expected consumer to be garbage collected")
	_ = wc.Close(context.Background())
}

func TestWorkerConsumer_DualWorkerPullGating_OnlyOwnerPulls(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Stream and subjects
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "PG2", Subjects: []string{"pg2.*.*"}, Retention: jetstream.LimitsPolicy, Storage: jetstream.MemoryStorage, MaxMsgs: -1})
	require.NoError(t, err)

	cfg := WorkerConsumerConfig{
		StreamName:        "PG2",
		ConsumerPrefix:    "wc2",
		SubjectTemplate:   "pg2.{{.PartitionID}}",
		PullGatingEnabled: true,
		ProcessingGate:    &ProcessingGateConfig{Enabled: true},
		Resolver: ResolverConfig{
			HandoffBucketName:   "pg2-handoff",
			HandoffClaimsPrefix: "claims/",
			BatchWindow:         5 * time.Millisecond,
			BatchMaxItems:       64,
		},
	}
	require.NoError(t, cfg.SetDefaults())

	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)

	var a1w1, a1w2, b1w1, b1w2 int32
	h1 := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		if msg.Subject() == "pg2.a.1" {
			atomic.AddInt32(&a1w1, 1)
		}
		if msg.Subject() == "pg2.b.1" {
			atomic.AddInt32(&b1w1, 1)
		}

		return nil
	})
	h2 := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		if msg.Subject() == "pg2.a.1" {
			atomic.AddInt32(&a1w2, 1)
		}
		if msg.Subject() == "pg2.b.1" {
			atomic.AddInt32(&b1w2, 1)
		}

		return nil
	})

	wc1 := &WorkerConsumer{js: js, config: cfg, logger: cfg.Logger, handler: h1, subjects: make(map[string]*partitionConsumer), iterFactory: defaultIterFactory, subjectTemplate: tmpl}
	wc2 := &WorkerConsumer{js: js, config: cfg, logger: cfg.Logger, handler: h2, subjects: make(map[string]*partitionConsumer), iterFactory: defaultIterFactory, subjectTemplate: tmpl}
	wc1.partitionPrefix = "pg2."
	wc2.partitionPrefix = "pg2."

	// Initialize resolvers
	require.NoError(t, wc1.ensureGateResolver(ctx))
	require.NoError(t, wc2.ensureGateResolver(ctx))

	// Claims: a.1 -> w1, b.1 -> w2
	kv, err := js.KeyValue(ctx, cfg.Resolver.HandoffBucketName)
	require.NoError(t, err)
	clA := handoff.NewInitialClaim("a.1", "w1", time.Now(), 10*time.Minute)
	b, err := clA.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, cfg.Resolver.HandoffClaimsPrefix+clA.PartitionID, b)
	require.NoError(t, err)
	clB := handoff.NewInitialClaim("b.1", "w2", time.Now(), 10*time.Minute)
	b, err = clB.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, cfg.Resolver.HandoffClaimsPrefix+clB.PartitionID, b)
	require.NoError(t, err)

	// Assign both partitions to both workers
	parts := []types.Partition{{Keys: []string{"a", "1"}}, {Keys: []string{"b", "1"}}}
	require.NoError(t, wc1.UpdateWorkerConsumer(ctx, "w1", parts))
	t.Cleanup(func() { _ = wc1.Close(context.Background()) })
	require.NoError(t, wc2.UpdateWorkerConsumer(ctx, "w2", parts))
	t.Cleanup(func() { _ = wc2.Close(context.Background()) })

	// Wait for consumers to exist to avoid race
	for _, subj := range []string{"pg2.a.1", "pg2.b.1"} {
		durable := wc1.perSubjectDurableName(cfg.ConsumerPrefix, subj)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := js.Consumer(ctx, cfg.StreamName, durable); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Publish to both subjects
	require.NoError(t, nc.Publish("pg2.a.1", []byte("1")))
	require.NoError(t, nc.Publish("pg2.b.1", []byte("1")))
	_ = nc.Flush()

	// Expect a.1 handled by w1 only, b.1 by w2 only
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&a1w1) >= 1 && atomic.LoadInt32(&b1w2) >= 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	require.GreaterOrEqual(t, atomic.LoadInt32(&a1w1), int32(1))
	require.Equal(t, int32(0), atomic.LoadInt32(&a1w2), "non-owner should be gated")
	require.GreaterOrEqual(t, atomic.LoadInt32(&b1w2), int32(1))
	require.Equal(t, int32(0), atomic.LoadInt32(&b1w1), "non-owner should be gated")
}

func TestWorkerConsumer_SubscribesAllPartitions_AfterAssignment(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "SUBALL", Subjects: []string{"suball.*.*"}, Retention: jetstream.LimitsPolicy, Storage: jetstream.MemoryStorage, MaxMsgs: -1})
	require.NoError(t, err)

	cfg := WorkerConsumerConfig{StreamName: "SUBALL", ConsumerPrefix: "wc2", SubjectTemplate: "suball.{{.PartitionID}}", BatchSize: 2}
	require.NoError(t, cfg.SetDefaults())
	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)

	var p1, p2, p3 int32
	h := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		switch msg.Subject() {
		case "suball.a.1":
			atomic.AddInt32(&p1, 1)
		case "suball.a.2":
			atomic.AddInt32(&p2, 1)
		case "suball.b.1":
			atomic.AddInt32(&p3, 1)
		}

		return nil
	})

	wc := &WorkerConsumer{js: js, config: cfg, logger: cfg.Logger, handler: h, subjects: make(map[string]*partitionConsumer), iterFactory: defaultIterFactory, subjectTemplate: tmpl}
	parts := []types.Partition{{Keys: []string{"a", "1"}}, {Keys: []string{"a", "2"}}, {Keys: []string{"b", "1"}}}
	require.NoError(t, wc.UpdateWorkerConsumer(ctx, "w1", parts))
	t.Cleanup(func() { _ = wc.Close(context.Background()) })

	// Ensure per-subject consumers are created for all partitions
	subjects := []string{"suball.a.1", "suball.a.2", "suball.b.1"}
	for _, subj := range subjects {
		durable := wc.perSubjectDurableName(cfg.ConsumerPrefix, subj)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := js.Consumer(ctx, cfg.StreamName, durable); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Publish one message to each subject and expect one handled each
	for _, subj := range subjects {
		require.NoError(t, nc.Publish(subj, []byte("x")))
	}
	_ = nc.Flush()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&p1) >= 1 && atomic.LoadInt32(&p2) >= 1 && atomic.LoadInt32(&p3) >= 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	require.GreaterOrEqual(t, atomic.LoadInt32(&p1), int32(1))
	require.GreaterOrEqual(t, atomic.LoadInt32(&p2), int32(1))
	require.GreaterOrEqual(t, atomic.LoadInt32(&p3), int32(1))
}

func TestWorkerConsumer_ManualAck_MaxAckPending_ThrottlesDelivery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "MACK",
		Subjects:  []string{"mack.*.*"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	cfg := WorkerConsumerConfig{
		StreamName:            "MACK",
		ConsumerPrefix:        "wc2",
		SubjectTemplate:       "mack.{{.PartitionID}}",
		ManualAck:             true,
		MaxAckPending:         2,
		MaxConcurrentSubjects: 10,
		BatchSize:             1,
		FetchTimeout:          1 * time.Second,
	}
	require.NoError(t, cfg.SetDefaults())

	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)

	msgCh := make(chan jetstream.Msg, 16)
	var delivered int32
	handler := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		atomic.AddInt32(&delivered, 1)
		// don't ack yet; push to channel for the test to control
		select {
		case msgCh <- msg:
		default:
		}
		return nil
	})

	wc := &WorkerConsumer{
		js:              js,
		config:          cfg,
		logger:          cfg.Logger,
		handler:         handler,
		subjects:        make(map[string]*partitionConsumer),
		iterFactory:     defaultIterFactory,
		subjectTemplate: tmpl,
	}
	wc.partitionPrefix = "mack."
	require.NoError(t, wc.UpdateWorkerConsumer(ctx, "worker-1", []types.Partition{{Keys: []string{"a", "1"}}}))
	t.Cleanup(func() { _ = wc.Close(context.Background()) })

	// Sanity check: loop was started for the subject
	require.Eventually(t, func() bool { return len(wc.subjects) == 1 }, 500*time.Millisecond, 20*time.Millisecond)

	// Wait for durable to exist before publishing
	durable := wc.perSubjectDurableName(cfg.ConsumerPrefix, "mack.a.1")
	waitDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(waitDeadline) {
		if _, err := js.Consumer(ctx, cfg.StreamName, durable); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Give the pull loop a brief moment to start before publishing
	time.Sleep(200 * time.Millisecond)

	// publish 5 messages
	for i := range 5 {
		require.NoError(t, nc.Publish("mack.a.1", fmt.Appendf(nil, "m%d", i)))
	}
	_ = nc.Flush()

	// Inspect consumer info for visibility during debugging
	if ci, err := js.Consumer(ctx, cfg.StreamName, durable); err == nil {
		if info, err := ci.Info(ctx); err == nil {
			t.Logf("consumer num_ack_pending=%d, num_pending=%d, num_waiting=%d", info.NumAckPending, info.NumPending, info.NumWaiting)
		}
	}

	// Expect delivery to stop at MaxAckPending (2)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&delivered) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// If nothing delivered yet, try a direct pull to validate server delivery works
	if atomic.LoadInt32(&delivered) == 0 {
		if ci, err := js.Consumer(ctx, cfg.StreamName, durable); err == nil {
			it, ierr := ci.Messages(jetstream.PullMaxMessages(1), jetstream.PullExpiry(1*time.Second))
			if ierr == nil {
				if m, merr := it.Next(); merr == nil && m != nil {
					t.Logf("direct pull received a message: subject=%s", m.Subject())
					_ = m.Ack()
				} else {
					t.Logf("direct pull received no message: err=%v", merr)
				}
			}
		}
	}
	require.Equal(t, int32(2), atomic.LoadInt32(&delivered), "should deliver up to MaxAckPending before acks")

	// Ensure it doesn't exceed while we don't ack
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, int32(2), atomic.LoadInt32(&delivered))

	// Ack one; expect one more to arrive soon (now 3 delivered)
	select {
	case m := <-msgCh:
		require.NoError(t, m.Ack())
	case <-time.After(1 * time.Second):
		t.Fatal("did not receive message to ack")
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&delivered) >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.GreaterOrEqual(t, atomic.LoadInt32(&delivered), int32(3))
}

func TestWorkerConsumer_UpdateRemovesSubject_StopsLoopKeepsDurable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx := context.Background()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "STOP",
		Subjects:  []string{"stop.*.*"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	cfg := WorkerConsumerConfig{StreamName: "STOP", ConsumerPrefix: "wc2", SubjectTemplate: "stop.{{.PartitionID}}"}
	require.NoError(t, cfg.SetDefaults())
	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)

	var handled int32
	h := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		atomic.AddInt32(&handled, 1)
		return nil
	})

	wc := &WorkerConsumer{
		js:              js,
		config:          cfg,
		logger:          cfg.Logger,
		handler:         h,
		subjects:        make(map[string]*partitionConsumer),
		iterFactory:     defaultIterFactory,
		subjectTemplate: tmpl,
	}

	part := []types.Partition{{Keys: []string{"a", "1"}}}
	require.NoError(t, wc.UpdateWorkerConsumer(ctx, "worker-2", part))
	// publish one and wait handled
	require.NoError(t, nc.Publish("stop.a.1", []byte("x")))
	_ = nc.Flush()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&handled) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.Equal(t, int32(1), atomic.LoadInt32(&handled))

	// Now remove the subject
	require.NoError(t, wc.UpdateWorkerConsumer(ctx, "worker-2", nil))

	// Durable should still exist
	durable := wc.perSubjectDurableName(cfg.ConsumerPrefix, "stop.a.1")
	_, err = js.Consumer(ctx, cfg.StreamName, durable)
	require.NoError(t, err)

	// Publish more; loop is stopped so handler should not increment
	for range 3 {
		require.NoError(t, nc.Publish("stop.a.1", []byte("y")))
	}
	_ = nc.Flush()
	// wait a bit; handled should stay the same
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, int32(1), atomic.LoadInt32(&handled))
}

func TestWorkerConsumer_Close_RespectsTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx := context.Background()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "CLOSE",
		Subjects:  []string{"close.*.*"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	cfg := WorkerConsumerConfig{
		StreamName:           "CLOSE",
		ConsumerPrefix:       "wc2",
		SubjectTemplate:      "close.{{.PartitionID}}",
		DrainOnRemoveTimeout: 500 * time.Millisecond, // Short timeout for test
	}
	require.NoError(t, cfg.SetDefaults())
	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)

	// Handler that blocks until signaled
	blockCh := make(chan struct{})
	handler := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		<-blockCh
		return nil
	})

	wc := &WorkerConsumer{
		js:              js,
		config:          cfg,
		logger:          cfg.Logger,
		handler:         handler,
		subjects:        make(map[string]*partitionConsumer),
		iterFactory:     defaultIterFactory,
		subjectTemplate: tmpl,
	}

	// Start consumer
	part := []types.Partition{{Keys: []string{"a", "1"}}}
	require.NoError(t, wc.UpdateWorkerConsumer(ctx, "worker-1", part))

	// Publish message to trigger blocking handler
	require.NoError(t, nc.Publish("close.a.1", []byte("msg")))
	_ = nc.Flush()

	// Give it a moment to pick up the message and block
	time.Sleep(100 * time.Millisecond)

	// Attempt to close with a timeout shorter than the block
	closeCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	err = wc.Close(closeCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded, "Close should return DeadlineExceeded when handler blocks")

	// Unblock handler to allow clean teardown
	close(blockCh)
}

// TestWorkerConsumer_RecoverySnapshotMatchesCreateConfig verifies the
// recovery snapshot built in addSubjectLoop (worker_consumer.go ~line 414)
// carries ConsumerMemoryStorage and ConsumerReplicas. The Dynamic
// integration test only inspects the live consumer's Info().Config (the
// ensurePerSubjectConsumer literal at ~line 459); this test inspects the
// SEPARATE literal at ~line 414 that's stored for recovery.
func TestWorkerConsumer_RecoverySnapshotMatchesCreateConfig(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "WCSNAP",
		Subjects: []string{"wcsnap.>"},
		Storage:  jetstream.FileStorage,
		Replicas: 1,
	})
	require.NoError(t, err)

	cfg := WorkerConsumerConfig{
		StreamName:            "WCSNAP",
		ConsumerPrefix:        "wcsnap",
		SubjectTemplate:       "wcsnap.{{.PartitionID}}",
		BatchSize:             2,
		ConsumerMemoryStorage: true,
		ConsumerReplicas:      1,
	}
	require.NoError(t, cfg.SetDefaults())

	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)

	handler := messageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	wc := &WorkerConsumer{
		js:              js,
		config:          cfg,
		logger:          cfg.Logger,
		handler:         handler,
		subjects:        make(map[string]*partitionConsumer),
		iterFactory:     defaultIterFactory,
		subjectTemplate: tmpl,
	}

	parts := []types.Partition{{Keys: []string{"p0"}}}
	require.NoError(t, wc.UpdateWorkerConsumer(ctx, "wcsnap-worker", parts))
	t.Cleanup(func() { _ = wc.Close(ctx) })

	subject := "wcsnap.p0"
	deadline := time.Now().Add(3 * time.Second)
	var pc *partitionConsumer
	for time.Now().Before(deadline) {
		wc.mu.RLock()
		pc = wc.subjects[subject]
		wc.mu.RUnlock()
		if pc != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.NotNil(t, pc, "partition consumer for %s not registered after 3s", subject)

	require.True(t, pc.consumerConfig.MemoryStorage,
		"recovery snapshot: MemoryStorage = false, want true (likely worker_consumer.go:414 missed the field)")
	require.Equal(t, 1, pc.consumerConfig.Replicas,
		"recovery snapshot: Replicas = %d, want 1", pc.consumerConfig.Replicas)
}

// TestWorkerConsumer_RecoverySnapshotDefaults verifies the recovery
// snapshot preserves zero-value defaults (MemoryStorage=false,
// Replicas=0). Without this, a regression that hard-coded Replicas=1
// in the recovery literal would still pass
// TestWorkerConsumer_RecoverySnapshotMatchesCreateConfig and the
// Dynamic live test (single-node embedded NATS reports server-inherited
// Replicas=1 for both default and explicit Replicas=1).
func TestWorkerConsumer_RecoverySnapshotDefaults(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "WCSNAPDEF",
		Subjects: []string{"wcsnapdef.>"},
		Storage:  jetstream.FileStorage,
		Replicas: 1,
	})
	require.NoError(t, err)

	cfg := WorkerConsumerConfig{
		StreamName:      "WCSNAPDEF",
		ConsumerPrefix:  "wcsnapdef",
		SubjectTemplate: "wcsnapdef.{{.PartitionID}}",
		BatchSize:       2,
		// ConsumerMemoryStorage and ConsumerReplicas intentionally unset.
	}
	require.NoError(t, cfg.SetDefaults())

	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)

	handler := messageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	wc := &WorkerConsumer{
		js:              js,
		config:          cfg,
		logger:          cfg.Logger,
		handler:         handler,
		subjects:        make(map[string]*partitionConsumer),
		iterFactory:     defaultIterFactory,
		subjectTemplate: tmpl,
	}

	parts := []types.Partition{{Keys: []string{"p0"}}}
	require.NoError(t, wc.UpdateWorkerConsumer(ctx, "wcsnapdef-worker", parts))
	t.Cleanup(func() { _ = wc.Close(ctx) })

	subject := "wcsnapdef.p0"
	deadline := time.Now().Add(3 * time.Second)
	var pc *partitionConsumer
	for time.Now().Before(deadline) {
		wc.mu.RLock()
		pc = wc.subjects[subject]
		wc.mu.RUnlock()
		if pc != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.NotNil(t, pc, "partition consumer for %s not registered after 3s", subject)

	require.False(t, pc.consumerConfig.MemoryStorage,
		"recovery snapshot default: MemoryStorage = true, want false (the recovery literal in addSubjectLoop must not hardcode true)")
	require.Equal(t, 0, pc.consumerConfig.Replicas,
		"recovery snapshot default: Replicas = %d, want 0 (the recovery literal must not hardcode a positive value)", pc.consumerConfig.Replicas)
}

// --- merged from worker_consumer_concurrency_test.go ---

// TestWorkerConsumer_ConcurrentAddRemove exercises repeated add/remove of subject loops
// from multiple goroutines to validate thread-safety, absence of deadlocks, and clean shutdown.
func TestWorkerConsumer_ConcurrentAddRemove(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "CONC",
		Subjects:  []string{"conc.*.*"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	cfg := WorkerConsumerConfig{
		StreamName:      "CONC",
		ConsumerPrefix:  "wc2",
		SubjectTemplate: "conc.{{.PartitionID}}",
		BatchSize:       1,
	}
	require.NoError(t, cfg.SetDefaults())

	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)

	wc := &WorkerConsumer{
		js:              js,
		config:          cfg,
		logger:          cfg.Logger,
		handler:         messageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil }),
		subjects:        make(map[string]*partitionConsumer),
		iterFactory:     defaultIterFactory,
		subjectTemplate: tmpl,
	}
	t.Cleanup(func() { _ = wc.Close(context.Background()) })

	// Two partitions we will rapidly add/remove
	pA := []types.Partition{{Keys: []string{"a", "1"}}}
	pB := []types.Partition{{Keys: []string{"b", "1"}}}

	var wg sync.WaitGroup

	// Goroutine 1 rapidly toggles partition A
	wg.Go(func() {
		for range 10 {
			if err := wc.UpdateWorkerConsumer(ctx, "w1", pA); err != nil {
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					return
				}
				require.NoError(t, err)
			}
			time.Sleep(10 * time.Millisecond)
			if err := wc.UpdateWorkerConsumer(ctx, "w1", nil); err != nil {
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					return
				}
				require.NoError(t, err)
			}
		}
	})

	// Goroutine 2 rapidly toggles partition B
	wg.Go(func() {
		for range 10 {
			if err := wc.UpdateWorkerConsumer(ctx, "w1", pB); err != nil {
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					return
				}
				require.NoError(t, err)
			}
			time.Sleep(10 * time.Millisecond)
			if err := wc.UpdateWorkerConsumer(ctx, "w1", nil); err != nil {
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					return
				}
				require.NoError(t, err)
			}
		}
	})

	// Wait for completion and ensure Close succeeds quickly
	wg.Wait()
	if err := wc.Close(ctx); err != nil {
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			require.NoError(t, err)
		}
	}
}

// TestWorkerConsumer_FlipSetsWithClose flips between multiple subject sets while
// invoking Close() mid-flip to stress lifecycle edges and ensure no deadlocks or races.
func TestWorkerConsumer_FlipSetsWithClose(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "FLIP",
		Subjects:  []string{"flip.*.*"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	cfg := WorkerConsumerConfig{
		StreamName:      "FLIP",
		ConsumerPrefix:  "wc2",
		SubjectTemplate: "flip.{{.PartitionID}}",
		BatchSize:       1,
	}
	require.NoError(t, cfg.SetDefaults())

	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)

	wc := &WorkerConsumer{
		js:              js,
		config:          cfg,
		logger:          cfg.Logger,
		handler:         messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error { return nil }),
		subjects:        make(map[string]*partitionConsumer),
		iterFactory:     defaultIterFactory,
		subjectTemplate: tmpl,
	}
	// Ensure cleanup even if the test fails earlier
	t.Cleanup(func() { _ = wc.Close(context.Background()) })

	// handler is stored on wc; no local handler needed

	// Define several subject sets to flip between
	set1 := []types.Partition{{Keys: []string{"a", "1"}}, {Keys: []string{"b", "1"}}}
	set2 := []types.Partition{{Keys: []string{"c", "1"}}, {Keys: []string{"d", "1"}}}
	set3 := []types.Partition{{Keys: []string{"a", "1"}}, {Keys: []string{"c", "1"}}}
	sets := [][]types.Partition{set1, set2, set3}

	flipCtx, flipCancel := context.WithCancel(ctx)
	t.Cleanup(flipCancel)

	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		idx := 0
		for range 20 {
			select {
			case <-flipCtx.Done():
				return
			default:
			}
			if err := wc.UpdateWorkerConsumer(flipCtx, "w1", sets[idx]); err != nil {
				// During Close() races, transient errors (e.g., context canceled) are acceptable
				t.Logf("update error during flip: %v", err)
			}
			idx = (idx + 1) % len(sets)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// Allow flipper to start and be mid-flight
	time.Sleep(50 * time.Millisecond)

	// Call Close while flipper is likely mid-update
	closeCtx, closeCancel := context.WithTimeout(ctx, 2*time.Second)
	require.NoError(t, wc.Close(closeCtx))
	closeCancel()

	// Stop flipper and wait; it may have attempted updates after Close but should not deadlock
	flipCancel()
	<-doneCh

	// Idempotent Close should still succeed
	closeCtx2, closeCancel2 := context.WithTimeout(ctx, 2*time.Second)
	require.NoError(t, wc.Close(closeCtx2))
	closeCancel2()

	// Verify internal map is empty
	wc.mu.RLock()
	defer wc.mu.RUnlock()
	require.Equal(t, 0, len(wc.subjects))
}
