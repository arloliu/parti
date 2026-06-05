package main

// TestE2E_HarnessToAggregatedCSV exercises the full harness → aggregate →
// CSV pipeline at tiny scale with an embedded NATS server and synthetic
// capture fixtures.
//
// Design rationale:
//   - The harness produces only manifest.yaml and rpc_counts.csv during a
//     real run; cgroup_io.raw, iostat.raw, jsz.raw, and node_exporter.prom
//     require a live host rig. This test synthesises those four files from
//     a fixture that passes the strict cgroup-vs-iostat cross-check, then
//     calls the internal/aggregate package directly to complete the pipeline.
//   - We do NOT add a --skip-disk-cross-check flag to the aggregator: that
//     would weaken the production operator's safety net. Synthetic fixtures
//     that genuinely agree are the correct solution.
//   - cmd/aggregate/run() is package-private and lives in a separate binary
//     package. Rather than crossing the package boundary, this test replays
//     the ~30-line orchestration from that function inline, calling only the
//     exported internal/aggregate API.
//
// Success criteria (§01-implementation-strategy.md Phase 5 "done means"):
//  1. Run() returns nil — harness lifecycle completed without degraded workers.
//  2. manifest.yaml exists and has status="ok".
//  3. rpc_counts.csv exists and has at least one data row.
//  4. aggregated.csv exists, has the required column prefix, and contains
//     at least one host row.

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/test/perf-measurement/internal/aggregate"
)

// syntheticCaptureFixture writes the four capture files that the harness
// does NOT produce (cgroup_io.raw, iostat.raw, jsz.raw, node_exporter.prom)
// into dir.  The cgroup and iostat numbers agree exactly so the aggregator's
// strict divergence check passes.  The jsz file has a single poll point so
// WriteCSV emits stream_* columns.
func syntheticCaptureFixture(t *testing.T, dir string) {
	t.Helper()
	// Use a fixed base time so the fixture is deterministic.
	base := time.Date(2026, 5, 16, 12, 0, 0, 0, time.Local)
	const seconds = 5

	// cgroup_io.raw — cumulative counters, one container, one device.
	// 4 read ops/s and 6 write ops/s → totals match iostat exactly.
	{
		var b strings.Builder
		b.WriteString("# t_unix_ns container device rbytes wbytes rios wios\n")
		for i := range seconds + 1 {
			tsNs := base.Add(time.Duration(i) * time.Second).UnixNano()
			rios := uint64(4) * uint64(i)
			wios := uint64(6) * uint64(i)
			rbytes := uint64(4096) * uint64(i)
			wbytes := uint64(8192) * uint64(i)
			fmt.Fprintf(&b, "%d perf-nats-1 259:0 %d %d %d %d\n", tsNs, rbytes, wbytes, rios, wios)
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup_io.raw"), []byte(b.String()), 0o644))
	}

	// iostat.raw — one since-boot block (discarded) then one block per second.
	// Splits the same 4 r/s and 6 w/s across two devices so the parser sums them.
	{
		var b strings.Builder
		b.WriteString("# iostat -x -d -t 1\n")
		// Since-boot block.
		fmt.Fprintf(&b, "%s\n", base.Format("01/02/2006 03:04:05 PM"))
		b.WriteString("Device r/s w/s\nsda 0 0\n\n")
		for i := 1; i <= seconds; i++ {
			ts := base.Add(time.Duration(i) * time.Second)
			fmt.Fprintf(&b, "%s\n", ts.Format("01/02/2006 03:04:05 PM"))
			b.WriteString("Device r/s w/s\n")
			fmt.Fprintf(&b, "sda %.2f %.2f\n", 2.0, 3.0) // half on sda
			fmt.Fprintf(&b, "sdb %.2f %.2f\n", 2.0, 3.0) // half on sdb
			b.WriteString("\n")
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, "iostat.raw"), []byte(b.String()), 0o644))
	}

	// jsz.raw — one ndjson poll point so WriteCSV can produce stream_* columns.
	{
		tsNs := base.UnixNano()
		line := fmt.Sprintf(
			`{"t_unix_ns":%d,"node":"localhost:8222","endpoint":"jsz","body":{"account_details":[{"stream_detail":[{"name":"PARTI_HEARTBEAT","state":{"messages":10,"bytes":512}}]}]}}`+"\n",
			tsNs,
		)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "jsz.raw"), []byte(line), 0o644))
	}

	// node_exporter.prom — only checked for presence; minimal content.
	{
		content := fmt.Sprintf("# t_unix_ns %d\nnode_disk_reads_completed_total{device=\"sda\"} 0\n", base.UnixNano())
		require.NoError(t, os.WriteFile(filepath.Join(dir, "node_exporter.prom"), []byte(content), 0o644))
	}
}

