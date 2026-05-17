// Package instrumentedjs wraps a [jetstream.JetStream] handle and the
// [jetstream.KeyValue] handles it produces so every call parti makes can
// be counted by (bucket, op). The counters feed the per-subsystem RPC
// attribution columns described in
// docs/plans/iops-investigation/00-attribution-plan.md (§R3).
//
// The wrapper embeds the upstream [jetstream.JetStream] so any method
// not explicitly overridden passes through unchanged; overrides exist
// only for the methods parti's v2.3.0 surface actually calls. Counters
// are keyed by (bucket_name, op_name); bucket name is captured when a
// KeyValue handle is produced. JetStream-level ops (publish / consumer
// create / stream / account-info) are filed under a synthetic
// "__js__" bucket so they don't get lost.
//
// Concurrency: every counter is an atomic; the bucket -> counters map
// is guarded by a sync.RWMutex. All methods are safe for concurrent
// use, matching the call patterns inside parti (election, heartbeat,
// assignment and handoff each run independent goroutines that share
// the same JetStream handle).
package instrumentedjs

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// JetStreamBucket is the synthetic bucket name used for counters
// attributed to the top-level [jetstream.JetStream] handle (publish,
// consumer creation, stream lookups, account info). Bucket-scoped
// counters use the real KV bucket name.
const JetStreamBucket = "__js__"

// CounterKey identifies a single counter cell.
type CounterKey struct {
	Bucket string
	Op     string
}

// Snapshot is a typed copy of the counter table returned by
// [InstrumentedJS.Snapshot]. The map is independent from the live
// counter state and may be modified freely by the caller.
type Snapshot map[CounterKey]int64

// counters is the concurrent counter table shared by an
// [InstrumentedJS] and every [instrumentedKV] it produces.
type counters struct {
	mu   sync.RWMutex
	data map[CounterKey]*atomic.Int64
}

func newCounters() *counters {
	return &counters{data: make(map[CounterKey]*atomic.Int64)}
}

func (c *counters) inc(bucket, op string) {
	key := CounterKey{Bucket: bucket, Op: op}
	c.mu.RLock()
	ctr, ok := c.data[key]
	c.mu.RUnlock()
	if ok {
		ctr.Add(1)
		return
	}
	c.mu.Lock()
	if ctr, ok = c.data[key]; !ok {
		ctr = new(atomic.Int64)
		c.data[key] = ctr
	}
	c.mu.Unlock()
	ctr.Add(1)
}

func (c *counters) snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(Snapshot, len(c.data))
	for k, v := range c.data {
		out[k] = v.Load()
	}
	return out
}

func (c *counters) reset() {
	c.mu.Lock()
	c.data = make(map[CounterKey]*atomic.Int64)
	c.mu.Unlock()
}

// InstrumentedJS wraps a [jetstream.JetStream] so every JetStream-
// and KeyValue-level call parti issues is counted by (bucket, op).
// Methods not explicitly overridden fall through to the embedded
// interface unchanged.
//
// InstrumentedJS can also mutate the [jetstream.ConsumerConfig] of
// in-flight CreateOrUpdateConsumer calls. This is used by the IOPS
// investigation's Plan 02 M2.x cells to apply server-side tuning
// (MemoryStorage / Replicas overrides) without touching parti's
// public consumer API. See [InstrumentedJS.SetConsumerOverrides].
type InstrumentedJS struct {
	jetstream.JetStream
	counters *counters

	// consumerMemoryStorage, when true, sets cfg.MemoryStorage=true on
	// every in-flight CreateOrUpdateConsumer call before forwarding.
	// Zero value (false) preserves parti's current behavior — the
	// override is opt-in.
	consumerMemoryStorage bool

	// consumerReplicas, when > 0, sets cfg.Replicas=consumerReplicas on
	// every in-flight CreateOrUpdateConsumer call before forwarding.
	// Zero is JetStream's wire-level "inherit stream replicas" sentinel
	// and is preserved by leaving cfg.Replicas untouched.
	consumerReplicas int
}

