// Package main implements the Parti workload harness described in
// docs/plans/iops-investigation/00-attribution-plan.md §R2.
//
// This file holds the harness primitives — flag struct, stream
// pre-creation, run lifecycle, snapshot aggregation, manifest emission —
// extracted from main() so the smoke test can drive them directly
// without spawning a child process.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"gopkg.in/yaml.v3"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"

	"github.com/arloliu/parti/test/iops-investigation/internal/instrumentedjs"
	"github.com/arloliu/parti/test/iops-investigation/internal/storageverify"
)

// ConsumerMode enumerates the three consumer wirings the matrix exercises.
type ConsumerMode string

const (
	// ConsumerModeDynamic exercises the partition-fanout consumer
	// (one durable per partition) — H2 baseline.
	ConsumerModeDynamic ConsumerMode = "dynamic"
	// ConsumerModeQueue exercises the single-durable Queue consumer —
	// H2.B ablation.
	ConsumerModeQueue ConsumerMode = "queue"
	// ConsumerModeNoneAttached starts no consumer module — H2 floor
	// (no per-partition pull consumers exist at all).
	ConsumerModeNoneAttached ConsumerMode = "none-attached"
)

// Options is the full set of harness knobs. Every field is wired to a
// command-line flag in main.go; defining the struct separately means
// tests can drive the run directly with a literal.
type Options struct {
	NATSURLs           string
	Workers            int
	N                  int
	Replicas           int
	TwoPhase           bool
	SweepInterval      time.Duration
	FetchTimeout       time.Duration
	ConsumerMode       ConsumerMode
	HeartbeatInterval  time.Duration
	HeartbeatTTL       time.Duration
	WorkerIDTTL        time.Duration
	ElectionTimeout    time.Duration
	KVStorage          jetstream.StorageType
	DataStorage        jetstream.StorageType
	DataStreamName     string
	PartitionSourceKey string // KV key inside the partition-source bucket
	Warmup             time.Duration
	CaptureWindow      time.Duration
	RPCDumpInterval    time.Duration
	OutputDir          string
	PartiVersion       string
	// FastConfig swaps in parti.TestConfig() timings so the smoke test
	// can converge in under a few seconds; production runs leave this
	// false and rely on the explicit duration flags above.
	FastConfig bool
}

// PartitionSourceBucket is the JetStream-KV bucket the harness creates
// for the partition source. Distinct from the parti-managed buckets so
// the §R3 coverage caveat is visible by name in rpc_counts.csv.
const PartitionSourceBucket = "iops-rig-partitions"

// PartitionSourceKey is the default partition-list key inside
// PartitionSourceBucket.
const DefaultPartitionSourceKey = "partitions"

// QueueConsumerName / QueueFilterSubject are the names used when
// ConsumerMode == queue. The filter subject mirrors the Dynamic
// template's >-wildcard so the same data stream backs both modes.
const (
	queueConsumerName  = "iops-rig-queue"
	dynamicSubjectTmpl = "iops.rig.{{.PartitionID}}"
	queueFilterSubject = "iops.rig.>"
	dataStreamSubject  = "iops.rig.>"
	dynamicPrefix      = "iops-rig"
)

// ParseStorage maps the CLI string ("file" / "memory") to a
// [jetstream.StorageType]. The error message echoes the raw input so
// typos surface clearly.
func ParseStorage(s string) (jetstream.StorageType, error) {
	switch s {
	case "file":
		return jetstream.FileStorage, nil
	case "memory":
		return jetstream.MemoryStorage, nil
	default:
		return jetstream.FileStorage, fmt.Errorf("invalid storage type %q (want file|memory)", s)
	}
}

// ParseConsumerMode validates and returns a ConsumerMode.
func ParseConsumerMode(s string) (ConsumerMode, error) {
	switch ConsumerMode(s) {
	case ConsumerModeDynamic, ConsumerModeQueue, ConsumerModeNoneAttached:
		return ConsumerMode(s), nil
	default:
		return "", fmt.Errorf("invalid consumer mode %q (want dynamic|queue|none-attached)", s)
	}
}

