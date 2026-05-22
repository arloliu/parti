package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arloliu/parti/v2/provision"
	"github.com/nats-io/nats.go/jetstream"
)

// writeStreamEnv writes a parti-env.yaml with instance "prod" and
// the oneStreamBlock streams section, returning its path.
func writeStreamEnv(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "env.yaml")
	doc := "apiVersion: parti.io/v1\n" +
		"instance: prod\n" +
		oneStreamBlock
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write stream config: %v", err)
	}

	return path
}

// writeFullEnvWithStream writes a parti-env.yaml that includes a controlPlane
// block, a partitionSource block, and the given streams block.
func writeFullEnvWithStream(t *testing.T, streamsBlock string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "env.yaml")
	doc := "apiVersion: parti.io/v1\n" +
		"controlPlane:\n" +
		"  workerIdTtl: 75s\n" +
		"  electionTimeout: 10s\n" +
		"  heartbeatTtl: 15s\n" +
		streamsBlock
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write full env with stream: %v", err)
	}

	return path
}

// createMarkedStream creates an application stream with the Parti ownership
// marker directly on the NATS server for use in tests.
func createMarkedStream(t *testing.T, js jetstream.JetStream, name string, subjects []string, instance string) {
	t.Helper()
	ctx := context.Background()
	marker := provision.BuildMarker(provision.ComponentStream, instance)
	_, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     name,
		Subjects: subjects,
		Metadata: marker,
	})
	if err != nil {
		t.Fatalf("create marked stream %q: %v", name, err)
	}
}

const oneStreamBlock = "streams:\n" +
	"  - name: orders\n" +
	"    subjects: [\"orders.>\"]\n"

// --- stream view tests ---

func TestStream_View_ListsMarkedStream(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	createMarkedStream(t, js, "orders", []string{"orders.>"}, "prod")

	stdout, _, code := runArgs("stream", "view", "-server", url)
	if code != ExitOK {
		t.Fatalf("stream view = %d, want 0", code)
	}
	if !strings.Contains(stdout, "orders") {
		t.Errorf("expected stream 'orders' in output: %q", stdout)
	}
	if !strings.Contains(stdout, "APPLICATION STREAMS") {
		t.Errorf("expected APPLICATION STREAMS header: %q", stdout)
	}
}

func TestStream_View_EmptyServer(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)

	stdout, _, code := runArgs("stream", "view", "-server", url)
	if code != ExitOK {
		t.Fatalf("stream view empty server = %d, want 0", code)
	}
	if !strings.Contains(stdout, "no Parti-managed resources found") {
		t.Errorf("expected 'no Parti-managed resources found': %q", stdout)
	}
}

func TestStream_View_JSON_APIVersion(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)

	stdout, _, code := runArgs("stream", "view", "-server", url, "-json")
	if code != ExitOK {
		t.Fatalf("stream view -json = %d, want 0", code)
	}
	if !strings.Contains(stdout, `"parti.io/provision/v1"`) {
		t.Errorf("JSON output missing apiVersion: %q", stdout)
	}

	var snap provision.Snapshot
	if err := json.Unmarshal([]byte(stdout), &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if snap.APIVersion != provision.APIVersionProvisionV1 {
		t.Errorf("apiVersion = %q, want %q", snap.APIVersion, provision.APIVersionProvisionV1)
	}
}

// TestStream_View_WithFile_InstanceScopedInventory verifies that with two
// marked streams in one instance and a config naming only one, stream view -f
// lists BOTH (instance-scoped inventory, not a name filter).
func TestStream_View_WithFile_InstanceScopedInventory(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)

	// Create two marked streams in the same instance.
	createMarkedStream(t, js, "orders", []string{"orders.>"}, "prod")
	createMarkedStream(t, js, "events", []string{"events.>"}, "prod")

	// Config names only one stream, but view -f should show all streams in
	// the instance (instance-scoped inventory, not a name filter).
	cfg := writeStreamEnv(t)

	stdout, _, code := runArgs("stream", "view", "-server", url, "-f", cfg)
	if code != ExitOK {
		t.Fatalf("stream view -f = %d, want 0", code)
	}
	// Both streams must appear.
	if !strings.Contains(stdout, "orders") {
		t.Errorf("expected 'orders' in output: %q", stdout)
	}
	if !strings.Contains(stdout, "events") {
		t.Errorf("expected 'events' (unlisted-by-config but same instance) in output: %q", stdout)
	}
}

