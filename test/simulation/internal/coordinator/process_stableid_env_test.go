package coordinator

import (
	"testing"
	"time"
)

// TestBuildWorkerEnv_NoOverrides verifies that an absent overrides struct
// produces exactly the historical env shape (WORKER_ID only — no STABLEID_*
// vars).
func TestBuildWorkerEnv_NoOverrides(t *testing.T) {
	env := buildWorkerEnv("worker-7")
	if !containsExact(env, "WORKER_ID=worker-7") {
		t.Fatalf("missing WORKER_ID; env=%v", env)
	}
	for _, e := range env {
		if startsWith(e, "STABLEID_") {
			t.Fatalf("unexpected STABLEID_* env var %q with zero overrides", e)
		}
	}
}

// TestBuildWorkerEnv_ZeroValueOverrides_NoEnvAdded verifies that an explicit
// zero-value overrides struct adds NO STABLEID_* vars (callers can pass it
// safely without producing noise).
func TestBuildWorkerEnv_ZeroValueOverrides_NoEnvAdded(t *testing.T) {
	env := buildWorkerEnv("worker-7", StableIDOverrides{})
	for _, e := range env {
		if startsWith(e, "STABLEID_") {
			t.Fatalf("unexpected STABLEID_* env var %q with zero overrides", e)
		}
	}
}

// TestBuildWorkerEnv_FullOverrides verifies that a fully-populated overrides
// struct produces all four STABLEID_* env vars with the expected values and
// TTL formatted via time.Duration.String().
func TestBuildWorkerEnv_FullOverrides(t *testing.T) {
	o := StableIDOverrides{
		Prefix: "freeze-victim",
		Min:    1,
		Max:    1,
		TTL:    6 * time.Second,
	}
	env := buildWorkerEnv("worker-3", o)
	wantPairs := map[string]bool{
		"WORKER_ID=worker-3":            false,
		"STABLEID_PREFIX=freeze-victim": false,
		"STABLEID_MIN=1":                false,
		"STABLEID_MAX=1":                false,
		"STABLEID_TTL=6s":               false,
	}
	for _, e := range env {
		if _, ok := wantPairs[e]; ok {
			wantPairs[e] = true
		}
	}
	for k, v := range wantPairs {
		if !v {
			t.Errorf("expected env var %q not found; env=%v", k, env)
		}
	}
}

// TestBuildWorkerEnv_PartialOverrides verifies that only the non-zero fields
// produce env vars. Zero-value fields are omitted so the receiving
// worker.Config inherits the sim default via override-if-set.
func TestBuildWorkerEnv_PartialOverrides(t *testing.T) {
	o := StableIDOverrides{Prefix: "tiny", TTL: 3 * time.Second}
	env := buildWorkerEnv("worker-9", o)
	if !containsExact(env, "STABLEID_PREFIX=tiny") {
		t.Fatal("missing STABLEID_PREFIX")
	}
	if !containsExact(env, "STABLEID_TTL=3s") {
		t.Fatal("missing STABLEID_TTL")
	}
	for _, e := range env {
		if e == "STABLEID_MIN=0" || e == "STABLEID_MAX=0" {
			t.Fatalf("unexpected zero-value env var %q", e)
		}
	}
}

func containsExact(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
