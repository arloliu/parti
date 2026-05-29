package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/provision"
	"github.com/nats-io/nats.go/jetstream"
)

// --- resolvePolicy unit tests ---

func TestResolvePolicy_BothAbsent(t *testing.T) {
	t.Parallel()
	got, ok := resolvePolicy("", "", "apply", "cfg.yaml", &bytes.Buffer{})
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != provision.PolicyWarn {
		t.Errorf("resolvePolicy both absent = %q, want %q", got, provision.PolicyWarn)
	}
}

func TestResolvePolicy_FlagAbsentYAMLPresent(t *testing.T) {
	t.Parallel()
	got, ok := resolvePolicy("", "safe-update", "apply", "cfg.yaml", &bytes.Buffer{})
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != provision.PolicySafeUpdate {
		t.Errorf("resolvePolicy flag absent, yaml=safe-update = %q, want %q", got, provision.PolicySafeUpdate)
	}
}

func TestResolvePolicy_FlagPresentYAMLAbsent(t *testing.T) {
	t.Parallel()
	got, ok := resolvePolicy("adopt", "", "apply", "cfg.yaml", &bytes.Buffer{})
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != provision.PolicyAdopt {
		t.Errorf("resolvePolicy flag=adopt, yaml absent = %q, want %q", got, provision.PolicyAdopt)
	}
}

func TestResolvePolicy_BothPresentEqual(t *testing.T) {
	t.Parallel()
	got, ok := resolvePolicy("warn", "warn", "apply", "cfg.yaml", &bytes.Buffer{})
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != provision.PolicyWarn {
		t.Errorf("resolvePolicy both=warn = %q, want %q", got, provision.PolicyWarn)
	}
}

func TestResolvePolicy_Conflict(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	_, ok := resolvePolicy("safe-update", "warn", "apply", "mycfg.yaml", &buf)
	if ok {
		t.Fatal("expected ok=false on conflict")
	}
	msg := buf.String()
	if !strings.Contains(msg, "--policy=safe-update") {
		t.Errorf("conflict message missing flag value: %q", msg)
	}
	if !strings.Contains(msg, "cfg.policy=warn") {
		t.Errorf("conflict message missing YAML value: %q", msg)
	}
	if !strings.Contains(msg, "mycfg.yaml") {
		t.Errorf("conflict message missing file path: %q", msg)
	}
	if !strings.Contains(msg, "partictl apply:") {
		t.Errorf("conflict message missing subcmd prefix: %q", msg)
	}
}

func TestResolvePolicy_ConflictSubcmdPrefix(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	// plan subcmd
	_, ok := resolvePolicy("adopt", "warn", "plan", "cfg.yaml", &buf)
	if ok {
		t.Fatal("expected ok=false")
	}
	if !strings.Contains(buf.String(), "partictl plan:") {
		t.Errorf("conflict message should use plan prefix: %q", buf.String())
	}
}

// --- validatePolicyFlag unit tests ---

func TestValidatePolicyFlag_Empty(t *testing.T) {
	t.Parallel()
	if !validatePolicyFlag("", "apply", &bytes.Buffer{}) {
		t.Error("empty flag should be valid")
	}
}

func TestValidatePolicyFlag_ValidValues(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"warn", "adopt", "safe-update", "force"} {
		t.Run(v, func(t *testing.T) {
			t.Parallel()
			if !validatePolicyFlag(v, "apply", &bytes.Buffer{}) {
				t.Errorf("--policy=%s should be valid", v)
			}
		})
	}
}

func TestValidatePolicyFlag_ForceAccepted(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if !validatePolicyFlag("force", "apply", &buf) {
		t.Errorf("--policy=force should be accepted, got message: %q", buf.String())
	}
}

func TestValidatePolicyFlag_UnknownRejected(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if validatePolicyFlag("magic", "apply", &buf) {
		t.Error("unknown policy should be rejected")
	}
	if !strings.Contains(buf.String(), "not recognized") {
		t.Errorf("expected 'not recognized' in message: %q", buf.String())
	}
}

