package main

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hdr "github.com/HdrHistogram/hdrhistogram-go"

	"github.com/arloliu/parti/test/iops-investigation/internal/costmodel"
	"github.com/arloliu/parti/test/iops-investigation/internal/latency"
)

// makeSnapshot records the given latency values (ns) into a recorder-shaped HDR
// histogram and exports the serialized snapshot, mirroring what the harness
// writes into latency.json.
func makeSnapshot(t *testing.T, valuesNs ...int64) (*hdr.Snapshot, int64) {
	t.Helper()
	h := hdr.New(1_000, 60_000_000_000, 3)
	for _, v := range valuesNs {
		if err := h.RecordValue(v); err != nil {
			t.Fatalf("RecordValue(%d): %v", v, err)
		}
	}

	return h.Export(), h.TotalCount()
}

// writeRep writes a manifest.yaml + latency.json into results/<cell>/rep<r>/.
// Storage is fixed to "file"; the model code path is storage-agnostic so the
// fixtures don't need to vary it.
//
//nolint:revive // argument-limit: test fixture writer; each arg is one manifest/latency field.
func writeRep(t *testing.T, root, cell, rep, status string, n int, mode string, aggX float64, snap *hdr.Snapshot, count int64) {
	t.Helper()
	const storage = "file"
	dir := filepath.Join(root, cell, rep)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mani := "" +
		"status: " + status + "\n" +
		"options:\n" +
		"  n: " + itoa(n) + "\n" +
		"  consumerMode: " + mode + "\n" +
		"  kvStorage: " + storage + "\n" +
		"  dataStorage: " + storage + "\n" +
		"  aggregateX: " + ftoa(aggX) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(mani), 0o644); err != nil {
		t.Fatal(err)
	}
	lr := latencyReport{Count: count, InWindowSent: count, Delivered: count, Snapshot: snap}
	buf, err := json.Marshal(lr)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "latency.json"), buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int) string { return strings.TrimSpace(jsonNum(float64(n))) }
func ftoa(f float64) string {
	return strings.TrimSpace(jsonNum(f))
}
func jsonNum(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// captureLog routes the package logger to a buffer for the duration of fn.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	flags := log.Flags()
	prefix := log.Prefix()
	log.SetOutput(&buf)
	log.SetFlags(0)
	log.SetPrefix("")
	defer func() {
		log.SetOutput(old)
		log.SetFlags(flags)
		log.SetPrefix(prefix)
	}()
	fn()

	return buf.String()
}

