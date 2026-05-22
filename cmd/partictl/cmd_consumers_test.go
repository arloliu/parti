package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/dynamicbuild"
	"github.com/nats-io/nats.go/jetstream"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

const (
	testConsumerStream   = "orders"
	testConsumerPrefix   = "ord-worker"
	testConsumerTemplate = "orders.{{.PartitionID}}"
	testConsumerBucket   = "test-partitions"
	testConsumerKey      = "partitions/v1"
)

// writeConsumersEnv writes a parti-env.yaml with a dynamicConsumers block that
// opts into precreation and returns its path. The stream/prefix/template match
// the constants above so tests can create the matching NATS stream directly.
func writeConsumersEnv(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "env.yaml")
	doc := "apiVersion: parti.io/v1\n" +
		"partitionSource:\n" +
		"  bucket: " + testConsumerBucket + "\n" +
		"  key: " + testConsumerKey + "\n" +
		"  partitions:\n" +
		"    - keys: [\"a\"]\n" +
		"      weight: 1\n" +
		"    - keys: [\"b\"]\n" +
		"dynamicConsumers:\n" +
		"  - streamName: " + testConsumerStream + "\n" +
		"    consumerPrefix: " + testConsumerPrefix + "\n" +
		"    subjectTemplate: \"" + testConsumerTemplate + "\"\n" +
		"    partitionsRef: " + testConsumerBucket + "\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write consumers config: %v", err)
	}

	return path
}

// writeBadConsumersEnv writes a parti-env.yaml where partitionsRef does not
// match partitionSource.bucket — a static-validation failure (exit 3).
func writeBadConsumersEnv(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "env-bad.yaml")
	doc := "apiVersion: parti.io/v1\n" +
		"partitionSource:\n" +
		"  bucket: " + testConsumerBucket + "\n" +
		"  key: " + testConsumerKey + "\n" +
		"  partitions:\n" +
		"    - keys: [\"a\"]\n" +
		"dynamicConsumers:\n" +
		"  - streamName: " + testConsumerStream + "\n" +
		"    consumerPrefix: " + testConsumerPrefix + "\n" +
		"    subjectTemplate: \"" + testConsumerTemplate + "\"\n" +
		"    partitionsRef: wrong-bucket\n" // does not match partitionSource.bucket
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write bad consumers config: %v", err)
	}

	return path
}

// createTestStream creates the test JetStream stream "orders" with memory
// storage (no persistence needed in tests).
func createTestStream(t *testing.T, js jetstream.JetStream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     testConsumerStream,
		Subjects: []string{"orders.>"},
		Storage:  jetstream.MemoryStorage,
	})
	if err != nil {
		t.Fatalf("create test stream: %v", err)
	}
}

// createConsumerDirect creates a consumer directly on the test stream with the
// given config.
func createConsumerDirect(t *testing.T, js jetstream.JetStream, cfg jetstream.ConsumerConfig) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := js.CreateConsumer(ctx, testConsumerStream, cfg); err != nil {
		t.Fatalf("create consumer direct: %v", err)
	}
}

// ---------------------------------------------------------------------------
// consumers plan — exit codes
// ---------------------------------------------------------------------------

func TestConsumers_Plan_NoDrift_ExitOK(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	createTestStream(t, js)

	// Pre-create all consumers so there is no drift.
	for _, key := range []string{"a", "b"} {
		pre, suf, _ := dynamicbuild.ParseSubjectTemplateParts(testConsumerTemplate)
		subject := "orders." + key
		durable := dynamicbuild.PerSubjectDurableName(testConsumerPrefix, subject, pre, suf)
		createConsumerDirect(t, js, dynamicbuild.ConsumerConfig(durable, subject, dynamicbuild.DefaultDynamicDefaults()))
	}

	cfg := writeConsumersEnv(t)
	_, _, code := runArgs("consumers", "plan", "-server", url, "-f", cfg)
	if code != ExitOK {
		t.Fatalf("consumers plan (no diff) = %d, want 0", code)
	}
}

func TestConsumers_Plan_InformationalOnly_ExitOK(t *testing.T) {
	// A fully-precreated consumer set emits only informational findings.
	// -fail-on-drift must exit 0 in this case — hasDrift excludes informational.
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	createTestStream(t, js)

	for _, key := range []string{"a", "b"} {
		pre, suf, _ := dynamicbuild.ParseSubjectTemplateParts(testConsumerTemplate)
		subject := "orders." + key
		durable := dynamicbuild.PerSubjectDurableName(testConsumerPrefix, subject, pre, suf)
		createConsumerDirect(t, js, dynamicbuild.ConsumerConfig(durable, subject, dynamicbuild.DefaultDynamicDefaults()))
	}

	cfg := writeConsumersEnv(t)
	_, _, code := runArgs("consumers", "plan", "-server", url, "-f", cfg, "-fail-on-drift")
	if code != ExitOK {
		t.Errorf("consumers plan -fail-on-drift with only informational = %d, want 0", code)
	}
}

