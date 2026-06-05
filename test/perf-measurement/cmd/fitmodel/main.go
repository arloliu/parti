// Command fitmodel turns a finished load-campaign results/ tree into a fitted
// cost model (design §11, plan Phase 9 Task 9.2 Step 1). It is the offline,
// operator-run companion to cmd/estimator: estimator PREDICTS from the
// model.json fitmodel WRITES.
//
// Inputs (per results/<cell>/rep<r>/, produced by scripts/run-load-matrix.sh):
//
//   - manifest.yaml  — cell params (N = partition count, aggregateX, kv/data
//     storage, consumerMode) + the run status. Reps whose status != "ok" or
//     that lack a latency.json are SKIPPED (mirrors the runner's valid-cell
//     resume gate). Every skip is logged — no silent drops.
//   - latency.json   — the per-rep LatencyReport, including the serialized HDR
//     Snapshot. fitmodel hdr.Imports every rep's Snapshot, Merges them into one
//     histogram, sums inWindowSent/delivered, then calls
//     latency.PercentilesFrom on the POOLED histogram + count so the §6 P99.9
//     gate uses the pooled sample size (never averaged per-rep percentiles).
//   - cgroup_io.raw  — cgroup v2 io.stat samples for the 5 NATS containers.
//     fitmodel reuses internal/aggregate's ParseCgroup → CgroupDeltas to derive
//     per-second block-write IOPS, sums across the NATS containers, and takes
//     the mean over the post-warmup window (the same window the latency
//     measurement uses, bounded below by rpc_counts.csv's first sample).
//   - cgroup_cpumem.raw — cgroup v2 cpu.stat (usage_usec) + memory.current
//     samples for the same 5 NATS containers. fitmodel reuses
//     ParseCgroupCPUMem → CPUMemDeltas to derive per-second CPU
//     fraction-of-one-core (1.0 == one full core) and instantaneous RSS, sums
//     CPU and RSS across the NATS containers per second, and takes the mean
//     over the same post-warmup window as the IOPS mean. This file is OPTIONAL:
//     IOPS-only campaigns (and the idle M1.x runner) do not write it, so a
//     missing/empty file logs a note and skips the CPU/RSS metrics rather than
//     failing — backward compatible with runs predating the cpumem capture.
//   - rpc_counts.csv — used only to find the capture-window start (post-warmup)
//     so the IOPS / CPU / RSS means are not biased by the ramp/warmup phase.
//
// Output: model.json, a costmodel.Model = metric → storage → Fit. The metric
// name folds in the consumer mode (e.g. "dynamic_write_iops",
// "queue_latency_p99_ns") because dynamic (N consumers) and queue (1 durable)
// have different cost-vs-N structure and MUST NOT be pooled into one affine
// fit. cmd/estimator keys on Model[metric][storage] with no mode axis, so
// mode-in-the-name keeps each metric self-describing.
package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"

	hdr "github.com/HdrHistogram/hdrhistogram-go"
	"gopkg.in/yaml.v3"

	"github.com/arloliu/parti/test/perf-measurement/internal/aggregate"
	"github.com/arloliu/parti/test/perf-measurement/internal/costmodel"
	"github.com/arloliu/parti/test/perf-measurement/internal/latency"
)

// natsContainers are the NATS server containers whose cgroup io.stat counters
// make up the cluster-wide block-write IOPS cost. Mirrors run-load-matrix.sh's
// CONTAINERS list (replicas pinned to 5 in load mode).
var natsContainers = []string{
	"perf-nats-1", "perf-nats-2", "perf-nats-3", "perf-nats-4", "perf-nats-5",
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("fitmodel: ")
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("fitmodel", flag.ContinueOnError)
	results := fs.String("results", "", "path to the campaign results tree (results/<cell>/rep<r>/...) (required)")
	out := fs.String("out", "model.json", "path to write the fitted cost model JSON")
	dump := fs.Bool("dump", false, "print per-cell measured metrics and exit (no fit; useful for a single cell where a curve can't be fitted)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *results == "" {
		return errors.New("--results is required")
	}

	cells, err := walkResults(*results)
	if err != nil {
		return err
	}
	if len(cells) == 0 {
		return fmt.Errorf("no valid cells found under %s", *results)
	}

	if *dump {
		dumpCells(cells)
		return nil
	}

	model := buildModel(cells)
	if len(model) == 0 {
		return errors.New("no (metric,storage) groups had >= 3 cells; nothing to fit")
	}

	if err := costmodel.WriteModel(*out, model); err != nil {
		return err
	}
	log.Printf("wrote %s (%d metrics)", *out, len(model))

	return nil
}