// TestWalkResults_PoolsAndSkips covers the three required behaviors:
//  1. an invalid-status rep is skipped;
//  2. percentiles are computed from the POOLED merged histogram (known P50);
//  3. a (metric,storage) with < 3 cells is skipped with a log.
func TestWalkResults_PoolsAndSkips(t *testing.T) {
	root := t.TempDir()

	// Cell A: two reps, both ok. Rep1 records four samples at 10ms; rep2 four
	// samples at 30ms. Pooled = eight samples (4×10ms, 4×30ms). The pooled
	// median (50th pct) of {10,10,10,10,30,30,30,30}ms falls in the 10ms bin
	// at the lower-middle — HDR returns the value at quantile 50 which for this
	// distribution is ~10ms (the 4th of 8 ordered samples). We assert it is far
	// below the 30ms reps so per-rep averaging (which would give ~20ms) is
	// ruled out, and within the 10ms HDR bin.
	const ms = int64(1_000_000)
	snapA1, cA1 := makeSnapshot(t, 10*ms, 10*ms, 10*ms, 10*ms)
	snapA2, cA2 := makeSnapshot(t, 30*ms, 30*ms, 30*ms, 30*ms)
	writeRep(t, root, "cellA", "rep1", "ok", 1000, "dynamic", 20, snapA1, cA1)
	writeRep(t, root, "cellA", "rep2", "ok", 1000, "dynamic", 20, snapA2, cA2)

	// Cell A also has an INVALID rep that must be skipped (status=degraded).
	snapBad, cBad := makeSnapshot(t, 999*ms, 999*ms)
	writeRep(t, root, "cellA", "rep3", "degraded", 1000, "dynamic", 20, snapBad, cBad)

	// Cell B: a different N so we have 2 distinct cells of the same
	// (mode,storage). Two valid cells < 3 ⇒ the dynamic/file metrics are
	// skipped by FitAffine's >=3 rule.
	snapB1, cB1 := makeSnapshot(t, 50*ms)
	writeRep(t, root, "cellB", "rep1", "ok", 2000, "dynamic", 40, snapB1, cB1)

	// Cell C: same N as cell A but a DIFFERENT mode (queue). It must form a
	// SEPARATE cell — modes are never pooled. Asserting this proves the mode is
	// part of the cell key.
	snapC1, cC1 := makeSnapshot(t, 80*ms)
	writeRep(t, root, "cellC", "rep1", "ok", 1000, "queue", 20, snapC1, cC1)

	var cells []cell
	logOut := captureLog(t, func() {
		var err error
		cells, err = walkResults(root)
		if err != nil {
			t.Fatalf("walkResults: %v", err)
		}
	})

	// (1) Invalid rep skipped.
	if !strings.Contains(logOut, "SKIP rep cellA/rep3: status=\"degraded\"") {
		t.Errorf("expected degraded-rep skip log, got:\n%s", logOut)
	}

	// Three valid cells: (dynamic,file,N=1000), (dynamic,file,N=2000),
	// (queue,file,N=1000). The queue cell is NOT pooled with the dynamic N=1000
	// cell — distinct mode ⇒ distinct cell.
	if len(cells) != 3 {
		t.Fatalf("want 3 cells, got %d: %+v", len(cells), cells)
	}
	var queueCells, dynamicCells int
	for _, c := range cells {
		switch c.key.mode {
		case "queue":
			queueCells++
		case "dynamic":
			dynamicCells++
		}
	}
	if queueCells != 1 || dynamicCells != 2 {
		t.Errorf("mode keying wrong: dynamic=%d queue=%d (want 2,1)", dynamicCells, queueCells)
	}

	// (2) Pooled percentiles. Find cell A (N=1000) and assert its pooled count
	// is 8 (4+4 from the two valid reps; the degraded rep's 2 excluded) and the
	// pooled P50 sits in the 10ms bin — NOT the ~20ms a per-rep average yields.
	var cellAResult *cell
	for i := range cells {
		if cells[i].key.n == 1000 && cells[i].key.mode == "dynamic" {
			cellAResult = &cells[i]
		}
	}
	if cellAResult == nil {
		t.Fatal("cell A (N=1000) not found")
	}
	if !cellAResult.hasLatency {
		t.Fatal("cell A has no latency")
	}
	if got := cellAResult.latency.Count; got != 8 {
		t.Errorf("pooled count = %d, want 8 (degraded rep excluded)", got)
	}
	p50 := cellAResult.latency.P50Ns
	if p50 < 9*ms || p50 > 11*ms {
		t.Errorf("pooled P50 = %dns, want ~10ms bin [9ms,11ms] (per-rep avg would be ~20ms)", p50)
	}

	// (3) <3 points skipped with a log. Build the model and assert the skip.
	modelLog := captureLog(t, func() {
		m := buildModel(cells)
		if len(m) != 0 {
			t.Errorf("expected empty model (every metric has <3 cells), got %d metrics: %v", len(m), m)
		}
	})
	if !strings.Contains(modelLog, "< 3 required points") {
		t.Errorf("expected a '<3 required points' skip log, got:\n%s", modelLog)
	}
}