// --- Static (no-NATS) CLI tests for --policy conflict paths ---

// TestApply_PolicyConflict_Exit3: --policy=safe-update with cfg.policy=warn → exit 3.
func TestApply_PolicyConflict_Exit3(t *testing.T) {
	t.Parallel()
	_, stderr, code := runArgs("apply",
		"-f", "testdata/policy_warn.yaml",
		"-policy", "safe-update",
	)
	if code != ExitValidation {
		t.Errorf("apply policy conflict = %d, want %d (ExitValidation)", code, ExitValidation)
	}
	if !strings.Contains(stderr, "conflicts with") {
		t.Errorf("expected conflict message in stderr: %q", stderr)
	}
}

// TestPlan_PolicyConflict_Exit3: --policy=adopt with cfg.policy=warn → exit 3.
func TestPlan_PolicyConflict_Exit3(t *testing.T) {
	t.Parallel()
	_, stderr, code := runArgs("plan",
		"-f", "testdata/policy_warn.yaml",
		"-policy", "adopt",
	)
	if code != ExitValidation {
		t.Errorf("plan policy conflict = %d, want %d (ExitValidation)", code, ExitValidation)
	}
	if !strings.Contains(stderr, "conflicts with") {
		t.Errorf("expected conflict message in stderr: %q", stderr)
	}
}

// TestAdopt_PolicyConflict_Exit3: partictl adopt with cfg.policy=warn → exit 3.
func TestAdopt_PolicyConflict_Exit3(t *testing.T) {
	t.Parallel()
	_, stderr, code := runArgs("adopt",
		"-f", "testdata/policy_warn.yaml",
	)
	if code != ExitValidation {
		t.Errorf("adopt with cfg.policy=warn = %d, want %d (ExitValidation)", code, ExitValidation)
	}
	if !strings.Contains(stderr, "conflicts with") {
		t.Errorf("expected conflict message in stderr: %q", stderr)
	}
	if !strings.Contains(stderr, "partictl adopt:") {
		t.Errorf("expected 'partictl adopt:' prefix in stderr: %q", stderr)
	}
}

// TestAdopt_PolicyAdoptInYAML_Proceeds: partictl adopt with cfg.policy=adopt → conflict-free.
// This reaches NATS (which is unreachable), confirming conflict resolution passed and we
// got to the NATS connect step.
func TestAdopt_PolicyAdoptInYAML_Proceeds(t *testing.T) {
	t.Parallel()
	_, _, code := runArgs("adopt",
		"-f", "testdata/policy_adopt.yaml",
		"-server", "nats://127.0.0.1:1",
		"-timeout", "500ms",
	)
	// Should get ExitNATS (conflict passed, NATS unreachable).
	if code != ExitNATS {
		t.Errorf("adopt policy_adopt.yaml = %d, want %d (ExitNATS — conflict-free, NATS unreachable)", code, ExitNATS)
	}
}

// TestAdopt_NoFile: partictl adopt without -f → exit 3.
func TestAdopt_NoFile(t *testing.T) {
	t.Parallel()
	_, stderr, code := runArgs("adopt")
	if code != ExitValidation {
		t.Errorf("adopt no -f = %d, want %d (ExitValidation)", code, ExitValidation)
	}
	if !strings.Contains(stderr, "-f is required") {
		t.Errorf("expected '-f is required' in stderr: %q", stderr)
	}
}

// TestApply_ForceAccepted_ReachesNATS: apply --policy=force is accepted at the
// flag layer and proceeds to the NATS connect step (which fails offline).
func TestApply_ForceAccepted_ReachesNATS(t *testing.T) {
	t.Parallel()
	_, stderr, code := runArgs("apply",
		"-f", "testdata/minimal.yaml",
		"-policy", "force",
		"-server", "nats://127.0.0.1:1",
		"-timeout", "500ms",
	)
	if code != ExitNATS {
		t.Errorf("apply --policy=force = %d, want %d (ExitNATS — accepted, NATS unreachable)", code, ExitNATS)
	}
	if strings.Contains(stderr, "not yet supported") {
		t.Errorf("apply --policy=force must not emit 'not yet supported': %q", stderr)
	}
}