// dumpCells prints each cell's pooled measured metrics. Used when a curve can't
// be fit (e.g. a single cell) — you still want the raw measured values.
func dumpCells(cells []cell) {
	ms := func(ns int64) float64 { return float64(ns) / 1e6 }
	for _, c := range cells {
		fmt.Printf("CELL %s  mode=%s storage=%s N=%d X=%.0f\n", c.name, c.key.mode, c.key.storage, c.key.n, c.key.x)
		if c.hasLatency {
			p999 := "n/a(under-sampled)"
			if c.latency.P999Present {
				p999 = fmt.Sprintf("%.3fms", ms(c.latency.P999Ns))
			}
			fmt.Printf("  latency(pooled n=%d): p50=%.3fms p90=%.3fms p95=%.3fms p99=%.3fms p99.9=%s max=%.3fms\n",
				c.latency.Count, ms(c.latency.P50Ns), ms(c.latency.P90Ns), ms(c.latency.P95Ns), ms(c.latency.P99Ns), p999, ms(c.latency.MaxNs))
		} else {
			fmt.Println("  latency: none")
		}
		if c.hasIOPS {
			fmt.Printf("  nats block-write IOPS (cluster, mean over window): %.1f\n", c.writeIOPS)
		} else {
			fmt.Println("  nats block-write IOPS: none (cgroup_io.raw missing/empty)")
		}
		if c.hasCPUMem {
			fmt.Printf("  nats CPU: %.3f cores (cluster sum)   RSS: %.1f MiB (cluster sum)\n", c.cpuCores, c.rssBytes/(1024*1024))
		} else {
			fmt.Println("  nats CPU/RSS: none (cgroup_cpumem.raw missing/empty)")
		}
	}
}

// manifest is the slice of the harness Manifest fitmodel reads. We do not import
// the harness package (it pulls the whole rig) — the YAML tags match
// cmd/harness's ManifestOptions exactly.
// manifestOptions mirrors the subset of cmd/harness's ManifestOptions YAML
// that fitmodel needs (extracted to a named type so the struct isn't nested).
type manifestOptions struct {
	N            int     `yaml:"n"`
	ConsumerMode string  `yaml:"consumerMode"`
	KVStorage    string  `yaml:"kvStorage"`
	DataStorage  string  `yaml:"dataStorage"`
	AggregateX   float64 `yaml:"aggregateX"`
}

type manifest struct {
	Status  string          `yaml:"status"`
	Options manifestOptions `yaml:"options"`
}

func parseManifest(path string) (manifest, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return manifest{}, fmt.Errorf("read manifest %s: %w", path, err)
	}
	var m manifest
	if err := yaml.Unmarshal(buf, &m); err != nil {
		return manifest{}, fmt.Errorf("parse manifest %s: %w", path, err)
	}

	return m, nil
}

// cellKey identifies one matrix cell across its reps. mode and storage drive
// the model's metric/storage keys; n and x are the regression axes.
type cellKey struct {
	mode    string
	storage string
	n       int
	x       float64
}

// cell holds the pooled, fitted-ready measurements for one matrix cell.
type cell struct {
	key  cellKey
	name string // results subdir name, for logging

	// Pooled latency (across all valid reps).
	latency    latency.Report
	hasLatency bool

	// Cluster block-write IOPS, mean over the post-warmup window, averaged
	// across the cell's reps. hasIOPS is false when no rep yielded a sample.
	writeIOPS float64
	hasIOPS   bool

	// Cluster CPU (fraction-of-one-core, summed across NATS containers) and
	// total RSS bytes, each meaned over the post-warmup window and averaged
	// across the cell's reps. hasCPUMem is false when no rep yielded a sample
	// (cgroup_cpumem.raw absent/empty — backward compatible with IOPS-only runs).
	cpuCores  float64
	rssBytes  float64
	hasCPUMem bool
}