func TestConsumers_Plan_FailOnDrift_ExitDrift(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	createTestStream(t, js)
	// No consumers pre-created → drift-mutable for all.

	cfg := writeConsumersEnv(t)
	_, _, code := runArgs("consumers", "plan", "-server", url, "-f", cfg, "-fail-on-drift")
	if code != ExitDrift {
		t.Errorf("consumers plan -fail-on-drift with drift = %d, want %d (ExitDrift)", code, ExitDrift)
	}
}

func TestConsumers_Plan_JSON(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	createTestStream(t, js)

	cfg := writeConsumersEnv(t)
	stdout, _, code := runArgs("consumers", "plan", "-server", url, "-f", cfg, "-json")
	if code != ExitOK {
		t.Fatalf("consumers plan -json = %d, want 0", code)
	}
	if !strings.Contains(stdout, `"parti.io/provision/v1"`) {
		t.Errorf("JSON output missing provision/v1 envelope: %q", stdout)
	}
	if !strings.Contains(stdout, `"Plan"`) {
		t.Errorf("JSON output missing Plan kind: %q", stdout)
	}
}

// ---------------------------------------------------------------------------
// consumers apply — exit codes
// ---------------------------------------------------------------------------

func TestConsumers_Apply_ExitOK(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	createTestStream(t, js)

	cfg := writeConsumersEnv(t)
	_, _, code := runArgs("consumers", "apply", "-server", url, "-f", cfg)
	if code != ExitOK {
		t.Fatalf("consumers apply = %d, want 0", code)
	}

	// Re-plan: all consumers exist, no actions expected, only informational drift.
	stdout, _, code := runArgs("consumers", "plan", "-server", url, "-f", cfg, "-fail-on-drift")
	if code != ExitOK {
		t.Errorf("consumers plan after apply = %d, want 0 (converged)", code)
	}
	if strings.Contains(stdout, "create-consumer") {
		t.Errorf("expected no create-consumer after apply: %q", stdout)
	}
}

func TestConsumers_Apply_JSON(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	createTestStream(t, js)

	cfg := writeConsumersEnv(t)
	stdout, _, code := runArgs("consumers", "apply", "-server", url, "-f", cfg, "-json")
	if code != ExitOK {
		t.Fatalf("consumers apply -json = %d, want 0", code)
	}
	if !strings.Contains(stdout, `"parti.io/provision/v1"`) {
		t.Errorf("apply JSON output missing provision/v1 envelope: %q", stdout)
	}
	if !strings.Contains(stdout, `"Report"`) {
		t.Errorf("apply JSON output missing Report kind: %q", stdout)
	}
}

func TestConsumers_Apply_ServerRejectedCreate_ExitRuntime(t *testing.T) {
	// The stream allows at most 1 consumer. The config declares 2 partitions,
	// so PlanConsumers emits 2 create-consumer actions. ApplyConsumers creates
	// the first successfully, then the server rejects the second with a plain
	// API error (no sentinel) → classifyError → ExitRuntime (1).
	t.Parallel()
	url, js := startTestNATSWithJS(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:         testConsumerStream,
		Subjects:     []string{"orders.>"},
		MaxConsumers: 1,
		Storage:      jetstream.MemoryStorage,
	})
	if err != nil {
		t.Fatalf("create stream with MaxConsumers=1: %v", err)
	}

	cfg := writeConsumersEnv(t)
	_, _, code := runArgs("consumers", "apply", "-server", url, "-f", cfg)
	if code != ExitRuntime {
		t.Errorf("consumers apply with server-rejected create = %d, want %d (ExitRuntime)", code, ExitRuntime)
	}
}

func TestConsumers_Apply_ErrConsumerStreamMissing_ExitValidation(t *testing.T) {
	// ErrConsumerStreamMissing wraps ErrLiveValidation → exit 3.
	t.Parallel()
	url := startTestNATS(t) // stream intentionally not created

	cfg := writeConsumersEnv(t)
	_, _, code := runArgs("consumers", "apply", "-server", url, "-f", cfg)
	if code != ExitValidation {
		t.Errorf("consumers apply with missing stream = %d, want %d (ExitValidation)", code, ExitValidation)
	}
}

