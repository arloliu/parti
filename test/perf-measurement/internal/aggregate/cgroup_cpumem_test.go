package aggregate

import (
	"math"
	"strings"
	"testing"
)

// TestParseCgroupCPUMem checks the row layout (4 fields) parses into samples.
func TestParseCgroupCPUMem(t *testing.T) {
	const raw = "# t_unix_ns container usage_usec memory_current_bytes\n" +
		"1000000000 perf-nats-1 500000 104857600\n" +
		"2000000000 perf-nats-1 1500000 110000000\n"
	got, err := parseCgroupCPUMemReader(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("parseCgroupCPUMemReader: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 samples, got %d", len(got))
	}
	if got[0].Container != "perf-nats-1" || got[0].UsageUsec != 500000 || got[0].MemoryBytes != 104857600 {
		t.Errorf("sample[0] wrong: %+v", got[0])
	}
}

// TestCPUMemDeltas_KnownFraction drives two ticks one wall-second apart whose
// usage_usec climbs by exactly 500_000 µs ⇒ 0.5 of one core. memory.current is
// carried through (the later sample's value), not differenced.
func TestCPUMemDeltas_KnownFraction(t *testing.T) {
	const sec = int64(1_000_000_000)
	samples := []CPUMemSample{
		// tick 1: baseline.
		{TUnixNs: 10 * sec, Container: "perf-nats-1", UsageUsec: 1_000_000, MemoryBytes: 100},
		// tick 2: +500_000 usage_usec over a 1s wall gap ⇒ 0.5 core.
		{TUnixNs: 11 * sec, Container: "perf-nats-1", UsageUsec: 1_500_000, MemoryBytes: 200},
	}
	deltas := CPUMemDeltas(samples)
	if len(deltas) != 1 {
		t.Fatalf("want 1 delta, got %d: %+v", len(deltas), deltas)
	}
	d := deltas[0]
	if d.TSec != 11 {
		t.Errorf("TSec = %d, want 11", d.TSec)
	}
	if math.Abs(d.CPUCores-0.5) > 1e-9 {
		t.Errorf("CPUCores = %v, want 0.5 (Δ500_000µs / 1_000_000µs wall)", d.CPUCores)
	}
	// memory is instantaneous: the LATER sample's value, not a delta.
	if d.MemoryBytes != 200 {
		t.Errorf("MemoryBytes = %d, want 200 (instantaneous, later sample)", d.MemoryBytes)
	}
}

// TestCPUMemDeltas_FullCoreAndMultiContainer confirms the dimensionless unit
// (1.0 = one full core, >1.0 = multiple cores) and per-container grouping.
func TestCPUMemDeltas_FullCoreAndMultiContainer(t *testing.T) {
	const sec = int64(1_000_000_000)
	samples := []CPUMemSample{
		// container 1: +1_000_000µs over 1s ⇒ 1.0 core.
		{TUnixNs: 10 * sec, Container: "perf-nats-1", UsageUsec: 0, MemoryBytes: 10},
		{TUnixNs: 11 * sec, Container: "perf-nats-1", UsageUsec: 1_000_000, MemoryBytes: 11},
		// container 2: +2_500_000µs over 1s ⇒ 2.5 cores.
		{TUnixNs: 10 * sec, Container: "perf-nats-2", UsageUsec: 0, MemoryBytes: 20},
		{TUnixNs: 11 * sec, Container: "perf-nats-2", UsageUsec: 2_500_000, MemoryBytes: 22},
	}
	deltas := CPUMemDeltas(samples)
	if len(deltas) != 2 {
		t.Fatalf("want 2 deltas, got %d", len(deltas))
	}
	byC := map[string]CPUMemDelta{}
	for _, d := range deltas {
		byC[d.Container] = d
	}
	if math.Abs(byC["perf-nats-1"].CPUCores-1.0) > 1e-9 {
		t.Errorf("nats-1 CPUCores = %v, want 1.0", byC["perf-nats-1"].CPUCores)
	}
	if math.Abs(byC["perf-nats-2"].CPUCores-2.5) > 1e-9 {
		t.Errorf("nats-2 CPUCores = %v, want 2.5", byC["perf-nats-2"].CPUCores)
	}
}

// TestCPUMemDeltas_CounterReset treats a backwards usage_usec as no progress.
func TestCPUMemDeltas_CounterReset(t *testing.T) {
	const sec = int64(1_000_000_000)
	samples := []CPUMemSample{
		{TUnixNs: 10 * sec, Container: "c", UsageUsec: 5_000_000, MemoryBytes: 1},
		{TUnixNs: 11 * sec, Container: "c", UsageUsec: 1_000_000, MemoryBytes: 2},
	}
	deltas := CPUMemDeltas(samples)
	if len(deltas) != 1 || deltas[0].CPUCores != 0 {
		t.Errorf("counter reset should yield CPUCores=0, got %+v", deltas)
	}
}
