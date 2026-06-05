// Command calibrate is the M4 disk-amplification calibration driver.
// It issues single-rate KV workloads against the NATS cluster and emits
// a single CSV row to stdout per invocation so the post-processing step
// can pair RPC totals with external block-IOPS captures (cgroup / iostat
// in scripts/capture-*.sh).
//
// Block-IOPS measurement happens via those external capture scripts.
// This binary does NOT measure IOPS itself; it only counts wrapper-level
// RPCs so the M4 calibration table can map RPC counts to per-op IOPS
// factors.
//
// # Sub-commands
//
//	calibrate kv-put     [--rate 100] [--duration 60s] [--storage file|memory] [--replicas 3] [--value-size 256] [--nats-urls ...]
//	calibrate kv-get     [--rate 100] [--duration 60s] [--storage file|memory] [--replicas 3] [--value-size 256] [--nats-urls ...]
//	calibrate kv-keys    [--keys 1000,3000] [--samples 100] [--storage file|memory] [--replicas 3] [--nats-urls ...]
//	calibrate kv-watch   [--watchers 1] [--rate 100] [--duration 60s] [--storage file|memory] [--replicas 3] [--value-size 256] [--nats-urls ...]
//	calibrate idle-pull  [--c-stream N] [--fetch-timeout 5s] [--data-storage file|memory] [--replicas 3] [--duration 60s] [...]
//
// # CSV output
//
// Each KV sub-command emits one line to stdout:
//
//	path,storage,replicas,rate,duration_s,bytes_per_op,read_rpc_ops,write_rpc_ops,wrapper_total_ops
//
// For kv-keys, `path` is `kv-keys-kN` where N is the bucket population
// (e.g. `kv-keys-k1000`). For kv-watch, `path` is `kv-watch-wW` where W
// is the number of watchers (e.g. `kv-watch-w5`). This variant-in-path
// encoding lets M3 index the calibration table by path string directly.
//
// The `rate` column is 0 for kv-keys (it runs at max throughput over
// --samples calls rather than at a fixed rate).
//
// # idle-pull schema extension
//
// The idle-pull sub-command (M4.1 — see docs/plans/iops-investigation/
// 00-attribution-plan.md §M4.1) appends four extra columns:
//
//	...,wrapper_total_ops,c_stream,fetch_timeout_s,leader_pre,leader_post,leader_changed
//
// where `path` is `idle-pull-c<C>-t<FT>` (e.g. `idle-pull-c100-t5s`).
// Block-IOPS is measured externally; the wrapper RPC counters are an
// ancillary signal and are expected to be near-zero on idle pull —
// pull-iterator pulls are server-internal protocol traffic and do not
// flow through the wrapped JetStream client API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/arloliu/parti/test/iops-investigation/internal/calibrate"
	"github.com/arloliu/parti/test/iops-investigation/internal/instrumentedjs"
)

// writer is the output sink for emitCSV. It defaults to os.Stdout and can
// be redirected by tests via setWriter to capture CSV output without
// spawning a subprocess.
var writer io.Writer = os.Stdout

// setWriter replaces the current output writer and returns the previous value.
// Callers must restore it after their test.
func setWriter(w io.Writer) {
	writer = w
}