func TestStream_View_WithFile_InstanceIgnoredWarning(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)
	cfg := writeStreamEnv(t)

	_, stderr, _ := runArgs("stream", "view", "-server", url, "-f", cfg,
		"-instance", "other", "-timeout", "100ms")
	if !strings.Contains(stderr, "-instance ignored") {
		t.Errorf("expected '-instance ignored' warning: %q", stderr)
	}
}

func TestStream_View_InstanceFilter(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)

	// Two streams in different instances.
	createMarkedStream(t, js, "orders", []string{"orders.>"}, "prod")
	createMarkedStream(t, js, "events", []string{"events.>"}, "staging")

	stdout, _, code := runArgs("stream", "view", "-server", url, "-instance", "prod")
	if code != ExitOK {
		t.Fatalf("stream view -instance=prod = %d, want 0", code)
	}
	if !strings.Contains(stdout, "orders") {
		t.Errorf("expected 'orders' (prod instance): %q", stdout)
	}
	if strings.Contains(stdout, "events") {
		t.Errorf("'events' (staging instance) should not appear: %q", stdout)
	}
}

// --- stream plan tests ---

func TestStream_Plan_NoDrift(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	ctx := context.Background()

	cfg := writeStreamEnv(t)

	// Pre-apply so there is no drift (stream is created and marked).
	pcfg, err := loadConfig(cfg)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	streamOnly := pcfg
	streamOnly.ControlPlane = nil
	streamOnly.PartitionSource = nil
	streamOnly.DynamicConsumers = nil
	if _, err := provision.Apply(ctx, js, streamOnly); err != nil {
		t.Fatalf("pre-apply: %v", err)
	}

	stdout, _, code := runArgs("stream", "plan", "-server", url, "-f", cfg)
	if code != ExitOK {
		t.Fatalf("stream plan no drift = %d, want 0", code)
	}
	// After a clean apply the stream is marked and converged. Plan produces
	// only informational drift (no actions, no mutable/immutable drift), so
	// no create-stream action appears.
	if strings.Contains(stdout, "create-stream") {
		t.Errorf("expected no create-stream action after apply: %q", stdout)
	}
}

func TestStream_Plan_FailOnDrift(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	ctx := context.Background()

	// Create an UNMARKED stream: this produces "adopted" drift (severity
	// "adopted") which hasDrift recognises, making -fail-on-drift exit 2.
	_, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "orders",
		Subjects: []string{"orders.>"},
	})
	if err != nil {
		t.Fatalf("create unmarked stream: %v", err)
	}

	cfg := writeStreamEnv(t)

	_, _, code := runArgs("stream", "plan", "-server", url, "-f", cfg,
		"-policy", "adopt", "-fail-on-drift")
	if code != ExitDrift {
		t.Errorf("stream plan -fail-on-drift with adopted drift = %d, want %d (ExitDrift)", code, ExitDrift)
	}
}

func TestStream_Plan_NoFailOnDrift(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)
	cfg := writeStreamEnv(t)

	// Without -fail-on-drift, should exit 0 even when there are actions.
	_, _, code := runArgs("stream", "plan", "-server", url, "-f", cfg)
	if code != ExitOK {
		t.Errorf("stream plan without -fail-on-drift = %d, want 0", code)
	}
}