// BuildPartiConfig translates harness Options to a parti.Config. The
// returned value is suitable for parti.NewManager after validation /
// defaulting (parti.NewManager calls SetDefaults internally so passing
// the partially-populated value is safe).
func BuildPartiConfig(o Options) parti.Config {
	var cfg parti.Config
	if o.FastConfig {
		cfg = parti.TestConfig()
	} else {
		cfg = parti.DefaultConfig()
	}
	cfg.WorkerIDPrefix = "iops-rig-worker"
	cfg.WorkerIDMin = 0
	cfg.WorkerIDMax = 9999
	if o.HeartbeatInterval > 0 {
		cfg.HeartbeatInterval = o.HeartbeatInterval
	}
	if o.HeartbeatTTL > 0 {
		cfg.HeartbeatTTL = o.HeartbeatTTL
	}
	if o.WorkerIDTTL > 0 {
		cfg.WorkerIDTTL = o.WorkerIDTTL
	}
	if o.ElectionTimeout > 0 {
		cfg.ElectionTimeout = o.ElectionTimeout
	}
	cfg.EnableTwoPhaseHandoff = o.TwoPhase
	if o.TwoPhase && o.SweepInterval > 0 {
		cfg.Handoff.SweepInterval = o.SweepInterval
	}
	// EmergencyGracePeriod is computed from HeartbeatInterval by parti
	// defaults; reset it to zero so SetDefaults (called inside
	// NewManager) recomputes against our possibly-shorter interval.
	cfg.EmergencyGracePeriod = 0
	return cfg
}

// streamSpec describes one stream the harness must pre-create. The
// harness builds one spec per parti-managed KV bucket plus one for the
// data stream and one for the partition source bucket.
type streamSpec struct {
	bucket  string // KV bucket name
	stream  string // JetStream stream name (== "KV_" + bucket for KV buckets)
	storage jetstream.StorageType
	ttl     time.Duration
}

// PartiBuckets returns the KV bucket specs the harness must pre-create
// to control storage class. Election + heartbeat are normally Memory in
// parti v2.3.0 but the harness over-rides every bucket with --kv-storage
// so the manifest's storage class is the single source of truth (see
// the §M5 / §R4 hygiene requirement).
func PartiBuckets(cfg parti.Config, kvStorage jetstream.StorageType, twoPhase bool) []streamSpec {
	specs := []streamSpec{
		{bucket: cfg.KVBuckets.StableIDBucket, stream: kvStreamName(cfg.KVBuckets.StableIDBucket), storage: kvStorage, ttl: cfg.WorkerIDTTL},
		{bucket: cfg.KVBuckets.ElectionBucket, stream: kvStreamName(cfg.KVBuckets.ElectionBucket), storage: kvStorage, ttl: cfg.ElectionTimeout},
		{bucket: cfg.KVBuckets.HeartbeatBucket, stream: kvStreamName(cfg.KVBuckets.HeartbeatBucket), storage: kvStorage, ttl: cfg.HeartbeatTTL},
		{bucket: cfg.KVBuckets.AssignmentBucket, stream: kvStreamName(cfg.KVBuckets.AssignmentBucket), storage: kvStorage, ttl: cfg.KVBuckets.AssignmentTTL},
	}
	if twoPhase {
		specs = append(specs, streamSpec{
			bucket: cfg.KVBuckets.HandoffBucket, stream: kvStreamName(cfg.KVBuckets.HandoffBucket),
			storage: kvStorage, ttl: cfg.KVBuckets.HandoffTTL,
		})
	}
	return specs
}

// kvStreamName mirrors nats.go's "KV_<bucket>" stream-naming
// convention so the rig can query stream info by stream name.
func kvStreamName(bucket string) string { return "KV_" + bucket }

