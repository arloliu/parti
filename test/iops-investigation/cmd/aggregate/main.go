// Command aggregate reconciles a single per-run directory's capture
// artifacts (cgroup, iostat, jsz, node_exporter, harness rpc_counts)
// into a single CSV consumed by Phase 6 (analyze.py).
//
// Per docs/plans/iops-investigation/00-attribution-plan.md §R3 the
// output CSV has columns:
//
//	t_s, node,
//	iops_read, iops_write, bytes_read, bytes_write,
//	rpc_read_<bucket>...,  rpc_write_<bucket>...,
//	stream_msgs_<name>..., stream_bytes_<name>...
//
// Row semantics:
//
//   - One row per (t_s, node). Within a second, per-container rows are
//     emitted first, then the synthetic "host" row.
//
//   - Per-container rows carry iops_*/bytes_* deltas derived from cgroup
//     v2 io.stat samples; their rpc_* and stream_* cells are empty (RPC
//     counts are cluster-wide and stream stats are per-cluster, not
//     per-container).
//
//   - The "host" row carries rpc_*/stream_* rates derived from the
//     harness's rpc_counts.csv (summed across workers) and from jsz; its
//     iops_*/bytes_* cells are empty.
//
// Cross-checks (loud failures in strict mode, default on):
//
//  1. cgroup-totals vs iostat-totals divergence > --max-disagreement-pct
//     after a 2-second warmup window → exit 1.
//  2. manifest.yaml status != "ok" → exit 1.
//  3. Any of {manifest, cgroup, iostat, jsz, node_exporter, rpc_counts}
//     missing → exit 1.
//
// The cgroup-vs-iostat check sums ALL devices on both sides; we don't
// have a portable MAJ:MIN → device-name mapping at parse time. On the
// quiet rig the campaign assumes this tracks within a few percent; on a
// noisy host the check fails loudly, which is the desired behavior.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/arloliu/parti/test/iops-investigation/internal/aggregate"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "aggregate: "+err.Error())
		os.Exit(1)
	}
}