func TestStream_Plan_JSON_APIVersion(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)
	cfg := writeStreamEnv(t)

	stdout, _, code := runArgs("stream", "plan", "-server", url, "-f", cfg, "-json")
	if code != ExitOK {
		t.Fatalf("stream plan -json = %d, want 0", code)
	}
	if !strings.Contains(stdout, `"parti.io/provision/v1"`) {
		t.Errorf("JSON output missing apiVersion: %q", stdout)
	}

	var plan provision.PlanResult
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
		t.Fatalf("unmarshal plan: %v", err)
	}
	if plan.APIVersion != provision.APIVersionProvisionV1 {
		t.Errorf("apiVersion = %q, want %q", plan.APIVersion, provision.APIVersionProvisionV1)
	}
}

func TestStream_Plan_ShowsCreateStream(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)
	cfg := writeStreamEnv(t)

	stdout, _, code := runArgs("stream", "plan", "-server", url, "-f", cfg)
	if code != ExitOK {
		t.Fatalf("stream plan = %d, want 0", code)
	}
	if !strings.Contains(stdout, "create-stream") {
		t.Errorf("expected 'create-stream' action: %q", stdout)
	}
}

func TestStream_Plan_PolicyAccepted(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)
	cfg := writeStreamEnv(t)

	_, _, code := runArgs("stream", "plan", "-server", url, "-f", cfg, "-policy", "safe-update")
	if code != ExitOK {
		t.Errorf("stream plan -policy safe-update = %d, want 0", code)
	}
}

func TestStream_Plan_PolicyForceRejected(t *testing.T) {
	t.Parallel()
	cfg := writeStreamEnv(t)

	_, stderr, code := runArgs("stream", "plan", "-f", cfg, "-policy", "force")
	if code != ExitValidation {
		t.Errorf("stream plan -policy force = %d, want %d (ExitValidation)", code, ExitValidation)
	}
	if !strings.Contains(stderr, "force") {
		t.Errorf("expected 'force' in error message: %q", stderr)
	}
}

func TestStream_Plan_BadConfig(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)

	// A config with a missing required controlPlane field.
	path := filepath.Join(t.TempDir(), "bad.yaml")
	bad := "apiVersion: parti.io/v1\n" +
		"controlPlane:\n" +
		"  workerIdTtl: 0s\n" + // invalid: workerIdTtl must be > 0
		oneStreamBlock
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatalf("write bad config: %v", err)
	}

	_, _, code := runArgs("stream", "plan", "-server", url, "-f", path)
	if code != ExitValidation {
		t.Errorf("stream plan bad config = %d, want %d (ExitValidation)", code, ExitValidation)
	}
}

func TestStream_Plan_NoFile(t *testing.T) {
	t.Parallel()
	_, stderr, code := runArgs("stream", "plan")
	if code != ExitValidation {
		t.Errorf("stream plan no -f = %d, want %d", code, ExitValidation)
	}
	if !strings.Contains(stderr, "-f is required") {
		t.Errorf("expected '-f is required': %q", stderr)
	}
}

// --- stream apply tests ---

func TestStream_Apply_CreatesStream(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)
	cfg := writeStreamEnv(t)

	stdout, _, code := runArgs("stream", "apply", "-server", url, "-f", cfg, "-json")
	if code != ExitOK {
		t.Fatalf("stream apply = %d, want 0", code)
	}

	var report provision.Report
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if report.APIVersion != provision.APIVersionProvisionV1 {
		t.Errorf("apiVersion = %q, want %q", report.APIVersion, provision.APIVersionProvisionV1)
	}
	if len(report.Executed) == 0 {
		t.Error("expected executed actions (create-stream), got none")
	}
}

func TestStream_Apply_JSON_APIVersion(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)
	cfg := writeStreamEnv(t)

	stdout, _, code := runArgs("stream", "apply", "-server", url, "-f", cfg, "-json")
	if code != ExitOK {
		t.Fatalf("stream apply -json = %d, want 0", code)
	}
	if !strings.Contains(stdout, `"parti.io/provision/v1"`) {
		t.Errorf("JSON output missing apiVersion: %q", stdout)
	}
}