// TestBuildModel_FitsWithThreeCells confirms the happy path: three distinct
// (N,X) cells for one (metric,storage) produce a fit, and the latency P99
// metric folds the mode into its name.
func TestBuildModel_FitsWithThreeCells(t *testing.T) {
	// X must vary independently of N or FitAffine is singular (N and X
	// collinear). Use two k values at two N values so the (N,X) rows span the
	// plane: (1000,20),(1000,40),(2000,40),(2000,80).
	cells := []cell{
		{key: cellKey{mode: "dynamic", storage: "file", n: 1000, x: 20}, hasLatency: true, hasIOPS: true,
			writeIOPS: 100, latency: report(50), hasCPUMem: true, cpuCores: 1.0, rssBytes: 1e9},
		{key: cellKey{mode: "dynamic", storage: "file", n: 1000, x: 40}, hasLatency: true, hasIOPS: true,
			writeIOPS: 130, latency: report(55), hasCPUMem: true, cpuCores: 1.2, rssBytes: 1.1e9},
		{key: cellKey{mode: "dynamic", storage: "file", n: 2000, x: 40}, hasLatency: true, hasIOPS: true,
			writeIOPS: 200, latency: report(60), hasCPUMem: true, cpuCores: 1.8, rssBytes: 1.4e9},
		{key: cellKey{mode: "dynamic", storage: "file", n: 2000, x: 80}, hasLatency: true, hasIOPS: true,
			writeIOPS: 260, latency: report(70), hasCPUMem: true, cpuCores: 2.3, rssBytes: 1.6e9},
	}
	model := buildModel(cells)
	if _, ok := model["dynamic_write_iops"]["file"]; !ok {
		t.Errorf("expected dynamic_write_iops/file fit, got metrics: %v", keys(model))
	}
	if _, ok := model["dynamic_latency_p99_ns"]["file"]; !ok {
		t.Errorf("expected dynamic_latency_p99_ns/file fit, got metrics: %v", keys(model))
	}
	if _, ok := model["dynamic_cpu_cores"]["file"]; !ok {
		t.Errorf("expected dynamic_cpu_cores/file fit, got metrics: %v", keys(model))
	}
	if _, ok := model["dynamic_rss_bytes"]["file"]; !ok {
		t.Errorf("expected dynamic_rss_bytes/file fit, got metrics: %v", keys(model))
	}
}

