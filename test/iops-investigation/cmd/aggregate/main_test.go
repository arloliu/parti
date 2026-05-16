package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fixtureSpec describes the synthetic per-run dir each test builds. The
// helpers below materialize the five capture artifacts so the
// aggregator has realistic-but-tiny inputs to chew on. Keeping the
// fixtures in-test (instead of as 5 separate committed files per test)
// makes the relationship between an injected anomaly and the cross-check
// it triggers obvious in one place.
type fixtureSpec struct {
	// secondsAfter is how many wall-clock seconds the fixture covers; the
	// first cgroup/iostat sample is the baseline (cumulative since boot),
	// later samples carry the per-second deltas configured below.
	secondsAfter int
	containers   []string
	// perContainerRIOs / WIOs / RBytes / WBytes per second per container.
	perRIOs   uint64
	perWIOs   uint64
	perRBytes uint64
	perWBytes uint64
	// iostatRIOs / WIOs per second (host-wide; cross-check sums this
	// vs cgroup totals).
	iostatRIOsPerSec float64
	iostatWIOsPerSec float64
	// streams: per-stream cumulative msgs/bytes added per 5-second poll.
	streams map[string]struct{ msgsPer5s, bytesPer5s uint64 }
	// rpc snapshots: (worker, bucket, op) cumulative count *per snapshot*.
	// One snapshot is emitted per second; rpcOps holds the linear-growth
	// per second so cumulative count at snapshot i is rpcOps[k]*i.
	rpcOps map[rpcKey]int64
	// manifestStatus is written into manifest.yaml.
	manifestStatus string
	// Skip these files when materializing (drives missing-input tests).
	skip map[string]bool
}

type rpcKey struct {
	worker int
	bucket string
	op     string
}

func (f fixtureSpec) materialize(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))

	// Use a fixed wall-clock base so test output is stable across runs.
	// Choose a midnight in local time to keep iostat timestamp parsing
	// in lockstep with whatever locale the test host uses.
	base := time.Date(2026, 5, 16, 12, 0, 0, 0, time.Local)

	// cgroup_io.raw — one row per (container, device) per sample.
	if !f.skip["cgroup_io.raw"] {
		var b strings.Builder
		b.WriteString("# t_unix_ns container device rbytes wbytes rios wios\n")
		for i := 0; i <= f.secondsAfter; i++ {
			t0 := base.Add(time.Duration(i) * time.Second).UnixNano()
			for _, c := range f.containers {
				// Single device per container, cumulative since "boot".
				ri := f.perRIOs * uint64(i)
				wi := f.perWIOs * uint64(i)
				rb := f.perRBytes * uint64(i)
				wb := f.perWBytes * uint64(i)
				fmt.Fprintf(&b, "%d %s 259:0 %d %d %d %d\n", t0, c, rb, wb, ri, wi)
			}
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup_io.raw"), []byte(b.String()), 0o644))
	}

	// iostat.raw — first block is "since boot" (skipped by parser), then
	// one block per second for f.secondsAfter seconds. Each block reports
	// host-wide r/s,w/s split equally across two synthetic devices so the
	// parser's per-device sum is exercised.
	if !f.skip["iostat.raw"] {
		var b strings.Builder
		b.WriteString("# iostat -x -d -t 1\n")
		// Since-boot block — content discarded by the parser.
		fmt.Fprintf(&b, "%s\n", base.Format("01/02/2006 03:04:05 PM"))
		b.WriteString("Device r/s w/s\n")
		b.WriteString("sda 0 0\n\n")
		for i := 1; i <= f.secondsAfter; i++ {
			ts := base.Add(time.Duration(i) * time.Second)
			fmt.Fprintf(&b, "%s\n", ts.Format("01/02/2006 03:04:05 PM"))
			b.WriteString("Device r/s w/s\n")
			fmt.Fprintf(&b, "sda %.2f %.2f\n", f.iostatRIOsPerSec/2, f.iostatWIOsPerSec/2)
			fmt.Fprintf(&b, "sdb %.2f %.2f\n", f.iostatRIOsPerSec/2, f.iostatWIOsPerSec/2)
			b.WriteString("\n")
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, "iostat.raw"), []byte(b.String()), 0o644))
	}

	// jsz.raw — ndjson, one body per 5-second poll. Cumulative counters
	// grow by the per-stream per-5s amounts.
	if !f.skip["jsz.raw"] {
		var b strings.Builder
		polls := f.secondsAfter/5 + 1
		for p := range polls {
			ts := base.Add(time.Duration(p*5) * time.Second).UnixNano()
			var streamJSON strings.Builder
			streamJSON.WriteString(`[`)
			first := true
			for name, sp := range f.streams {
				if !first {
					streamJSON.WriteString(",")
				}
				first = false
				fmt.Fprintf(&streamJSON, `{"name":"%s","state":{"messages":%d,"bytes":%d}}`,
					name, sp.msgsPer5s*uint64(p), sp.bytesPer5s*uint64(p))
			}
			streamJSON.WriteString(`]`)
			fmt.Fprintf(&b, `{"t_unix_ns":%d,"node":"localhost:8222","endpoint":"jsz","body":{"account_details":[{"stream_detail":%s}]}}`+"\n",
				ts, streamJSON.String())
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, "jsz.raw"), []byte(b.String()), 0o644))
	}

	// node_exporter.prom — only checked for existence; minimal content.
	if !f.skip["node_exporter.prom"] {
		var b strings.Builder
		fmt.Fprintf(&b, "# t_unix_ns %d\n", base.UnixNano())
		b.WriteString("node_disk_reads_completed_total{device=\"sda\"} 0\n\n")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "node_exporter.prom"), []byte(b.String()), 0o644))
	}

	// rpc_counts.csv — one snapshot per second, cumulative counters.
	if !f.skip["rpc_counts.csv"] {
		var b strings.Builder
		b.WriteString("t_unix_ns,worker_idx,bucket,op,count\n")
		for i := 0; i <= f.secondsAfter; i++ {
			ts := base.Add(time.Duration(i) * time.Second).UnixNano()
			for k, perTick := range f.rpcOps {
				fmt.Fprintf(&b, "%d,%d,%s,%s,%d\n", ts, k.worker, k.bucket, k.op, perTick*int64(i))
			}
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, "rpc_counts.csv"), []byte(b.String()), 0o644))
	}

	// manifest.yaml.
	if !f.skip["manifest.yaml"] {
		manifest := fmt.Sprintf("status: %s\n", f.manifestStatus)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o644))
	}
}