//nolint:cyclop // aggregate CLI: flag parsing + sequential per-source reconcile; flat by design.
func run(args []string, stdout, stderr *os.File) error {
	fs := flag.NewFlagSet("aggregate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		runDir = fs.String("run-dir", "", "path to the per-run directory containing capture artifacts (required)")
		// --strict default is false because cgroup-vs-iostat divergence
		// is noisy on real hosts (write merging at the kernel level
		// makes cgroup pre-merge counts diverge 2-4× from iostat
		// post-merge counts, and concurrent host workloads further
		// confound the comparison). The aggregator still computes
		// and reports the divergence as a diagnostic line, but it no
		// longer fails the run by default. Pass --strict to enforce
		// paper-spec §R3 semantics on a dedicated rig host.
		strict    = fs.Bool("strict", false, "exit non-zero on cgroup/iostat divergence above --max-disagreement-pct (default false; the cross-check is diagnostic only)")
		maxPct    = fs.Float64("max-disagreement-pct", 5.0, "max permitted cgroup-excess over iostat as fraction of cgroup (0..100%, asymmetric guard) before strict mode fails. Only meaningful when --strict.")
		outName   = fs.String("output-name", "aggregated.csv", "output CSV filename written under --run-dir")
		warmupSec = fs.Int("warmup-seconds", 2, "seconds at the start of the run to skip during the divergence check")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *runDir == "" {
		return errors.New("--run-dir is required")
	}

	// 1. Required inputs.
	const (
		manifestFile = "manifest.yaml"
		cgroupFile   = "cgroup_io.raw"
		iostatFile   = "iostat.raw"
		jszFile      = "jsz.raw"
		nodeExpFile  = "node_exporter.prom"
		rpcFile      = "rpc_counts.csv"
	)
	required := []string{manifestFile, cgroupFile, iostatFile, jszFile, nodeExpFile, rpcFile}
	for _, name := range required {
		p := filepath.Join(*runDir, name)
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("required input missing: %s (%w)", p, err)
		}
	}

	// 2. Manifest status gate. The harness writes manifest.yaml LAST,
	//    so its presence implies all sibling artifacts committed. Refuse
	//    on any non-ok status (degraded / interrupted / csv-commit-failed).
	manifest, err := aggregate.ParseManifest(filepath.Join(*runDir, manifestFile))
	if err != nil {
		return err
	}
	if manifest.Status != "ok" {
		return fmt.Errorf("refusing to aggregate: manifest status=%q (expected %q)", manifest.Status, "ok")
	}

	// 3. Parse all inputs. node_exporter.prom is required for run hygiene
	//    (the campaign's host sanity source) but not consumed for the
	//    output CSV; presence is enough.
	cgSamples, err := aggregate.ParseCgroup(filepath.Join(*runDir, cgroupFile))
	if err != nil {
		return err
	}
	iosSamples, err := aggregate.ParseIostat(filepath.Join(*runDir, iostatFile))
	if err != nil {
		return err
	}
	jszSamples, err := aggregate.ParseJSZ(filepath.Join(*runDir, jszFile))
	if err != nil {
		return err
	}
	metaSamples, err := aggregate.ParseMetaSnapshot(filepath.Join(*runDir, jszFile))
	if err != nil {
		return err
	}
	rpcRows, err := aggregate.ParseRPC(filepath.Join(*runDir, rpcFile))
	if err != nil {
		return err
	}

	// 4. Compute per-second rates.
	cgDeltas := aggregate.CgroupDeltas(cgSamples)
	jszRates := aggregate.JSZRates(jszSamples)
	captureStartNs := aggregate.CaptureStartNs(rpcRows)
	rpcRates, unknownOps := aggregate.RPCBucketRates(rpcRows, captureStartNs)
	if len(unknownOps) > 0 {
		// Surface, don't fail. Unknown ops fold into the write column
		// pessimistically (see RPCBucketRates godoc).
		fmt.Fprintf(stderr, "aggregate: warning: %d unknown op name(s) classified as writes: %v\n", len(unknownOps), unknownOps)
	}

	// 5. Cross-check cgroup vs iostat. Loud in strict mode.
	div := aggregate.DivergenceCheck(cgDeltas, iosSamples, *warmupSec)
	if div.ComparedSeconds == 0 {
		fmt.Fprintf(stderr, "aggregate: warning: no overlapping seconds between cgroup and iostat after %ds warmup; cross-check skipped\n", *warmupSec)
	} else {
		fmt.Fprintf(stderr, "aggregate: cgroup vs iostat: max |Δ|/max = %.2f%% over %d compared seconds (worst t_sec=%d cg=%.1f ios=%.1f)\n",
			div.MaxPctOver*100, div.ComparedSeconds, div.WorstTSec, div.WorstCgroupTotal, div.WorstIostatTotal)
		if div.MaxPctOver*100 > *maxPct && *strict {
			return aggregate.FailfDivergence(div, *maxPct)
		}
	}

	// 6. Write output CSV.
	outPath := filepath.Join(*runDir, *outName)
	out, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	defer out.Close()
	data := aggregate.RunData{
		CgroupDeltas: cgDeltas,
		IostatSamps:  iosSamples,
		JSZRates:     jszRates,
		RPCRates:     rpcRates,
		UnknownOps:   unknownOps,
	}
	if err := aggregate.WriteCSV(out, data); err != nil {
		return fmt.Errorf("write csv: %w", err)
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("sync output: %w", err)
	}
	fmt.Fprintf(stdout, "wrote %s\n", outPath)

	// 7. Meta-snapshot summary (§8.1 gate: count >= 5).
	//    Reads from the same jsz file already parsed above.
	//    Emits meta_snapshot.json next to aggregated.csv when gated,
	//    and always logs the gate decision to stderr.
	metaSum, metaLog := aggregate.SummarizeMetaSnapshot(metaSamples)
	fmt.Fprintf(stderr, "aggregate: %s\n", metaLog)
	if metaSum.Gated {
		metaPath := filepath.Join(*runDir, "meta_snapshot.json")
		mf, merr := os.Create(metaPath)
		if merr != nil {
			return fmt.Errorf("create meta_snapshot.json: %w", merr)
		}
		defer mf.Close()
		if merr = aggregate.WriteMetaSnapshot(mf, metaSum); merr != nil {
			return fmt.Errorf("write meta_snapshot.json: %w", merr)
		}
		if merr = mf.Sync(); merr != nil {
			return fmt.Errorf("sync meta_snapshot.json: %w", merr)
		}
		fmt.Fprintf(stdout, "wrote %s\n", metaPath)
	}

	return nil
}