// PreCreate pre-creates every parti-managed KV bucket, the partition
// source bucket, and the data stream against the supplied setup
// wrapper. The setup wrapper is a separate InstrumentedJS that does
// NOT share counters with any per-worker manager — its traffic is
// pre-population, not workload, and must not contaminate rpc_counts.
//
// Returns the wrapped partition-source KeyValue so the caller can hand
// it to source.NewNatsKV (per §R3 coverage caveat); writes into that
// handle will be counted under PartitionSourceBucket on the SETUP
// wrapper, which is what we want for visibility.
func PreCreate(
	ctx context.Context,
	setup *instrumentedjs.InstrumentedJS,
	o Options,
	cfg parti.Config,
) (jetstream.KeyValue, error) {
	// KV buckets.
	for _, spec := range PartiBuckets(cfg, o.KVStorage, o.TwoPhase) {
		kvCfg := jetstream.KeyValueConfig{
			Bucket:   spec.bucket,
			History:  1,
			Storage:  spec.storage,
			Replicas: o.Replicas,
		}
		if spec.ttl > 0 {
			kvCfg.TTL = spec.ttl
		}
		if _, err := setup.CreateKeyValue(ctx, kvCfg); err != nil {
			return nil, fmt.Errorf("pre-create KV bucket %q: %w", spec.bucket, err)
		}
	}

	// Partition source bucket — file storage by default (matches the
	// user's prod assumption; an O(1) bucket so storage class does not
	// move the needle).
	srcKV, err := setup.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:   PartitionSourceBucket,
		History:  1,
		Storage:  o.KVStorage,
		Replicas: o.Replicas,
	})
	if err != nil {
		return nil, fmt.Errorf("pre-create partition-source bucket: %w", err)
	}

	// Data stream.
	if _, err := setup.CreateStream(ctx, jetstream.StreamConfig{
		Name:     o.DataStreamName,
		Subjects: []string{dataStreamSubject},
		Storage:  o.DataStorage,
		Replicas: o.Replicas,
	}); err != nil {
		return nil, fmt.Errorf("pre-create data stream %q: %w", o.DataStreamName, err)
	}

	return srcKV, nil
}

// ExpectedStreams returns the storageverify.Expected slice covering
// every stream the harness pre-created. The caller hands this to
// storageverify.Verify so a silent mismatch surfaces before the capture
// window opens.
func ExpectedStreams(o Options, cfg parti.Config) []storageverify.Expected {
	out := []storageverify.Expected{}
	for _, spec := range PartiBuckets(cfg, o.KVStorage, o.TwoPhase) {
		out = append(out, storageverify.Expected{
			Stream: spec.stream, Storage: spec.storage, Replicas: o.Replicas,
		})
	}
	out = append(out, storageverify.Expected{
		Stream: kvStreamName(PartitionSourceBucket), Storage: o.KVStorage, Replicas: o.Replicas,
	})
	out = append(out, storageverify.Expected{
		Stream: o.DataStreamName, Storage: o.DataStorage, Replicas: o.Replicas,
	})
	return out
}

// SeedPartitions writes a partition list of length n to the
// partition-source KV via source.NatsKV.Update, the same code path
// parti would use on its own. Partitions are named "p-<index>".
func SeedPartitions(ctx context.Context, srcKV jetstream.KeyValue, key string, n int) error {
	src := source.NewNatsKV(srcKV, key, nil, source.WithReconcileInterval(0))
	if err := src.Start(ctx); err != nil {
		return fmt.Errorf("start partition source for seeding: %w", err)
	}
	defer func() { _ = src.Stop(ctx) }()

	parts := make([]types.Partition, n)
	for i := range n {
		parts[i] = types.Partition{Keys: []string{fmt.Sprintf("p-%d", i)}}
	}
	if err := src.Update(ctx, parts); err != nil {
		return fmt.Errorf("seed %d partitions: %w", n, err)
	}
	return nil
}

// noopHandler is the message handler attached to consumers. Idle
// captures should never fire it, but it must exist so the consumer
// constructors accept the wiring.
type noopHandler struct{}

func (noopHandler) Handle(_ context.Context, _ jetstream.Msg) error { return nil }

// WorkerHandle bundles the per-worker resources the run loop tracks.
// Each manager owns its own NATS connection, its own InstrumentedJS
// (so its counters are independent — Phase 3 aggregates across workers
// by worker_idx, not in-process), and optionally a consumer module.
type WorkerHandle struct {
	idx         int
	nc          *nats.Conn
	ijs         *instrumentedjs.InstrumentedJS
	manager     *parti.Manager
	consumerDyn *consumer.Dynamic
	consumerQ   *consumer.Queue
	degraded    *atomic.Int64 // count of degraded transitions observed
}