// happyFixture builds a fixture spec where cgroup and iostat agree
// exactly: per-second iops_read+write per container × N containers =
// iostat's host-wide r/s+w/s.
func happyFixture() fixtureSpec {
	containers := []string{"iops-nats-1", "iops-nats-2", "iops-nats-3"}
	const rPerContainer = 4.0  // ops/s read per container
	const wPerContainer = 6.0  // ops/s write per container
	iostatR := rPerContainer * float64(len(containers))
	iostatW := wPerContainer * float64(len(containers))
	return fixtureSpec{
		secondsAfter: 10,
		containers:   containers,
		perRIOs:      uint64(rPerContainer),
		perWIOs:      uint64(wPerContainer),
		perRBytes:    4096,
		perWBytes:    8192,
		iostatRIOsPerSec: iostatR,
		iostatWIOsPerSec: iostatW,
		streams: map[string]struct{ msgsPer5s, bytesPer5s uint64 }{
			"PARTI_HEARTBEAT": {msgsPer5s: 25, bytesPer5s: 1024},
			"PARTI_DATA":      {msgsPer5s: 0, bytesPer5s: 0},
		},
		rpcOps: map[rpcKey]int64{
			{worker: 0, bucket: "PARTI_HEARTBEAT", op: "Put"}:   1, // 1 Put/s/worker
			{worker: 1, bucket: "PARTI_HEARTBEAT", op: "Put"}:   1,
			{worker: 0, bucket: "PARTI_HEARTBEAT", op: "Keys"}:  1, // leader-only fallback, but fine for fixture
			{worker: 0, bucket: "PARTI_HANDOFF", op: "ListKeys"}: 1,
			{worker: 0, bucket: "PARTI_HANDOFF", op: "Get"}:      2,
		},
		manifestStatus: "ok",
	}
}

