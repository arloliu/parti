package main

import (
	"context"
	"log"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti/v2/test/simulation/internal/config"
	"github.com/arloliu/parti/v2/test/simulation/internal/coordinator"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	defaultHandoffBucket = "parti-handoff"
	handoffClaimPrefix   = "claims/"
)

type simKVFaultController struct {
	mu sync.RWMutex

	kvUnavailableBuckets map[string]struct{}
	handoffClaimWrite    bool
	kvUnavailableToken   uint64
	handoffClaimToken    uint64

	kvUnavailableInjected     atomic.Int64
	handoffClaimWriteInjected atomic.Int64
}

func newSimKVFaultController() *simKVFaultController {
	return &simKVFaultController{
		kvUnavailableBuckets: make(map[string]struct{}),
	}
}

func (fc *simKVFaultController) armKVUnavailable(buckets []string) uint64 {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	fc.kvUnavailableToken++
	fc.kvUnavailableBuckets = make(map[string]struct{}, len(buckets))
	for _, bucket := range buckets {
		if bucket == "" {
			continue
		}
		fc.kvUnavailableBuckets[bucket] = struct{}{}
	}

	return fc.kvUnavailableToken
}

func (fc *simKVFaultController) armHandoffClaimWrite() uint64 {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	fc.handoffClaimToken++
	fc.handoffClaimWrite = true

	return fc.handoffClaimToken
}

func (fc *simKVFaultController) disarm() {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	fc.kvUnavailableToken++
	fc.handoffClaimToken++
	clear(fc.kvUnavailableBuckets)
	fc.handoffClaimWrite = false
}

func (fc *simKVFaultController) disarmKVUnavailable(token uint64) bool {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	if token != fc.kvUnavailableToken {
		return false
	}
	clear(fc.kvUnavailableBuckets)

	return true
}

func (fc *simKVFaultController) disarmHandoffClaimWrite(token uint64) bool {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	if token != fc.handoffClaimToken {
		return false
	}
	fc.handoffClaimWrite = false

	return true
}

func (fc *simKVFaultController) isKVUnavailableArmed(bucket string) bool {
	fc.mu.RLock()
	defer fc.mu.RUnlock()

	_, ok := fc.kvUnavailableBuckets[bucket]

	return ok
}

func (fc *simKVFaultController) isHandoffClaimWriteArmed() bool {
	fc.mu.RLock()
	defer fc.mu.RUnlock()

	return fc.handoffClaimWrite
}

func (fc *simKVFaultController) shouldFaultKVUnavailable(bucket string) bool {
	fc.mu.RLock()
	defer fc.mu.RUnlock()

	_, ok := fc.kvUnavailableBuckets[bucket]
	if ok {
		fc.kvUnavailableInjected.Add(1)
	}

	return ok
}

func (fc *simKVFaultController) shouldFaultHandoffClaimWrite(bucket, key string) bool {
	fc.mu.RLock()
	defer fc.mu.RUnlock()

	ok := fc.handoffClaimWrite && bucket == defaultHandoffBucket && strings.HasPrefix(key, handoffClaimPrefix)
	if ok {
		fc.handoffClaimWriteInjected.Add(1)
	}

	return ok
}

type simKVFaultJetStream struct {
	jetstream.JetStream

	fc *simKVFaultController
}

func newSimKVFaultJetStream(inner jetstream.JetStream, fc *simKVFaultController) jetstream.JetStream {
	if fc == nil {
		return inner
	}

	return &simKVFaultJetStream{JetStream: inner, fc: fc}
}

func (js *simKVFaultJetStream) KeyValue(ctx context.Context, bucket string) (jetstream.KeyValue, error) {
	kv, err := js.JetStream.KeyValue(ctx, bucket)
	if err != nil {
		return kv, err
	}

	return js.wrapKV(kv, bucket), nil
}

func (js *simKVFaultJetStream) CreateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) {
	kv, err := js.JetStream.CreateKeyValue(ctx, cfg)
	if err != nil {
		return kv, err
	}

	return js.wrapKV(kv, cfg.Bucket), nil
}

func (js *simKVFaultJetStream) CreateOrUpdateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) {
	kv, err := js.JetStream.CreateOrUpdateKeyValue(ctx, cfg)
	if err != nil {
		return kv, err
	}

	return js.wrapKV(kv, cfg.Bucket), nil
}