// New returns an [InstrumentedJS] that wraps js. All KeyValue handles
// obtained via the wrapper's KeyValue / CreateKeyValue /
// CreateOrUpdateKeyValue / UpdateKeyValue methods will share its
// counter table.
//
// The returned wrapper has no consumer-config overrides; call
// [InstrumentedJS.SetConsumerOverrides] to opt in.
func New(js jetstream.JetStream) *InstrumentedJS {
	return &InstrumentedJS{JetStream: js, counters: newCounters()}
}

// SetConsumerOverrides configures mutations that will be applied to
// the [jetstream.ConsumerConfig] of every subsequent
// CreateOrUpdateConsumer call before it is forwarded to the embedded
// JetStream client.
//
// Parameters:
//   - memoryStorage: when true, force cfg.MemoryStorage = true. When
//     false, parti's value is preserved (default behavior).
//   - replicas: when > 0, force cfg.Replicas = replicas. When 0,
//     parti's value is preserved (which is itself typically 0, the
//     wire-level "inherit stream replicas" sentinel).
//
// Used by the harness to apply Plan 02 M2.A / M2.B server-side
// tuning (see docs/plans/iops-investigation/02-nats-tuning-plan.md).
// Safe to call before any consumer is created; not safe to call
// concurrently with CreateOrUpdateConsumer.
func (i *InstrumentedJS) SetConsumerOverrides(memoryStorage bool, replicas int) {
	i.consumerMemoryStorage = memoryStorage
	i.consumerReplicas = replicas
}

// Snapshot returns a typed copy of every (bucket, op) counter. The
// returned map is not aliased to internal state.
func (i *InstrumentedJS) Snapshot() Snapshot {
	return i.counters.snapshot()
}

// Reset clears all counters. Useful for separating warmup traffic
// from the capture window.
func (i *InstrumentedJS) Reset() {
	i.counters.reset()
}

// --- jetstream.JetStream overrides ---------------------------------

// KeyValue overrides [jetstream.JetStream.KeyValue]. The returned
// [jetstream.KeyValue] is wrapped so its per-method calls flow into
// the shared counter table under the bucket name.
func (i *InstrumentedJS) KeyValue(ctx context.Context, bucket string) (jetstream.KeyValue, error) { //nolint:ireturn // matches JetStream interface
	i.counters.inc(JetStreamBucket, "KeyValue")
	kv, err := i.JetStream.KeyValue(ctx, bucket)
	if err != nil {
		return nil, err
	}
	return &instrumentedKV{KeyValue: kv, bucket: bucket, counters: i.counters}, nil
}

