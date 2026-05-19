package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/arloliu/parti/v2/provision"
)

// runArgs is a helper to call run() and capture stdout/stderr.
func runArgs(args ...string) (stdout, stderr string, code int) {
	var outBuf, errBuf bytes.Buffer
	code = run(args, &outBuf, &errBuf)
	return outBuf.String(), errBuf.String(), code
}

// --- Top-level dispatch tests ---

func TestRunNoArgs(t *testing.T) {
	t.Parallel()
	_, _, code := runArgs()
	if code != ExitValidation {
		t.Errorf("run() with no args = %d, want %d (ExitValidation)", code, ExitValidation)
	}
}

func TestRunHelp(t *testing.T) {
	t.Parallel()
	stdout, _, code := runArgs("--help")
	if code != ExitOK {
		t.Errorf("run(--help) = %d, want %d (ExitOK)", code, ExitOK)
	}
	if !strings.Contains(stdout, "partictl") {
		t.Errorf("help output missing 'partictl': %q", stdout)
	}
}

func TestRunUnknownCommand(t *testing.T) {
	t.Parallel()
	_, stderr, code := runArgs("bogus-command")
	if code != ExitValidation {
		t.Errorf("run(bogus-command) = %d, want %d (ExitValidation)", code, ExitValidation)
	}
	if !strings.Contains(stderr, "unknown command") {
		t.Errorf("expected 'unknown command' in stderr: %q", stderr)
	}
}

// --- validate command (static, no NATS) ---

func TestValidateNoFlag(t *testing.T) {
	t.Parallel()
	_, stderr, code := runArgs("validate")
	if code != ExitValidation {
		t.Errorf("validate (no -f) = %d, want %d", code, ExitValidation)
	}
	if !strings.Contains(stderr, "-f is required") {
		t.Errorf("expected -f required message in stderr: %q", stderr)
	}
}

func TestValidateFlagParseError(t *testing.T) {
	t.Parallel()
	// Unknown flag → flag.ContinueOnError returns error → ExitValidation.
	_, _, code := runArgs("validate", "-unknown-flag")
	if code != ExitValidation {
		t.Errorf("validate -unknown-flag = %d, want %d (ExitValidation)", code, ExitValidation)
	}
}

func TestValidateStaticOK(t *testing.T) {
	t.Parallel()
	stdout, _, code := runArgs("validate", "-f", "testdata/minimal.yaml")
	if code != ExitOK {
		t.Errorf("validate minimal.yaml = %d, want 0", code)
	}
	if !strings.Contains(stdout, "ok") {
		t.Errorf("expected 'ok' in stdout: %q", stdout)
	}
}

func TestValidateStaticBadVersion(t *testing.T) {
	t.Parallel()
	_, _, code := runArgs("validate", "-f", "testdata/bad_version.yaml")
	if code != ExitValidation {
		t.Errorf("validate bad_version.yaml = %d, want %d (ExitValidation)", code, ExitValidation)
	}
}

func TestValidateStaticFullYAML(t *testing.T) {
	t.Parallel()
	stdout, _, code := runArgs("validate", "-f", "testdata/full.yaml")
	if code != ExitOK {
		t.Errorf("validate full.yaml = %d, want 0", code)
	}
	if !strings.Contains(stdout, "ok") {
		t.Errorf("expected 'ok' in stdout: %q", stdout)
	}
}

func TestValidateStaticJSONEnvelope(t *testing.T) {
	t.Parallel()
	stdout, _, code := runArgs("validate", "-f", "testdata/minimal.yaml", "-json")
	if code != ExitOK {
		t.Errorf("validate -json = %d, want 0", code)
	}
	if !strings.Contains(stdout, `"apiVersion"`) {
		t.Errorf("expected apiVersion in JSON output: %q", stdout)
	}
	if !strings.Contains(stdout, `"parti.io/provision/v1"`) {
		t.Errorf("expected parti.io/provision/v1 in JSON output: %q", stdout)
	}
	if !strings.Contains(stdout, `"kind"`) {
		t.Errorf("expected kind in JSON output: %q", stdout)
	}
}

func TestValidateStaticBadVersionJSON(t *testing.T) {
	t.Parallel()
	stdout, _, code := runArgs("validate", "-f", "testdata/bad_version.yaml", "-json")
	if code != ExitValidation {
		t.Errorf("validate bad_version.yaml -json = %d, want %d", code, ExitValidation)
	}
	// JSON output should be a Report-shaped envelope with at least one error.
	var report provision.Report
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("JSON unmarshal failed: %v\noutput: %q", err, stdout)
	}
	if len(report.Errors) == 0 {
		t.Errorf("expected at least one error in report, got none; output: %q", stdout)
	}
}

// --- plan command (no NATS, static validation) ---

