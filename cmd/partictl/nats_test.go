package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/provision"
	"github.com/nats-io/nats.go/jetstream"
)

// startTestNATS starts an embedded NATS server and returns the client URL
// for use with the CLI flags.
func startTestNATS(t *testing.T) string {
	t.Helper()
	_, nc := partitest.StartEmbeddedNATS(t)
	return nc.ConnectedUrl()
}

// startTestNATSWithJS starts an embedded NATS server and returns both the URL
// and a JetStream context for direct provisioning in tests.
func startTestNATSWithJS(t *testing.T) (string, jetstream.JetStream) {
	t.Helper()
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("create jetstream: %v", err)
	}
	return nc.ConnectedUrl(), js
}

// runWithNATS calls run() injecting the -server flag pointing to the
// embedded NATS server. It returns stdout and the exit code; stderr is
// discarded (callers that need stderr should call run() directly).
func runWithNATS(t *testing.T, url string, args ...string) (stdout string, code int) {
	t.Helper()
	// Splice -server after the subcommand name.
	if len(args) == 0 {
		var outBuf, errBuf bytes.Buffer
		code = run(args, &outBuf, &errBuf)

		return outBuf.String(), code
	}
	subcmd := args[0]
	rest := args[1:]
	full := append([]string{subcmd, "-server", url}, rest...)
	var outBuf, errBuf bytes.Buffer
	code = run(full, &outBuf, &errBuf)

	return outBuf.String(), code
}

// runWithNATSBytes is like runWithNATS but returns stdout as []byte.
// Used for byte-identity assertions (bytes.Equal).
func runWithNATSBytes(t *testing.T, url string, args ...string) (stdout []byte, code int) {
	t.Helper()
	if len(args) == 0 {
		var outBuf, errBuf bytes.Buffer
		code = run(args, &outBuf, &errBuf)

		return outBuf.Bytes(), code
	}
	subcmd := args[0]
	rest := args[1:]
	full := append([]string{subcmd, "-server", url}, rest...)
	var outBuf, errBuf bytes.Buffer
	code = run(full, &outBuf, &errBuf)

	return outBuf.Bytes(), code
}

// --- view tests ---

func TestView_EmptyServer(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)
	stdout, code := runWithNATS(t, url, "view")
	if code != ExitOK {
		t.Errorf("view empty server = %d, want 0", code)
	}
	if !strings.Contains(stdout, "no Parti-managed resources found") {
		t.Errorf("expected 'no Parti-managed resources found': %q", stdout)
	}
}

func TestView_EmptyServer_JSON(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)
	stdout, code := runWithNATS(t, url, "view", "-json")
	if code != ExitOK {
		t.Errorf("view empty server -json = %d, want 0", code)
	}
	// Must have apiVersion and kind.
	if !strings.Contains(stdout, `"apiVersion"`) {
		t.Errorf("missing apiVersion: %q", stdout)
	}
	if !strings.Contains(stdout, `"parti.io/provision/v1"`) {
		t.Errorf("missing provision/v1: %q", stdout)
	}
	if !strings.Contains(stdout, `"kind"`) {
		t.Errorf("missing kind: %q", stdout)
	}
	if !strings.Contains(stdout, `"Snapshot"`) {
		t.Errorf("missing Snapshot kind: %q", stdout)
	}

	// Unmarshal and verify shape.
	var snap provision.Snapshot
	if err := json.Unmarshal([]byte(stdout), &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if snap.APIVersion != provision.APIVersionProvisionV1 {
		t.Errorf("apiVersion = %q, want %q", snap.APIVersion, provision.APIVersionProvisionV1)
	}
	if snap.Kind != provision.KindSnapshot {
		t.Errorf("kind = %q, want %q", snap.Kind, provision.KindSnapshot)
	}
	if snap.ObservedAt.IsZero() {
		t.Error("observedAt should be non-zero in JSON output")
	}
}