func main() {
	if len(os.Args) < 2 {
		usageAndExit()
	}
	sub := os.Args[1]
	args := os.Args[2:]

	var err error
	switch sub {
	case "kv-put":
		err = runKVPut(args)
	case "kv-get":
		err = runKVGet(args)
	case "kv-keys":
		err = runKVKeys(args)
	case "kv-watch":
		err = runKVWatch(args)
	case "idle-pull":
		err = runIdlePull(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown sub-command %q\n", sub)
		usageAndExit()
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", sub, err)
		os.Exit(1)
	}
}

//nolint:revive // deep-exit: top-level CLI usage helper, exiting here is intentional.
func usageAndExit() {
	fmt.Fprintln(os.Stderr, "usage: calibrate <kv-put|kv-get|kv-keys|kv-watch|idle-pull> [flags]")
	os.Exit(2)
}

// emitCSV writes one result row to writer (os.Stdout by default; tests redirect it).
func emitCSV(path, storage string, replicas, rate int, durationS float64, bytesPerOp, readRPCs, writeRPCs int64) {
	total := readRPCs + writeRPCs
	fmt.Fprintf(writer, "%s,%s,%d,%d,%.1f,%d,%d,%d,%d\n",
		path, storage, replicas, rate, durationS, bytesPerOp, readRPCs, writeRPCs, total)
}

// commonFlags holds the flags shared by kv-put, kv-get, and kv-watch.
type commonFlags struct {
	natsURLs  string
	rate      int
	duration  time.Duration
	storage   string
	replicas  int
	valueSize int
}

func parseCommonFlags(fs *flag.FlagSet, args []string) (commonFlags, error) {
	var f commonFlags
	fs.StringVar(&f.natsURLs, "nats-urls", "nats://localhost:4222", "comma-separated NATS URLs")
	fs.IntVar(&f.rate, "rate", 100, "target ops/s")
	fs.DurationVar(&f.duration, "duration", 60*time.Second, "workload duration")
	fs.StringVar(&f.storage, "storage", "file", "file|memory")
	fs.IntVar(&f.replicas, "replicas", 3, "JetStream replication factor")
	fs.IntVar(&f.valueSize, "value-size", 256, "KV value size in bytes")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	if _, err := calibrate.ParseStorage(f.storage); err != nil {
		return f, err
	}

	return f, nil
}

// freshBucket creates (or recreates) a KV bucket with the given name,
// deleting any existing bucket first so consecutive runs start clean.
func freshBucket(ctx context.Context, ijs *instrumentedjs.InstrumentedJS, bucket string, storage jetstream.StorageType, replicas int) (jetstream.KeyValue, error) {
	// Attempt delete of any existing bucket; ignore not-found.
	_ = ijs.DeleteKeyValue(ctx, bucket)

	kv, err := ijs.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:   bucket,
		History:  1,
		Storage:  storage,
		Replicas: replicas,
	})
	if err != nil {
		return nil, fmt.Errorf("create bucket %q: %w", bucket, err)
	}

	return kv, nil
}

// makeValue returns a byte slice of exactly size bytes.
func makeValue(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = 'x'
	}
	return b
}

// ---- kv-put ----------------------------------------------------------------

func runKVPut(args []string) error {
	fs := flag.NewFlagSet("kv-put", flag.ContinueOnError)
	f, err := parseCommonFlags(fs, args)
	if err != nil {
		return err
	}

	storage, _ := calibrate.ParseStorage(f.storage)
	ijs, nc, err := calibrate.NewInstrumentedJS(f.natsURLs)
	if err != nil {
		return err
	}
	defer nc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), f.duration+5*time.Second)
	defer cancel()

	kv, err := freshBucket(ctx, ijs, "calibrate-kv-put", storage, f.replicas)
	if err != nil {
		return err
	}

	value := makeValue(f.valueSize)
	ijs.Reset()
	start := time.Now()
	workCtx, workCancel := context.WithTimeout(ctx, f.duration)
	defer workCancel()
	_ = calibrate.RunRate(workCtx, f.rate, func() error {
		_, err := kv.Put(ctx, "k", value)
		return err
	})
	elapsed := time.Since(start).Seconds()

	snap := ijs.Snapshot()
	reads, writes := countRPCs(snap)
	emitCSV("kv-put", f.storage, f.replicas, f.rate, elapsed, int64(f.valueSize), reads, writes)

	return nil
}

// ---- kv-get ----------------------------------------------------------------