// CreateKeyValue overrides [jetstream.JetStream.CreateKeyValue].
func (i *InstrumentedJS) CreateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) { //nolint:ireturn // matches JetStream interface
	i.counters.inc(JetStreamBucket, "CreateKeyValue")
	kv, err := i.JetStream.CreateKeyValue(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &instrumentedKV{KeyValue: kv, bucket: cfg.Bucket, counters: i.counters}, nil
}

// UpdateKeyValue overrides [jetstream.JetStream.UpdateKeyValue].
func (i *InstrumentedJS) UpdateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) { //nolint:ireturn // matches JetStream interface
	i.counters.inc(JetStreamBucket, "UpdateKeyValue")
	kv, err := i.JetStream.UpdateKeyValue(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &instrumentedKV{KeyValue: kv, bucket: cfg.Bucket, counters: i.counters}, nil
}

// CreateOrUpdateKeyValue overrides
// [jetstream.JetStream.CreateOrUpdateKeyValue].
func (i *InstrumentedJS) CreateOrUpdateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) { //nolint:ireturn // matches JetStream interface
	i.counters.inc(JetStreamBucket, "CreateOrUpdateKeyValue")
	kv, err := i.JetStream.CreateOrUpdateKeyValue(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &instrumentedKV{KeyValue: kv, bucket: cfg.Bucket, counters: i.counters}, nil
}

// AccountInfo overrides [jetstream.JetStream.AccountInfo] (parti uses
// it during testutil bring-up; counting it is cheap and lets us spot
// unexpected polling).
func (i *InstrumentedJS) AccountInfo(ctx context.Context) (*jetstream.AccountInfo, error) {
	i.counters.inc(JetStreamBucket, "AccountInfo")
	return i.JetStream.AccountInfo(ctx)
}

// Publish overrides [jetstream.JetStream.Publish] (used by
// partition/js_publisher.go).
func (i *InstrumentedJS) Publish(ctx context.Context, subj string, data []byte, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	i.counters.inc(JetStreamBucket, "Publish")
	return i.JetStream.Publish(ctx, subj, data, opts...)
}

// PublishMsg overrides [jetstream.JetStream.PublishMsg].
func (i *InstrumentedJS) PublishMsg(ctx context.Context, msg *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	i.counters.inc(JetStreamBucket, "PublishMsg")
	return i.JetStream.PublishMsg(ctx, msg, opts...)
}

// PublishAsync overrides [jetstream.JetStream.PublishAsync].
func (i *InstrumentedJS) PublishAsync(subj string, data []byte, opts ...jetstream.PublishOpt) (jetstream.PubAckFuture, error) { //nolint:ireturn // matches JetStream interface
	i.counters.inc(JetStreamBucket, "PublishAsync")
	return i.JetStream.PublishAsync(subj, data, opts...)
}

// PublishMsgAsync overrides [jetstream.JetStream.PublishMsgAsync] for
// completeness; parti v2.3.0 does not call it but the consumer/* code
// may grow into it.
func (i *InstrumentedJS) PublishMsgAsync(msg *nats.Msg, opts ...jetstream.PublishOpt) (jetstream.PubAckFuture, error) { //nolint:ireturn // matches JetStream interface
	i.counters.inc(JetStreamBucket, "PublishMsgAsync")
	return i.JetStream.PublishMsgAsync(msg, opts...)
}

// CreateOrUpdateConsumer overrides
// [jetstream.JetStream.CreateOrUpdateConsumer] (used by
// internal/durable/partition_consumer.go and jsutil.EnsureConsumer).
//
// If consumer overrides have been configured via
// [InstrumentedJS.SetConsumerOverrides], the in-flight cfg is
// mutated before being forwarded. The override is opt-in: with
// both fields at their zero values the call is unchanged.
func (i *InstrumentedJS) CreateOrUpdateConsumer(ctx context.Context, stream string, cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) { //nolint:ireturn // matches JetStream interface
	i.counters.inc(JetStreamBucket, "CreateOrUpdateConsumer")
	if i.consumerMemoryStorage {
		cfg.MemoryStorage = true
	}
	if i.consumerReplicas > 0 {
		cfg.Replicas = i.consumerReplicas
	}
	return i.JetStream.CreateOrUpdateConsumer(ctx, stream, cfg)
}

// Stream overrides [jetstream.JetStream.Stream] (used by
// consumer/common.go to read stream info).
func (i *InstrumentedJS) Stream(ctx context.Context, name string) (jetstream.Stream, error) { //nolint:ireturn // matches JetStream interface
	i.counters.inc(JetStreamBucket, "Stream")
	return i.JetStream.Stream(ctx, name)
}

// CreateStream overrides [jetstream.JetStream.CreateStream] (used by
// jsutil.EnsureStreamWithRetry in jsutil/stream.go when a stream is
// missing). Counting it closes an attribution hole that was visible
// whenever a Parti path creates a stream rather than just looking it up.
func (i *InstrumentedJS) CreateStream(ctx context.Context, cfg jetstream.StreamConfig) (jetstream.Stream, error) { //nolint:ireturn // matches JetStream interface
	i.counters.inc(JetStreamBucket, "CreateStream")
	return i.JetStream.CreateStream(ctx, cfg)
}

// --- jetstream.KeyValue wrapper ------------------------------------

// instrumentedKV wraps a [jetstream.KeyValue] handle and increments a
// counter for every method parti calls. Bucket name is captured at
// wrap time so attribution flows by bucket, not by call site.
type instrumentedKV struct {
	jetstream.KeyValue
	bucket   string
	counters *counters
}

// Bucket returns the KV store name. We intentionally do not count
// this: parti calls it as a cheap accessor for logging and it is not
// a server RPC.
func (k *instrumentedKV) Bucket() string {
	return k.KeyValue.Bucket()
}

func (k *instrumentedKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) { //nolint:ireturn // matches KeyValue interface
	k.counters.inc(k.bucket, "Get")
	return k.KeyValue.Get(ctx, key)
}

func (k *instrumentedKV) GetRevision(ctx context.Context, key string, revision uint64) (jetstream.KeyValueEntry, error) { //nolint:ireturn // matches KeyValue interface
	k.counters.inc(k.bucket, "GetRevision")
	return k.KeyValue.GetRevision(ctx, key, revision)
}

func (k *instrumentedKV) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	k.counters.inc(k.bucket, "Put")
	return k.KeyValue.Put(ctx, key, value)
}