func TestRun_HappyPath_WritesCSVAndDoesNotFail(t *testing.T) {
	dir := t.TempDir()
	happyFixture().materialize(t, dir)

	err := run([]string{"--run-dir", dir}, os.Stdout, os.Stderr)
	require.NoError(t, err)

	out, err := os.ReadFile(filepath.Join(dir, "aggregated.csv"))
	require.NoError(t, err)
	body := string(out)
	lines := strings.Split(strings.TrimSpace(body), "\n")
	require.GreaterOrEqual(t, len(lines), 2, "want header + at least one data row")

	header := lines[0]
	// Header must include the fixed prefix in order.
	require.True(t, strings.HasPrefix(header,
		"t_s,node,iops_read,iops_write,bytes_read,bytes_write"),
		"unexpected header prefix: %s", header)
	// Per-bucket columns appear in sorted order for both reads and writes.
	require.Contains(t, header, "rpc_read_PARTI_HANDOFF")
	require.Contains(t, header, "rpc_read_PARTI_HEARTBEAT")
	require.Contains(t, header, "rpc_write_PARTI_HEARTBEAT")
	require.Contains(t, header, "stream_msgs_PARTI_HEARTBEAT")
	require.Contains(t, header, "stream_bytes_PARTI_DATA")

	// Expect host row and per-container rows present.
	var sawHost bool
	containers := map[string]bool{}
	for _, l := range lines[1:] {
		fields := strings.Split(l, ",")
		switch fields[1] {
		case "host":
			sawHost = true
		default:
			if strings.HasPrefix(fields[1], "iops-nats-") {
				containers[fields[1]] = true
			}
		}
	}
	require.True(t, sawHost, "missing host row")
	require.Len(t, containers, 3, "expected 3 per-container rows present")
}

func TestRun_DivergenceStrictFails(t *testing.T) {
	dir := t.TempDir()
	fx := happyFixture()
	// Inflate iostat by 25 % so the cross-check trips well past 5 %.
	fx.iostatRIOsPerSec *= 1.25
	fx.iostatWIOsPerSec *= 1.25
	fx.materialize(t, dir)

	err := run([]string{"--run-dir", dir}, os.Stdout, os.Stderr)
	require.Error(t, err)
	require.Contains(t, err.Error(), "divergence")
}

func TestRun_DivergenceNonStrictPasses(t *testing.T) {
	dir := t.TempDir()
	fx := happyFixture()
	fx.iostatRIOsPerSec *= 1.25
	fx.iostatWIOsPerSec *= 1.25
	fx.materialize(t, dir)

	err := run([]string{"--run-dir", dir, "--strict=false"}, os.Stdout, os.Stderr)
	require.NoError(t, err)
	_, statErr := os.Stat(filepath.Join(dir, "aggregated.csv"))
	require.NoError(t, statErr, "csv must be written when divergence is non-fatal")
}

func TestRun_DivergenceCheckActuallyDistinguishes(t *testing.T) {
	// Sanity: at strict 0% tolerance even the happy fixture trips.
	// Confirms the cross-check is not vacuously passing because the
	// fixture is too small.
	dir := t.TempDir()
	fx := happyFixture()
	// Add a tiny ~1% perturbation that is visible only when the
	// tolerance is squeezed below 1%.
	fx.iostatRIOsPerSec *= 1.01
	fx.materialize(t, dir)

	err := run([]string{"--run-dir", dir, "--max-disagreement-pct", "0.1"}, os.Stdout, os.Stderr)
	require.Error(t, err, "0.1% tolerance must fail a 1% perturbation")
	require.Contains(t, err.Error(), "divergence")

	// And the same fixture passes at the default 5%.
	err = run([]string{"--run-dir", dir}, os.Stdout, os.Stderr)
	require.NoError(t, err)
}

func TestRun_RefusesDegradedManifest(t *testing.T) {
	dir := t.TempDir()
	fx := happyFixture()
	fx.manifestStatus = "degraded"
	fx.materialize(t, dir)

	err := run([]string{"--run-dir", dir}, os.Stdout, os.Stderr)
	require.Error(t, err)
	require.Contains(t, err.Error(), "degraded")
}

func TestRun_RefusesMissingRPCCounts(t *testing.T) {
	dir := t.TempDir()
	fx := happyFixture()
	fx.skip = map[string]bool{"rpc_counts.csv": true}
	fx.materialize(t, dir)

	err := run([]string{"--run-dir", dir}, os.Stdout, os.Stderr)
	require.Error(t, err)
	require.Contains(t, err.Error(), "rpc_counts.csv")
}

