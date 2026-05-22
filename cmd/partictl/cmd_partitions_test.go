package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// writePartitionsEnv writes a parti-env.yaml with the given partitions block
// (which may be empty) and returns its path.
func writePartitionsEnv(t *testing.T, partitionsBlock string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "env.yaml")
	doc := "apiVersion: parti.io/v1\n" +
		"partitionSource:\n" +
		"  bucket: parti-partitions\n" +
		"  key: partitions/v1\n" +
		partitionsBlock
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return path
}

// createPartitionBucket provisions the partition-source KV bucket directly.
func createPartitionBucket(t *testing.T, js jetstream.JetStream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "parti-partitions"}); err != nil {
		t.Fatalf("create partition-source bucket: %v", err)
	}
}

const twoPartitions = "  partitions:\n" +
	"    - keys: [\"a\"]\n" +
	"      weight: 1\n" +
	"    - keys: [\"b\"]\n"

func TestPartitions_Plan_FirstPublish(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	createPartitionBucket(t, js)
	cfg := writePartitionsEnv(t, twoPartitions)

	stdout, _, code := runArgs("partitions", "plan", "-server", url, "-f", cfg)
	if code != ExitOK {
		t.Fatalf("partitions plan = %d, want 0", code)
	}
	if !strings.Contains(stdout, "write-partitions") {
		t.Errorf("expected a write-partitions action in plan output: %q", stdout)
	}
}

func TestPartitions_Plan_FailOnDrift(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	createPartitionBucket(t, js)
	cfg := writePartitionsEnv(t, twoPartitions)

	_, _, code := runArgs("partitions", "plan", "-server", url, "-f", cfg, "-fail-on-drift")
	if code != ExitDrift {
		t.Errorf("partitions plan -fail-on-drift with a diff = %d, want %d (ExitDrift)", code, ExitDrift)
	}
}

func TestPartitions_Plan_JSON(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	createPartitionBucket(t, js)
	cfg := writePartitionsEnv(t, twoPartitions)

	stdout, _, code := runArgs("partitions", "plan", "-server", url, "-f", cfg, "-json")
	if code != ExitOK {
		t.Fatalf("partitions plan -json = %d, want 0", code)
	}
	if !strings.Contains(stdout, `"parti.io/provision/v1"`) {
		t.Errorf("JSON output missing provision/v1 envelope: %q", stdout)
	}
}

func TestPartitions_Apply_FirstPublishThenIdempotent(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	createPartitionBucket(t, js)
	cfg := writePartitionsEnv(t, twoPartitions)

	_, _, code := runArgs("partitions", "apply", "-server", url, "-f", cfg)
	if code != ExitOK {
		t.Fatalf("partitions apply = %d, want 0", code)
	}

	// Re-plan: the table is converged, so no write-partitions action.
	stdout, _, code := runArgs("partitions", "plan", "-server", url, "-f", cfg, "-fail-on-drift")
	if code != ExitOK {
		t.Errorf("partitions plan after apply = %d, want 0 (converged)", code)
	}
	if strings.Contains(stdout, "write-partitions") {
		t.Errorf("expected no write-partitions action after apply: %q", stdout)
	}
}

func TestPartitions_Apply_DryRunWritesNothing(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	createPartitionBucket(t, js)
	cfg := writePartitionsEnv(t, twoPartitions)

	_, _, code := runArgs("partitions", "apply", "-server", url, "-f", cfg, "-dry-run")
	if code != ExitOK {
		t.Fatalf("partitions apply -dry-run = %d, want 0", code)
	}

	// The diff must still be present — dry-run wrote nothing.
	stdout, _, code := runArgs("partitions", "plan", "-server", url, "-f", cfg)
	if code != ExitOK {
		t.Fatalf("partitions plan after dry-run = %d, want 0", code)
	}
	if !strings.Contains(stdout, "write-partitions") {
		t.Errorf("dry-run must not write; expected the diff to remain: %q", stdout)
	}
}

func TestPartitions_Apply_RemovalNeedsPrune(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	createPartitionBucket(t, js)

	// Seed the table with three partitions.
	seed := writePartitionsEnv(t, "  partitions:\n"+
		"    - keys: [\"a\"]\n"+
		"    - keys: [\"b\"]\n"+
		"    - keys: [\"c\"]\n")
	if _, _, code := runArgs("partitions", "apply", "-server", url, "-f", seed); code != ExitOK {
		t.Fatalf("seed apply = %d, want 0", code)
	}

	// A config with only "a" removes b and c.
	shrunk := writePartitionsEnv(t, "  partitions:\n    - keys: [\"a\"]\n")

	if _, _, code := runArgs("partitions", "apply", "-server", url, "-f", shrunk); code != ExitRuntime {
		t.Errorf("removal without -prune = %d, want %d (ExitRuntime)", code, ExitRuntime)
	}
	if _, _, code := runArgs("partitions", "apply", "-server", url, "-f", shrunk, "-prune"); code != ExitOK {
		t.Errorf("removal with -prune = %d, want 0", code)
	}
}