func (js *simKVFaultJetStream) wrapKV(kv jetstream.KeyValue, bucket string) jetstream.KeyValue {
	return &simKVFaultKeyValue{
		KeyValue: kv,
		bucket:   bucket,
		fc:       js.fc,
	}
}

type simKVFaultKeyValue struct {
	jetstream.KeyValue

	bucket string
	fc     *simKVFaultController
}

func (kv *simKVFaultKeyValue) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if kv.fc.shouldFaultKVUnavailable(kv.bucket) {
		return nil, context.DeadlineExceeded
	}

	return kv.KeyValue.Get(ctx, key)
}

func (kv *simKVFaultKeyValue) GetRevision(ctx context.Context, key string, revision uint64) (jetstream.KeyValueEntry, error) {
	if kv.fc.shouldFaultKVUnavailable(kv.bucket) {
		return nil, context.DeadlineExceeded
	}

	return kv.KeyValue.GetRevision(ctx, key, revision)
}

func (kv *simKVFaultKeyValue) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	if kv.shouldFaultWrite(key) {
		return 0, context.DeadlineExceeded
	}

	return kv.KeyValue.Put(ctx, key, value)
}

func (kv *simKVFaultKeyValue) PutString(ctx context.Context, key string, value string) (uint64, error) {
	if kv.shouldFaultWrite(key) {
		return 0, context.DeadlineExceeded
	}

	return kv.KeyValue.PutString(ctx, key, value)
}

func (kv *simKVFaultKeyValue) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	if kv.shouldFaultWrite(key) {
		return 0, context.DeadlineExceeded
	}

	return kv.KeyValue.Create(ctx, key, value, opts...)
}

func (kv *simKVFaultKeyValue) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if kv.shouldFaultWrite(key) {
		return 0, context.DeadlineExceeded
	}

	return kv.KeyValue.Update(ctx, key, value, revision)
}

func (kv *simKVFaultKeyValue) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	if kv.fc.shouldFaultKVUnavailable(kv.bucket) {
		return context.DeadlineExceeded
	}

	return kv.KeyValue.Delete(ctx, key, opts...)
}

func (kv *simKVFaultKeyValue) Purge(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	if kv.fc.shouldFaultKVUnavailable(kv.bucket) {
		return context.DeadlineExceeded
	}

	return kv.KeyValue.Purge(ctx, key, opts...)
}

func (kv *simKVFaultKeyValue) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	if kv.fc.shouldFaultKVUnavailable(kv.bucket) {
		return nil, context.DeadlineExceeded
	}

	return kv.KeyValue.Watch(ctx, keys, opts...)
}

func (kv *simKVFaultKeyValue) WatchAll(ctx context.Context, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	if kv.fc.shouldFaultKVUnavailable(kv.bucket) {
		return nil, context.DeadlineExceeded
	}

	return kv.KeyValue.WatchAll(ctx, opts...)
}

func (kv *simKVFaultKeyValue) WatchFiltered(ctx context.Context, keys []string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	if kv.fc.shouldFaultKVUnavailable(kv.bucket) {
		return nil, context.DeadlineExceeded
	}

	return kv.KeyValue.WatchFiltered(ctx, keys, opts...)
}

func (kv *simKVFaultKeyValue) Keys(ctx context.Context, opts ...jetstream.WatchOpt) ([]string, error) {
	if kv.fc.shouldFaultKVUnavailable(kv.bucket) {
		return nil, context.DeadlineExceeded
	}

	return kv.KeyValue.Keys(ctx, opts...)
}

func (kv *simKVFaultKeyValue) History(ctx context.Context, key string, opts ...jetstream.WatchOpt) ([]jetstream.KeyValueEntry, error) {
	if kv.fc.shouldFaultKVUnavailable(kv.bucket) {
		return nil, context.DeadlineExceeded
	}

	return kv.KeyValue.History(ctx, key, opts...)
}

func (kv *simKVFaultKeyValue) Status(ctx context.Context) (jetstream.KeyValueStatus, error) {
	if kv.fc.shouldFaultKVUnavailable(kv.bucket) {
		return nil, context.DeadlineExceeded
	}

	return kv.KeyValue.Status(ctx)
}

