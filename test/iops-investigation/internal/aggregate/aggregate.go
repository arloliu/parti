package aggregate

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
	"strconv"
)

// HostNode is the synthetic node name used for rows that hold the
// cluster-wide RPC totals. Phase 3 / 6 readers can rely on this string
// being stable.
const HostNode = "host"

// RunData holds the parsed per-source inputs for one run dir. The
// fields are exported so callers (CLI + tests) can construct it
// directly when desired.
type RunData struct {
	CgroupDeltas []CgroupDelta
	IostatSamps  []IostatSample
	JSZRates     []StreamRate
	RPCRates     []BucketRate
	UnknownOps   []string
}

// DivergenceReport describes the cgroup-vs-iostat cross-check.
// Per the plan §R3, this is the load-bearing safety property of the
// aggregator: silent mismatch invalidates attribution.
type DivergenceReport struct {
	MaxPctOver           float64 // worst per-second |cg-io|/cg ratio (0..1) after warmup
	WorstTSec            int64
	WorstCgroupTotal     float64
	WorstIostatTotal     float64
	SkippedWarmupSeconds int
	ComparedSeconds      int
}

// DivergenceCheck computes per-second total IOPS from cgroup
// (Σ across containers of read+write) and from iostat (Σ across host
// devices of r/s+w/s) at the same second, ignoring the first
// `warmupSec` of the run. Returns the worst ratio observed.
//
// Device-identity caveat: iostat reports device names (e.g. sda) and
// cgroup reports MAJ:MIN — we don't have a portable mapping at parse
// time, so this compares ALL-device totals on both sides. On a quiet
// rig (the campaign assumption) these track within a few percent. On a
// noisy host this will fail loudly — which is the desired behavior:
// see plan §R4 (run hygiene).
func DivergenceCheck(cg []CgroupDelta, ios []IostatSample, warmupSec int) DivergenceReport {
	// Sum cgroup per t_sec.
	cgTotal := map[int64]float64{}
	for _, d := range cg {
		cgTotal[d.TSec] += d.IOPSRead + d.IOPSWrite
	}
	iosTotal := map[int64]float64{}
	for _, s := range ios {
		iosTotal[s.TSec] += s.IOPSRead + s.IOPSWrite
	}
	// Determine the earliest second present in cgroup → defines the
	// warmup window we skip.
	var (
		minT  int64
		first = true
	)
	for t := range cgTotal {
		if first || t < minT {
			minT = t
			first = false
		}
	}
	rep := DivergenceReport{SkippedWarmupSeconds: warmupSec}
	for t, cgV := range cgTotal {
		if t-minT < int64(warmupSec) {
			continue
		}
		iosV, ok := iosTotal[t]
		if !ok {
			continue
		}
		rep.ComparedSeconds++
		// Use max(cg, ios) as the denominator so a tiny cgroup total
		// doesn't blow the ratio to infinity when both are small noise.
		denom := math.Max(cgV, iosV)
		if denom < 1.0 {
			continue
		}
		pct := math.Abs(cgV-iosV) / denom
		if pct > rep.MaxPctOver {
			rep.MaxPctOver = pct
			rep.WorstTSec = t
			rep.WorstCgroupTotal = cgV
			rep.WorstIostatTotal = iosV
		}
	}
	return rep
}

