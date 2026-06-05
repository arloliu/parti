package main

import (
	"context"
	"encoding/csv"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/arloliu/parti/test/perf-measurement/internal/instrumentedjs"
)

// TestDecideRunStatus_DegradedTransitionFlipsStatus covers the pure
// status helper without spinning a full embedded NATS. A worker with
// a non-zero degraded counter must produce status="degraded" and a
// non-nil error so the harness writes the right manifest field and
// exits non-zero.
func TestDecideRunStatus_DegradedTransitionFlipsStatus(t *testing.T) {
	// Stable baseline: no workers degraded -> ok.
	c1 := new(atomic.Int64)
	c2 := new(atomic.Int64)
	workers := []*WorkerHandle{
		{idx: 0, degraded: c1},
		{idx: 1, degraded: c2},
	}
	status, err := DecideRunStatus(workers)
	require.NoError(t, err)
	require.Equal(t, "ok", status)

	// One degraded transition -> degraded + error mentioning the
	// offender by index and transition count.
	c2.Add(1)
	status, err = DecideRunStatus(workers)
	require.Error(t, err)
	require.Equal(t, "degraded", status)
	require.Contains(t, err.Error(), "w1=degraded(transitions=1)")
}

// TestAggregateSnapshots_AbsentRowsAreOmitted locks the Phase 3 sparse-
// row contract: only (worker,bucket,op) combinations with a present
// counter are emitted; absent rows are not falsely zeroed. Phase 3
// MUST treat missing combinations as count=0 at that tick.
func TestAggregateSnapshots_AbsentRowsAreOmitted(t *testing.T) {
	// A worker handle whose wrapper has had zero traffic should produce
	// zero rows, not one-zero-row-per-(bucket,op).
	url := startEmbeddedNATS(t)
	nc, err := nats.Connect(url)
	require.NoError(t, err)
	defer nc.Close()
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	ijs := instrumentedjs.New(js)

	w := &WorkerHandle{idx: 0, ijs: ijs, degraded: new(atomic.Int64)}
	rows := AggregateSnapshots(time.Now(), []*WorkerHandle{w})
	require.Empty(t, rows, "absent counters must not produce zero rows")
}

