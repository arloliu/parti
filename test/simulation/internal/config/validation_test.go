package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// minimalValidConfig returns a Config that passes validation after
// applyDefaults — ready for individual fields to be mutated in tests.
func minimalValidConfig() *Config {
	c := &Config{}
	c.Simulation.Mode = "all-in-one"
	c.Simulation.Duration = 1 * time.Hour
	c.Partitions.Count = 10
	c.Partitions.MessageRatePerPartition = 0.1
	c.Partitions.Distribution = "uniform"
	c.Producers.Count = 1
	c.Workers.Count = 1
	c.Workers.AssignmentStrategy = "ConsistentHash"
	c.Workers.ConsumerBatchSize = 4
	c.Workers.HandlerConcurrency = 1
	c.Workers.ProcessingDelay.Min = 10 * time.Millisecond
	c.Workers.ProcessingDelay.Max = 50 * time.Millisecond
	c.Workers.AckWait = 30 * time.Second
	c.Coordinator.ValidationWindow = 5 * time.Minute
	c.Coordinator.GapAging = 45 * time.Second
	c.Coordinator.WorkerCacheMaxPerPartition = 4096
	c.NATS.Mode = "embedded"
	c.NATS.URL = "nats://localhost:4222"

	return c
}

// TestValidate_AckWaitGteGapAging_Rejects exercises B2: the simulation
// must reject configs where JetStream's redelivery window can exceed the
// oracle's hole-escalation window. Both equal and greater violate.
func TestValidate_AckWaitGteGapAging_Rejects(t *testing.T) {
	tests := []struct {
		name     string
		ackWait  time.Duration
		gapAging time.Duration
	}{
		{"equal_30s", 30 * time.Second, 30 * time.Second},
		{"ackWaitGreater", 30 * time.Second, 20 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := minimalValidConfig()
			c.Workers.AckWait = tc.ackWait
			c.Coordinator.GapAging = tc.gapAging

			err := validateConfig(c)
			if err == nil {
				t.Fatalf("expected validation error for AckWait=%s GapAging=%s, got nil",
					tc.ackWait, tc.gapAging)
			}
			if !strings.Contains(err.Error(), "ack_wait") || !strings.Contains(err.Error(), "gap_aging") {
				t.Errorf("error must name both fields; got: %v", err)
			}
		})
	}
}

func TestValidate_AckWaitLtGapAging_Passes(t *testing.T) {
	c := minimalValidConfig()
	c.Workers.AckWait = 10 * time.Second
	c.Coordinator.GapAging = 30 * time.Second
	if err := validateConfig(c); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

// TestValidate_AckWaitOmitted_AppliesDefault proves that omitting
// workers.ack_wait does not bypass B2: after applyDefaults populates the
// 30s default, validation must catch an aggressive gap_aging < 30s.
func TestValidate_AckWaitOmitted_AppliesDefault(t *testing.T) {
	c := minimalValidConfig()
	c.Workers.AckWait = 0 // omitted in YAML
	c.Coordinator.GapAging = 20 * time.Second

	applyDefaults(c)
	if c.Workers.AckWait != 30*time.Second {
		t.Fatalf("applyDefaults did not set AckWait=30s; got %s", c.Workers.AckWait)
	}

	err := validateConfig(c)
	if err == nil {
		t.Fatal("expected validation error after defaulting; got nil")
	}
	if !strings.Contains(err.Error(), "ack_wait") {
		t.Errorf("error must reference ack_wait; got: %v", err)
	}
}

// TestAllShippedConfigsValidate loads every YAML config under
// test/simulation/configs/ and asserts it survives load + defaults +
// validate. Catches yaml drift introduced by the AckWait addition and
// verifies chaos_comprehensive.yaml (the only CI config) is unaffected.
func TestAllShippedConfigsValidate(t *testing.T) {
	// Walk up from the package dir to test/simulation/configs/.
	configsDir := filepath.Join("..", "..", "configs")
	entries, err := os.ReadDir(configsDir)
	if err != nil {
		t.Fatalf("read configs dir %s: %v", configsDir, err)
	}

	loaded := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		path := filepath.Join(configsDir, e.Name())
		t.Run(e.Name(), func(t *testing.T) {
			if _, err := LoadConfig(path); err != nil {
				t.Errorf("LoadConfig(%s): %v", path, err)
			}
		})
		loaded++
	}
	if loaded == 0 {
		t.Fatalf("no yaml configs found under %s (test setup wrong?)", configsDir)
	}
}