func TestConsumers_Apply_BadConfig_ExitValidation(t *testing.T) {
	// Static-validation failure (partitionsRef mismatch) → exit 3 before NATS
	// connect.
	t.Parallel()

	cfg := writeBadConsumersEnv(t)
	_, stderr, code := runArgs("consumers", "apply", "-f", cfg)
	if code != ExitValidation {
		t.Errorf("consumers apply with bad config = %d, want %d (ExitValidation)", code, ExitValidation)
	}
	if !strings.Contains(stderr, "partitionsRef") {
		t.Errorf("expected 'partitionsRef' in stderr: %q", stderr)
	}
}

// ---------------------------------------------------------------------------
// consumers apply -dry-run — writes nothing
// ---------------------------------------------------------------------------

func TestConsumers_Apply_DryRunWritesNothing(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	createTestStream(t, js)

	cfg := writeConsumersEnv(t)
	_, _, code := runArgs("consumers", "apply", "-server", url, "-f", cfg, "-dry-run")
	if code != ExitOK {
		t.Fatalf("consumers apply -dry-run = %d, want 0", code)
	}

	// The diff must still be present — dry-run wrote nothing.
	stdout, _, code := runArgs("consumers", "plan", "-server", url, "-f", cfg)
	if code != ExitOK {
		t.Fatalf("consumers plan after dry-run = %d, want 0", code)
	}
	if !strings.Contains(stdout, "create-consumer") {
		t.Errorf("dry-run must not write; expected the diff to remain: %q", stdout)
	}
}

// ---------------------------------------------------------------------------
// -policy flag on consumers — acceptance and rejection
// ---------------------------------------------------------------------------

// TestConsumers_PolicyForce_Plan_ReachesNATS: consumers plan -policy force is
// accepted at the flag/validation layer and proceeds to NATS connect (fails offline).
func TestConsumers_PolicyForce_Plan_ReachesNATS(t *testing.T) {
	t.Parallel()
	cfg := writeConsumersEnv(t)

	_, stderr, code := runArgs("consumers", "plan",
		"-f", cfg,
		"-policy", "force",
		"-server", "nats://127.0.0.1:1",
		"-timeout", "500ms",
	)
	if code != ExitNATS {
		t.Errorf("consumers plan -policy force = %d, want %d (ExitNATS — accepted, NATS unreachable)", code, ExitNATS)
	}
	if strings.Contains(stderr, "not recognized") || strings.Contains(stderr, "accepts only") {
		t.Errorf("consumers plan -policy force must not emit a policy rejection: %q", stderr)
	}
}

// TestConsumers_PolicyForce_Apply_ReachesNATS: consumers apply -policy force is
// accepted at the flag/validation layer and proceeds to NATS connect (fails offline).
func TestConsumers_PolicyForce_Apply_ReachesNATS(t *testing.T) {
	t.Parallel()
	cfg := writeConsumersEnv(t)

	_, stderr, code := runArgs("consumers", "apply",
		"-f", cfg,
		"-policy", "force",
		"-server", "nats://127.0.0.1:1",
		"-timeout", "500ms",
	)
	if code != ExitNATS {
		t.Errorf("consumers apply -policy force = %d, want %d (ExitNATS — accepted, NATS unreachable)", code, ExitNATS)
	}
	if strings.Contains(stderr, "not recognized") || strings.Contains(stderr, "accepts only") {
		t.Errorf("consumers apply -policy force must not emit a policy rejection: %q", stderr)
	}
}

// TestConsumers_PolicyAdopt_Rejected: consumers plan -policy adopt → ExitValidation.
// adopt is meaningless for consumer precreation.
func TestConsumers_PolicyAdopt_Rejected(t *testing.T) {
	t.Parallel()
	cfg := writeConsumersEnv(t)

	_, stderr, code := runArgs("consumers", "plan", "-f", cfg, "-policy", "adopt")
	if code != ExitValidation {
		t.Errorf("consumers plan -policy adopt = %d, want %d (ExitValidation)", code, ExitValidation)
	}
	if !strings.Contains(stderr, "accepts only") {
		t.Errorf("expected 'accepts only' in rejection message: %q", stderr)
	}
}

// TestConsumers_PolicySafeUpdate_Rejected: consumers plan -policy safe-update → ExitValidation.
// safe-update is meaningless for consumer precreation.
func TestConsumers_PolicySafeUpdate_Rejected(t *testing.T) {
	t.Parallel()
	cfg := writeConsumersEnv(t)

	_, stderr, code := runArgs("consumers", "plan", "-f", cfg, "-policy", "safe-update")
	if code != ExitValidation {
		t.Errorf("consumers plan -policy safe-update = %d, want %d (ExitValidation)", code, ExitValidation)
	}
	if !strings.Contains(stderr, "accepts only") {
		t.Errorf("expected 'accepts only' in rejection message: %q", stderr)
	}
}