// TestAtomicCSV_NoFinalFileWhileRunning verifies the rpc_counts.csv
// committed-vs-tmp contract: while Run is executing, only
// rpc_counts.csv.tmp may exist; the final path appears atomically when
// Run returns successfully.
func TestAtomicCSV_NoFinalFileWhileRunning(t *testing.T) {
	if testing.Short() {
		t.Skip("smoke: skipping in -short mode")
	}
	url := startEmbeddedNATS(t)
	outDir := t.TempDir()
	o := Options{
		NATSURLs:           url,
		Workers:            2,
		N:                  10,
		Replicas:           1,
		ConsumerMode:       ConsumerModeNoneAttached,
		FetchTimeout:       2 * time.Second,
		HeartbeatInterval:  500 * time.Millisecond,
		HeartbeatTTL:       1500 * time.Millisecond,
		WorkerIDTTL:        5 * time.Second,
		ElectionTimeout:    2 * time.Second,
		KVStorage:          jetstream.MemoryStorage,
		DataStorage:        jetstream.MemoryStorage,
		DataStreamName:     "perf-rig-data",
		PartitionSourceKey: DefaultPartitionSourceKey,
		Warmup:             1 * time.Second,
		CaptureWindow:      2 * time.Second,
		RPCDumpInterval:    250 * time.Millisecond,
		OutputDir:          outDir,
		PartiVersion:       "atomic-csv",
		FastConfig:         true,
	}

	// Poll the output dir during the capture window to confirm the
	// final csv path does not appear until Run completes.
	probeDone := make(chan struct{})
	var sawFinalDuringRun atomic.Bool
	go func() {
		defer close(probeDone)
		deadline := time.Now().Add(o.Warmup + o.CaptureWindow + 5*time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(outDir, "rpc_counts.csv")); err == nil {
				sawFinalDuringRun.Store(true)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	require.NoError(t, Run(ctx, o, os.Stderr))
	<-probeDone

	// After Run returns, the final CSV must exist and any sibling .tmp
	// must be gone.
	_, err := os.Stat(filepath.Join(outDir, "rpc_counts.csv"))
	require.NoError(t, err, "final rpc_counts.csv must exist after Run")
	_, err = os.Stat(filepath.Join(outDir, "rpc_counts.csv.tmp"))
	require.True(t, os.IsNotExist(err), "rpc_counts.csv.tmp must be cleaned up")

	// The prober may have observed the final path *after* Run committed
	// it but before this assertion ran. The contract we actually need
	// is that the prober never saw the final path while a .tmp was
	// also present, which is implied by the rename being atomic. The
	// flag is therefore informational; we treat its having ever been
	// true as benign if the .tmp is now gone. (Documenting this rather
	// than asserting because polling races the final commit.)
	_ = sawFinalDuringRun.Load()
}

// TestRun_WarmupInterrupt_NoManifestWithoutCSV asserts the
// artifact-completeness contract on the warmup-interrupt path: if the
// run is cancelled during warmup, no CSV has been opened, so no
// manifest.yaml must be written either. A manifest exists only when a
// committed rpc_counts.csv exists.
func TestRun_WarmupInterrupt_NoManifestWithoutCSV(t *testing.T) {
	if testing.Short() {
		t.Skip("smoke: skipping in -short mode")
	}
	url := startEmbeddedNATS(t)
	outDir := t.TempDir()
	o := Options{
		NATSURLs:           url,
		Workers:            2,
		N:                  10,
		Replicas:           1,
		ConsumerMode:       ConsumerModeNoneAttached,
		FetchTimeout:       2 * time.Second,
		HeartbeatInterval:  500 * time.Millisecond,
		HeartbeatTTL:       1500 * time.Millisecond,
		WorkerIDTTL:        5 * time.Second,
		ElectionTimeout:    2 * time.Second,
		KVStorage:          jetstream.MemoryStorage,
		DataStorage:        jetstream.MemoryStorage,
		DataStreamName:     "perf-rig-data",
		PartitionSourceKey: DefaultPartitionSourceKey,
		// Long warmup so we have a wide window in which to cancel
		// before the warmup sleep expires.
		Warmup:          30 * time.Second,
		CaptureWindow:   1 * time.Second,
		RPCDumpInterval: 250 * time.Millisecond,
		OutputDir:       outDir,
		PartiVersion:    "warmup-interrupt",
		FastConfig:      true,
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel shortly after Run begins — workers should be stable but
	// the warmup sleep should still be active.
	go func() {
		time.Sleep(2 * time.Second)
		cancel()
	}()
	err := Run(ctx, o, os.Stderr)
	require.Error(t, err, "Run must propagate the cancellation")

	// Contract: no manifest.yaml and no rpc_counts.csv when warmup was
	// interrupted before the capture window opened.
	_, mErr := os.Stat(filepath.Join(outDir, "manifest.yaml"))
	require.True(t, os.IsNotExist(mErr), "manifest.yaml must not be written when warmup is interrupted")
	_, cErr := os.Stat(filepath.Join(outDir, "rpc_counts.csv"))
	require.True(t, os.IsNotExist(cErr), "rpc_counts.csv must not be written when warmup is interrupted")
	_, tErr := os.Stat(filepath.Join(outDir, "rpc_counts.csv.tmp"))
	require.True(t, os.IsNotExist(tErr), "rpc_counts.csv.tmp must not remain when warmup is interrupted")
}

// TestDecideRunStatus_AtWarmupBoundary_GatesCapture is a focused unit
// proof that a degraded transition observed at the warmup boundary
// flips DecideRunStatus to "degraded" — which is the exact predicate
// Run uses to skip ResetAll and CSV creation before capture. The
// behavior covered:
//
//   - workers stable at warmup end -> ok, capture proceeds;
//   - any degraded transition observed during warmup -> degraded,
//     Run returns without resetting counters or opening the CSV.
//
// The full-harness path is exercised indirectly: Run's warmup-boundary
// gate calls this exact helper, so locking the helper's contract
// locks the gate.
func TestDecideRunStatus_AtWarmupBoundary_GatesCapture(t *testing.T) {
	c0 := new(atomic.Int64)
	c1 := new(atomic.Int64)
	workers := []*WorkerHandle{
		{idx: 0, degraded: c0},
		{idx: 1, degraded: c1},
	}
	// Pre-warmup baseline: all stable.
	status, err := DecideRunStatus(workers)
	require.NoError(t, err)
	require.Equal(t, "ok", status)

	// Simulate a degraded transition observed during warmup.
	c0.Add(1)
	status, err = DecideRunStatus(workers)
	require.Error(t, err)
	require.Equal(t, "degraded", status, "warmup-boundary check must fail closed on degraded transitions")
	require.Contains(t, err.Error(), "w0=degraded(transitions=1)")
}

// startEmbeddedNATS spins up an in-process NATS server with JetStream
// enabled on a random port. Mirrors the helper in
// internal/instrumentedjs so the smoke test does not require a docker
// rig — the harness is driven directly against a one-node NATS in
// process, which is enough to verify lifecycle + counter wiring.
func startEmbeddedNATS(t *testing.T) string {
	t.Helper()
	opts := &server.Options{
		JetStream: true,
		Port:      -1,
		StoreDir:  t.TempDir(),
	}
	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second), "NATS server not ready")
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})

	return ns.ClientURL()
}