// TestValidate_WorkerCacheMaxPerPartition_NonPositive_Rejects guards
// the config invariant: zero or negative caps are meaningless and
// would silently fall back to a default — better to surface as a
// config error so operators tune it deliberately.
func TestValidate_WorkerCacheMaxPerPartition_NonPositive_Rejects(t *testing.T) {
	for _, n := range []int{0, -1} {
		c := minimalValidConfig()
		c.Coordinator.WorkerCacheMaxPerPartition = n
		err := validateConfig(c)
		if err == nil {
			t.Fatalf("WorkerCacheMaxPerPartition=%d should fail validation", n)
		}
		if !strings.Contains(err.Error(), "worker_cache_max_per_partition") {
			t.Errorf("error must reference worker_cache_max_per_partition; got: %v", err)
		}
	}
}

// TestApplyDefaults_WorkerCacheMaxPerPartition asserts the default fires.
func TestApplyDefaults_WorkerCacheMaxPerPartition(t *testing.T) {
	c := minimalValidConfig()
	c.Coordinator.WorkerCacheMaxPerPartition = 0
	applyDefaults(c)
	if c.Coordinator.WorkerCacheMaxPerPartition != 4096 {
		t.Errorf("default = %d, want 4096", c.Coordinator.WorkerCacheMaxPerPartition)
	}
	if err := validateConfig(c); err != nil {
		t.Errorf("validation must pass after defaults; got %v", err)
	}
}

// TestValidate_AckWaitNonPositive_Rejects keeps the field-level invariant
// visible: explicit zero or negative ack_wait (after defaulting would not
// kick in if user set a literal 0) is rejected.
func TestValidate_AckWaitNonPositive_Rejects(t *testing.T) {
	c := minimalValidConfig()
	c.Workers.AckWait = -1 * time.Second
	err := validateConfig(c)
	if err == nil || !strings.Contains(err.Error(), "ack_wait") {
		t.Fatalf("expected ack_wait validation error; got %v", err)
	}
}

func TestValidateConfig_LabeledSubsetOutOfRange(t *testing.T) {
	t.Parallel()
	cfg := minimalValidConfig()
	cfg.Partitions.Count = 10
	cfg.Partitions.LabeledSubsets = []LabeledPartitionSubset{
		{Label: "vip-a", Start: 8, End: 12}, // End exceeds Count
	}
	applyDefaults(cfg)
	err := validateConfig(cfg)
	if err == nil {
		t.Fatal("expected error for labeled_subsets range exceeding partitions.count")
	}
}

func TestValidateConfig_LabeledSubsetInvalidRange(t *testing.T) {
	t.Parallel()
	cfg := minimalValidConfig()
	cfg.Partitions.Count = 10
	cfg.Partitions.LabeledSubsets = []LabeledPartitionSubset{
		{Label: "vip-a", Start: 5, End: 3}, // Start >= End
	}
	applyDefaults(cfg)
	err := validateConfig(cfg)
	if err == nil {
		t.Fatal("expected error for labeled_subsets start >= end")
	}
}

func TestValidateConfig_LabelsUnknownWorkerID(t *testing.T) {
	t.Parallel()
	cfg := minimalValidConfig()
	cfg.Workers.Count = 3
	cfg.Workers.Labels = map[string][]string{
		"worker-9": {"vip-a"}, // out of range for Count=3 (worker-0..worker-2)
	}
	applyDefaults(cfg)
	err := validateConfig(cfg)
	if err == nil {
		t.Fatal("expected error for workers.labels referencing an out-of-range worker ID")
	}
}

func TestValidateConfig_LabelsAndSubsets_Valid(t *testing.T) {
	t.Parallel()
	cfg := minimalValidConfig()
	cfg.Workers.Count = 3
	cfg.Workers.Labels = map[string][]string{"worker-0": {"vip-a"}}
	cfg.Partitions.Count = 10
	cfg.Partitions.LabeledSubsets = []LabeledPartitionSubset{{Label: "vip-a", Start: 0, End: 2}}
	applyDefaults(cfg)
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}
}