func (k *instrumentedKV) PutString(ctx context.Context, key string, value string) (uint64, error) {
	k.counters.inc(k.bucket, "PutString")
	return k.KeyValue.PutString(ctx, key, value)
}

func (k *instrumentedKV) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	k.counters.inc(k.bucket, "Create")
	return k.KeyValue.Create(ctx, key, value, opts...)
}

func (k *instrumentedKV) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	k.counters.inc(k.bucket, "Update")
	return k.KeyValue.Update(ctx, key, value, revision)
}

func (k *instrumentedKV) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	k.counters.inc(k.bucket, "Delete")
	return k.KeyValue.Delete(ctx, key, opts...)
}

func (k *instrumentedKV) Purge(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	k.counters.inc(k.bucket, "Purge")
	return k.KeyValue.Purge(ctx, key, opts...)
}

func (k *instrumentedKV) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) { //nolint:ireturn // matches KeyValue interface
	k.counters.inc(k.bucket, "Watch")
	return k.KeyValue.Watch(ctx, keys, opts...)
}

func (k *instrumentedKV) WatchAll(ctx context.Context, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) { //nolint:ireturn // matches KeyValue interface
	k.counters.inc(k.bucket, "WatchAll")
	return k.KeyValue.WatchAll(ctx, opts...)
}

func (k *instrumentedKV) WatchFiltered(ctx context.Context, keys []string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) { //nolint:ireturn // matches KeyValue interface
	k.counters.inc(k.bucket, "WatchFiltered")
	return k.KeyValue.WatchFiltered(ctx, keys, opts...)
}

func (k *instrumentedKV) Keys(ctx context.Context, opts ...jetstream.WatchOpt) ([]string, error) {
	k.counters.inc(k.bucket, "Keys")
	return k.KeyValue.Keys(ctx, opts...)
}

func (k *instrumentedKV) ListKeys(ctx context.Context, opts ...jetstream.WatchOpt) (jetstream.KeyLister, error) { //nolint:ireturn // matches KeyValue interface
	k.counters.inc(k.bucket, "ListKeys")
	return k.KeyValue.ListKeys(ctx, opts...)
}

func (k *instrumentedKV) ListKeysFiltered(ctx context.Context, filters ...string) (jetstream.KeyLister, error) { //nolint:ireturn // matches KeyValue interface
	k.counters.inc(k.bucket, "ListKeysFiltered")
	return k.KeyValue.ListKeysFiltered(ctx, filters...)
}

func (k *instrumentedKV) History(ctx context.Context, key string, opts ...jetstream.WatchOpt) ([]jetstream.KeyValueEntry, error) {
	k.counters.inc(k.bucket, "History")
	return k.KeyValue.History(ctx, key, opts...)
}

func (k *instrumentedKV) PurgeDeletes(ctx context.Context, opts ...jetstream.KVPurgeOpt) error {
	k.counters.inc(k.bucket, "PurgeDeletes")
	return k.KeyValue.PurgeDeletes(ctx, opts...)
}

func (k *instrumentedKV) Status(ctx context.Context) (jetstream.KeyValueStatus, error) { //nolint:ireturn // matches KeyValue interface
	k.counters.inc(k.bucket, "Status")
	return k.KeyValue.Status(ctx)
}
