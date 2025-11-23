package subscription

import (
	"context"
	"strings"
	"testing"
	"text/template"

	"github.com/arloliu/parti/types"
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
		subjects:        make(map[string]*subjectLoop),
		subjectTemplate: tmpl,
	}
	wc.workerID = "w1"
	wc.config.AllowWorkerIDChange = false

	h := MessageHandlerFunc(func(ctx context.Context, _ jetstream.Msg) error { return nil })

	// Call Update with a different workerID and no partitions; expect early ErrWorkerIDMutation
	wc.handler = h
	err = wc.UpdateWorkerConsumer(context.Background(), "w2", nil)
	require.ErrorIs(t, err, ErrWorkerIDMutation)
}