func TestRun_RefusesMissingManifest(t *testing.T) {
	dir := t.TempDir()
	fx := happyFixture()
	fx.skip = map[string]bool{"manifest.yaml": true}
	fx.materialize(t, dir)

	err := run([]string{"--run-dir", dir}, os.Stdout, os.Stderr)
	require.Error(t, err)
	require.Contains(t, err.Error(), "manifest.yaml")
}

func TestRun_SparseRPCBecomesZero(t *testing.T) {
	// A fixture with no rpc rows at all produces rpc_* columns absent
	// (no buckets observed). A fixture with rows in only some windows
	// produces zeros for the windows without growth — RPCBucketRates
	// forward-fills only the windows the delta covers.
	dir := t.TempDir()
	fx := happyFixture()
	// Strip rpc ops entirely; bucket-derived columns disappear from
	// the header but every per-second row is still present (from
	// cgroup + jsz). This is the "absent rows mean 0" contract.
	fx.rpcOps = map[rpcKey]int64{}
	fx.materialize(t, dir)

	require.NoError(t, run([]string{"--run-dir", dir}, os.Stdout, os.Stderr))
	out, err := os.ReadFile(filepath.Join(dir, "aggregated.csv"))
	require.NoError(t, err)
	header := strings.Split(string(out), "\n")[0]
	require.NotContains(t, header, "rpc_read_", "no rpc rows → no rpc_ columns")
	require.NotContains(t, header, "rpc_write_")
}

// TestRun_M1_0ControlSyntheticInputs guards the Phase 3 P0-2 fix: M1.0
// control runs in run-matrix.sh synthesize a minimal manifest.yaml
// (status=ok, partiVersion=control) and a header-only rpc_counts.csv
// so the aggregator can produce an aggregated.csv even though no
// harness ran. This test simulates that on-disk layout and asserts a
// clean aggregate is written with no rpc_* columns.
func TestRun_M1_0ControlSyntheticInputs(t *testing.T) {
	dir := t.TempDir()
	fx := happyFixture()
	// No rpc ops at all (control run); use the manifest synth path.
	fx.rpcOps = map[rpcKey]int64{}
	fx.skip = map[string]bool{
		"manifest.yaml":  true,
		"rpc_counts.csv": true,
	}
	fx.materialize(t, dir)

	// Exactly what run-matrix.sh writes for M1.0 control runs.
	manifest := "status: ok\ncell: M1.0\npartiVersion: control\nrunIndex: 1\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o644))
	// Header-only rpc_counts.csv — same shape as the harness writes but
	// without any data rows.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rpc_counts.csv"),
		[]byte("t_unix_ns,worker_idx,bucket,op,count\n"), 0o644))

	require.NoError(t, run([]string{"--run-dir", dir}, os.Stdout, os.Stderr))
	out, err := os.ReadFile(filepath.Join(dir, "aggregated.csv"))
	require.NoError(t, err)
	header := strings.Split(string(out), "\n")[0]
	require.NotContains(t, header, "rpc_read_", "M1.0 control: no rpc rows → no rpc_ columns")
	require.NotContains(t, header, "rpc_write_")
	// Cgroup/iostat content is still aggregated normally.
	require.Contains(t, header, "iops_read")
	require.Contains(t, header, "bytes_write")
}