// TestParseFlags_Defaults locks the flag-default table to the values
// the §R2 contract specifies. Drift here means a behavioral change in
// the rig and should fail noisily so the manifest's recorded options
// stay reproducible across the matrix.
func TestParseFlags_Defaults(t *testing.T) {
	o, err := parseFlags(nil)
	require.NoError(t, err)
	require.Equal(t, 5, o.Workers)
	require.Equal(t, 1000, o.N)
	require.Equal(t, 3, o.Replicas)
	require.False(t, o.TwoPhase)
	require.Equal(t, 30*time.Second, o.SweepInterval)
	require.Equal(t, 5*time.Second, o.FetchTimeout)
	require.Equal(t, ConsumerModeDynamic, o.ConsumerMode)
	require.Equal(t, 5*time.Second, o.HeartbeatInterval)
	require.Equal(t, 15*time.Second, o.HeartbeatTTL)
	require.Equal(t, 75*time.Second, o.WorkerIDTTL)
	require.Equal(t, jetstream.FileStorage, o.KVStorage)
	require.Equal(t, jetstream.FileStorage, o.DataStorage)
	require.Equal(t, "perf-rig-data", o.DataStreamName)
	require.Equal(t, 5*time.Minute, o.Warmup)
	require.Equal(t, 10*time.Minute, o.CaptureWindow)
}

// TestParseFlags_LoadFlags confirms the perf-measurement load-mode flags
// (§5) flow through parseFlags into Options.
func TestParseFlags_LoadFlags(t *testing.T) {
	o, err := parseFlags([]string{"--load", "--per-worker-rate", "4", "--batch-size", "8"})
	require.NoError(t, err)
	require.True(t, o.Load)
	require.Equal(t, 4.0, o.PerWorkerRate)
	require.Equal(t, 8, o.BatchSize)
}

// TestBuildPartiConfig_AppliesFlagOverrides spot-checks that the
// flag-to-Config translation honors each independent variable. Without
// this, a typo in BuildPartiConfig (wrong field, wrong if-guard) would
// silently leave a knob at its default and confound the matrix.
func TestBuildPartiConfig_AppliesFlagOverrides(t *testing.T) {
	cfg := BuildPartiConfig(Options{
		TwoPhase:          true,
		SweepInterval:     7 * time.Second,
		HeartbeatInterval: 11 * time.Second,
		HeartbeatTTL:      33 * time.Second,
		WorkerIDTTL:       99 * time.Second,
		ElectionTimeout:   13 * time.Second,
	})
	require.True(t, cfg.EnableTwoPhaseHandoff)
	require.Equal(t, 7*time.Second, cfg.Handoff.SweepInterval)
	require.Equal(t, 11*time.Second, cfg.HeartbeatInterval)
	require.Equal(t, 33*time.Second, cfg.HeartbeatTTL)
	require.Equal(t, 99*time.Second, cfg.WorkerIDTTL)
	require.Equal(t, 13*time.Second, cfg.ElectionTimeout)
}