func TestPartitionsConfig_ExpandLabels(t *testing.T) {
	t.Parallel()
	pc := PartitionsConfig{
		Count: 5,
		LabeledSubsets: []LabeledPartitionSubset{
			{Label: "vip-a", Start: 1, End: 3},
		},
	}
	got := pc.ExpandLabels()
	want := []string{"", "vip-a", "vip-a", "", ""}
	if len(got) != len(want) {
		t.Fatalf("length mismatch: got %d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: got %q want %q", i, got[i], want[i])
		}
	}
}

// TestValidateConfig_UnlabeledPartitionPolicy tests validation of the
// unlabeled_partition_policy field.
func TestValidateConfig_UnlabeledPartitionPolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		policy  string
		wantErr bool
	}{
		{"empty_string_passes", "", false},
		{"dedicated_passes", "dedicated", false},
		{"shared_passes", "shared", false},
		{"invalid_bogus", "bogus", true},
		{"invalid_random", "random", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := minimalValidConfig()
			cfg.Workers.UnlabeledPartitionPolicy = tc.policy
			applyDefaults(cfg)
			err := validateConfig(cfg)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for policy %q, got nil", tc.policy)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error for policy %q, got: %v", tc.policy, err)
			}
		})
	}
}

// TestValidateConfig_LabelSpillGrace tests validation of the label_spill_grace field.
func TestValidateConfig_LabelSpillGrace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		grace   time.Duration
		wantErr bool
	}{
		{"zero_passes", 0, false},
		{"positive_passes", 5 * time.Second, false},
		{"negative_fails", -1 * time.Second, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := minimalValidConfig()
			cfg.Workers.LabelSpillGrace = tc.grace
			applyDefaults(cfg)
			err := validateConfig(cfg)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for grace %s, got nil", tc.grace)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error for grace %s, got: %v", tc.grace, err)
			}
		})
	}
}

// TestValidateConfig_LabeledSubsetsNonOverlapping tests that non-overlapping
// ranges pass validation, including adjacent ranges (half-open ranges touching
// at the boundary is NOT an overlap).
func TestValidateConfig_LabeledSubsetsNonOverlapping(t *testing.T) {
	t.Parallel()
	cfg := minimalValidConfig()
	cfg.Partitions.Count = 10
	cfg.Partitions.LabeledSubsets = []LabeledPartitionSubset{
		{Label: "vip-a", Start: 0, End: 5},
		{Label: "vip-b", Start: 5, End: 8}, // Touching boundary, not overlapping
	}
	applyDefaults(cfg)
	err := validateConfig(cfg)
	if err != nil {
		t.Fatalf("expected no error for non-overlapping adjacent ranges, got: %v", err)
	}
}

// TestValidateConfig_LabeledSubsetsOverlapping tests that overlapping ranges
// are rejected, regardless of whether the labels are the same or different.
func TestValidateConfig_LabeledSubsetsOverlapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		subsets []LabeledPartitionSubset
		wantErr bool
	}{
		{
			"different_labels_overlap",
			[]LabeledPartitionSubset{
				{Label: "vip-a", Start: 0, End: 5},
				{Label: "vip-b", Start: 3, End: 8}, // Overlaps with vip-a
			},
			true,
		},
		{
			"same_label_overlap",
			[]LabeledPartitionSubset{
				{Label: "vip-a", Start: 0, End: 5},
				{Label: "vip-a", Start: 3, End: 8}, // Overlaps with first vip-a
			},
			true,
		},
		{
			"multiple_non_overlapping",
			[]LabeledPartitionSubset{
				{Label: "vip-a", Start: 0, End: 2},
				{Label: "vip-b", Start: 2, End: 5},
				{Label: "vip-c", Start: 5, End: 8},
			},
			false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := minimalValidConfig()
			cfg.Partitions.Count = 10
			cfg.Partitions.LabeledSubsets = tc.subsets
			applyDefaults(cfg)
			err := validateConfig(cfg)
			if tc.wantErr && err == nil {
				t.Fatal("expected error for overlapping ranges, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
		})
	}
}