func TestPlanNoFlag(t *testing.T) {
	t.Parallel()
	_, stderr, code := runArgs("plan")
	if code != ExitValidation {
		t.Errorf("plan (no -f) = %d, want %d", code, ExitValidation)
	}
	_ = stderr
}

func TestPlanBadVersion(t *testing.T) {
	// Bad apiVersion → static validation fails (exit 3) before NATS connect.
	t.Parallel()
	_, _, code := runArgs("plan", "-f", "testdata/bad_version.yaml")
	if code != ExitValidation {
		t.Errorf("plan bad_version.yaml = %d, want %d (ExitValidation)", code, ExitValidation)
	}
}

func TestPlanNATSUnreachable(t *testing.T) {
	// Valid config but NATS unreachable → exit 4.
	t.Parallel()
	_, _, code := runArgs("plan",
		"-f", "testdata/minimal.yaml",
		"-server", "nats://127.0.0.1:1", // unreachable port
		"-timeout", "500ms",
	)
	if code != ExitNATS {
		t.Errorf("plan with unreachable NATS = %d, want %d (ExitNATS)", code, ExitNATS)
	}
}

func TestPlanContextTimeout(t *testing.T) {
	// Extremely short timeout → context deadline exceeded → exit 4.
	t.Parallel()
	_, _, code := runArgs("plan",
		"-f", "testdata/minimal.yaml",
		"-server", "nats://127.0.0.1:1", // unreachable
		"-timeout", "1ns",
	)
	if code != ExitNATS {
		t.Errorf("plan with 1ns timeout = %d, want %d (ExitNATS)", code, ExitNATS)
	}
}

// --- apply command (static validation) ---

func TestApplyNoFlag(t *testing.T) {
	t.Parallel()
	_, stderr, code := runArgs("apply")
	if code != ExitValidation {
		t.Errorf("apply (no -f) = %d, want %d", code, ExitValidation)
	}
	_ = stderr
}

func TestApplyBadVersion(t *testing.T) {
	t.Parallel()
	_, _, code := runArgs("apply", "-f", "testdata/bad_version.yaml")
	if code != ExitValidation {
		t.Errorf("apply bad_version.yaml = %d, want %d (ExitValidation)", code, ExitValidation)
	}
}

func TestApplyDryRunNATSUnreachable(t *testing.T) {
	t.Parallel()
	_, _, code := runArgs("apply",
		"-f", "testdata/minimal.yaml",
		"-dry-run",
		"-server", "nats://127.0.0.1:1",
		"-timeout", "500ms",
	)
	if code != ExitNATS {
		t.Errorf("apply -dry-run with unreachable NATS = %d, want %d (ExitNATS)", code, ExitNATS)
	}
}

// --- view command ---

func TestViewNATSUnreachable(t *testing.T) {
	t.Parallel()
	_, _, code := runArgs("view",
		"-server", "nats://127.0.0.1:1",
		"-timeout", "500ms",
	)
	if code != ExitNATS {
		t.Errorf("view with unreachable NATS = %d, want %d (ExitNATS)", code, ExitNATS)
	}
}

// --- config YAML loading ---

func TestConfigLoad_MissingFile(t *testing.T) {
	t.Parallel()
	_, _, code := runArgs("validate", "-f", "testdata/nonexistent.yaml")
	if code != ExitValidation {
		t.Errorf("validate missing file = %d, want %d (ExitValidation)", code, ExitValidation)
	}
}

func TestConfigLoad_FullYAML(t *testing.T) {
	t.Parallel()
	// Full YAML round-trips and passes static validation.
	stdout, _, code := runArgs("validate", "-f", "testdata/full.yaml")
	if code != ExitOK {
		t.Errorf("validate full.yaml = %d, want 0", code)
	}
	if !strings.Contains(stdout, "ok") {
		t.Errorf("expected 'ok': %q", stdout)
	}
}

// --- exit-code precedence tests ---

// TestPrecedence_ParseErrorBeforeNATS: a flag parse error → exit 3, even though
// NATS is unreachable (the NATS connect never happens).
func TestPrecedence_ParseErrorBeforeNATS(t *testing.T) {
	t.Parallel()
	_, _, code := runArgs("validate", "-bad-flag-should-not-exist")
	if code != ExitValidation {
		t.Errorf("parse error precedence = %d, want %d (ExitValidation)", code, ExitValidation)
	}
}

// TestPrecedence_StaticValidationBeforeNATS: bad apiVersion → exit 3 (no NATS
// connect attempted).
func TestPrecedence_StaticValidationBeforeNATS(t *testing.T) {
	t.Parallel()
	_, _, code := runArgs("plan",
		"-f", "testdata/bad_version.yaml",
		"-server", "nats://127.0.0.1:1",
	)
	if code != ExitValidation {
		t.Errorf("static validation before NATS = %d, want %d (ExitValidation)", code, ExitValidation)
	}
}