func runKVGet(args []string) error {
	fs := flag.NewFlagSet("kv-get", flag.ContinueOnError)
	f, err := parseCommonFlags(fs, args)
	if err != nil {
		return err
	}

	storage, _ := calibrate.ParseStorage(f.storage)
	ijs, nc, err := calibrate.NewInstrumentedJS(f.natsURLs)
	if err != nil {
		return err
	}
	defer nc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), f.duration+5*time.Second)
	defer cancel()

	kv, err := freshBucket(ctx, ijs, "calibrate-kv-get", storage, f.replicas)
	if err != nil {
		return err
	}

	// Seed a key so Gets find a warm entry (matching §M4 "warm" requirement).
	value := makeValue(f.valueSize)
	if _, err := kv.Put(ctx, "k", value); err != nil {
		return fmt.Errorf("seed key: %w", err)
	}

	ijs.Reset()
	start := time.Now()
	workCtx, workCancel := context.WithTimeout(ctx, f.duration)
	defer workCancel()
	_ = calibrate.RunRate(workCtx, f.rate, func() error {
		_, err := kv.Get(ctx, "k")
		return err
	})
	elapsed := time.Since(start).Seconds()

	snap := ijs.Snapshot()
	reads, writes := countRPCs(snap)
	emitCSV("kv-get", f.storage, f.replicas, f.rate, elapsed, int64(f.valueSize), reads, writes)

	return nil
}

// ---- kv-keys ---------------------------------------------------------------

func runKVKeys(args []string) error {
	fs := flag.NewFlagSet("kv-keys", flag.ContinueOnError)
	var (
		natsURLs string
		keysStr  string
		samples  int
		storage  string
		replicas int
	)
	fs.StringVar(&natsURLs, "nats-urls", "nats://localhost:4222", "comma-separated NATS URLs")
	fs.StringVar(&keysStr, "keys", "1000,3000", "comma-separated bucket population sizes to test")
	fs.IntVar(&samples, "samples", 100, "number of Keys() calls per population size")
	fs.StringVar(&storage, "storage", "file", "file|memory")
	fs.IntVar(&replicas, "replicas", 3, "JetStream replication factor")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := calibrate.ParseStorage(storage); err != nil {
		return err
	}

	storageSt, _ := calibrate.ParseStorage(storage)

	populations, err := parseIntList(keysStr)
	if err != nil {
		return fmt.Errorf("--keys: %w", err)
	}

	ijs, nc, err := calibrate.NewInstrumentedJS(natsURLs)
	if err != nil {
		return err
	}
	defer nc.Close()

	ctx := context.Background()

	for _, pop := range populations {
		path := fmt.Sprintf("kv-keys-k%d", pop)
		bucket := fmt.Sprintf("calibrate-kv-keys-%d", pop)

		kv, err := freshBucket(ctx, ijs, bucket, storageSt, replicas)
		if err != nil {
			return err
		}

		// Populate the bucket.
		for i := range pop {
			if _, err := kv.Put(ctx, fmt.Sprintf("key-%d", i), []byte("v")); err != nil {
				return fmt.Errorf("populate key %d: %w", i, err)
			}
		}

		ijs.Reset()
		start := time.Now()
		for range samples {
			if _, err := kv.Keys(ctx); err != nil {
				return fmt.Errorf("keys: %w", err)
			}
		}
		elapsed := time.Since(start).Seconds()

		snap := ijs.Snapshot()
		reads, writes := countRPCs(snap)
		// rate=0 signals "max-throughput" (no rate limiting); bytes_per_op is the key count
		// since there is no fixed value payload — callers interpret it as "keys per call".
		emitCSV(path, storage, replicas, 0, elapsed, int64(pop), reads, writes)
	}

	return nil
}

// ---- kv-watch --------------------------------------------------------------