// TestConsumers_PolicyBogus_Rejected: consumers plan -policy bogus → ExitValidation
// (unrecognized policy).
func TestConsumers_PolicyBogus_Rejected(t *testing.T) {
	t.Parallel()
	cfg := writeConsumersEnv(t)

	_, stderr, code := runArgs("consumers", "plan", "-f", cfg, "-policy", "bogus")
	if code != ExitValidation {
		t.Errorf("consumers plan -policy bogus = %d, want %d (ExitValidation)", code, ExitValidation)
	}
	if !strings.Contains(stderr, "not recognized") {
		t.Errorf("expected 'not recognized' in rejection message: %q", stderr)
	}
}

// ---------------------------------------------------------------------------
// Dispatch edge cases
// ---------------------------------------------------------------------------

func TestConsumers_NoSubcommand(t *testing.T) {
	t.Parallel()
	_, stderr, code := runArgs("consumers")
	if code != ExitValidation {
		t.Errorf("consumers with no subcommand = %d, want %d", code, ExitValidation)
	}
	if !strings.Contains(stderr, "subcommand") {
		t.Errorf("expected a 'subcommand required' message: %q", stderr)
	}
}

func TestConsumers_UnknownSubcommand(t *testing.T) {
	t.Parallel()
	_, stderr, code := runArgs("consumers", "bogus")
	if code != ExitValidation {
		t.Errorf("consumers bogus = %d, want %d", code, ExitValidation)
	}
	if !strings.Contains(stderr, "unknown subcommand") {
		t.Errorf("expected 'unknown subcommand': %q", stderr)
	}
}

func TestConsumers_Plan_NoFile(t *testing.T) {
	t.Parallel()
	_, stderr, code := runArgs("consumers", "plan")
	if code != ExitValidation {
		t.Errorf("consumers plan with no -f = %d, want %d", code, ExitValidation)
	}
	if !strings.Contains(stderr, "-f is required") {
		t.Errorf("expected '-f is required': %q", stderr)
	}
}

func TestConsumers_Apply_NoFile(t *testing.T) {
	t.Parallel()
	_, stderr, code := runArgs("consumers", "apply")
	if code != ExitValidation {
		t.Errorf("consumers apply with no -f = %d, want %d", code, ExitValidation)
	}
	if !strings.Contains(stderr, "-f is required") {
		t.Errorf("expected '-f is required': %q", stderr)
	}
}

// ---------------------------------------------------------------------------
// Two-level arg construction (documents the runWithNATS caveat)
// ---------------------------------------------------------------------------

// TestConsumers_TwoLevelArgsBuiltManually documents that args are built
// manually — runWithNATS splices -server after args[0], which would produce
// "consumers -server <url> plan ..." instead of "consumers plan -server <url>
// ...". We use runArgs directly throughout this file.
func TestConsumers_TwoLevelArgsBuiltManually(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	createTestStream(t, js)

	cfg := writeConsumersEnv(t)

	// Correct two-level form: consumers plan -server <url> -f <cfg>
	_, _, code := runArgs("consumers", "plan", "-server", url, "-f", cfg)
	if code != ExitOK {
		t.Errorf("two-level manual args: consumers plan = %d, want 0", code)
	}
}

// ---------------------------------------------------------------------------
// JSON envelope apiVersion present on both plan and apply
// ---------------------------------------------------------------------------

func TestConsumers_JSONEnvelope_BothPlanAndApply(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	createTestStream(t, js)

	cfg := writeConsumersEnv(t)

	planOut, _, planCode := runArgs("consumers", "plan", "-server", url, "-f", cfg, "-json")
	if planCode != ExitOK {
		t.Fatalf("consumers plan -json = %d", planCode)
	}
	if !strings.Contains(planOut, `"apiVersion"`) {
		t.Errorf("plan JSON missing apiVersion: %q", planOut)
	}
	if !strings.Contains(planOut, `"parti.io/provision/v1"`) {
		t.Errorf("plan JSON missing parti.io/provision/v1: %q", planOut)
	}

	applyOut, _, applyCode := runArgs("consumers", "apply", "-server", url, "-f", cfg, "-json")
	if applyCode != ExitOK {
		t.Fatalf("consumers apply -json = %d", applyCode)
	}
	if !strings.Contains(applyOut, `"apiVersion"`) {
		t.Errorf("apply JSON missing apiVersion: %q", applyOut)
	}
	if !strings.Contains(applyOut, `"parti.io/provision/v1"`) {
		t.Errorf("apply JSON missing parti.io/provision/v1: %q", applyOut)
	}
}