// StartWorker connects to NATS, wraps the resulting JetStream in a
// fresh InstrumentedJS, builds a parti.Manager with config from the
// harness Options, wires the configured consumer mode, and calls
// Start.
//
// The caller is responsible for invoking the returned WorkerHandle's
// Stop method.
func StartWorker(
	ctx context.Context,
	idx int,
	o Options,
	cfg parti.Config,
) (*WorkerHandle, error) {
	nc, err := nats.Connect(o.NATSURLs)
	if err != nil {
		return nil, fmt.Errorf("worker %d: connect: %w", idx, err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("worker %d: jetstream.New: %w", idx, err)
	}
	ijs := instrumentedjs.New(js)

	// Source: each manager opens its own wrapped KV handle to the
	// partition-source bucket so its watcher / reconcile traffic is
	// attributed to this worker's wrapper (covering the §R3 caveat).
	srcKV, err := ijs.KeyValue(ctx, PartitionSourceBucket)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("worker %d: open partition source bucket: %w", idx, err)
	}
	src := source.NewNatsKV(srcKV, o.PartitionSourceKey, nil)

	wh := &WorkerHandle{idx: idx, nc: nc, ijs: ijs, degraded: new(atomic.Int64)}

	hooks := &types.Hooks{
		OnStateChanged: func(_ context.Context, _, to parti.State) error {
			if to == parti.StateDegraded {
				wh.degraded.Add(1)
			}
			return nil
		},
	}

	opts := []parti.Option{parti.WithHooks(hooks)}

	// Wire the consumer. Dynamic registers as the WorkerConsumerUpdater
	// so the manager applies assignment changes to it; Queue runs
	// independently (one durable on the data stream).
	switch o.ConsumerMode {
	case ConsumerModeDynamic:
		dyn, derr := consumer.NewDynamic(
			ijs, o.DataStreamName, dynamicPrefix, dynamicSubjectTmpl, noopHandler{},
			consumer.WithFetchTimeout(o.FetchTimeout),
		)
		if derr != nil {
			nc.Close()
			return nil, fmt.Errorf("worker %d: consumer.NewDynamic: %w", idx, derr)
		}
		wh.consumerDyn = dyn
		opts = append(opts, parti.WithWorkerConsumerUpdater(dyn))
	case ConsumerModeQueue:
		// Queue is single-consumer-name across the cluster; all
		// workers join the same durable.
		q, qerr := consumer.NewQueue(
			ijs, o.DataStreamName, queueConsumerName, queueFilterSubject, noopHandler{},
			consumer.WithFetchTimeout(o.FetchTimeout),
		)
		if qerr != nil {
			nc.Close()
			return nil, fmt.Errorf("worker %d: consumer.NewQueue: %w", idx, qerr)
		}
		wh.consumerQ = q
	case ConsumerModeNoneAttached:
		// No consumer module — H2 floor.
	}

	mgr, err := parti.NewManager(&cfg, ijs, src, strategy.NewConsistentHash(), opts...)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("worker %d: NewManager: %w", idx, err)
	}
	wh.manager = mgr

	if err := mgr.Start(ctx); err != nil {
		nc.Close()
		return nil, fmt.Errorf("worker %d: Start: %w", idx, err)
	}

	if wh.consumerQ != nil {
		if err := wh.consumerQ.Start(ctx); err != nil {
			_ = mgr.Stop(ctx)
			nc.Close()
			return nil, fmt.Errorf("worker %d: Queue.Start: %w", idx, err)
		}
	}

	return wh, nil
}

// Stop performs a best-effort orderly shutdown of one worker. Errors
// are collected and returned via errors.Join so the caller sees every
// failure rather than only the first.
func (wh *WorkerHandle) Stop(ctx context.Context) error {
	var errs []error
	if wh.consumerQ != nil {
		if err := wh.consumerQ.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("queue stop: %w", err))
		}
	}
	if wh.consumerDyn != nil {
		if err := wh.consumerDyn.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("dynamic stop: %w", err))
		}
	}
	if wh.manager != nil {
		if err := wh.manager.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("manager stop: %w", err))
		}
	}
	if wh.nc != nil {
		wh.nc.Close()
	}
	return errors.Join(errs...)
}

// WaitStableAll waits for every worker to reach StateStable within
// timeout, then returns an error if any worker is StateDegraded. The
// function polls on a short interval because Manager.WaitState is
// per-instance and we need a cluster-wide gate.
func WaitStableAll(workers []*WorkerHandle, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		allStable := true
		for _, w := range workers {
			s := w.manager.State()
			if s == parti.StateDegraded {
				return fmt.Errorf("worker %d entered StateDegraded before warmup", w.idx)
			}
			if s != parti.StateStable {
				allStable = false
			}
		}
		if allStable {
			return nil
		}
		if time.Now().After(deadline) {
			states := make([]string, len(workers))
			for i, w := range workers {
				states[i] = fmt.Sprintf("w%d=%s", w.idx, w.manager.State().String())
			}
			return fmt.Errorf("timeout %s waiting for all workers Stable: %v", timeout, states)
		}
		<-tick.C
	}
}