// repArtifacts is the per-rep data fitmodel pools.
type repArtifacts struct {
	dir      string
	manifest manifest
}

// walkResults discovers every results/<cell>/rep<r>/ dir, validates it, groups
// reps by cell, and pools each cell's reps. Invalid reps are logged and skipped.
//
// Reps are grouped by the <cell> DIRECTORY NAME, not by the manifest-derived
// (mode,storage,N,X) tuple. The matrix has cells that share an identical
// (mode,storage,N,X) — e.g. the meta-sweep cells meta-default / meta-16MB /
// meta-64MB all run N=5000,k=2,file,dynamic (⇒ same X) yet are DIFFERENT runs.
// Keying by params would silently pool those three configs (and the plain
// dyn-N5000-k2-file cell) into one fit point. The directory name is the
// runner's unit of a cell, so we group on it and read the regression axes
// (N, X, mode, storage) from each cell's manifest.
func walkResults(root string) ([]cell, error) {
	cellEntries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read results dir %s: %w", root, err)
	}

	cells := make([]cell, 0, len(cellEntries))

	for _, ce := range cellEntries {
		if !ce.IsDir() {
			continue
		}
		cellDir := filepath.Join(root, ce.Name())
		repEntries, derr := os.ReadDir(cellDir)
		if derr != nil {
			log.Printf("SKIP cell %s: read dir: %v", ce.Name(), derr)
			continue
		}

		var reps []repArtifacts
		var key cellKey
		haveKey := false
		for _, re := range repEntries {
			if !re.IsDir() {
				continue
			}
			repDir := filepath.Join(cellDir, re.Name())
			label := ce.Name() + "/" + re.Name()

			manifestPath := filepath.Join(repDir, "manifest.yaml")
			if _, statErr := os.Stat(manifestPath); statErr != nil {
				log.Printf("SKIP rep %s: no manifest.yaml", label)
				continue
			}
			m, perr := parseManifest(manifestPath)
			if perr != nil {
				log.Printf("SKIP rep %s: %v", label, perr)
				continue
			}
			if m.Status != "ok" {
				log.Printf("SKIP rep %s: status=%q (want ok)", label, m.Status)
				continue
			}
			if _, statErr := os.Stat(filepath.Join(repDir, "latency.json")); statErr != nil {
				log.Printf("SKIP rep %s: no latency.json", label)
				continue
			}

			repKey := cellKey{
				mode:    m.Options.ConsumerMode,
				storage: storageOf(m),
				n:       m.Options.N,
				x:       m.Options.AggregateX,
			}
			if !haveKey {
				key = repKey
				haveKey = true
			} else if repKey != key {
				// Reps of one cell dir must agree on params; surface drift and
				// keep the first key (still pooled — the operator should look).
				log.Printf("WARN rep %s: params %+v differ from cell's %+v; pooling anyway",
					label, repKey, key)
			}
			reps = append(reps, repArtifacts{dir: repDir, manifest: m})
		}

		if len(reps) == 0 {
			log.Printf("SKIP cell %s: no valid reps", ce.Name())
			continue
		}
		c, ok := poolCell(key, ce.Name(), reps)
		if !ok {
			continue
		}
		cells = append(cells, c)
	}
	// Stable order for deterministic logging/output.
	slices.SortFunc(cells, func(a, b cell) int {
		if a.key.mode != b.key.mode {
			return cmp.Compare(a.key.mode, b.key.mode)
		}
		if a.key.storage != b.key.storage {
			return cmp.Compare(a.key.storage, b.key.storage)
		}
		if a.key.n != b.key.n {
			return cmp.Compare(a.key.n, b.key.n)
		}

		return cmp.Compare(a.key.x, b.key.x)
	})

	return cells, nil
}