// TestPlan_ForceAccepted_ReachesNATS: plan --policy=force is accepted at the
// flag layer and proceeds to the NATS connect step (which fails offline).
func TestPlan_ForceAccepted_ReachesNATS(t *testing.T) {
	t.Parallel()
	_, stderr, code := runArgs("plan",
		"-f", "testdata/minimal.yaml",
		"-policy", "force",
		"-server", "nats://127.0.0.1:1",
		"-timeout", "500ms",
	)
	if code != ExitNATS {
		t.Errorf("plan --policy=force = %d, want %d (ExitNATS — accepted, NATS unreachable)", code, ExitNATS)
	}
	if strings.Contains(stderr, "not yet supported") {
		t.Errorf("plan --policy=force must not emit 'not yet supported': %q", stderr)
	}
}

// TestPolicy_EmptyYAML_TreatedAsAbsent: policy: "" in YAML is treated as absent;
// --policy=safe-update proceeds (no conflict) → reaches NATS unreachable.
func TestPolicy_EmptyYAML_TreatedAsAbsent(t *testing.T) {
	t.Parallel()
	_, _, code := runArgs("apply",
		"-f", "testdata/policy_empty.yaml",
		"-policy", "safe-update",
		"-server", "nats://127.0.0.1:1",
		"-timeout", "500ms",
	)
	if code != ExitNATS {
		t.Errorf("apply policy_empty.yaml --policy=safe-update = %d, want %d (ExitNATS — no conflict, NATS unreachable)", code, ExitNATS)
	}
}

// TestPolicy_EmptyYAML_AdoptTreatedAsAbsent: policy: "" with adopt command — no conflict.
func TestPolicy_EmptyYAML_AdoptTreatedAsAbsent(t *testing.T) {
	t.Parallel()
	_, _, code := runArgs("adopt",
		"-f", "testdata/policy_empty.yaml",
		"-server", "nats://127.0.0.1:1",
		"-timeout", "500ms",
	)
	if code != ExitNATS {
		t.Errorf("adopt policy_empty.yaml = %d, want %d (ExitNATS — no conflict, NATS unreachable)", code, ExitNATS)
	}
}

// TestApply_SamePolicyFlag_NoConflict: --policy=warn with cfg.policy=warn → no conflict.
func TestApply_SamePolicyFlag_NoConflict(t *testing.T) {
	t.Parallel()
	_, _, code := runArgs("apply",
		"-f", "testdata/policy_warn.yaml",
		"-policy", "warn",
		"-server", "nats://127.0.0.1:1",
		"-timeout", "500ms",
	)
	if code != ExitNATS {
		t.Errorf("apply same policy = %d, want %d (ExitNATS — no conflict, NATS unreachable)", code, ExitNATS)
	}
}

// TestAdopt_UnknownCommand_IsNowKnown: dispatcher recognizes "adopt".
func TestAdopt_UnknownCommand_IsNowKnown(t *testing.T) {
	t.Parallel()
	_, stderr, code := runArgs("adopt")
	// Should not emit "unknown command" — it should emit "-f is required" instead.
	if strings.Contains(stderr, "unknown command") {
		t.Errorf("adopt should be a known command, got: %q", stderr)
	}
	if code != ExitValidation {
		t.Errorf("adopt no args = %d, want %d", code, ExitValidation)
	}
}

// TestHelp_ContainsAdopt: top-level help output lists adopt command.
func TestHelp_ContainsAdopt(t *testing.T) {
	t.Parallel()
	stdout, _, code := runArgs("help")
	if code != ExitOK {
		t.Errorf("help = %d, want 0", code)
	}
	if !strings.Contains(stdout, "adopt") {
		t.Errorf("help output should mention adopt: %q", stdout)
	}
	if !strings.Contains(stdout, "-policy") {
		t.Errorf("help output should mention -policy flag: %q", stdout)
	}
	// adopt should no longer be in the "Deferred commands" line.
	deferred := extractDeferredLine(stdout)
	if strings.Contains(deferred, "adopt") {
		t.Errorf("adopt should not appear in deferred commands: %q", deferred)
	}
}

