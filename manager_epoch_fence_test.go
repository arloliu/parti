package parti

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/kvutil"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// epochSpy captures the OnDegraded callback so the F1 reproducers can
// observe (a) whether degraded mode was entered and (b) the reason
// string. degraded-mode entry is what trips the readiness probe in
// production.
type epochSpy struct {
	mu      sync.Mutex
	reasons []string
}

func (s *epochSpy) record(_ context.Context, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reasons = append(s.reasons, reason)
	return nil
}

func (s *epochSpy) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.reasons))
	copy(out, s.reasons)
	return out
}

// makeBucket creates a freshly-named bucket and returns its handle.
// Bucket names are caller-provided so tests can probe a specific
// expected name (e.g. the heartbeat bucket the Manager would pick).
func makeBucket(t *testing.T, js jetstream.JetStream, name string) jetstream.KeyValue {
	t.Helper()
	kv, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{
		Bucket:  name,
		Storage: jetstream.MemoryStorage,
	})
	require.NoError(t, err)
	return kv
}

// TestManager_F1_BucketRecreate_TripsDegraded is the primary F1
// reproducer. For each Parti-owned bucket name shape the Manager's
// captureBucketEpoch flow would record, delete and recreate the
// backing stream and assert checkBucketEpochs trips degraded mode
// with the expected reason. The test drives checkBucketEpochs
// directly so the assertion is deterministic (no polling on the
// production OperationTimeout tick).
func TestManager_F1_BucketRecreate_TripsDegraded(t *testing.T) {
	t.Parallel()
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	cases := []string{
		"parti-epoch-test-stableid",
		"parti-epoch-test-election",
		"parti-epoch-test-heartbeat",
		"parti-epoch-test-assignment",
		"parti-epoch-test-handoff",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bucket := name + "-" + t.Name()[len("TestManager_F1_BucketRecreate_TripsDegraded/"):]
			// Bucket names are not allowed to contain '/'; strip just in case.
			bucket = strings.ReplaceAll(bucket, "/", "-")

			kv := makeBucket(t, js, bucket)

			spy := &epochSpy{}
			m := &Manager{
				cfg: Config{
					OperationTimeout: 500 * time.Millisecond,
					DegradedAlert:    DegradedAlertConfig{AlertInterval: time.Second},
				},
				logger: logging.NewNop(),
				hooks:  &Hooks{OnDegraded: spy.record},
			}
			m.state.Store(int32(StateStable))
			m.degradedSince.Store(0)
			m.metrics = nopManagerMetrics{}
			testCtx, cancel := context.WithCancel(ctx)
			m.ctx = testCtx
			m.cancel = cancel
			t.Cleanup(cancel)

			m.captureBucketEpoch(ctx, bucket, kv)
			require.Contains(t, m.bucketEpochs, bucket,
				"sanity: captureBucketEpoch must have recorded the bucket")

			// Wipe and recreate so the backing stream gets a fresh Created.
			require.NoError(t, js.DeleteKeyValue(ctx, bucket))
			// time.Now() advances; recreate gets a strictly later Created.
			time.Sleep(50 * time.Millisecond)
			kv2 := makeBucket(t, js, bucket)
			liveCreated, err := kvutil.BucketStreamCreated(ctx, kv2)
			require.NoError(t, err)
			require.True(t, liveCreated.After(m.bucketEpochs[bucket].created),
				"sanity: recreate must produce a later Created timestamp")

			// The cached kv handle points at the OLD stream and would
			// return an error on a status read; replace it with the new
			// handle to model the production path where the Manager's
			// cached handle silently re-binds after the recreate.
			ep := m.bucketEpochs[bucket]
			ep.kv = kv2
			m.bucketEpochs[bucket] = ep

			m.checkBucketEpochs(ctx)
			// OnDegraded fires asynchronously via invokeHook → wg.Go;
			// poll briefly for the hook to land.
			require.Eventually(t, func() bool {
				return len(spy.snapshot()) >= 1
			}, time.Second, 10*time.Millisecond,
				"epoch fence must fire OnDegraded after checkBucketEpochs returns")
			reasons := spy.snapshot()
			require.Len(t, reasons, 1,
				"epoch fence must fire OnDegraded exactly once")
			require.Equal(t, "bucket-recreated:"+bucket, reasons[0])
		})
	}
}

// TestManager_F1_HappyPath_NoDegraded confirms that a bucket whose
// Created timestamp is unchanged (no recreate) does NOT trip the
// fence. False-positive guard — the fence must be silent on
// healthy operation.
func TestManager_F1_HappyPath_NoDegraded(t *testing.T) {
	t.Parallel()
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	bucket := "parti-epoch-happy"
	kv := makeBucket(t, js, bucket)

	spy := &epochSpy{}
	m := &Manager{
		cfg: Config{
			OperationTimeout: 500 * time.Millisecond,
			DegradedAlert:    DegradedAlertConfig{AlertInterval: time.Second},
		},
		logger: logging.NewNop(),
		hooks:  &Hooks{OnDegraded: spy.record},
	}
	m.state.Store(int32(StateStable))
	m.metrics = nopManagerMetrics{}
	ctx, cancel := context.WithCancel(t.Context())
	m.ctx = ctx
	m.cancel = cancel
	t.Cleanup(cancel)

	m.captureBucketEpoch(ctx, bucket, kv)

	for range 3 {
		m.checkBucketEpochs(ctx)
	}
	require.Empty(t, spy.snapshot(),
		"three consecutive ticks against an unchanged bucket must NOT trip degraded")
}

// nopManagerMetrics is the minimum metrics surface monitorBucketEpochs
// needs (enterDegraded calls SetDegradedMode). Implements just enough
// of MetricsCollector to satisfy the field type.
type nopManagerMetrics struct{ types.MetricsCollector }

func (nopManagerMetrics) SetDegradedMode(float64)                     {}
func (nopManagerMetrics) RecordStateTransition(_, _ State, _ float64) {}