func TestStream_Apply_DryRunWritesNothing(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)
	cfg := writeStreamEnv(t)

	// dry-run: should emit plan output but write nothing.
	_, _, code := runArgs("stream", "apply", "-server", url, "-f", cfg, "-dry-run")
	if code != ExitOK {
		t.Fatalf("stream apply -dry-run = %d, want 0", code)
	}

	// plan should still show create-stream (nothing was written).
	stdout, _, code := runArgs("stream", "plan", "-server", url, "-f", cfg)
	if code != ExitOK {
		t.Fatalf("stream plan after dry-run = %d, want 0", code)
	}
	if !strings.Contains(stdout, "create-stream") {
		t.Errorf("dry-run must not write; expected create-stream action to remain: %q", stdout)
	}
}

func TestStream_Apply_BadConfig(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)

	path := filepath.Join(t.TempDir(), "bad.yaml")
	bad := "apiVersion: parti.io/v1\n" +
		"controlPlane:\n" +
		"  workerIdTtl: 0s\n" + // invalid
		oneStreamBlock
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatalf("write bad config: %v", err)
	}

	_, _, code := runArgs("stream", "apply", "-server", url, "-f", path)
	if code != ExitValidation {
		t.Errorf("stream apply bad config = %d, want %d (ExitValidation)", code, ExitValidation)
	}
}

func TestStream_Apply_PolicyAccepted(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)
	cfg := writeStreamEnv(t)

	_, _, code := runArgs("stream", "apply", "-server", url, "-f", cfg, "-policy", "safe-update")
	if code != ExitOK {
		t.Errorf("stream apply -policy safe-update = %d, want 0", code)
	}
}

func TestStream_Apply_PolicyForceRejected(t *testing.T) {
	t.Parallel()
	cfg := writeStreamEnv(t)

	_, stderr, code := runArgs("stream", "apply", "-f", cfg, "-policy", "force")
	if code != ExitValidation {
		t.Errorf("stream apply -policy force = %d, want %d (ExitValidation)", code, ExitValidation)
	}
	if !strings.Contains(stderr, "force") {
		t.Errorf("expected 'force' in error message: %q", stderr)
	}
}

func TestStream_Apply_NoFile(t *testing.T) {
	t.Parallel()
	_, stderr, code := runArgs("stream", "apply")
	if code != ExitValidation {
		t.Errorf("stream apply no -f = %d, want %d", code, ExitValidation)
	}
	if !strings.Contains(stderr, "-f is required") {
		t.Errorf("expected '-f is required': %q", stderr)
	}
}

// TestStream_Apply_ServerRejectedUpdate_ExitsRuntime verifies that a
// server-rejected UpdateStream (subject overlap with another stream) causes
// stream apply to exit 1 (ExitRuntime), not 0 or 3.
//
// Setup: pre-create a "claimer" stream owning "shared.>" and a marked "orders"
// stream owning "orders.>". The config then requests that "orders" adopt
// "shared.>" as its subject — the server rejects the overlap at UpdateStream
// time, surfacing a ResourceError → classifyError → ExitRuntime.
func TestStream_Apply_ServerRejectedUpdate_ExitsRuntime(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	ctx := context.Background()

	// Create the "claimer" stream that owns "shared.>".
	_, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "claimer",
		Subjects: []string{"shared.>"},
	})
	if err != nil {
		t.Fatalf("create claimer stream: %v", err)
	}

	// Create the marked "orders" stream owning "orders.>".
	createMarkedStream(t, js, "orders", []string{"orders.>"}, "prod")

	// Config requests "orders" to adopt "shared.>" — overlaps "claimer".
	path := filepath.Join(t.TempDir(), "overlap.yaml")
	doc := "apiVersion: parti.io/v1\n" +
		"instance: prod\n" +
		"streams:\n" +
		"  - name: orders\n" +
		"    subjects: [\"shared.>\"]\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write overlap config: %v", err)
	}

	_, _, code := runArgs("stream", "apply", "-server", url, "-f", path, "-policy", "safe-update")
	if code != ExitRuntime {
		t.Errorf("stream apply with server-rejected update = %d, want %d (ExitRuntime)", code, ExitRuntime)
	}
}