func TestView_WithMarkedBuckets(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)

	ctx := context.Background()

	// Create a marked bucket (control-plane:id).
	marker := provision.BuildMarker(provision.ComponentControlPlaneID, "test-instance")
	_, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:   "test-stableid",
		Storage:  jetstream.FileStorage,
		History:  1,
		Metadata: marker,
	})
	if err != nil {
		t.Fatalf("create KV: %v", err)
	}

	// Create an unmarked bucket (should not appear in view).
	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  "unrelated-bucket",
		Storage: jetstream.MemoryStorage,
		History: 1,
	})
	if err != nil {
		t.Fatalf("create unmarked KV: %v", err)
	}

	stdout, code := runWithNATS(t, url, "view")
	if code != ExitOK {
		t.Errorf("view with marked buckets = %d, want 0", code)
	}
	if !strings.Contains(stdout, "test-stableid") {
		t.Errorf("expected test-stableid in output: %q", stdout)
	}
	if strings.Contains(stdout, "unrelated-bucket") {
		t.Errorf("unmarked bucket should not appear in output: %q", stdout)
	}
}

func TestView_InstanceFilter(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	ctx := context.Background()

	// Create a bucket stamped with instance "prod".
	prodMarker := provision.BuildMarker(provision.ComponentControlPlaneID, "prod")
	_, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:   "prod-stableid",
		Storage:  jetstream.FileStorage,
		History:  1,
		Metadata: prodMarker,
	})
	if err != nil {
		t.Fatalf("create prod KV: %v", err)
	}

	// Create a bucket stamped with instance "staging".
	stagingMarker := provision.BuildMarker(provision.ComponentControlPlaneID, "staging")
	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:   "staging-stableid",
		Storage:  jetstream.FileStorage,
		History:  1,
		Metadata: stagingMarker,
	})
	if err != nil {
		t.Fatalf("create staging KV: %v", err)
	}

	// view -instance=prod should only return prod bucket.
	stdout, code := runWithNATS(t, url, "view", "-instance", "prod")
	if code != ExitOK {
		t.Errorf("view -instance=prod = %d, want 0", code)
	}
	if !strings.Contains(stdout, "prod-stableid") {
		t.Errorf("expected prod-stableid: %q", stdout)
	}
	if strings.Contains(stdout, "staging-stableid") {
		t.Errorf("staging-stableid should not appear: %q", stdout)
	}
}

// --- plan tests ---

func TestPlan_EmptyServer_JSON(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)
	stdout, code := runWithNATS(t, url, "plan", "-f", "testdata/minimal.yaml", "-json")
	if code != ExitOK {
		t.Errorf("plan empty server = %d, want 0", code)
	}

	var plan provision.PlanResult
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
		t.Fatalf("unmarshal plan: %v", err)
	}
	if plan.APIVersion != provision.APIVersionProvisionV1 {
		t.Errorf("apiVersion = %q, want %q", plan.APIVersion, provision.APIVersionProvisionV1)
	}
	if plan.Kind != provision.KindPlan {
		t.Errorf("kind = %q, want %q", plan.Kind, provision.KindPlan)
	}
	// Against an empty server, minimal.yaml should produce 4 create-kv actions.
	if len(plan.Actions) != 4 {
		t.Errorf("expected 4 actions, got %d: %+v", len(plan.Actions), plan.Actions)
	}
	for _, a := range plan.Actions {
		if a.Kind != provision.ActionCreateKV {
			t.Errorf("expected all actions to be create-kv, got %q", a.Kind)
		}
	}
}

func TestPlan_FailOnDrift_NoDrift(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	ctx := context.Background()

	// Pre-create the bucket properly with the Parti marker so there's no drift.
	cfg, err := loadConfig("testdata/minimal.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	_, err = provision.Apply(ctx, js, cfg)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	// plan -fail-on-drift with no drift should exit 0.
	_, code := runWithNATS(t, url, "plan",
		"-f", "testdata/minimal.yaml",
		"-fail-on-drift",
	)
	if code != ExitOK {
		t.Errorf("plan -fail-on-drift no drift = %d, want 0", code)
	}
}

func TestPlan_FailOnDrift_WithDrift(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	ctx := context.Background()

	// Create an unmarked bucket with the same name as what the config expects
	// → adopted drift.
	cfg, err := loadConfig("testdata/minimal.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfgN, err := provision.ApplyDefaults(cfg)
	if err != nil {
		t.Fatalf("apply defaults: %v", err)
	}
	// Create without marker.
	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  cfgN.ControlPlane.StableIDBucket,
		Storage: jetstream.FileStorage,
		History: 1,
	})
	if err != nil {
		t.Fatalf("create unmarked bucket: %v", err)
	}

	// plan -fail-on-drift should exit 2.
	_, code := runWithNATS(t, url, "plan",
		"-f", "testdata/minimal.yaml",
		"-fail-on-drift",
	)
	if code != ExitDrift {
		t.Errorf("plan -fail-on-drift with drift = %d, want %d (ExitDrift)", code, ExitDrift)
	}
}