// TestRun_SparseFirstObservedDiffsAgainstCaptureStart guards the Phase 3
// P0 fix: when a (worker, bucket, op) tuple is absent from the initial
// snapshot (Reset()→first writeSnapshot) and first appears in a later
// snapshot with count=K>0, the aggregator must treat the missed interval
// as K ops over the gap, not silently skip K. We materialize a fixture
// that omits ("PARTI_HANDOFF","Put") from the first snapshot and emits
// count=10 at i=2, count=20 at i=3. Without the synthetic capture-start
// anchor, the i=2 row is treated as a baseline and the 10 ops over the
// (capture_start, t=2s] interval are dropped from rpc_write_PARTI_HANDOFF.
func TestRun_SparseFirstObservedDiffsAgainstCaptureStart(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 5, 16, 12, 0, 0, 0, time.Local)

	// Build a minimal but complete capture set. Cgroup/iostat/jsz/node_exporter
	// content is borrowed from happyFixture's logic; the only thing that
	// matters for this test is the rpc_counts.csv shape.
	fx := happyFixture()
	// Strip rpcOps from the fixture's auto-materialize: we write the RPC
	// CSV by hand.
	fx.rpcOps = nil
	fx.skip = map[string]bool{"rpc_counts.csv": true}
	fx.materialize(t, dir)

	// Hand-roll rpc_counts.csv:
	//   - At i=0 (capture-start tick), emit only ("PARTI_HEARTBEAT","Put")
	//     with count=0 — so PARTI_HANDOFF/Put is absent at the first snapshot.
	//   - At i=1, still no PARTI_HANDOFF/Put row.
	//   - At i=2, emit ("PARTI_HANDOFF","Put") with count=10 for the first time.
	//   - At i=3, emit ("PARTI_HANDOFF","Put") with count=20.
	//   - Continue PARTI_HEARTBEAT/Put through all 4 ticks so the
	//     header has at least one familiar column.
	var sb strings.Builder
	sb.WriteString("t_unix_ns,worker_idx,bucket,op,count\n")
	for i := range 4 {
		ts := base.Add(time.Duration(i) * time.Second).UnixNano()
		// HEARTBEAT/Put present at every tick (cumulative 0, 1, 2, 3).
		fmt.Fprintf(&sb, "%d,0,%s,%s,%d\n", ts, "PARTI_HEARTBEAT", "Put", int64(i))
		// HANDOFF/Put only appears at i>=2.
		if i >= 2 {
			fmt.Fprintf(&sb, "%d,0,%s,%s,%d\n", ts, "PARTI_HANDOFF", "Put", int64(10*(i-1)))
		}
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rpc_counts.csv"), []byte(sb.String()), 0o644))

	require.NoError(t, run([]string{"--run-dir", dir}, os.Stdout, os.Stderr))
	out, err := os.ReadFile(filepath.Join(dir, "aggregated.csv"))
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	header := lines[0]
	cols := strings.Split(header, ",")
	colIdx := func(name string) int {
		for i, c := range cols {
			if c == name {
				return i
			}
		}
		return -1
	}
	idxWriteHandoff := colIdx("rpc_write_PARTI_HANDOFF")
	require.GreaterOrEqual(t, idxWriteHandoff, 0, "rpc_write_PARTI_HANDOFF column missing; header=%s", header)

	// Sum the host-row rpc_write_PARTI_HANDOFF column across all seconds.
	// Without the fix it would be 10 (only the i=2→i=3 diff counted).
	// With the fix it's 20 (the full count=20 cumulative at i=3, attributed
	// across (capture_start, t=3s]).
	var totalHandoffWrites float64
	for _, l := range lines[1:] {
		f := strings.Split(l, ",")
		if f[1] != "host" {
			continue
		}
		if f[idxWriteHandoff] == "" {
			continue
		}
		v, perr := strconv.ParseFloat(f[idxWriteHandoff], 64)
		require.NoError(t, perr)
		totalHandoffWrites += v
	}
	// Expect 20 ops total (the cumulative count at i=3). Pre-fix value
	// would be 10 (only the i=2→i=3 increment).
	require.InDelta(t, 20.0, totalHandoffWrites, 0.01,
		"first-observed sparse interval was dropped; got total=%v expected 20", totalHandoffWrites)
}

func TestRun_MultiNodeRowsPresent(t *testing.T) {
	// Already exercised by the happy fixture (3 containers + host),
	// but check explicitly: every t_sec emits a host row and 3 container rows.
	dir := t.TempDir()
	happyFixture().materialize(t, dir)
	require.NoError(t, run([]string{"--run-dir", dir}, os.Stdout, os.Stderr))
	out, err := os.ReadFile(filepath.Join(dir, "aggregated.csv"))
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")[1:]
	// Group rows by t_sec.
	bySec := map[string][]string{}
	for _, l := range lines {
		fields := strings.Split(l, ",")
		bySec[fields[0]] = append(bySec[fields[0]], fields[1])
	}
	require.NotEmpty(t, bySec)
	for sec, nodes := range bySec {
		require.Contains(t, nodes, "host", "t_sec=%s missing host row", sec)
		require.GreaterOrEqual(t, len(nodes), 2, "t_sec=%s expected ≥1 container + host", sec)
	}
}