// TestStream_Apply_Idempotent verifies that running apply twice produces no
// errors on the second run (informational only, no actions).
func TestStream_Apply_Idempotent(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)
	cfg := writeStreamEnv(t)

	_, _, code := runArgs("stream", "apply", "-server", url, "-f", cfg)
	if code != ExitOK {
		t.Fatalf("first stream apply = %d", code)
	}

	stdout, _, code := runArgs("stream", "apply", "-server", url, "-f", cfg, "-json")
	if code != ExitOK {
		t.Errorf("second stream apply = %d, want 0", code)
	}
	var report provision.Report
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if len(report.Errors) != 0 {
		t.Errorf("second apply had errors: %+v", report.Errors)
	}
}

// --- top-level plan/apply with streams ---

// TestTopLevel_Plan_WithStreams verifies that top-level `partictl plan` on a
// config with a streams: block includes stream actions.
func TestTopLevel_Plan_WithStreams(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)
	cfg := writeFullEnvWithStream(t, oneStreamBlock)

	stdout, _, code := runArgs("plan", "-server", url, "-f", cfg)
	if code != ExitOK {
		t.Fatalf("top-level plan with streams = %d, want 0", code)
	}
	if !strings.Contains(stdout, "create-stream") {
		t.Errorf("expected 'create-stream' action in top-level plan: %q", stdout)
	}
}

// TestTopLevel_Apply_WithStreams verifies that top-level `partictl apply` on a
// config with a streams: block provisions the stream.
func TestTopLevel_Apply_WithStreams(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)
	cfg := writeFullEnvWithStream(t, oneStreamBlock)

	_, _, code := runArgs("apply", "-server", url, "-f", cfg)
	if code != ExitOK {
		t.Fatalf("top-level apply with streams = %d, want 0", code)
	}

	// Re-plan should include informational finding for the stream (no more
	// create-stream action).
	stdout, _, code := runArgs("plan", "-server", url, "-f", cfg)
	if code != ExitOK {
		t.Errorf("top-level plan after apply = %d, want 0", code)
	}
	// After apply the stream exists; should not have create-stream again.
	if strings.Contains(stdout, "create-stream") {
		t.Errorf("create-stream action after apply: %q", stdout)
	}
}

// --- stream command dispatch ---

func TestStream_NoSubcommand(t *testing.T) {
	t.Parallel()
	_, stderr, code := runArgs("stream")
	if code != ExitValidation {
		t.Errorf("stream with no subcommand = %d, want %d", code, ExitValidation)
	}
	if !strings.Contains(stderr, "subcommand") {
		t.Errorf("expected 'subcommand' in error: %q", stderr)
	}
}

func TestStream_UnknownSubcommand(t *testing.T) {
	t.Parallel()
	_, stderr, code := runArgs("stream", "bogus")
	if code != ExitValidation {
		t.Errorf("stream bogus = %d, want %d", code, ExitValidation)
	}
	if !strings.Contains(stderr, "unknown subcommand") {
		t.Errorf("expected 'unknown subcommand': %q", stderr)
	}
}

func TestStream_Help(t *testing.T) {
	t.Parallel()
	stdout, _, code := runArgs("stream", "help")
	if code != ExitOK {
		t.Errorf("stream help = %d, want 0", code)
	}
	if !strings.Contains(stdout, "partictl stream") {
		t.Errorf("expected 'partictl stream' in help: %q", stdout)
	}
}