func runKVWatch(args []string) error {
	fs := flag.NewFlagSet("kv-watch", flag.ContinueOnError)
	var watchersStr string
	fs.StringVar(&watchersStr, "watchers", "1,5", "comma-separated watcher counts to test")
	f, err := parseCommonFlags(fs, args)
	if err != nil {
		return err
	}

	storage, _ := calibrate.ParseStorage(f.storage)

	watcherCounts, err := parseIntList(watchersStr)
	if err != nil {
		return fmt.Errorf("--watchers: %w", err)
	}

	ijs, nc, err := calibrate.NewInstrumentedJS(f.natsURLs)
	if err != nil {
		return err
	}
	defer nc.Close()

	for _, W := range watcherCounts {
		path := fmt.Sprintf("kv-watch-w%d", W)
		bucket := fmt.Sprintf("calibrate-kv-watch-%d", W)

		ctx, cancel := context.WithTimeout(context.Background(), f.duration+5*time.Second)

		kv, err := freshBucket(ctx, ijs, bucket, storage, f.replicas)
		if err != nil {
			cancel()
			return err
		}

		// Seed a key so watchers have something to observe on start.
		value := makeValue(f.valueSize)
		if _, err := kv.Put(ctx, "k", value); err != nil {
			cancel()
			return fmt.Errorf("seed key: %w", err)
		}

		// Reset counters here so bucket-create and seed traffic is excluded.
		// Watch() registrations and subsequent Put() calls are the workload
		// window; both show up in read_rpc_ops and write_rpc_ops respectively.
		ijs.Reset()

		// Start W watchers; each drains asynchronously.
		watchers := make([]jetstream.KeyWatcher, W)
		for i := range W {
			watcher, werr := kv.Watch(ctx, "k")
			if werr != nil {
				cancel()
				return fmt.Errorf("watcher %d: %w", i, werr)
			}
			watchers[i] = watcher
			// Drain the initial value the watcher delivers immediately.
			<-watcher.Updates()
		}

		// Start watcher drain goroutines.
		watchDone := make(chan struct{})
		go func() {
			defer close(watchDone)
			for _, w := range watchers {
				for {
					select {
					case _, ok := <-w.Updates():
						if !ok {
							return
						}
					case <-ctx.Done():
						return
					}
				}
			}
		}()

		start := time.Now()
		workCtx, workCancel := context.WithTimeout(ctx, f.duration)
		_ = calibrate.RunRate(workCtx, f.rate, func() error {
			_, err := kv.Put(ctx, "k", value)
			return err
		})
		workCancel()
		elapsed := time.Since(start).Seconds()

		// Stop watchers.
		for _, w := range watchers {
			_ = w.Stop()
		}
		cancel()
		<-watchDone

		snap := ijs.Snapshot()
		reads, writes := countRPCs(snap)
		emitCSV(path, f.storage, f.replicas, f.rate, elapsed, int64(f.valueSize), reads, writes)
	}

	return nil
}

// ---- idle-pull -------------------------------------------------------------

// emitIdlePullCSV writes a single idle-pull result row. The first nine
// columns match the Phase 4a schema (with rate=0 and bytes_per_op=0
// since neither applies); the appended columns carry M4.1 grid context.
//
// leaderSamples is the ;-joined list of per-poll leader observations
// (interior of the capture window). The CSV value is enclosed in double
// quotes so a single column survives CSV parsers despite containing
// semicolons; "<error>" appears for any sample whose stream.Info call
// failed and is treated as a leader-move by leaderChanged.
//
//nolint:revive // argument-limit: CSV emitter with one positional arg per output column.
func emitIdlePullCSV(path, storage string, replicas int, durationS float64, readRPCs, writeRPCs int64, cStream int, fetchTimeout time.Duration, leaderPre, leaderPost string, leaderChanged bool, leaderSamples []string) {
	total := readRPCs + writeRPCs
	samplesJoined := strings.Join(leaderSamples, ";")
	fmt.Fprintf(writer, "%s,%s,%d,%d,%.1f,%d,%d,%d,%d,%d,%.3f,%s,%s,%t,%d,%q\n",
		path, storage, replicas, 0, durationS, 0,
		readRPCs, writeRPCs, total,
		cStream, fetchTimeout.Seconds(), leaderPre, leaderPost, leaderChanged,
		len(leaderSamples), samplesJoined)
}

