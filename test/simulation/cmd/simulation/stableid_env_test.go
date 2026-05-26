package main

import (
	"strings"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/test/simulation/internal/coordinator"
	"github.com/arloliu/parti/v2/test/simulation/internal/worker"
)

// stubGetenv returns a getenv-like function backed by the given map.
func stubGetenv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestApplyStableIDEnv_AllSet(t *testing.T) {
	cfg := &worker.Config{}
	get := stubGetenv(map[string]string{
		"STABLEID_PREFIX": "freeze-victim",
		"STABLEID_MIN":    "1",
		"STABLEID_MAX":    "1",
		"STABLEID_TTL":    "6s",
	})
	if err := applyStableIDEnv(cfg, get); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.WorkerIDPrefix != "freeze-victim" {
		t.Errorf("Prefix: got %q want %q", cfg.WorkerIDPrefix, "freeze-victim")
	}
	if cfg.WorkerIDMin != 1 || cfg.WorkerIDMax != 1 {
		t.Errorf("Min/Max: got %d/%d want 1/1", cfg.WorkerIDMin, cfg.WorkerIDMax)
	}
	if cfg.WorkerIDTTL != 6*time.Second {
		t.Errorf("TTL: got %v want 6s", cfg.WorkerIDTTL)
	}
}

func TestApplyStableIDEnv_EmptyLeavesZero(t *testing.T) {
	cfg := &worker.Config{}
	get := stubGetenv(map[string]string{})
	if err := applyStableIDEnv(cfg, get); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.WorkerIDPrefix != "" || cfg.WorkerIDMin != 0 || cfg.WorkerIDMax != 0 || cfg.WorkerIDTTL != 0 {
		t.Fatalf("expected all-zero config, got %+v", cfg)
	}
}

func TestApplyStableIDEnv_PartialOverride(t *testing.T) {
	cfg := &worker.Config{}
	get := stubGetenv(map[string]string{"STABLEID_PREFIX": "tiny"})
	if err := applyStableIDEnv(cfg, get); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.WorkerIDPrefix != "tiny" {
		t.Error("Prefix not applied")
	}
	if cfg.WorkerIDMin != 0 || cfg.WorkerIDMax != 0 || cfg.WorkerIDTTL != 0 {
		t.Errorf("unexpected non-zero field: %+v", cfg)
	}
}

func TestApplyStableIDEnv_MalformedMinFailsLoudly(t *testing.T) {
	cfg := &worker.Config{}
	get := stubGetenv(map[string]string{"STABLEID_MIN": "not-a-number"})
	err := applyStableIDEnv(cfg, get)
	if err == nil {
		t.Fatal("expected non-nil error for malformed STABLEID_MIN")
	}
	if !strings.Contains(err.Error(), "STABLEID_MIN") {
		t.Errorf("error did not name the field: %v", err)
	}
}

// TestStableIDOverridesRoundTrip verifies that buildWorkerEnv + applyStableIDEnv
// is a faithful round-trip: a StableIDOverrides round-trips through the env
// transport unchanged. This is the actual contract Phase 7b depends on.
func TestStableIDOverridesRoundTrip(t *testing.T) {
	in := coordinator.StableIDOverrides{
		Prefix: "freeze-victim",
		Min:    1,
		Max:    1,
		TTL:    16 * time.Second,
	}
	env := coordinator.ExportedBuildWorkerEnv("worker-7", in)
	envMap := make(map[string]string, len(env))
	for _, e := range env {
		if i := strings.Index(e, "="); i > 0 {
			envMap[e[:i]] = e[i+1:]
		}
	}
	cfg := &worker.Config{}
	if err := applyStableIDEnv(cfg, func(k string) string { return envMap[k] }); err != nil {
		t.Fatalf("applyStableIDEnv: %v", err)
	}
	if cfg.WorkerIDPrefix != in.Prefix || cfg.WorkerIDMin != in.Min || cfg.WorkerIDMax != in.Max || cfg.WorkerIDTTL != in.TTL {
		t.Errorf("round-trip mismatch: in=%+v out={Prefix:%s Min:%d Max:%d TTL:%v}",
			in, cfg.WorkerIDPrefix, cfg.WorkerIDMin, cfg.WorkerIDMax, cfg.WorkerIDTTL)
	}
}

func TestApplyStableIDEnv_MalformedTTLFailsLoudly(t *testing.T) {
	cfg := &worker.Config{}
	get := stubGetenv(map[string]string{"STABLEID_TTL": "garbage"})
	err := applyStableIDEnv(cfg, get)
	if err == nil {
		t.Fatal("expected non-nil error for malformed STABLEID_TTL")
	}
	if !strings.Contains(err.Error(), "STABLEID_TTL") {
		t.Errorf("error did not name the field: %v", err)
	}
}
