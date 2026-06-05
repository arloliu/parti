// Command estimator predicts per-metric cost for a hypothetical deployment from
// a fitted cost model (design §11). It derives the structural size M = n/50 and
// aggregate load X = k·M, then evaluates each metric's affine fit at (M, X) for
// the requested storage type.
//
// The model is fit at replication factor RF=5 only; estimating at any other RF
// is unsupported and exits non-zero. Predictions for n outside the calibrated
// range [1000, 5000] are flagged as extrapolation and printed with a caveat.
package main

import (
	"flag"
	"fmt"
	"os"
	"slices"

	"github.com/arloliu/parti/test/perf-measurement/internal/costmodel"
)

// Calibrated structural range (number of partitions n) the model was fit over.
// Predictions outside this band are extrapolation.
const (
	minCalibratedN = 1000
	maxCalibratedN = 5000

	// fittedRF is the only replication factor the model is fit at.
	fittedRF = 5

	// partitionsPerMember derives M (members) from n (partitions): M = n / 50.
	partitionsPerMember = 50
)

// result is the outcome of a prediction: one value per metric plus whether the
// requested size required extrapolating beyond the calibrated range.
type result struct {
	M             float64            // derived structural size (members)
	X             float64            // derived aggregate load (k·M)
	Metrics       map[string]float64 // metric → predicted cost
	Extrapolation bool
}

// predict evaluates the model for the given deployment parameters. It is pure
// and independent of any CLI plumbing so it can be table-tested directly.
//
// The cost model's structural axis is the PARTITION COUNT n (== consumer
// count, the thing that drives IOPS / metacontroller / state-file cost — see
// design §11 and costmodel.Point). The members count M=n/50 is derived only to
// compute the aggregate load X=k·M; predictions are made at partition count n,
// NOT at M.
//
//   - M = n / 50, X = k · M.
//   - cost is predicted at (N=n, X) — n is the structural axis.
//   - rf != 5 returns an error (model is fit at RF=5 only).
//   - n outside [1000, 5000] sets Extrapolation=true (boundaries inclusive).
//   - an unknown storage for a metric returns an error.
func predict(model costmodel.Model, n, k, rf int, storage string) (result, error) {
	if rf != fittedRF {
		return result{}, fmt.Errorf("model fit at RF=%d only, got RF=%d", fittedRF, rf)
	}

	m := float64(n) / partitionsPerMember
	x := float64(k) * m

	res := result{
		M:             m,
		X:             x,
		Metrics:       make(map[string]float64),
		Extrapolation: n < minCalibratedN || n > maxCalibratedN,
	}

	for metric, byStorage := range model {
		fit, ok := byStorage[storage]
		if !ok {
			return result{}, fmt.Errorf("metric %q has no fit for storage %q", metric, storage)
		}
		res.Metrics[metric] = fit.Predict(float64(n), x)
	}

	return res, nil
}

func main() {
	modelPath := flag.String("model", "", "path to the fitted cost-model JSON (required)")
	n := flag.Int("n", 0, "number of partitions")
	k := flag.Int("k", 0, "per-member load factor (X = k·M)")
	rf := flag.Int("rf", fittedRF, "replication factor (model fit at RF=5 only)")
	storage := flag.String("storage", "file", "storage type to predict for (e.g. file, memory)")
	flag.Parse()

	if *modelPath == "" {
		fmt.Fprintln(os.Stderr, "estimator: --model is required")
		flag.Usage()
		os.Exit(2)
	}

	model, err := costmodel.LoadModel(*modelPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "estimator: %v\n", err)
		os.Exit(1)
	}

	res, err := predict(model, *n, *k, *rf, *storage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "estimator: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Deployment: n=%d partitions, k=%d, RF=%d, storage=%s\n", *n, *k, *rf, *storage)
	fmt.Printf("Derived:    M=%.1f members, X=%.1f aggregate load\n\n", res.M, res.X)

	metrics := make([]string, 0, len(res.Metrics))
	for metric := range res.Metrics {
		metrics = append(metrics, metric)
	}
	slices.Sort(metrics)

	fmt.Printf("%-24s %16s\n", "METRIC", "PREDICTED")
	for _, metric := range metrics {
		fmt.Printf("%-24s %16.4f\n", metric, res.Metrics[metric])
	}

	if res.Extrapolation {
		fmt.Fprintf(os.Stderr, "\n!! CAVEAT: n=%d is outside the calibrated range [%d, %d];\n",
			*n, minCalibratedN, maxCalibratedN)
		fmt.Fprintln(os.Stderr, "!! these predictions are EXTRAPOLATIONS and may be unreliable.")
	}
}