func TestPlan_FailOnDrift_WithoutFlag(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	ctx := context.Background()

	// Same adopted-drift setup as above.
	cfg, err := loadConfig("testdata/minimal.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfgN, err := provision.ApplyDefaults(cfg)
	if err != nil {
		t.Fatalf("apply defaults: %v", err)
	}
	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  cfgN.ControlPlane.StableIDBucket,
		Storage: jetstream.FileStorage,
		History: 1,
	})
	if err != nil {
		t.Fatalf("create unmarked bucket: %v", err)
	}

	// Without -fail-on-drift, should exit 0 even with drift.
	_, code := runWithNATS(t, url, "plan", "-f", "testdata/minimal.yaml")
	if code != ExitOK {
		t.Errorf("plan without -fail-on-drift = %d, want 0", code)
	}
}

// --- apply tests ---

func TestApply_DryRun_ByteIdenticalToPlan(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)

	// plan -json and apply -dry-run -json must produce byte-identical output.
	// This is the literal byte-identity check from the spec (bytes.Equal, not string comparison).
	planOut, planCode := runWithNATSBytes(t, url, "plan", "-f", "testdata/minimal.yaml", "-json")
	if planCode != ExitOK {
		t.Fatalf("plan -json = %d", planCode)
	}

	applyOut, applyCode := runWithNATSBytes(t, url, "apply",
		"-f", "testdata/minimal.yaml", "-json", "-dry-run")
	if applyCode != ExitOK {
		t.Fatalf("apply -dry-run -json = %d", applyCode)
	}

	// Literal byte-identity check from the spec: plan -json and apply -dry-run -json
	// must produce the exact same bytes, not just equal strings.
	if !bytes.Equal(planOut, applyOut) {
		t.Errorf("plan -json and apply -dry-run -json are not byte-identical\nplan:\n%s\napply:\n%s",
			planOut, applyOut)
	}
}

func TestApply_DryRun_ByteIdentical_MultipleRuns(t *testing.T) {
	// Verify byte-identity holds across two separate plan runs (deterministic).
	t.Parallel()
	url := startTestNATS(t)

	planOut1, code1 := runWithNATS(t, url, "plan", "-f", "testdata/minimal.yaml", "-json")
	planOut2, code2 := runWithNATS(t, url, "plan", "-f", "testdata/minimal.yaml", "-json")
	if code1 != ExitOK || code2 != ExitOK {
		t.Fatalf("plan runs failed: %d %d", code1, code2)
	}
	if planOut1 != planOut2 {
		t.Errorf("plan output is not deterministic:\nrun1:\n%s\nrun2:\n%s", planOut1, planOut2)
	}
}

func TestApply_CreatesResources(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)

	stdout, code := runWithNATS(t, url, "apply",
		"-f", "testdata/minimal.yaml", "-json")
	if code != ExitOK {
		t.Errorf("apply = %d, want 0", code)
	}

	var report provision.Report
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if report.APIVersion != provision.APIVersionProvisionV1 {
		t.Errorf("apiVersion = %q, want %q", report.APIVersion, provision.APIVersionProvisionV1)
	}
	if report.Kind != provision.KindReport {
		t.Errorf("kind = %q, want %q", report.Kind, provision.KindReport)
	}
	if len(report.Executed) == 0 {
		t.Error("expected executed actions, got none")
	}
	if report.Aborted {
		t.Error("report.Aborted should be false")
	}

	// Re-run plan: should show no actions (everything was created).
	planOut, planCode := runWithNATS(t, url, "plan",
		"-f", "testdata/minimal.yaml", "-json")
	if planCode != ExitOK {
		t.Errorf("second plan = %d, want 0", planCode)
	}
	var planResult provision.PlanResult
	if err := json.Unmarshal([]byte(planOut), &planResult); err != nil {
		t.Fatalf("unmarshal plan: %v", err)
	}
	if len(planResult.Actions) != 0 {
		t.Errorf("expected no actions after apply, got %d", len(planResult.Actions))
	}
}