// formatFetchTimeout renders a duration into a compact path-friendly
// suffix (e.g. 5s, 30s, 200ms) matching the M4.1 grid notation.
func formatFetchTimeout(d time.Duration) string {
	// time.Duration.String() already does this compactly: "5s", "30s",
	// "200ms". Strip any leading "0" hour prefix that does not appear
	// for sub-hour durations anyway.
	return d.String()
}

func runIdlePull(args []string) error {
	fs := flag.NewFlagSet("idle-pull", flag.ContinueOnError)
	var (
		natsURLs          string
		cStream           int
		fetchTimeout      time.Duration
		dataStorage       string
		replicas          int
		duration          time.Duration
		streamName        string
		consumerPrefix    string
		abortOnLeaderMove bool
		leaderPollEvery   time.Duration
	)
	fs.StringVar(&natsURLs, "nats-urls", "nats://localhost:4222", "comma-separated NATS URLs")
	fs.IntVar(&cStream, "c-stream", 0, "number of pull consumers to create on the data stream")
	fs.DurationVar(&fetchTimeout, "fetch-timeout", 5*time.Second, "pull expiry (per M4.1 grid: 5s or 30s)")
	fs.StringVar(&dataStorage, "data-storage", "file", "file|memory")
	fs.IntVar(&replicas, "replicas", 3, "JetStream replication factor")
	fs.DurationVar(&duration, "duration", 60*time.Second, "idle capture window length")
	fs.StringVar(&streamName, "stream-name", "calibrate-idle-pull", "data stream name")
	fs.StringVar(&consumerPrefix, "consumer-prefix", "calibrate-pull", "durable consumer name prefix")
	fs.BoolVar(&abortOnLeaderMove, "abort-on-leader-move", true, "exit non-zero if stream leader changes during capture")
	fs.DurationVar(&leaderPollEvery, "leader-poll-interval", 5*time.Second, "cadence for sampling stream leader during the capture window (set <=0 to disable; pre/post checks always run)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if cStream < 0 {
		return errors.New("--c-stream must be >= 0")
	}
	if fetchTimeout <= 0 {
		return errors.New("--fetch-timeout must be > 0")
	}
	if duration <= 0 {
		return errors.New("--duration must be > 0")
	}
	storage, err := calibrate.ParseStorage(dataStorage)
	if err != nil {
		return err
	}

	ijs, nc, err := calibrate.NewInstrumentedJS(natsURLs)
	if err != nil {
		return err
	}
	defer nc.Close()

	// Budget: setup + warmup (one FetchTimeout cycle) + capture + cleanup.
	totalBudget := duration + fetchTimeout + 30*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), totalBudget)
	defer cancel()

	stream, err := calibrate.CreateIdleStream(ctx, ijs, streamName, replicas, storage)
	if err != nil {
		return err
	}
	// Best-effort stream teardown so the next grid point starts fresh.
	defer func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanCancel()
		_ = ijs.DeleteStream(cleanCtx, streamName)
	}()

	leaderPre, err := calibrate.LeaderOf(ctx, stream)
	if err != nil {
		return fmt.Errorf("leader lookup (pre): %w", err)
	}

	consumers, err := calibrate.CreatePullConsumers(ctx, stream, consumerPrefix, cStream, fetchTimeout)
	if err != nil {
		return err
	}
	// Stop goroutines before returning so iterator log spam does not
	// follow stream deletion.
	defer func() {
		for _, pc := range consumers {
			pc.Stop()
		}
	}()

	// Let the consumers reach steady state: one full FetchTimeout cycle
	// plus a small slack so every iterator has issued its first pull.
	if cStream > 0 {
		warmup := fetchTimeout + 500*time.Millisecond
		select {
		case <-time.After(warmup):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	ijs.Reset()
	start := time.Now()

	// Sample leadership throughout the capture window so a leader can
	// not move-away-move-back undetected — pre/post equality would miss
	// that, but PollLeader records every intermediate sample (M4.1 line
	// 735: "If leadership moves during a capture, the grid point is
	// discarded and re-run").
	pollCtx, pollCancel := context.WithCancel(ctx)
	pollDone := make(chan []string, 1)
	go func() {
		pollDone <- calibrate.PollLeader(pollCtx, leaderPollEvery, func(c context.Context) (string, error) {
			return calibrate.LeaderOf(c, stream)
		})
	}()

	select {
	case <-time.After(duration):
	case <-ctx.Done():
		pollCancel()
		<-pollDone
		return ctx.Err()
	}
	elapsed := time.Since(start).Seconds()
	pollCancel()
	leaderSamples := <-pollDone

	leaderPost, err := calibrate.LeaderOf(ctx, stream)
	if err != nil {
		return fmt.Errorf("leader lookup (post): %w", err)
	}
	leaderMovedDuring := calibrate.LeaderMoved(leaderPre, leaderSamples)
	leaderChanged := leaderPre != leaderPost || leaderMovedDuring
	if leaderChanged && abortOnLeaderMove {
		// Keep the canonical error prefix so run-m4.sh's grep on
		// "stream leader moved during capture" still classifies this
		// as a retryable leader-move (script line ~358).
		return fmt.Errorf("stream leader moved during capture: pre=%q post=%q samples=%v (discard this grid point per M4.1)", leaderPre, leaderPost, leaderSamples)
	}

	snap := ijs.Snapshot()
	reads, writes := countRPCs(snap)

	path := fmt.Sprintf("idle-pull-c%d-t%s", cStream, formatFetchTimeout(fetchTimeout))
	emitIdlePullCSV(path, dataStorage, replicas, elapsed, reads, writes,
		cStream, fetchTimeout, leaderPre, leaderPost, leaderChanged, leaderSamples)

	return nil
}

// ---- helpers ---------------------------------------------------------------

// countRPCs partitions wrapper counters into read-side and write-side totals.
// Only KV-bucket counters are included; the synthetic [instrumentedjs.JetStreamBucket]
// (__js__) entries are excluded because they reflect JetStream-level overhead
// (stream lookups, consumer creation) rather than KV-path amplification.
//
// Reads: Get, GetRevision, Keys, ListKeys, ListKeysFiltered, Watch, WatchAll,
// WatchFiltered, History, Status.
//
// Writes: Put, PutString, Create, Update, Delete, Purge, PurgeDeletes.
func countRPCs(snap instrumentedjs.Snapshot) (reads, writes int64) {
	readOps := map[string]bool{
		"Get": true, "GetRevision": true, "Keys": true,
		"ListKeys": true, "ListKeysFiltered": true,
		"Watch": true, "WatchAll": true, "WatchFiltered": true,
		"History": true, "Status": true,
	}
	writeOps := map[string]bool{
		"Put": true, "PutString": true, "Create": true, "Update": true,
		"Delete": true, "Purge": true, "PurgeDeletes": true,
	}
	for k, v := range snap {
		if k.Bucket == instrumentedjs.JetStreamBucket {
			continue
		}
		if readOps[k.Op] {
			reads += v
		} else if writeOps[k.Op] {
			writes += v
		}
	}

	return reads, writes
}

// parseIntList splits a comma-separated string of integers and returns
// them as a slice. Returns an error if any element is not a valid integer.
func parseIntList(s string) ([]int, error) {
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("not an integer: %q", p)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, errors.New("empty list")
	}

	return out, nil
}
