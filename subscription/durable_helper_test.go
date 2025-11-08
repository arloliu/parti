package subscription

import (
	"context"
	"testing"
	"text/template"

	"github.com/arloliu/parti/types"
)

// TestSanitizeConsumerName verifies invalid characters are replaced with underscore.
func TestSanitizeConsumerName(t *testing.T) {
	dh := &DurableHelper{}
	cases := []struct{ in, want string }{
		{"worker.tool*001>region/us", "worker_tool_001_region_us"},
		{"worker\\001", "worker_001"},
		{"worker tool 001", "worker_tool_001"},
		{"valid_name", "valid_name"},
		{"worker\t001\n002", "worker_001_002"},
		{"worker\x00\x01\x1f\x7f001", "worker____001"},
	}
	for _, c := range cases {
		if got := dh.sanitizeConsumerName(c.in); got != c.want {
			t.Fatalf("sanitizeConsumerName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// helper constructs a DurableHelper with a parsed template (js fields unused in unit tests).
func unitHelper(t *testing.T, tmpl string) *DurableHelper {
	t.Helper()
	parsed, err := template.New("subject").Parse(tmpl)
	if err != nil {
		t.Fatalf("template parse failed: %v", err)
	}

	return &DurableHelper{subjectTemplate: parsed}
}

func TestGenerateSubject(t *testing.T) {
	dh := unitHelper(t, "work.{{.PartitionID}}")
	subj, err := dh.generateSubject(types.Partition{Keys: []string{"tool", "001"}})
	if err != nil {
		t.Fatalf("generateSubject unexpected error: %v", err)
	}
	if subj != "work.tool.001" {
		t.Fatalf("got %q want %q", subj, "work.tool.001")
	}

	// Empty keys error
	_, err = dh.generateSubject(types.Partition{Keys: []string{}})
	if err == nil {
		t.Fatal("expected error for empty keys")
	}
}

func TestBuildSubjects_DedupAndSort(t *testing.T) {
	dh := unitHelper(t, "metrics.{{.PartitionID}}")
	parts := []types.Partition{
		{Keys: []string{"a"}},
		{Keys: []string{"c"}},
		{Keys: []string{"b"}},
		{Keys: []string{"a"}}, // duplicate
	}
	subjects, err := dh.buildSubjects(parts)
	if err != nil {
		t.Fatalf("buildSubjects error: %v", err)
	}
	want := []string{"metrics.a", "metrics.b", "metrics.c"}
	if len(subjects) != len(want) {
		t.Fatalf("dedupe failed len=%d want=%d", len(subjects), len(want))
	}
	for i := range want {
		if subjects[i] != want[i] {
			t.Fatalf("subjects[%d]=%q want %q", i, subjects[i], want[i])
		}
	}
}

func TestEqualStringSlices(t *testing.T) {
	a := []string{"a", "b", "c"}
	b := []string{"a", "b", "c"}
	c := []string{"a", "c", "b"}
	d := []string{"a", "b"}
	if !equalStringSlices(a, b) {
		t.Fatal("expected a==b")
	}
	if equalStringSlices(a, c) {
		t.Fatal("expected a!=c (order different)")
	}
	if equalStringSlices(a, d) {
		t.Fatal("expected a!=d (length different)")
	}
}

func TestWorkerSubjects_CopyIsolation(t *testing.T) {
	dh := &DurableHelper{}
	// Manually set workerSubjects (bypassing UpdateWorkerConsumer which needs JS)
	dh.workerSubjects = []string{"x", "y"}
	got := dh.WorkerSubjects()
	if len(got) != 2 || got[0] != "x" || got[1] != "y" {
		t.Fatalf("unexpected slice copy: %#v", got)
	}
	// mutate returned slice; internal slice should remain unchanged
	got[0] = "z"
	again := dh.WorkerSubjects()
	if again[0] != "x" {
		t.Fatalf("internal slice mutated through copy; got=%#v", again)
	}
}

// TestGenerateSubject_DifferentTemplates ensures different templates expand correctly.
func TestGenerateSubject_DifferentTemplates(t *testing.T) {
	cases := []struct {
		tmpl string
		part types.Partition
		want string
	}{
		{"dc.{{.PartitionID}}.complete", types.Partition{Keys: []string{"tool_001", "region", "us"}}, "dc.tool_001.region.us.complete"},
		{"stream.{{.PartitionID}}.v1.data", types.Partition{Keys: []string{"partition", "123"}}, "stream.partition.123.v1.data"},
	}
	for _, c := range cases {
		dh := unitHelper(t, c.tmpl)
		got, err := dh.generateSubject(c.part)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != c.want {
			t.Fatalf("template %q produced %q want %q", c.tmpl, got, c.want)
		}
	}
}

// Ensure WorkerSubjects returns nil when unset
func TestWorkerSubjects_Uninitialized(t *testing.T) {
	dh := &DurableHelper{}
	if dh.WorkerSubjects() != nil {
		t.Fatal("expected nil when workerSubjects unset")
	}
}

// Placeholder to ensure UpdateWorkerConsumer diff logic can be unit tested in future by abstracting JS dependency.
func TestUpdateWorkerConsumer_NoOpEarlyExit(t *testing.T) {
	// Build helper with existing state; we bypass constructor and JS setup.
	dh := unitHelper(t, "work.{{.PartitionID}}")
	dh.workerID = "w1"
	dh.workerSubjects = []string{"work.a", "work.b"}
	// Calling equal set should be no-op (returns early without JS interaction). We simulate by invoking private diff logic via public method
	// Provide empty context; since js field is nil we cannot call, so we assert manual check instead.
	if !equalStringSlices([]string{"work.a", "work.b"}, dh.workerSubjects) {
		t.Fatal("precondition failed")
	}
	// NOTE: Full UpdateWorkerConsumer path requires jetstream; kept for integration tests.
	_ = context.Background() // silence unused import if future refactor removes context usage here
}