// storageOf resolves a cell's storage label. The matrix sets kv and data
// storage to the same value per cell; if they ever diverge, prefer dataStorage
// (the message stream is the dominant block-write source) and note it.
func storageOf(m manifest) string {
	if m.Options.KVStorage != "" && m.Options.KVStorage != m.Options.DataStorage {
		log.Printf("note: kvStorage=%q != dataStorage=%q; keying on dataStorage",
			m.Options.KVStorage, m.Options.DataStorage)
	}
	if m.Options.DataStorage != "" {
		return m.Options.DataStorage
	}

	return m.Options.KVStorage
}

// poolCell pools all valid reps of one cell: merges the HDR snapshots into one
// pooled latency Report and averages the per-rep block-write IOPS.
func poolCell(key cellKey, name string, reps []repArtifacts) (cell, bool) {
	c := cell{key: key, name: name}

	// Bounds MUST match internal/latency.Recorder (1µs..60s, 3 sig figs) so
	// imported per-rep snapshots Merge into identically-bucketed bins.
	merged := hdr.New(1_000, 60_000_000_000, 3)
	var pooledN, pooledSent, pooledDelivered int64
	mergedAny := false

	var iopsSum float64
	var iopsCount int

	var cpuSum, rssSum float64
	var cpuMemCount int

	for _, r := range reps {
		lr, lerr := readLatencyReport(filepath.Join(r.dir, "latency.json"))
		if lerr != nil {
			log.Printf("SKIP rep %s: latency.json: %v", name, lerr)
			continue
		}
		if lr.Snapshot != nil {
			imported := hdr.Import(lr.Snapshot)
			merged.Merge(imported)
			pooledN += imported.TotalCount()
			mergedAny = true
		} else {
			log.Printf("note: rep in cell %s has nil latency snapshot; counts still pooled", name)
			pooledN += lr.Count
		}
		pooledSent += lr.InWindowSent
		pooledDelivered += lr.Delivered

		iops, ok := cellWriteIOPS(r.dir)
		if ok {
			iopsSum += iops
			iopsCount++
		}

		cpu, rss, cmOK := cellCPUMem(r.dir)
		if cmOK {
			cpuSum += cpu
			rssSum += rss
			cpuMemCount++
		}
	}

	if mergedAny {
		c.latency = latency.PercentilesFrom(merged, pooledN)
		c.hasLatency = true
		ratio := 0.0
		if pooledSent > 0 {
			ratio = float64(pooledDelivered) / float64(pooledSent)
		}
		log.Printf("cell %s (%s, %s, N=%d): pooled count=%d sent=%d delivered=%d deliveryRatio=%.3f",
			name, key.mode, key.storage, key.n, pooledN, pooledSent, pooledDelivered, ratio)
	} else {
		log.Printf("SKIP cell %s (%s, %s, N=%d): no usable latency snapshot across reps",
			name, key.mode, key.storage, key.n)
	}

	if iopsCount > 0 {
		c.writeIOPS = iopsSum / float64(iopsCount)
		c.hasIOPS = true
	} else {
		log.Printf("note: cell %s (%s, %s, N=%d): no block-write IOPS samples (cgroup_io.raw missing/empty)",
			name, key.mode, key.storage, key.n)
	}

	if cpuMemCount > 0 {
		c.cpuCores = cpuSum / float64(cpuMemCount)
		c.rssBytes = rssSum / float64(cpuMemCount)
		c.hasCPUMem = true
	} else {
		log.Printf("note: cell %s (%s, %s, N=%d): no CPU/RSS samples (cgroup_cpumem.raw missing/empty)",
			name, key.mode, key.storage, key.n)
	}

	// A cell with no metric at all is useless.
	if !c.hasLatency && !c.hasIOPS && !c.hasCPUMem {
		return cell{}, false
	}

	return c, true
}

// readLatencyReport decodes a rep's latency.json into the harness LatencyReport
// shape. We redeclare the relevant fields locally to avoid importing the harness
// command package.
func readLatencyReport(path string) (*latencyReport, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lr latencyReport
	if err := json.Unmarshal(buf, &lr); err != nil {
		return nil, err
	}

	return &lr, nil
}