// TestCellWriteIOPS exercises the NATS-side block-write IOPS extraction:
// per-second CgroupDeltas summed across the iops-nats-* containers, with the
// pre-warmup window excluded via rpc_counts.csv's first timestamp.
func TestCellWriteIOPS(t *testing.T) {
	dir := t.TempDir()
	const sec = int64(1_000_000_000)

	// cgroup_io.raw: format "t_unix_ns container device rbytes wbytes rios wios".
	// For each of the 5 NATS containers, cumulative wios climbs by 1000/sec.
	// Pre-window samples (t=5s,6s) carry a huge jump that MUST be excluded; the
	// in-window pair (t=10s,11s) yields 1000 wios/sec/container ⇒ 5000 summed.
	var b strings.Builder
	b.WriteString("# t_unix_ns container device rbytes wbytes rios wios\n")
	for _, c := range natsContainers {
		// pre-window: 5s->6s a 9_000_000 wios jump (would dwarf the real rate).
		b.WriteString(rowf(5*sec, c, 0))
		b.WriteString(rowf(6*sec, c, 9_000_000))
		// in-window: 10s->11s a +1000 wios delta.
		b.WriteString(rowf(10*sec, c, 9_000_000))
		b.WriteString(rowf(11*sec, c, 9_001_000))
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup_io.raw"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	// rpc_counts.csv: capture window starts at t=11s (post-warmup). Only the
	// t_sec=11 delta is in window; the t_sec=10 boundary delta (pre-window→
	// in-window, value 0) and the t_sec=6 pre-warmup jump are both excluded.
	rpc := "t_unix_ns,worker_idx,bucket,op,count\n" +
		ftoaI(11*sec) + ",0,b,Put,1\n" +
		ftoaI(12*sec) + ",0,b,Put,2\n"
	if err := os.WriteFile(filepath.Join(dir, "rpc_counts.csv"), []byte(rpc), 0o644); err != nil {
		t.Fatal(err)
	}

	iops, ok := cellWriteIOPS(dir)
	if !ok {
		t.Fatal("cellWriteIOPS returned ok=false; want a sample")
	}
	// In-window: one t_sec (11) with 5 containers × 1000 wios/s = 5000.
	if iops < 4999 || iops > 5001 {
		t.Errorf("writeIOPS = %.1f, want ~5000 (pre-warmup 9M jump must be excluded)", iops)
	}
}

// TestCellCPUMem exercises the NATS-side CPU + RSS extraction: per-second
// CPUMemDeltas summed across the iops-nats-* containers, restricted to the
// post-warmup window via rpc_counts.csv. Mirrors TestCellWriteIOPS.
func TestCellCPUMem(t *testing.T) {
	dir := t.TempDir()
	const sec = int64(1_000_000_000)

	// cgroup_cpumem.raw: "t_unix_ns container usage_usec memory_current_bytes".
	// Each NATS container: a pre-window pair (5s,6s) with a huge usage jump that
	// MUST be excluded, then an in-window pair (10s,11s) of +500_000 µs over a
	// 1s wall gap ⇒ 0.5 core per container ⇒ 2.5 cores summed across 5. RSS in
	// window is 200 MiB per container ⇒ 1 GiB summed.
	const mib = int64(1024 * 1024)
	var b strings.Builder
	b.WriteString("# t_unix_ns container usage_usec memory_current_bytes\n")
	for _, c := range natsContainers {
		// pre-window: 5s->6s a 9_000_000 µs jump (would dwarf the real rate).
		b.WriteString(cmRowf(5*sec, c, 0, 100*mib))
		b.WriteString(cmRowf(6*sec, c, 9_000_000, 100*mib))
		// in-window: 10s->11s a +500_000 µs delta ⇒ 0.5 core; RSS 200 MiB.
		b.WriteString(cmRowf(10*sec, c, 9_000_000, 200*mib))
		b.WriteString(cmRowf(11*sec, c, 9_500_000, 200*mib))
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup_cpumem.raw"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	// rpc_counts.csv: capture window starts at t=11s (post-warmup). Only the
	// t_sec=11 delta is in window; the t_sec=6 pre-warmup jump is excluded.
	rpc := "t_unix_ns,worker_idx,bucket,op,count\n" +
		ftoaI(11*sec) + ",0,b,Put,1\n" +
		ftoaI(12*sec) + ",0,b,Put,2\n"
	if err := os.WriteFile(filepath.Join(dir, "rpc_counts.csv"), []byte(rpc), 0o644); err != nil {
		t.Fatal(err)
	}

	cpu, rss, ok := cellCPUMem(dir)
	if !ok {
		t.Fatal("cellCPUMem returned ok=false; want a sample")
	}
	// 5 containers × 0.5 core = 2.5 cores summed.
	if cpu < 2.49 || cpu > 2.51 {
		t.Errorf("cpuCores = %.3f, want ~2.5 (pre-warmup 9M jump must be excluded)", cpu)
	}
	// 5 containers × 200 MiB = 1 GiB.
	want := float64(5 * 200 * mib)
	if rss < want*0.999 || rss > want*1.001 {
		t.Errorf("rssBytes = %.0f, want ~%.0f (5 × 200 MiB)", rss, want)
	}
}

// TestCellCPUMem_Absent confirms a missing cgroup_cpumem.raw yields ok=false
// (backward compatible with IOPS-only runs), not a failure.
func TestCellCPUMem_Absent(t *testing.T) {
	if _, _, ok := cellCPUMem(t.TempDir()); ok {
		t.Error("cellCPUMem on empty dir returned ok=true; want false")
	}
}

// rowf formats one cgroup_io.raw line: device dev0, wios=wios, others 0.
func rowf(tNs int64, container string, wios int64) string {
	return ftoaI(tNs) + " " + container + " 259:0 0 0 0 " + ftoaI(wios) + "\n"
}

// cmRowf formats one cgroup_cpumem.raw line: t container usage_usec mem_bytes.
func cmRowf(tNs int64, container string, usageUsec, memBytes int64) string {
	return ftoaI(tNs) + " " + container + " " + ftoaI(usageUsec) + " " + ftoaI(memBytes) + "\n"
}

func ftoaI(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// report builds a latency.Report whose percentiles equal pMs milliseconds,
// for fit-shape tests.
func report(pMs int64) latency.Report {
	v := pMs * 1_000_000
	return latency.Report{P50Ns: v, P90Ns: v, P95Ns: v, P99Ns: v, MaxNs: v}
}

func keys(m costmodel.Model) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