func (kv *simKVFaultKeyValue) shouldFaultWrite(key string) bool {
	return kv.fc.shouldFaultKVUnavailable(kv.bucket) ||
		kv.fc.shouldFaultHandoffClaimWrite(kv.bucket, key)
}

func bucketListFromParam(v any, defaults []string) []string {
	switch buckets := v.(type) {
	case []string:
		return nonEmptyBuckets(buckets, defaults)
	case []any:
		out := make([]string, 0, len(buckets))
		for _, bucket := range buckets {
			if s, ok := bucket.(string); ok {
				out = append(out, s)
			}
		}
		return nonEmptyBuckets(out, defaults)
	case string:
		return nonEmptyBuckets(strings.Split(buckets, ","), defaults)
	default:
		return slices.Clone(defaults)
	}
}

func nonEmptyBuckets(buckets, defaults []string) []string {
	out := make([]string, 0, len(buckets))
	for _, bucket := range buckets {
		bucket = strings.TrimSpace(bucket)
		if bucket != "" {
			out = append(out, bucket)
		}
	}
	if len(out) == 0 {
		return slices.Clone(defaults)
	}

	return out
}

func handleKVUnavailableFault(
	ctx context.Context,
	fc *simKVFaultController,
	coord *coordinator.Coordinator,
	params map[string]any,
) {
	if fc == nil {
		log.Print("[Chaos] kv_unavailable requested but fault controller is not installed")
		return
	}

	defaultBuckets := []string{"parti-election", "parti-heartbeat", "parti-stableid"}
	buckets := bucketListFromParam(params["buckets"], defaultBuckets)
	duration := durationFromParams(params, "duration", 20*time.Second)
	expectDegraded := boolFromParams(params, "expect_degraded", true)

	if expectDegraded && coord != nil {
		if o := coord.DegradedReasonOracle(); o != nil {
			o.ExpectAfter("kv_unavailable:"+strings.Join(buckets, ","), []string{"kv-unavailable"}, duration+30*time.Second, "", false)
		}
	}

	log.Printf("[Chaos] kv_unavailable: arming buckets=%v duration=%v expect_degraded=%v", buckets, duration, expectDegraded)
	token := fc.armKVUnavailable(buckets)
	time.AfterFunc(duration, func() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if fc.disarmKVUnavailable(token) {
			log.Printf("[Chaos] kv_unavailable: disarmed buckets=%v", buckets)
		}
	})
}

func handleHandoffClaimWriteFault(ctx context.Context, fc *simKVFaultController, params map[string]any) {
	if fc == nil {
		log.Print("[Chaos] handoff_claim_write_fault requested but fault controller is not installed")
		return
	}

	duration := durationFromParams(params, "duration", 20*time.Second)
	log.Printf("[Chaos] handoff_claim_write_fault: arming duration=%v", duration)
	token := fc.armHandoffClaimWrite()
	time.AfterFunc(duration, func() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if fc.disarmHandoffClaimWrite(token) {
			log.Print("[Chaos] handoff_claim_write_fault: disarmed")
		}
	})
}

func handoffClaimWriteFaultInjected() int64 {
	if aioKVFaults == nil {
		return 0
	}

	return aioKVFaults.handoffClaimWriteInjected.Load()
}

func installStartupKVFaults(ctx context.Context, fc *simKVFaultController, cfg *config.Config) {
	if fc == nil || cfg == nil {
		return
	}
	if !cfg.Chaos.Faults.HandoffClaimWriteOnStart {
		return
	}
	duration := cfg.Chaos.Faults.HandoffClaimWriteDuration
	if duration <= 0 {
		duration = 20 * time.Second
	}
	log.Printf("[Chaos] startup handoff_claim_write_fault: arming duration=%v", duration)
	token := fc.armHandoffClaimWrite()
	time.AfterFunc(duration, func() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if fc.disarmHandoffClaimWrite(token) {
			log.Print("[Chaos] startup handoff_claim_write_fault: disarmed")
		}
	})
}

func boolFromParams(params map[string]any, key string, defaultValue bool) bool {
	v, ok := params[key]
	if !ok {
		return defaultValue
	}
	b, ok := v.(bool)
	if !ok {
		return defaultValue
	}

	return b
}