// latencyReport mirrors the fields of cmd/harness.LatencyReport that fitmodel
// consumes. The JSON tags match the harness writer exactly. Snapshot is the
// serialized merged HDR histogram fitmodel re-imports for cross-rep pooling.
type latencyReport struct {
	Count        int64         `json:"count"`
	InWindowSent int64         `json:"inWindowSent"`
	Delivered    int64         `json:"delivered"`
	Snapshot     *hdr.Snapshot `json:"snapshot"`
}

// cellWriteIOPS computes one rep's cluster block-write IOPS: per-second
// CgroupDeltas summed across the NATS containers, restricted to the post-warmup
// capture window (bounded below by rpc_counts.csv's first sample), then meaned
// over those seconds. Returns ok=false when cgroup_io.raw is absent/empty.
func cellWriteIOPS(repDir string) (float64, bool) {
	cgPath := filepath.Join(repDir, "cgroup_io.raw")
	if _, err := os.Stat(cgPath); err != nil {
		return 0, false
	}
	samples, err := aggregate.ParseCgroup(cgPath)
	if err != nil {
		log.Printf("note: %s: parse cgroup_io.raw: %v", repDir, err)
		return 0, false
	}
	deltas := aggregate.CgroupDeltas(samples)
	if len(deltas) == 0 {
		return 0, false
	}

	// Lower bound = post-warmup capture start, derived from rpc_counts.csv.
	var startSec int64
	if rows, rerr := aggregate.ParseRPC(filepath.Join(repDir, "rpc_counts.csv")); rerr == nil {
		if ns := aggregate.CaptureStartNs(rows); ns > 0 {
			startSec = ns / 1e9
		}
	}

	natsSet := map[string]bool{}
	for _, c := range natsContainers {
		natsSet[c] = true
	}

	// Sum write IOPS across NATS containers per t_sec, then mean over seconds
	// in the window.
	perSec := map[int64]float64{}
	for _, d := range deltas {
		if !natsSet[d.Container] {
			continue
		}
		if d.TSec < startSec {
			continue
		}
		perSec[d.TSec] += d.IOPSWrite
	}
	if len(perSec) == 0 {
		return 0, false
	}
	var sum float64
	for _, v := range perSec {
		sum += v
	}

	return sum / float64(len(perSec)), true
}

// cellCPUMem computes one rep's cluster CPU + RSS over the post-warmup window:
// per-second CPUMemDeltas summed across the NATS containers (CPU as
// fraction-of-one-core where 1.0 == one full core; RSS as bytes), restricted to
// the same post-warmup window as cellWriteIOPS (bounded below by
// rpc_counts.csv's first sample), then meaned over those seconds. Returns
// ok=false when cgroup_cpumem.raw is absent/empty — IOPS-only campaigns omit it.
func cellCPUMem(repDir string) (cpuCores, rssBytes float64, ok bool) {
	cmPath := filepath.Join(repDir, "cgroup_cpumem.raw")
	if _, err := os.Stat(cmPath); err != nil {
		return 0, 0, false
	}
	samples, err := aggregate.ParseCgroupCPUMem(cmPath)
	if err != nil {
		log.Printf("note: %s: parse cgroup_cpumem.raw: %v", repDir, err)
		return 0, 0, false
	}
	deltas := aggregate.CPUMemDeltas(samples)
	if len(deltas) == 0 {
		return 0, 0, false
	}

	// Lower bound = post-warmup capture start, derived from rpc_counts.csv —
	// identical to cellWriteIOPS so the two means cover the same window.
	var startSec int64
	if rows, rerr := aggregate.ParseRPC(filepath.Join(repDir, "rpc_counts.csv")); rerr == nil {
		if ns := aggregate.CaptureStartNs(rows); ns > 0 {
			startSec = ns / 1e9
		}
	}

	natsSet := map[string]bool{}
	for _, c := range natsContainers {
		natsSet[c] = true
	}

	// Sum CPU + RSS across NATS containers per t_sec, then mean over seconds.
	cpuPerSec := map[int64]float64{}
	rssPerSec := map[int64]float64{}
	for _, d := range deltas {
		if !natsSet[d.Container] {
			continue
		}
		if d.TSec < startSec {
			continue
		}
		cpuPerSec[d.TSec] += d.CPUCores
		rssPerSec[d.TSec] += float64(d.MemoryBytes)
	}
	if len(cpuPerSec) == 0 {
		return 0, 0, false
	}
	var cpuTotal, rssTotal float64
	for _, v := range cpuPerSec {
		cpuTotal += v
	}
	for _, v := range rssPerSec {
		rssTotal += v
	}
	n := float64(len(cpuPerSec))

	return cpuTotal / n, rssTotal / n, true
}