// SnapshotRow is one CSV row emitted by Capture: a single
// (worker_idx, bucket, op, count) tuple at one tick. Phase 3's
// aggregator sums across workers to produce per-bucket cluster totals.
type SnapshotRow struct {
	TUnixNs  int64
	WorkerID int
	Bucket   string
	Op       string
	Count    int64
}

// AggregateSnapshots returns one SnapshotRow per (worker, bucket, op)
// at time t for the supplied wrappers. The rows are deterministically
// sorted by (worker_idx, bucket, op) so test golden output is stable
// regardless of Go's map iteration order.
//
// CSV sparse-row contract for Phase 3: only (worker, bucket, op)
// combinations that have a non-zero counter at snapshot time are
// emitted. Absent rows MUST be interpreted as count=0 for that tick.
// This contract avoids coupling Phase 2 to a static op catalogue;
// Phase 3's aggregator folds over present rows and treats missing
// combinations as zero.
func AggregateSnapshots(t time.Time, workers []*WorkerHandle) []SnapshotRow {
	out := []SnapshotRow{}
	tNs := t.UnixNano()
	for _, w := range workers {
		snap := w.ijs.Snapshot()
		for k, v := range snap {
			out = append(out, SnapshotRow{
				TUnixNs: tNs, WorkerID: w.idx, Bucket: k.Bucket, Op: k.Op, Count: v,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].WorkerID != out[j].WorkerID {
			return out[i].WorkerID < out[j].WorkerID
		}
		if out[i].Bucket != out[j].Bucket {
			return out[i].Bucket < out[j].Bucket
		}
		return out[i].Op < out[j].Op
	})
	return out
}

// ResetAll clears the counters on every worker's wrapper. Called once
// between warmup and capture so the capture window starts at zero.
func ResetAll(workers []*WorkerHandle) {
	for _, w := range workers {
		w.ijs.Reset()
	}
}

// Manifest is the per-run record written to manifest.yaml. The shape
// matches what Phase 3 reads when it joins harness output to cgroup
// iostat — Phase 3 needs every flag value plus the resolved storage
// classes so the (config, N) cell can be reconstructed from disk alone.
type Manifest struct {
	StartedAt                    time.Time        `yaml:"startedAt"`
	EndedAt                      time.Time        `yaml:"endedAt"`
	Status                       string           `yaml:"status"`
	PartiVersion                 string           `yaml:"partiVersion"`
	NATSImage                    string           `yaml:"natsImage,omitempty"`
	NATSImageDigest              string           `yaml:"natsImageDigest,omitempty"`
	RunIndex                     string           `yaml:"runIndex,omitempty"`
	Options                      ManifestOptions  `yaml:"options"`
	ConfirmedStorage             []ManifestStream `yaml:"confirmedStorage"`
	DegradedTransitionsPerWorker map[int]int64    `yaml:"degradedTransitionsPerWorker"`
}

// ManifestOptions mirrors the harness flag set. Stored as strings for
// the bool/storage/duration fields so the YAML is round-trippable by
// any reader, not just Go.
type ManifestOptions struct {
	NATSURLs          string `yaml:"natsUrls"`
	Workers           int    `yaml:"workers"`
	N                 int    `yaml:"n"`
	Replicas          int    `yaml:"replicas"`
	TwoPhase          bool   `yaml:"twoPhase"`
	SweepInterval     string `yaml:"sweepInterval"`
	FetchTimeout      string `yaml:"fetchTimeout"`
	ConsumerMode      string `yaml:"consumerMode"`
	HeartbeatInterval string `yaml:"heartbeatInterval"`
	HeartbeatTTL      string `yaml:"heartbeatTtl"`
	WorkerIDTTL       string `yaml:"workerIdTtl"`
	ElectionTimeout   string `yaml:"electionTimeout"`
	KVStorage         string `yaml:"kvStorage"`
	DataStorage       string `yaml:"dataStorage"`
	DataStreamName    string `yaml:"dataStreamName"`
	Warmup            string `yaml:"warmup"`
	CaptureWindow     string `yaml:"captureWindow"`
	RPCDumpInterval   string `yaml:"rpcDumpInterval"`
	OutputDir         string `yaml:"outputDir"`
}

// ManifestStream records the storage class confirmed by
// storageverify.Verify. Phase 3 sanity-checks these against the
// configured Options.
type ManifestStream struct {
	Stream   string `yaml:"stream"`
	Storage  string `yaml:"storage"`
	Replicas int    `yaml:"replicas"`
}

// storageTypeName returns the lowercase wire name for a JetStream
// storage type, matching the --kv-storage flag values so the manifest
// can be diffed against flag inputs by string compare.
func storageTypeName(s jetstream.StorageType) string {
	if s == jetstream.MemoryStorage {
		return "memory"
	}
	return "file"
}

// buildManifestOptions snapshots o for emission.
func buildManifestOptions(o Options) ManifestOptions {
	return ManifestOptions{
		NATSURLs:          o.NATSURLs,
		Workers:           o.Workers,
		N:                 o.N,
		Replicas:          o.Replicas,
		TwoPhase:          o.TwoPhase,
		SweepInterval:     o.SweepInterval.String(),
		FetchTimeout:      o.FetchTimeout.String(),
		ConsumerMode:      string(o.ConsumerMode),
		HeartbeatInterval: o.HeartbeatInterval.String(),
		HeartbeatTTL:      o.HeartbeatTTL.String(),
		WorkerIDTTL:       o.WorkerIDTTL.String(),
		ElectionTimeout:   o.ElectionTimeout.String(),
		KVStorage:         storageTypeName(o.KVStorage),
		DataStorage:       storageTypeName(o.DataStorage),
		DataStreamName:    o.DataStreamName,
		Warmup:            o.Warmup.String(),
		CaptureWindow:     o.CaptureWindow.String(),
		RPCDumpInterval:   o.RPCDumpInterval.String(),
		OutputDir:         o.OutputDir,
	}
}

// WriteManifest serialises a Manifest to <outputDir>/manifest.yaml.
// The output dir is created if it does not exist. The write is atomic
// (tmp + fsync + rename) so Phase 3 never sees a half-written manifest;
// callers should write the manifest LAST so any manifest at all implies
// the other artifacts (rpc_counts.csv) are also committed.
func WriteManifest(dir string, m Manifest) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %q: %w", dir, err)
	}
	buf, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	path := filepath.Join(dir, "manifest.yaml")
	if err := writeFileAtomic(path, buf, 0o644); err != nil {
		return fmt.Errorf("write %q: %w", path, err)
	}
	return nil
}