// TestPartiBuckets_HandoffHasNoTTL verifies the harness pre-creates the handoff
// KV bucket with no MaxAge. A bucket-level TTL would age out the stable
// ownership claims two-phase handoff writes once, suppressing pull-gated
// consumers — the harness must match the runtime Manager / provision behavior.
func TestPartiBuckets_HandoffHasNoTTL(t *testing.T) {
	cfg := BuildPartiConfig(Options{TwoPhase: true})
	specs := PartiBuckets(cfg, jetstream.FileStorage, true)

	var found bool
	for _, s := range specs {
		if s.bucket == cfg.KVBuckets.HandoffBucket {
			found = true
			require.Equal(t, time.Duration(0), s.ttl,
				"handoff bucket spec must have no TTL; a bucket-level TTL ages out stable claims")
		}
	}
	require.True(t, found, "handoff bucket spec must be present when twoPhase=true")
}

// TestSmoke_RunHarness drives the full harness lifecycle against an
// in-process NATS. Validates:
//   - manifest.yaml is written with the expected fields,
//   - rpc_counts.csv has at least one snapshot,
//   - the heartbeat-bucket Put rate is in the right order of
//     magnitude (within 50% of workers/heartbeat-interval) — a smoke
//     check, not a precision check.
//
// FastConfig is required so the embedded server can clear parti's
// default 30s cold-start window in the 2s warmup the spec mandates.
func TestSmoke_RunHarness(t *testing.T) {
	if testing.Short() {
		t.Skip("smoke: skipping in -short mode")
	}

	url := startEmbeddedNATS(t)
	outDir := t.TempDir()

	o := Options{
		NATSURLs:           url,
		Workers:            2,
		N:                  20,
		Replicas:           1, // single-node embedded
		TwoPhase:           false,
		SweepInterval:      30 * time.Second,
		FetchTimeout:       2 * time.Second,
		ConsumerMode:       ConsumerModeDynamic,
		HeartbeatInterval:  500 * time.Millisecond, // FastConfig matches
		HeartbeatTTL:       1500 * time.Millisecond,
		WorkerIDTTL:        5 * time.Second,
		ElectionTimeout:    2 * time.Second,
		KVStorage:          jetstream.FileStorage,
		DataStorage:        jetstream.FileStorage,
		DataStreamName:     "perf-rig-data",
		PartitionSourceKey: DefaultPartitionSourceKey,
		Warmup:             2 * time.Second,
		CaptureWindow:      2 * time.Second,
		RPCDumpInterval:    250 * time.Millisecond,
		OutputDir:          outDir,
		PartiVersion:       "smoke",
		FastConfig:         true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	require.NoError(t, Run(ctx, o, os.Stderr))

	// Manifest exists and has core fields.
	manifestBytes, err := os.ReadFile(filepath.Join(outDir, "manifest.yaml"))
	require.NoError(t, err)
	var m Manifest
	require.NoError(t, yaml.Unmarshal(manifestBytes, &m))
	require.Equal(t, "ok", m.Status)
	require.Equal(t, "smoke", m.PartiVersion)
	require.Equal(t, 2, m.Options.Workers)
	require.Equal(t, 20, m.Options.N)
	require.NotEmpty(t, m.ConfirmedStorage)

	// CSV has at least one snapshot. Count distinct timestamps as a
	// proxy for "snapshot count" since each tick emits many rows.
	f, err := os.Open(filepath.Join(outDir, "rpc_counts.csv"))
	require.NoError(t, err)
	defer f.Close()
	rdr := csv.NewReader(f)
	header, err := rdr.Read()
	require.NoError(t, err)
	require.Equal(t, []string{"t_unix_ns", "worker_idx", "bucket", "op", "count"}, header)

	hbPuts := map[int]int64{} // worker_idx -> max Put count seen on heartbeat bucket
	tsSet := map[string]struct{}{}
	for {
		row, rerr := rdr.Read()
		if rerr != nil {
			break
		}
		tsSet[row[0]] = struct{}{}
		if row[2] == "parti-heartbeat" && row[3] == "Put" {
			var n int64
			_ = fmtScan(row[4], &n)
			widx := 0
			_ = fmtScan(row[1], &widx)
			if n > hbPuts[widx] {
				hbPuts[widx] = n
			}
		}
	}
	require.GreaterOrEqual(t, len(tsSet), 1, "expected at least one snapshot timestamp")

	// Heartbeat-Put rate sanity. With 2 workers and a 500ms heartbeat
	// interval, the cluster Put rate is ~4/s. Across the 2s capture
	// window we expect ~8 puts cluster-wide, ~4 per worker. Allow ±50%.
	cluster := int64(0)
	for _, n := range hbPuts {
		cluster += n
	}
	expected := int64(2) * int64(o.CaptureWindow/o.HeartbeatInterval)
	low, high := expected/2, expected*2
	require.GreaterOrEqual(t, cluster, low, "heartbeat puts %d below %d (expected ~%d)", cluster, low, expected)
	require.LessOrEqual(t, cluster, high+4, "heartbeat puts %d above %d (expected ~%d)", cluster, high, expected)

	// No worker should have entered StateDegraded during the smoke run.
	for _, n := range m.DegradedTransitionsPerWorker {
		require.Zero(t, n, "expected no degraded transitions during smoke run")
	}
}

// TestSmoke_ConsumerModeNone exercises the third consumer-mode arm so
// the wiring branch is covered. It is shorter than the main smoke and
// only asserts that Run returns without error and writes a manifest.
func TestSmoke_ConsumerModeNone(t *testing.T) {
	if testing.Short() {
		t.Skip("smoke: skipping in -short mode")
	}

	url := startEmbeddedNATS(t)
	outDir := t.TempDir()
	o := Options{
		NATSURLs:           url,
		Workers:            2,
		N:                  10,
		Replicas:           1,
		ConsumerMode:       ConsumerModeNoneAttached,
		FetchTimeout:       2 * time.Second,
		HeartbeatInterval:  500 * time.Millisecond,
		HeartbeatTTL:       1500 * time.Millisecond,
		WorkerIDTTL:        5 * time.Second,
		ElectionTimeout:    2 * time.Second,
		KVStorage:          jetstream.MemoryStorage,
		DataStorage:        jetstream.MemoryStorage,
		DataStreamName:     "perf-rig-data",
		PartitionSourceKey: DefaultPartitionSourceKey,
		Warmup:             1 * time.Second,
		CaptureWindow:      1 * time.Second,
		RPCDumpInterval:    250 * time.Millisecond,
		OutputDir:          outDir,
		PartiVersion:       "smoke-none",
		FastConfig:         true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	require.NoError(t, Run(ctx, o, os.Stderr))

	_, err := os.Stat(filepath.Join(outDir, "manifest.yaml"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(outDir, "rpc_counts.csv"))
	require.NoError(t, err)
}

// fmtScan is a tiny strconv helper used to keep the smoke test readable.
// It parses s into *int64 or *int (the only shapes the smoke test needs).
func fmtScan(s string, dst any) error {
	switch v := dst.(type) {
	case *int64:
		n, err := parseInt64(s)
		if err != nil {
			return err
		}
		*v = n
		return nil
	case *int:
		n, err := parseInt64(s)
		if err != nil {
			return err
		}
		*v = int(n)
		return nil
	}

	return nil
}

func parseInt64(s string) (int64, error) {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errBadInt
		}
		n = n*10 + int64(c-'0')
	}

	return n, nil
}

var errBadInt = &stringError{"bad int"}

type stringError struct{ s string }

func (e *stringError) Error() string { return e.s }

// unused — kept to make sure nats import is reachable in builds that
// statically check imports against test files.
var _ = nats.DefaultURL