// WriteCSV writes the per-run aggregated CSV. The column order is:
//
//	t_s, node,
//	iops_read, iops_write, bytes_read, bytes_write,
//	rpc_read_<bucket>...   (sorted by bucket, host rows only)
//	rpc_write_<bucket>...  (sorted by bucket, host rows only)
//	stream_msgs_<name>...  (sorted by stream, host rows only)
//	stream_bytes_<name>... (sorted by stream, host rows only)
//
// Per-container rows (`node=<container>`) carry only iops_*/bytes_* and
// have empty cells for the rpc_* and stream_* columns. Cluster-wide
// rows (`node="host"`) carry rpc_* and stream_* rates and have empty
// cells for iops_*/bytes_* (those are per-container by construction).
//
// One row per (t_sec, node). Within a second, the host row is emitted
// last for readability.
func WriteCSV(w io.Writer, data RunData) error {
	// Discover dimension members.
	bucketSet := map[string]bool{}
	for _, r := range data.RPCRates {
		bucketSet[r.Bucket] = true
	}
	streamSet := map[string]bool{}
	for _, r := range data.JSZRates {
		streamSet[r.Stream] = true
	}
	containerSet := map[string]bool{}
	tSecSet := map[int64]bool{}
	for _, d := range data.CgroupDeltas {
		containerSet[d.Container] = true
		tSecSet[d.TSec] = true
	}
	for _, r := range data.RPCRates {
		tSecSet[r.TSec] = true
	}
	for _, r := range data.JSZRates {
		tSecSet[r.TSec] = true
	}
	buckets := sortedKeys(bucketSet)
	streams := sortedKeys(streamSet)
	containers := sortedKeys(containerSet)
	tSecs := make([]int64, 0, len(tSecSet))
	for t := range tSecSet {
		tSecs = append(tSecs, t)
	}
	slices.Sort(tSecs)

	// Build the header.
	header := []string{"t_s", "node", "iops_read", "iops_write", "bytes_read", "bytes_write"}
	for _, b := range buckets {
		header = append(header, "rpc_read_"+b)
	}
	for _, b := range buckets {
		header = append(header, "rpc_write_"+b)
	}
	for _, s := range streams {
		header = append(header, "stream_msgs_"+s)
	}
	for _, s := range streams {
		header = append(header, "stream_bytes_"+s)
	}

	// Index cgroup by (tSec, container) for O(1) lookup.
	cgIdx := map[int64]map[string]CgroupDelta{}
	for _, d := range data.CgroupDeltas {
		if cgIdx[d.TSec] == nil {
			cgIdx[d.TSec] = map[string]CgroupDelta{}
		}
		cgIdx[d.TSec][d.Container] = d
	}
	// Index rpc by (tSec, bucket).
	rpcIdx := map[int64]map[string]BucketRate{}
	for _, r := range data.RPCRates {
		if rpcIdx[r.TSec] == nil {
			rpcIdx[r.TSec] = map[string]BucketRate{}
		}
		rpcIdx[r.TSec][r.Bucket] = r
	}
	// Index jsz by (tSec, stream).
	jszIdx := map[int64]map[string]StreamRate{}
	for _, r := range data.JSZRates {
		if jszIdx[r.TSec] == nil {
			jszIdx[r.TSec] = map[string]StreamRate{}
		}
		jszIdx[r.TSec][r.Stream] = r
	}

	cw := csv.NewWriter(w)
	if err := cw.Write(header); err != nil {
		return err
	}

	for _, t := range tSecs {
		// Per-container rows.
		for _, c := range containers {
			d, ok := cgIdx[t][c]
			if !ok {
				continue
			}
			row := make([]string, len(header))
			row[0] = strconv.FormatInt(t, 10)
			row[1] = c
			row[2] = formatFloat(d.IOPSRead)
			row[3] = formatFloat(d.IOPSWrite)
			row[4] = formatFloat(d.BytesRead)
			row[5] = formatFloat(d.BytesWrt)
			// Remaining cells (rpc_*, stream_*) left empty per spec.
			if err := cw.Write(row); err != nil {
				return err
			}
		}
		// Host row (always emitted at every t_sec; carries cluster-wide signals).
		row := make([]string, len(header))
		row[0] = strconv.FormatInt(t, 10)
		row[1] = HostNode
		// iops_*/bytes_* intentionally empty on the host row.
		col := 6
		for _, b := range buckets {
			if r, ok := rpcIdx[t][b]; ok {
				row[col] = formatFloat(r.ReadOps)
			} else {
				row[col] = "0"
			}
			col++
		}
		for _, b := range buckets {
			if r, ok := rpcIdx[t][b]; ok {
				row[col] = formatFloat(r.WriteOps)
			} else {
				row[col] = "0"
			}
			col++
		}
		for _, s := range streams {
			if r, ok := jszIdx[t][s]; ok {
				row[col] = formatFloat(r.MsgsRate)
			} else {
				row[col] = "0"
			}
			col++
		}
		for _, s := range streams {
			if r, ok := jszIdx[t][s]; ok {
				row[col] = formatFloat(r.BytesRt)
			} else {
				row[col] = "0"
			}
			col++
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// formatFloat formats x with up to 6 significant digits, trimming
// trailing zeros so golden CSVs stay readable. Integers stay integral
// (e.g. "0" not "0.000000").
func formatFloat(x float64) string {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return "0"
	}
	if x == math.Trunc(x) && math.Abs(x) < 1e15 {
		return strconv.FormatInt(int64(x), 10)
	}
	return strconv.FormatFloat(x, 'g', 6, 64)
}

// FailfDivergence formats a DivergenceReport into an error message for
// the strict-mode loud failure.
func FailfDivergence(d DivergenceReport, maxPct float64) error {
	return fmt.Errorf("cgroup vs iostat divergence exceeded %.1f%% at t_sec=%d: cgroup_total=%.1f iops, iostat_total=%.1f iops (|Δ|/max = %.1f%%); compared %d seconds, skipped %d warmup",
		maxPct, d.WorstTSec, d.WorstCgroupTotal, d.WorstIostatTotal, d.MaxPctOver*100, d.ComparedSeconds, d.SkippedWarmupSeconds)
}
