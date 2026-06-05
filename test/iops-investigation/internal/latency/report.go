package latency

import hdr "github.com/HdrHistogram/hdrhistogram-go"

// Report is the per-cell latency summary (design §6). Percentiles are in
// nanoseconds. A percentile is "present" only if the pooled sample count
// gives ≥ minTailSamples expected samples beyond it (n·(1−p) ≥ 10).
type Report struct {
	Count       int64
	P50Ns       int64
	P90Ns       int64
	P95Ns       int64
	P99Ns       int64
	P999Ns      int64
	P999Present bool
	MaxNs       int64
}

const minTailSamples = 10.0

// MergeRecorders folds per-worker recorders into one histogram (one rep),
// merging each under its lock (race-free even if called before full drain).
func MergeRecorders(recs []*Recorder) (*hdr.Histogram, int64) {
	merged := hdr.New(minLatencyNs, maxLatencyNs, sigFigs)
	var n int64
	for _, r := range recs {
		n += r.snapshotInto(merged)
	}
	return merged, n
}

// PercentilesFrom builds a gated Report from an already-merged histogram and
// its pooled sample count. Used both per-rep (BuildReport) and across reps
// (cmd/fitmodel merges rep snapshots first, then calls this) so the §6
// gating is always applied to the POOLED count, never to averaged
// percentiles.
func PercentilesFrom(h *hdr.Histogram, n int64) Report {
	rep := Report{
		Count:  n,
		P50Ns:  h.ValueAtQuantile(50),
		P90Ns:  h.ValueAtQuantile(90),
		P95Ns:  h.ValueAtQuantile(95),
		P99Ns:  h.ValueAtQuantile(99),
		P999Ns: h.ValueAtQuantile(99.9),
		MaxNs:  h.Max(),
	}
	// Gate P99.9: need n·(1−0.999) ≥ 10 ⇒ n ≥ 10000.
	rep.P999Present = float64(n)*(1.0-0.999) >= minTailSamples

	return rep
}

// BuildReport produces the single-rep report.
func BuildReport(recs []*Recorder) Report {
	merged, n := MergeRecorders(recs)
	return PercentilesFrom(merged, n)
}

// Snapshot is the JSON-serializable form of a merged histogram. hdrhistogram-go
// exposes Export() *hdr.Snapshot and hdr.Import(*hdr.Snapshot) *Histogram; we
// persist the Snapshot in latency.json so cmd/fitmodel can Import + Merge
// across the 3 reps and compute POOLED percentiles (averaging per-rep
// percentiles would be statistically invalid — design §6/§11).
func ExportSnapshot(recs []*Recorder) *hdr.Snapshot {
	merged, _ := MergeRecorders(recs)
	return merged.Export()
}