// buildModel assembles costmodel.Points per (metric, storage) and fits each.
// Metrics with < 3 cells are logged and skipped (FitAffine needs >= 3 points).
func buildModel(cells []cell) costmodel.Model {
	// metric -> storage -> points
	pts := map[string]map[string][]costmodel.Point{}
	add := func(metric, storage string, p costmodel.Point) {
		if pts[metric] == nil {
			pts[metric] = map[string][]costmodel.Point{}
		}
		pts[metric][storage] = append(pts[metric][storage], p)
	}

	for _, c := range cells {
		n := float64(c.key.n)
		x := c.key.x
		st := c.key.storage
		mode := c.key.mode

		if c.hasIOPS {
			add(mode+"_write_iops", st, costmodel.Point{N: n, X: x, Cost: c.writeIOPS})
		}
		if c.hasCPUMem {
			add(mode+"_cpu_cores", st, costmodel.Point{N: n, X: x, Cost: c.cpuCores})
			add(mode+"_rss_bytes", st, costmodel.Point{N: n, X: x, Cost: c.rssBytes})
		}
		if c.hasLatency {
			r := c.latency
			add(mode+"_latency_p50_ns", st, costmodel.Point{N: n, X: x, Cost: float64(r.P50Ns)})
			add(mode+"_latency_p90_ns", st, costmodel.Point{N: n, X: x, Cost: float64(r.P90Ns)})
			add(mode+"_latency_p95_ns", st, costmodel.Point{N: n, X: x, Cost: float64(r.P95Ns)})
			add(mode+"_latency_p99_ns", st, costmodel.Point{N: n, X: x, Cost: float64(r.P99Ns)})
			add(mode+"_latency_max_ns", st, costmodel.Point{N: n, X: x, Cost: float64(r.MaxNs)})
			// P99.9 only when the pooled count clears the §6 gate.
			if r.P999Present {
				add(mode+"_latency_p999_ns", st, costmodel.Point{N: n, X: x, Cost: float64(r.P999Ns)})
			}
		}
	}

	model := costmodel.Model{}
	metrics := make([]string, 0, len(pts))
	for m := range pts {
		metrics = append(metrics, m)
	}
	slices.Sort(metrics)

	for _, metric := range metrics {
		storages := make([]string, 0, len(pts[metric]))
		for s := range pts[metric] {
			storages = append(storages, s)
		}
		slices.Sort(storages)
		for _, st := range storages {
			points := pts[metric][st]
			if len(points) < 3 {
				log.Printf("SKIP fit %s/%s: %d cell(s) < 3 required points", metric, st, len(points))
				continue
			}
			fit, err := costmodel.FitAffine(points)
			if err != nil {
				// Expected for collinear cells (e.g. all k fixed ⇒ X∝N): the
				// system is singular. Log and skip, don't fail the campaign.
				log.Printf("SKIP fit %s/%s: %v (%d points)", metric, st, err, len(points))
				continue
			}
			if model[metric] == nil {
				model[metric] = map[string]costmodel.Fit{}
			}
			model[metric][st] = fit
			log.Printf("fit %s/%s: a=%.4g b=%.4g c=%.4g R2=%.4f n=%d",
				metric, st, fit.A, fit.B, fit.C, fit.R2, fit.N)
		}
	}

	return model
}