func TestPartitions_Apply_JSON(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	createPartitionBucket(t, js)
	cfg := writePartitionsEnv(t, twoPartitions)

	stdout, _, code := runArgs("partitions", "apply", "-server", url, "-f", cfg, "-json")
	if code != ExitOK {
		t.Fatalf("partitions apply -json = %d, want 0", code)
	}
	if !strings.Contains(stdout, `"parti.io/provision/v1"`) {
		t.Errorf("apply JSON output missing provision/v1 envelope: %q", stdout)
	}
	if !strings.Contains(stdout, `"Report"`) {
		t.Errorf("apply JSON output missing Report kind: %q", stdout)
	}
}

func TestPartitions_Apply_InvalidRecord(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	createPartitionBucket(t, js)
	cfg := writePartitionsEnv(t, "  partitions:\n    - keys: [\"a.b\"]\n")

	_, _, code := runArgs("partitions", "apply", "-server", url, "-f", cfg)
	if code != ExitValidation {
		t.Errorf("partitions apply with an invalid record = %d, want %d", code, ExitValidation)
	}
}

func TestPartitions_Apply_EmptyPartitionsRejected(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	createPartitionBucket(t, js)
	cfg := writePartitionsEnv(t, "  partitions: []\n")

	// An explicit empty set is rejected with and without -prune: Phase 3 has
	// no surface for pruning the table to zero.
	if _, _, code := runArgs("partitions", "apply", "-server", url, "-f", cfg); code != ExitValidation {
		t.Errorf("partitions apply with empty partitions = %d, want %d", code, ExitValidation)
	}
	if _, _, code := runArgs("partitions", "apply", "-server", url, "-f", cfg, "-prune"); code != ExitValidation {
		t.Errorf("partitions apply -prune with empty partitions = %d, want %d", code, ExitValidation)
	}
}

func TestPartitions_Plan_BucketMissing(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t) // bucket intentionally not created
	cfg := writePartitionsEnv(t, twoPartitions)

	_, _, code := runArgs("partitions", "plan", "-server", url, "-f", cfg)
	if code != ExitValidation {
		t.Errorf("partitions plan with a missing bucket = %d, want %d (ExitValidation)", code, ExitValidation)
	}
}

func TestPartitions_Plan_NoPartitionsDeclared(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	createPartitionBucket(t, js)
	cfg := writePartitionsEnv(t, "") // no partitions block

	_, _, code := runArgs("partitions", "plan", "-server", url, "-f", cfg)
	if code != ExitValidation {
		t.Errorf("partitions plan with no declared partitions = %d, want %d", code, ExitValidation)
	}
}

func TestPartitions_Plan_InvalidRecord(t *testing.T) {
	t.Parallel()
	url, js := startTestNATSWithJS(t)
	createPartitionBucket(t, js)
	// A dotted key is rejected by ValidatePartitionSet.
	cfg := writePartitionsEnv(t, "  partitions:\n    - keys: [\"a.b\"]\n")

	_, _, code := runArgs("partitions", "plan", "-server", url, "-f", cfg)
	if code != ExitValidation {
		t.Errorf("partitions plan with an invalid record = %d, want %d", code, ExitValidation)
	}
}

func TestPartitions_RejectsPolicyFlag(t *testing.T) {
	t.Parallel()
	cfg := writePartitionsEnv(t, twoPartitions)

	_, stderr, code := runArgs("partitions", "plan", "-f", cfg, "-policy", "warn")
	if code != ExitValidation {
		t.Errorf("partitions plan -policy = %d, want %d", code, ExitValidation)
	}
	if !strings.Contains(stderr, "policy") {
		t.Errorf("expected an unknown-flag error mentioning policy: %q", stderr)
	}
}

func TestPartitions_NoSubcommand(t *testing.T) {
	t.Parallel()
	_, stderr, code := runArgs("partitions")
	if code != ExitValidation {
		t.Errorf("partitions with no subcommand = %d, want %d", code, ExitValidation)
	}
	if !strings.Contains(stderr, "subcommand") {
		t.Errorf("expected a 'subcommand required' message: %q", stderr)
	}
}

func TestPartitions_UnknownSubcommand(t *testing.T) {
	t.Parallel()
	_, stderr, code := runArgs("partitions", "bogus")
	if code != ExitValidation {
		t.Errorf("partitions bogus = %d, want %d", code, ExitValidation)
	}
	if !strings.Contains(stderr, "unknown subcommand") {
		t.Errorf("expected 'unknown subcommand': %q", stderr)
	}
}

func TestPartitions_Plan_NoFile(t *testing.T) {
	t.Parallel()
	_, stderr, code := runArgs("partitions", "plan")
	if code != ExitValidation {
		t.Errorf("partitions plan with no -f = %d, want %d", code, ExitValidation)
	}
	if !strings.Contains(stderr, "-f is required") {
		t.Errorf("expected '-f is required': %q", stderr)
	}
}