// TestPrecedence_StaticValidationBeforeDrift: bad apiVersion with -fail-on-drift →
// exit 3 (static validation error takes precedence over drift check).
func TestPrecedence_StaticValidationBeforeDrift(t *testing.T) {
	t.Parallel()
	_, _, code := runArgs("plan",
		"-f", "testdata/bad_version.yaml",
		"-fail-on-drift",
		"-server", "nats://127.0.0.1:1",
	)
	if code != ExitValidation {
		t.Errorf("static validation + -fail-on-drift = %d, want %d (ExitValidation)", code, ExitValidation)
	}
}

// --- P0-2: invalid -timeout exits 3 ---

// TestValidate_InvalidTimeout_ExitsParseError: -timeout with garbage value →
// exit 3 (flag-parse error), even on the static path (no -live, no NATS).
func TestValidate_InvalidTimeout_ExitsParseError(t *testing.T) {
	t.Parallel()
	_, stderr, code := runArgs("validate",
		"-f", "testdata/minimal.yaml",
		"-timeout", "garbage",
	)
	if code != ExitValidation {
		t.Errorf("validate -timeout garbage = %d, want %d (ExitValidation)", code, ExitValidation)
	}
	if !strings.Contains(stderr, "invalid") {
		t.Errorf("expected 'invalid' in stderr: %q", stderr)
	}
}

// TestPlan_InvalidTimeout_ExitsParseError: -timeout with garbage value →
// exit 3 even before any NATS connect.
func TestPlan_InvalidTimeout_ExitsParseError(t *testing.T) {
	t.Parallel()
	_, stderr, code := runArgs("plan",
		"-f", "testdata/minimal.yaml",
		"-timeout", "garbage",
	)
	if code != ExitValidation {
		t.Errorf("plan -timeout garbage = %d, want %d (ExitValidation)", code, ExitValidation)
	}
	if !strings.Contains(stderr, "invalid") {
		t.Errorf("expected 'invalid' in stderr: %q", stderr)
	}
}

// --- P1-2: precedence tests for malformed YAML and permission-denied ---

// TestPrecedence_MalformedYAMLWinsOverUnreachableNATS: a YAML parse error →
// exit 3, even though NATS is unreachable (parse error wins before NATS connect).
func TestPrecedence_MalformedYAMLWinsOverUnreachableNATS(t *testing.T) {
	t.Parallel()
	_, _, code := runArgs("plan",
		"-f", "testdata/malformed.yaml",
		"-server", "nats://127.0.0.1:1", // unreachable
	)
	if code != ExitValidation {
		t.Errorf("malformed YAML + unreachable NATS = %d, want %d (ExitValidation)", code, ExitValidation)
	}
}

// TestPrecedence_PermissionDenied_PlanExits3 verifies via classifyError that a
// NATS permission error returned from provision.Plan maps to exit 3 (not exit 1).
// Full NATS-server-with-auth test not required; classifyError is the load-bearing path.
// (nats.ErrPermissionViolation coverage is in TestClassifyError_NATSPermissionDenied.)
func TestPrecedence_PermissionDenied_PlanExits3(t *testing.T) {
	t.Parallel()
	// ErrLiveValidation wraps permission errors from provision.ValidateLive, so
	// a wrapped ErrLiveValidation must also map to ExitValidation.
	permWrap := fmt.Errorf("provision: validatelive: permission denied: %w", provision.ErrLiveValidation)
	code := classifyError(permWrap)
	if code != ExitValidation {
		t.Errorf("ErrLiveValidation-wrapped permission error → %d, want %d (ExitValidation)", code, ExitValidation)
	}
}

// --- P2-2: -instance ignored warning ---

// TestView_InstanceIgnoredWithFile: view -f ... -instance ... → stderr warning.
func TestView_InstanceIgnoredWithFile(t *testing.T) {
	t.Parallel()
	// -f set and -instance set → warning, then fails with unreachable NATS (exit 4).
	_, stderr, _ := runArgs("view",
		"-f", "testdata/minimal.yaml",
		"-instance", "prod",
		"-server", "nats://127.0.0.1:1",
		"-timeout", "100ms",
	)
	if !strings.Contains(stderr, "-instance ignored") {
		t.Errorf("expected '-instance ignored' warning in stderr: %q", stderr)
	}
}

// TestValidate_InstanceIgnoredWithFile: validate -f ... -instance ... → stderr warning.
func TestValidate_InstanceIgnoredWithFile(t *testing.T) {
	t.Parallel()
	// -f and -instance both set; validate is always config-scoped → warning.
	_, stderr, code := runArgs("validate",
		"-f", "testdata/minimal.yaml",
		"-instance", "prod",
	)
	if code != ExitOK {
		t.Errorf("validate with -instance = %d, want 0 (ExitOK)", code)
	}
	if !strings.Contains(stderr, "-instance ignored") {
		t.Errorf("expected '-instance ignored' warning in stderr: %q", stderr)
	}
}