// writeFileAtomic writes data to path via a sibling tmp file, fsyncs,
// then renames into place. Renames within a directory are atomic on
// POSIX, so a reader that sees the final path is guaranteed to see
// complete contents.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return commitAtomic(tmp, path)
}

// commitAtomic renames tmp -> final and then best-effort fsyncs the
// parent directory so the rename survives a crash. Caller is
// responsible for flushing/fsyncing/closing the tmp file before
// invoking this helper. On rename failure the tmp file is removed and
// the error is returned. The directory fsync is ignored on platforms
// where opening a directory for writing is not supported.
func commitAtomic(tmp, final string) error {
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if d, derr := os.Open(filepath.Dir(final)); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// DecideRunStatus inspects the workers' current state and degraded
// transition counters to decide whether the run should be marked
// degraded. It is a pure function over the worker handles so it can be
// unit-tested without spinning a full embedded NATS.
//
// A run is degraded if any worker has logged at least one degraded
// transition OR is currently in a non-Stable state. The returned error
// describes the offending worker(s) and is suitable for inclusion in
// the manifest log.
func DecideRunStatus(workers []*WorkerHandle) (string, error) {
	var offenders []string
	for _, w := range workers {
		if n := w.degraded.Load(); n > 0 {
			offenders = append(offenders, fmt.Sprintf("w%d=degraded(transitions=%d)", w.idx, n))
			continue
		}
		if w.manager != nil {
			if s := w.manager.State(); s != parti.StateStable {
				offenders = append(offenders, fmt.Sprintf("w%d=%s", w.idx, s.String()))
			}
		}
	}
	if len(offenders) > 0 {
		return "degraded", fmt.Errorf("degraded transitions observed: %v", offenders)
	}

	return "ok", nil
}

// confirmedStreams builds a ManifestStream slice from the post-verify
// state (the values the harness was about to assert, since Verify
// passed). Phase 3 cross-checks these against ManifestOptions.
func confirmedStreams(o Options, cfg parti.Config) []ManifestStream {
	expected := ExpectedStreams(o, cfg)
	out := make([]ManifestStream, 0, len(expected))
	for _, s := range expected {
		out = append(out, ManifestStream{
			Stream: s.Stream, Storage: storageTypeName(s.Storage), Replicas: s.Replicas,
		})
	}
	return out
}