// extractDeferredLine returns the line containing "Deferred commands" from s.
func extractDeferredLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if strings.Contains(line, "Deferred") {
			return line
		}
	}
	return ""
}

// --- NATS-backed tests for policy behavior ---

// TestApply_PolicySafeUpdate_EmitsUpdateKVWhereWarnDoesNot verifies the
// observable difference between the two policies: against a Parti-marked
// bucket whose TTL has drifted from config, --policy=safe-update emits an
// update-kv action while the default warn policy reports the drift but
// emits no action.
func TestApply_PolicySafeUpdate_EmitsUpdateKVWhereWarnDoesNot(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	ctx := context.Background()

	// Pre-create the stableid bucket, Parti-marked, with a TTL that drifts
	// from minimal.yaml's workerIdTtl (75s). The other control-plane buckets
	// stay absent.
	_, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:   "parti-stableid",
		Storage:  jetstream.FileStorage,
		History:  1,
		TTL:      99 * time.Second,
		Metadata: provision.BuildMarker(provision.ComponentControlPlaneID, ""),
	})
	if err != nil {
		t.Fatalf("create drifted bucket: %v", err)
	}

	emitsUpdateKV := func(args ...string) bool {
		t.Helper()
		stdout, code := runWithNATS(t, url, args...)
		if code != ExitOK {
			t.Fatalf("%v = %d, want 0\nstdout: %s", args, code, stdout)
		}
		var plan provision.PlanResult
		if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
			t.Fatalf("unmarshal plan: %v\nstdout: %s", err, stdout)
		}
		for _, a := range plan.Actions {
			if a.Kind == provision.ActionUpdateKV {
				return true
			}
		}

		return false
	}

	if !emitsUpdateKV("apply", "-f", "testdata/minimal.yaml",
		"-policy", "safe-update", "-dry-run", "-json") {
		t.Error("--policy=safe-update should emit an update-kv action for the drifted bucket")
	}
	if emitsUpdateKV("apply", "-f", "testdata/minimal.yaml", "-dry-run", "-json") {
		t.Error("default warn policy must not emit an update-kv action")
	}
}

// TestAdopt_DryRun_ByteIdenticalToPlanAdopt verifies that
// "adopt -dry-run -json" produces byte-identical output to "plan --policy=adopt -json".
func TestAdopt_DryRun_ByteIdenticalToPlanAdopt(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)

	planOut, planCode := runWithNATSBytes(t, url, "plan",
		"-f", "testdata/minimal.yaml", "-policy", "adopt", "-json")
	if planCode != ExitOK {
		t.Fatalf("plan --policy=adopt -json = %d", planCode)
	}

	adoptOut, adoptCode := runWithNATSBytes(t, url, "adopt",
		"-f", "testdata/minimal.yaml", "-dry-run", "-json")
	if adoptCode != ExitOK {
		t.Fatalf("adopt -dry-run -json = %d", adoptCode)
	}

	if !bytes.Equal(planOut, adoptOut) {
		t.Errorf("plan --policy=adopt -json and adopt -dry-run -json are not byte-identical\nplan:\n%s\nadopt:\n%s",
			planOut, adoptOut)
	}
}

// TestAdopt_EmptyServer_NoActions verifies that adopt against an empty server
// emits no actions (adopt only stamps markers on existing resources).
func TestAdopt_EmptyServer_NoActions(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)

	stdout, code := runWithNATS(t, url, "adopt",
		"-f", "testdata/minimal.yaml", "-dry-run", "-json")
	if code != ExitOK {
		t.Fatalf("adopt -dry-run = %d, want 0\nstdout: %s", code, stdout)
	}
	var plan provision.PlanResult
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
		t.Fatalf("unmarshal plan: %v", err)
	}
	// Adopt does NOT create missing buckets — empty server means no stamp-marker actions.
	if len(plan.Actions) != 0 {
		t.Errorf("adopt against empty server should emit no actions, got %d: %+v", len(plan.Actions), plan.Actions)
	}
}
