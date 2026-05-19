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