func TestE2E_HarnessToAggregatedCSV(t *testing.T) {
	if testing.Short() {
		t.Skip("smoke: skipping in -short mode")
	}

	// 1. Start an embedded NATS server (reuses the helper in main_test.go).
	url := startEmbeddedNATS(t)
	outDir := t.TempDir()

	// 2. Run the harness at minimal scale.  FastConfig is required so the
	//    embedded server clears parti's cold-start window in the 2 s warmup.
	o := Options{
		NATSURLs:           url,
		Workers:            2,
		N:                  10,
		Replicas:           1,
		ConsumerMode:       ConsumerModeDynamic,
		FetchTimeout:       2 * time.Second,
		HeartbeatInterval:  500 * time.Millisecond,
		HeartbeatTTL:       1500 * time.Millisecond,
		WorkerIDTTL:        5 * time.Second,
		ElectionTimeout:    2 * time.Second,
		KVStorage:          jetstream.MemoryStorage,
		DataStorage:        jetstream.MemoryStorage,
		DataStreamName:     "perf-rig-data",
		PartitionSourceKey: DefaultPartitionSourceKey,
		Warmup:             2 * time.Second,
		CaptureWindow:      2 * time.Second,
		RPCDumpInterval:    200 * time.Millisecond,
		OutputDir:          outDir,
		PartiVersion:       "e2e-smoke",
		FastConfig:         true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	require.NoError(t, Run(ctx, o, os.Stderr), "harness Run must succeed")

	// 3. Verify harness artifacts.
	manifestPath := filepath.Join(outDir, "manifest.yaml")
	rpcPath := filepath.Join(outDir, "rpc_counts.csv")

	_, err := os.Stat(manifestPath)
	require.NoError(t, err, "manifest.yaml must exist after Run")
	_, err = os.Stat(rpcPath)
	require.NoError(t, err, "rpc_counts.csv must exist after Run")

	// Confirm rpc_counts.csv has at least one data row (header + ≥1 row).
	rpcF, err := os.Open(rpcPath)
	require.NoError(t, err)
	defer rpcF.Close()
	rpcRows, err := csv.NewReader(rpcF).ReadAll()
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(rpcRows), 2, "rpc_counts.csv must have header + at least one data row")

	// 4. Synthesise the four capture files the harness doesn't produce.
	syntheticCaptureFixture(t, outDir)

	// 5. Run the aggregate pipeline inline (mirrors cmd/aggregate/main.go:run
	//    but calls internal/aggregate directly to avoid the package boundary).
	const warmupSec = 2
	const maxPct = 5.0

	manifest, err := aggregate.ParseManifest(manifestPath)
	require.NoError(t, err)
	require.Equal(t, "ok", manifest.Status, "manifest status must be ok")

	cgSamples, err := aggregate.ParseCgroup(filepath.Join(outDir, "cgroup_io.raw"))
	require.NoError(t, err)
	iosSamples, err := aggregate.ParseIostat(filepath.Join(outDir, "iostat.raw"))
	require.NoError(t, err)
	jszSamples, err := aggregate.ParseJSZ(filepath.Join(outDir, "jsz.raw"))
	require.NoError(t, err)
	rpcRowsParsed, err := aggregate.ParseRPC(rpcPath)
	require.NoError(t, err)

	cgDeltas := aggregate.CgroupDeltas(cgSamples)
	jszRates := aggregate.JSZRates(jszSamples)
	captureStartNs := aggregate.CaptureStartNs(rpcRowsParsed)
	rpcRates, unknownOps := aggregate.RPCBucketRates(rpcRowsParsed, captureStartNs)
	if len(unknownOps) > 0 {
		t.Logf("aggregate: warning: %d unknown op name(s) classified as writes: %v", len(unknownOps), unknownOps)
	}

	div := aggregate.DivergenceCheck(cgDeltas, iosSamples, warmupSec)
	if div.ComparedSeconds == 0 {
		t.Log("aggregate: no overlapping seconds between cgroup and iostat after warmup; cross-check skipped")
	} else {
		t.Logf("aggregate: cgroup vs iostat: max |Δ|/max = %.2f%% over %d compared seconds",
			div.MaxPctOver*100, div.ComparedSeconds)
		if div.MaxPctOver*100 > maxPct {
			t.Fatalf("cgroup/iostat divergence %.2f%% exceeds %.1f%%: %v",
				div.MaxPctOver*100, maxPct, aggregate.FailfDivergence(div, maxPct))
		}
	}

	outPath := filepath.Join(outDir, "aggregated.csv")
	outFile, err := os.Create(outPath)
	require.NoError(t, err)
	defer outFile.Close()

	data := aggregate.RunData{
		CgroupDeltas: cgDeltas,
		IostatSamps:  iosSamples,
		JSZRates:     jszRates,
		RPCRates:     rpcRates,
		UnknownOps:   unknownOps,
	}
	require.NoError(t, aggregate.WriteCSV(outFile, data))
	require.NoError(t, outFile.Sync())

	// 6. Verify aggregated.csv structure.
	raw, err := os.ReadFile(outPath)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	require.GreaterOrEqual(t, len(lines), 2, "aggregated.csv must have header + at least one row")

	header := lines[0]
	require.True(t,
		strings.HasPrefix(header, "t_s,node,iops_read,iops_write,bytes_read,bytes_write"),
		"aggregated.csv header must start with the required column prefix; got: %s", header)

	var sawHost bool
	for _, l := range lines[1:] {
		fields := strings.SplitN(l, ",", 3)
		if len(fields) >= 2 && fields[1] == "host" {
			sawHost = true
			break
		}
	}
	require.True(t, sawHost, "aggregated.csv must contain at least one host row")
}