func TestApply_Idempotent(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)

	// First apply.
	_, code := runWithNATS(t, url, "apply", "-f", "testdata/minimal.yaml")
	if code != ExitOK {
		t.Fatalf("first apply = %d", code)
	}

	// Second apply should be a no-op (no errors).
	stdout, code := runWithNATS(t, url, "apply", "-f", "testdata/minimal.yaml", "-json")
	if code != ExitOK {
		t.Errorf("second apply = %d, want 0", code)
	}
	var report provision.Report
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if len(report.Errors) != 0 {
		t.Errorf("second apply had errors: %+v", report.Errors)
	}
}

// --- golden JSON output tests ---
// These tests validate the JSON envelope structure (apiVersion/kind) by
// parsing the output rather than byte-comparing against a file, because
// Snapshot.ObservedAt varies per run. For PlanResult (no timestamp), we
// assert structural determinism.

func TestGolden_ViewJSON_SnapshotEnvelope(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	ctx := context.Background()

	// Pre-create a stamped bucket.
	marker := provision.BuildMarker(provision.ComponentControlPlaneID, "golden")
	_, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:   "golden-stableid",
		Storage:  jetstream.FileStorage,
		History:  1,
		Metadata: marker,
	})
	if err != nil {
		t.Fatalf("create KV: %v", err)
	}

	stdout, code := runWithNATS(t, url, "view", "-json")
	if code != ExitOK {
		t.Fatalf("view -json = %d", code)
	}

	var snap provision.Snapshot
	if err := json.Unmarshal([]byte(stdout), &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if snap.APIVersion != provision.APIVersionProvisionV1 {
		t.Errorf("apiVersion = %q, want %q", snap.APIVersion, provision.APIVersionProvisionV1)
	}
	if snap.Kind != provision.KindSnapshot {
		t.Errorf("kind = %q, want %q", snap.Kind, provision.KindSnapshot)
	}

	// ObservedAt should be set and recent.
	if snap.ObservedAt.IsZero() {
		t.Error("observedAt should not be zero")
	}
	if time.Since(snap.ObservedAt) > 30*time.Second {
		t.Errorf("observedAt looks stale: %v", snap.ObservedAt)
	}

	found := false
	for _, b := range snap.ControlPlane {
		if b.Bucket == "golden-stableid" {
			found = true
			if b.Instance != "golden" {
				t.Errorf("instance = %q, want %q", b.Instance, "golden")
			}
		}
	}
	if !found {
		t.Errorf("golden-stableid not found in ControlPlane: %+v", snap.ControlPlane)
	}
}

func TestGolden_PlanJSON_Deterministic(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)

	// Run plan twice; both runs must produce identical JSON bytes.
	out1, code1 := runWithNATS(t, url, "plan", "-f", "testdata/minimal.yaml", "-json")
	out2, code2 := runWithNATS(t, url, "plan", "-f", "testdata/minimal.yaml", "-json")

	if code1 != ExitOK || code2 != ExitOK {
		t.Fatalf("plan failed: code1=%d code2=%d", code1, code2)
	}
	if out1 != out2 {
		t.Errorf("plan is not deterministic\nrun1:\n%s\nrun2:\n%s", out1, out2)
	}
}

// --- validate -live tests ---

func TestValidateLive_OK(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)
	stdout, code := runWithNATS(t, url, "validate",
		"-f", "testdata/minimal.yaml", "-live")
	if code != ExitOK {
		t.Errorf("validate -live = %d, want 0", code)
	}
	if !strings.Contains(stdout, "ok") {
		t.Errorf("expected 'ok': %q", stdout)
	}
}

func TestValidateLive_Unreachable(t *testing.T) {
	t.Parallel()
	_, _, code := runArgs("validate",
		"-f", "testdata/minimal.yaml",
		"-live",
		"-server", "nats://127.0.0.1:1",
		"-timeout", "500ms",
	)
	if code != ExitNATS {
		t.Errorf("validate -live unreachable = %d, want %d (ExitNATS)", code, ExitNATS)
	}
}

// --- NATS connect with nats.Connect error → exit 4 ---

func TestNATSConnect_ErrorExitCode(t *testing.T) {
	t.Parallel()
	_, _, code := runArgs("view",
		"-server", "nats://127.0.0.1:1", // guaranteed refused
		"-timeout", "500ms",
	)
	if code != ExitNATS {
		t.Errorf("NATS connect failure = %d, want %d (ExitNATS)", code, ExitNATS)
	}
}
