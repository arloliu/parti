// Command harness is the Parti workload binary used by the IOPS
// investigation. See docs/plans/iops-investigation/00-attribution-plan.md
// §R2 for the full lifecycle contract.
//
// The binary parses CLI flags, connects to the configured NATS cluster,
// pre-creates every parti-managed KV bucket and the data stream with
// the requested storage class, seeds the partition source, spawns the
// requested number of parti.Manager workers (each with its own NATS
// connection + InstrumentedJS wrapper), waits until every worker
// reaches StateStable, sleeps the warmup window, resets all counter
// wrappers, and then streams per-(worker,bucket,op) counter snapshots
// to rpc_counts.csv for the capture window. On exit it writes a
// manifest.yaml describing the run.
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/arloliu/parti/v2"

	"github.com/arloliu/parti/test/iops-investigation/internal/instrumentedjs"
	"github.com/arloliu/parti/test/iops-investigation/internal/storageverify"
)

func main() {
	o, err := parseFlags(os.Args[1:])
	if err != nil {
		log.Fatalf("flag parse: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := Run(ctx, o, os.Stderr); err != nil {
		log.Fatalf("run: %v", err)
	}
}

// parseFlags maps a string slice (os.Args[1:]) to an Options value.
// Extracted so tests can drive it without going through os.Args.
func parseFlags(args []string) (Options, error) {
	fs := flag.NewFlagSet("harness", flag.ContinueOnError)

	var (
		natsURLs        = fs.String("nats-urls", "nats://localhost:4222", "comma-separated NATS URLs")
		workers         = fs.Int("workers", 5, "number of parti.Manager workers")
		n               = fs.Int("n", 1000, "partition count")
		replicas        = fs.Int("replicas", 3, "JetStream replication factor")
		twoPhase        = fs.Bool("two-phase", false, "EnableTwoPhaseHandoff")
		sweepInterval   = fs.Duration("sweep-interval", 30*time.Second, "Handoff.SweepInterval")
		fetchTimeout    = fs.Duration("fetch-timeout", 5*time.Second, "consumer FetchTimeout")
		consumerMode    = fs.String("consumer-mode", "dynamic", "dynamic | queue | none-attached")
		hbInterval      = fs.Duration("heartbeat-interval", 5*time.Second, "HeartbeatInterval")
		hbTTL           = fs.Duration("heartbeat-ttl", 15*time.Second, "HeartbeatTTL")
		widTTL          = fs.Duration("worker-id-ttl", 75*time.Second, "WorkerIDTTL")
		electionTimeout = fs.Duration("election-timeout", 10*time.Second, "ElectionTimeout")
		kvStorage       = fs.String("kv-storage", "file", "file | memory; applies to ALL parti KV buckets")
		dataStorage     = fs.String("data-storage", "file", "file | memory; applies to the data stream")
		dataStream      = fs.String("data-stream-name", "iops-rig-data", "data stream name")
		warmup          = fs.Duration("warmup", 5*time.Minute, "warmup window before counter reset")
		capture         = fs.Duration("capture-window", 10*time.Minute, "measurement window length")
		dumpInterval    = fs.Duration("rpc-dump-interval", 1*time.Second, "counter CSV dump rate")
		outputDir       = fs.String("output-dir", "./results/run", "manifest + counter CSV destination")
		partiVersion    = fs.String("parti-version", "v2.3.0", "parti version recorded in manifest")
		fastConfig      = fs.Bool("fast-config", false, "use parti.TestConfig() timings (smoke-test only)")
	)

	if err := fs.Parse(args); err != nil {
		return Options{}, err
	}

	kvSt, err := ParseStorage(*kvStorage)
	if err != nil {
		return Options{}, fmt.Errorf("--kv-storage: %w", err)
	}
	dataSt, err := ParseStorage(*dataStorage)
	if err != nil {
		return Options{}, fmt.Errorf("--data-storage: %w", err)
	}
	mode, err := ParseConsumerMode(*consumerMode)
	if err != nil {
		return Options{}, fmt.Errorf("--consumer-mode: %w", err)
	}

	return Options{
		NATSURLs:           *natsURLs,
		Workers:            *workers,
		N:                  *n,
		Replicas:           *replicas,
		TwoPhase:           *twoPhase,
		SweepInterval:      *sweepInterval,
		FetchTimeout:       *fetchTimeout,
		ConsumerMode:       mode,
		HeartbeatInterval:  *hbInterval,
		HeartbeatTTL:       *hbTTL,
		WorkerIDTTL:        *widTTL,
		ElectionTimeout:    *electionTimeout,
		KVStorage:          kvSt,
		DataStorage:        dataSt,
		DataStreamName:     *dataStream,
		PartitionSourceKey: DefaultPartitionSourceKey,
		Warmup:             *warmup,
		CaptureWindow:      *capture,
		RPCDumpInterval:    *dumpInterval,
		OutputDir:          *outputDir,
		PartiVersion:       *partiVersion,
		FastConfig:         *fastConfig,
	}, nil
}

// Run executes the harness end-to-end. It is exposed (rather than
// inlined into main) so the smoke test can drive it against an
// embedded NATS server without spawning a child process.
//
// Lifecycle:
//
//  1. Connect a setup NATS client; build a setup InstrumentedJS.
//  2. Pre-create every parti-managed KV bucket, the partition-source
//     bucket, and the data stream with the requested storage class.
//  3. Seed the partition source with --n entries.
//  4. Call storageverify.Verify; abort if any stream's actual storage
//     class disagrees with the manifest.
//  5. Spawn --workers parti.Manager workers, each on its own NATS
//     connection and its own InstrumentedJS.
//  6. Wait for every worker to reach StateStable (timeout = 3× warmup
//     or 30s, whichever is greater) and abort on StateDegraded.
//  7. Sleep --warmup.
//  8. ResetAll counter wrappers (clean slate for the capture window).
//  9. Stream counter snapshots to rpc_counts.csv every
//     --rpc-dump-interval for --capture-window.
//
// 10. Stop workers; write manifest.yaml.
//
// On signal (SIGINT/SIGTERM), Run aborts the capture window early and
// writes a manifest with status="interrupted".
func Run(ctx context.Context, o Options, errLog io.Writer) error {
	started := time.Now()
	cfg := BuildPartiConfig(o)

	// Step 1: setup NATS + wrapper. The setup wrapper is intentionally
	// disjoint from per-worker wrappers — its counters reflect
	// pre-population traffic, not workload, and we discard them.
	setupConn, err := ConnectNATS(o.NATSURLs)
	if err != nil {
		return fmt.Errorf("setup connect: %w", err)
	}
	defer setupConn.Close()
	setupJS, err := jetstream.New(setupConn)
	if err != nil {
		return fmt.Errorf("setup jetstream.New: %w", err)
	}
	setupIJS := instrumentedjs.New(setupJS)

	// Wait for the JetStream meta-cluster to elect a leader. A fresh
	// rig accepts client connections in ~1s but takes ~5-10s to elect
	// a meta-leader; PreCreate would otherwise fail with "context
	// deadline exceeded" with no useful diagnostic.
	if err := WaitForJetStream(ctx, setupIJS, o.Replicas, 60*time.Second); err != nil {
		return fmt.Errorf("wait for jetstream: %w", err)
	}

	// Steps 2 + 3: pre-create + seed.
	srcKV, err := PreCreate(ctx, setupIJS, o, cfg)
	if err != nil {
		return fmt.Errorf("pre-create streams: %w", err)
	}
	if err := SeedPartitions(ctx, srcKV, o.PartitionSourceKey, o.N); err != nil {
		return fmt.Errorf("seed partitions: %w", err)
	}

	// Step 4: storage-class verification.
	if err := storageverify.Verify(ctx, setupIJS, ExpectedStreams(o, cfg)); err != nil {
		return fmt.Errorf("storage verify: %w", err)
	}

	// Step 5: spawn workers.
	workers := make([]*WorkerHandle, 0, o.Workers)
	cleanup := func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var wg sync.WaitGroup
		for _, w := range workers {
			wg.Add(1)
			go func(w *WorkerHandle) {
				defer wg.Done()
				if err := w.Stop(stopCtx); err != nil {
					fmt.Fprintf(errLog, "worker %d stop: %v\n", w.idx, err)
				}
			}(w)
		}
		wg.Wait()
	}

	for i := range o.Workers {
		w, werr := StartWorker(ctx, i, o, cfg)
		if werr != nil {
			cleanup()
			return fmt.Errorf("start workers: %w", werr)
		}
		workers = append(workers, w)
	}
	defer cleanup()

	// Step 6: wait for cluster Stable.
	gateTimeout := max(3*o.Warmup, 30*time.Second)
	if err := WaitStableAll(workers, gateTimeout); err != nil {
		return fmt.Errorf("wait stable: %w", err)
	}

	// Step 7: warmup.
	if err := sleepCtx(ctx, o.Warmup); err != nil {
		// No CSV exists yet; per the artifact-completeness contract a
		// manifest must not exist without a committed CSV, so we do
		// not write one here. Log instead.
		fmt.Fprintf(errLog, "warmup interrupted before capture: %v (no manifest written)\n", err)
		return err
	}

	// Warmup-boundary degraded gate: if any worker logged a degraded
	// transition or is no longer Stable during warmup, abort before
	// ResetAll/capture-setup so we do not produce CSV artifacts for a
	// run we already know is degraded. No CSV exists yet, so no
	// manifest is written either (same artifact-completeness rule as
	// the warmup-interrupted path).
	if status, derr := DecideRunStatus(workers); derr != nil {
		fmt.Fprintf(errLog, "warmup-boundary status=%s: %v (no manifest written)\n", status, derr)
		return derr
	}

	// Step 8: reset counters.
	ResetAll(workers)
	captureStart := time.Now()

	// Step 9: capture loop.
	//
	// The CSV is streamed to "<final>.tmp" inside o.OutputDir and only
	// renamed into place after the capture window closes cleanly. This
	// keeps Phase 3 from ever consuming a half-written CSV when the
	// harness is killed mid-run.
	finalCSVPath := filepath.Join(o.OutputDir, "rpc_counts.csv")
	tmpCSVPath := finalCSVPath + ".tmp"
	if err := os.MkdirAll(o.OutputDir, 0o755); err != nil {
		return fmt.Errorf("mkdir output dir: %w", err)
	}
	f, err := os.Create(tmpCSVPath)
	if err != nil {
		return fmt.Errorf("create rpc_counts.csv tmp: %w", err)
	}
	// csvCommitted tracks whether the .tmp was renamed to its final
	// path; the deferred cleanup removes a stray tmp file if not.
	var csvCommitted bool
	defer func() {
		_ = f.Close()
		if !csvCommitted {
			_ = os.Remove(tmpCSVPath)
		}
	}()
	w := csv.NewWriter(f)
	// Per AggregateSnapshots godoc: only present (worker,bucket,op)
	// rows are emitted; Phase 3 treats absent rows as count=0 at that
	// tick.
	if err := w.Write([]string{"t_unix_ns", "worker_idx", "bucket", "op", "count"}); err != nil {
		return fmt.Errorf("write csv header: %w", err)
	}

	captureDeadline := captureStart.Add(o.CaptureWindow)
	ticker := time.NewTicker(o.RPCDumpInterval)
	defer ticker.Stop()

	// Emit an initial snapshot at t=0 of the capture window so
	// downstream tooling can see a row even if the capture is
	// truncated.
	if err := writeSnapshot(w, time.Now(), workers); err != nil {
		return err
	}

	commitCSV := func() error {
		w.Flush()
		if err := w.Error(); err != nil {
			return fmt.Errorf("flush csv: %w", err)
		}
		if err := f.Sync(); err != nil {
			return fmt.Errorf("sync csv: %w", err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close csv: %w", err)
		}
		if err := commitAtomic(tmpCSVPath, finalCSVPath); err != nil {
			return fmt.Errorf("rename csv: %w", err)
		}
		csvCommitted = true
		return nil
	}

captureLoop:
	for {
		select {
		case <-ctx.Done():
			// Drain the latest snapshot then exit early. If we
			// cannot commit the partial CSV the run produces no
			// final CSV and we must not write a manifest either
			// (artifact-completeness contract).
			if serr := writeSnapshot(w, time.Now(), workers); serr != nil {
				fmt.Fprintf(errLog, "interrupt: final snapshot: %v\n", serr)
			}
			if cerr := commitCSV(); cerr != nil {
				fmt.Fprintf(errLog, "interrupt: csv commit failed, no manifest written: %v\n", cerr)
				return cerr
			}
			return finishRun(o, cfg, workers, started, "interrupted", ctx.Err())
		case t := <-ticker.C:
			if err := writeSnapshot(w, t, workers); err != nil {
				return err
			}
			// Mid-run degraded gate: if any worker has logged a
			// degraded transition or is no longer Stable, abort with
			// status=degraded so the manifest accurately records the
			// failure mode.
			if status, derr := DecideRunStatus(workers); derr != nil {
				if cerr := commitCSV(); cerr != nil {
					fmt.Fprintf(errLog, "degraded: csv commit failed, no manifest written: %v\n", cerr)
					return cerr
				}
				return finishRun(o, cfg, workers, started, status, derr)
			}
			if !t.Before(captureDeadline) {
				break captureLoop
			}
		}
	}

	// Final post-capture degraded check, then commit CSV and manifest.
	status, derr := DecideRunStatus(workers)
	if err := commitCSV(); err != nil {
		return err
	}
	if derr != nil {
		return finishRun(o, cfg, workers, started, status, derr)
	}

	// Step 10: manifest (written LAST so its presence implies CSV
	// completion).
	return WriteManifest(o.OutputDir, Manifest{
		StartedAt:                    started,
		EndedAt:                      time.Now(),
		Status:                       status,
		PartiVersion:                 o.PartiVersion,
		NATSImage:                    os.Getenv("IOPS_RIG_NATS_IMAGE"),
		NATSImageDigest:              resolveImageDigest(),
		RunIndex:                     os.Getenv("IOPS_RIG_RUN_INDEX"),
		Options:                      buildManifestOptions(o),
		ConfirmedStorage:             confirmedStreams(o, cfg),
		DegradedTransitionsPerWorker: degradedMap(workers),
	})
}

// writeSnapshot pulls a fresh aggregated snapshot from every worker
// wrapper and appends it to the CSV writer.
func writeSnapshot(w *csv.Writer, t time.Time, workers []*WorkerHandle) error {
	rows := AggregateSnapshots(t, workers)
	for _, r := range rows {
		if err := w.Write([]string{
			strconv.FormatInt(r.TUnixNs, 10),
			strconv.Itoa(r.WorkerID),
			r.Bucket,
			r.Op,
			strconv.FormatInt(r.Count, 10),
		}); err != nil {
			return fmt.Errorf("write csv row: %w", err)
		}
	}
	return nil
}

// sleepCtx sleeps for d or returns ctx.Err() if the context fires
// first. Used so the warmup honours SIGINT.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// finishRun writes a partial manifest with the supplied status
// ("interrupted" or "degraded") then returns the original cause.
// Best-effort: any error writing the manifest itself is suppressed in
// favor of the cause so callers always see the underlying failure.
func finishRun(o Options, cfg parti.Config, workers []*WorkerHandle, started time.Time, status string, cause error) error {
	_ = WriteManifest(o.OutputDir, Manifest{
		StartedAt:                    started,
		EndedAt:                      time.Now(),
		Status:                       status,
		PartiVersion:                 o.PartiVersion,
		NATSImage:                    os.Getenv("IOPS_RIG_NATS_IMAGE"),
		NATSImageDigest:              resolveImageDigest(),
		RunIndex:                     os.Getenv("IOPS_RIG_RUN_INDEX"),
		Options:                      buildManifestOptions(o),
		ConfirmedStorage:             confirmedStreams(o, cfg),
		DegradedTransitionsPerWorker: degradedMap(workers),
	})

	return cause
}

// degradedMap collects each worker's degraded transition count.
func degradedMap(workers []*WorkerHandle) map[int]int64 {
	out := make(map[int]int64, len(workers))
	for _, w := range workers {
		out[w.idx] = w.degraded.Load()
	}
	return out
}

// resolveImageDigest reads IOPS_RIG_NATS_IMAGE_DIGEST if the operator
// captured it via `docker image inspect` ahead of time. The harness
// itself does not shell out to docker — keeping the binary
// host-agnostic — so the digest is opt-in via env var.
func resolveImageDigest() string {
	return os.Getenv("IOPS_RIG_NATS_IMAGE_DIGEST")
}
