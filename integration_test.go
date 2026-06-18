package parti_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// captureLogger is a types.Logger that records every log line so a test can
// assert on the "pull gating resolve failed" warning the worker consumer emits
// when its claim resolver cannot find a partition.
type captureLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *captureLogger) record(level, msg string, kv ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+msg+" "+fmt.Sprint(kv...))
}

func (l *captureLogger) Debug(msg string, kv ...any) { l.record("DEBUG", msg, kv...) }
func (l *captureLogger) Info(msg string, kv ...any)  { l.record("INFO", msg, kv...) }
func (l *captureLogger) Warn(msg string, kv ...any)  { l.record("WARN", msg, kv...) }
func (l *captureLogger) Error(msg string, kv ...any) { l.record("ERROR", msg, kv...) }
func (l *captureLogger) Fatal(msg string, kv ...any) { l.record("FATAL", msg, kv...) }

func (l *captureLogger) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, ln := range l.lines {
		if strings.Contains(ln, substr) {
			n++
		}
	}

	return n
}

// warnErrLines returns up to limit recent WARN/ERROR/FATAL lines, for diagnostics.
func (l *captureLogger) warnErrLines(limit int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, ln := range l.lines {
		if strings.HasPrefix(ln, "WARN") || strings.HasPrefix(ln, "ERROR") || strings.HasPrefix(ln, "FATAL") {
			out = append(out, ln)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}

	return out
}

// pgWorker bundles a running Manager + Dynamic consumer wired exactly the way
// the production report describes: two-phase handoff on the Manager, pull
// gating + processing gate + an auto claim resolver on the consumer, both
// pointed at the same handoff bucket.
type pgWorker struct {
	mgr       *parti.Manager
	js        jetstream.JetStream
	log       *captureLogger
	received  chan string
	delivered *int32
	parts     []types.Partition
}

// startPullGatingWorker brings up a fresh embedded NATS server, an EVENTS
// stream, an (initially empty) NATS KV partition source, a Manager with
// two-phase handoff, and a Dynamic consumer with pull gating. It then assigns
// the given partitions and waits for the worker to receive them.
func startPullGatingWorker(
	ctx context.Context,
	t *testing.T,
	handoffTTL, reconcileInterval time.Duration,
	parts []types.Partition,
) *pgWorker {
	t.Helper()

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "EVENTS",
		Subjects: []string{"events.>"},
	})
	require.NoError(t, err)

	// Empty NATS KV partition source — partitions are assigned after start.
	srcKV := partitest.CreateJetStreamKV(t, nc, "parti-partitions")
	src := source.NewNatsKV(srcKV, "partitions", nil)

	// Manager config mirrors the user setup: two-phase handoff enabled.
	cfg := testutil.IntegrationTestConfig()
	cfg.EnableTwoPhaseHandoff = true
	cfg.KVBuckets.HandoffBucket = "parti-handoff"
	cfg.KVBuckets.HandoffTTL = handoffTTL

	// Pre-create the handoff KV bucket with a MaxAge TTL, mirroring a
	// provisioned deployment (the user's "create kv bucket and stream" step
	// runs before the workers start). Whoever opens the bucket next — the
	// consumer's claim resolver or the Manager's setupHandoff — inherits this
	// TTL because bucket creation is get-first.
	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: cfg.KVBuckets.HandoffBucket,
		TTL:    handoffTTL,
	})
	require.NoError(t, err)

	var delivered int32
	received := make(chan string, 128)
	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		atomic.AddInt32(&delivered, 1)
		received <- string(msg.Data())
		return msg.Ack()
	})
	logCap := &captureLogger{}

	resolverCfg := consumer.ResolverConfig{HandoffBucketName: cfg.KVBuckets.HandoffBucket}
	if reconcileInterval > 0 {
		resolverCfg.ReconcileInterval = reconcileInterval
	}

	dyn, err := consumer.NewDynamic(
		js, "EVENTS", "pullgate", "events.{{.PartitionID}}", handler,
		consumer.WithLogger(logCap),
		consumer.WithProcessingGate(&consumer.ProcessingGateConfig{Enabled: true}),
		consumer.WithPullGating(true),
		consumer.WithResolver(resolverCfg),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dyn.Stop(context.Background()) })

	mgr, err := parti.NewManager(&cfg, js, src, strategy.NewConsistentHash(),
		parti.WithWorkerConsumerUpdater(dyn))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	require.NoError(t, mgr.Start(ctx))

	require.NoError(t, src.Update(ctx, parts))

	require.Eventually(t, func() bool {
		return len(mgr.CurrentAssignment().Partitions) == len(parts)
	}, 30*time.Second, 100*time.Millisecond,
		"worker never received the partition assignment")

	return &pgWorker{
		mgr: mgr, js: js, log: logCap,
		received: received, delivered: &delivered, parts: parts,
	}
}

// publishAndExpectDelivery publishes one tagged message per owned partition and
// fails the test unless every one is delivered to the handler. The tag keeps
// rounds isolated so a straggler from an earlier round cannot satisfy a later
// one.
func (w *pgWorker) publishAndExpectDelivery(ctx context.Context, t *testing.T, tag string) {
	t.Helper()

	for _, p := range w.parts {
		// The subject template renders {{.PartitionID}} as Partition.SubjectKey()
		// (dot-joined), so the per-partition subject is "events.<SubjectKey()>".
		subject := "events." + p.SubjectKey()
		_, err := w.js.Publish(ctx, subject, []byte(tag+"-"+p.SubjectKey()))
		require.NoError(t, err, "publish %s", subject)
	}

	got := make(map[string]bool, len(w.parts))
	deadline := time.After(20 * time.Second)
	for len(got) < len(w.parts) {
		select {
		case data := <-w.received:
			if strings.HasPrefix(data, tag+"-") {
				got[data] = true
			}
		case <-deadline:
			t.Fatalf("[%s] consumer delivered only %d/%d messages; "+
				"\"pull gating resolve failed: partition not found\" warnings=%d\n"+
				"recent WARN/ERROR log lines:\n  %s",
				tag, len(got), len(w.parts), w.log.count("pull gating resolve failed"),
				strings.Join(w.log.warnErrLines(15), "\n  "))
		}
	}
}

// TestPullGating_BasicAssignment_DeliversMessages is the happy-path guard: on a
// fresh cluster with two-phase handoff, pull gating, and a claim resolver, a
// worker that owns its partitions delivers messages. This must keep passing.
func TestPullGating_BasicAssignment_DeliversMessages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// HandoffTTL well beyond the test duration: claims do not age out here.
	w := startPullGatingWorker(ctx, t, 5*time.Minute, 0, []types.Partition{
		{Keys: []string{"p1"}},
		{Keys: []string{"p2"}},
		{Keys: []string{"p3"}},
	})

	claims, err := w.mgr.InspectHandoffClaims(ctx)
	require.NoError(t, err)
	require.Len(t, claims, len(w.parts),
		"two-phase handoff must write one stable claim per partition")

	// Let the claim resolver's watcher observe the claims.
	time.Sleep(2 * time.Second)

	w.publishAndExpectDelivery(ctx, t, "basic")
}

// TestPullGating_StableClaimExpiry_DoesNotSuppressConsumer reproduces the
// production bug and verifies the fix.
//
// Root cause: the handoff KV bucket was created with MaxAge ==
// Config.KVBuckets.HandoffTTL. Two-phase handoff writes one *stable* claim per
// partition at initial assignment and never rewrites it (a stable cluster runs
// no further Apply cycles, and the claim sweep skips stable claims). Once the
// bucket's MaxAge elapsed, every stable claim's KV key aged out; the consumer's
// claim resolver reconcile loop then listed an empty bucket and tombstoned every
// vanished claim, so GetOwner returned "not found" forever and pull gating
// permanently suppressed every pull.
//
// The fix: the handoff bucket is created with no MaxAge, and a bucket
// pre-created with one (e.g. by an older parti version) has its MaxAge cleared
// by the Manager at startup. `startPullGatingWorker` pre-creates the handoff
// bucket with a short MaxAge to mirror that older-version state.
//
// This test waits well past the bucket's original MaxAge, then asserts the
// stable claims are still present and messages are still delivered. It FAILS on
// the buggy revision (claims age out, consumer suppressed) and PASSES once the
// bug is fixed (the Manager cleared the MaxAge, claims persist).
func TestPullGating_StableClaimExpiry_DoesNotSuppressConsumer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// The handoff bucket is pre-created with this MaxAge (see
	// startPullGatingWorker). The fix must clear it so stable claims persist.
	const handoffMaxAge = 10 * time.Second

	// Short reconcile interval so the resolver would observe an expiry quickly;
	// production uses the 30s default, which only makes the failure slower.
	w := startPullGatingWorker(ctx, t, handoffMaxAge, 1*time.Second, []types.Partition{
		{Keys: []string{"p1"}},
		{Keys: []string{"p2"}},
		{Keys: []string{"p3"}},
	})

	// Claims exist immediately after assignment and the consumer works.
	claims, err := w.mgr.InspectHandoffClaims(ctx)
	require.NoError(t, err)
	require.Len(t, claims, len(w.parts),
		"two-phase handoff must write one stable claim per partition")

	time.Sleep(2 * time.Second)
	w.publishAndExpectDelivery(ctx, t, "before-window")

	// Wait well past the handoff bucket's original MaxAge. On the buggy revision
	// the stable claims age out here; with the fix the Manager cleared the
	// MaxAge at startup so they persist.
	time.Sleep(handoffMaxAge + 6*time.Second)

	claimsAfter, err := w.mgr.InspectHandoffClaims(ctx)
	require.NoError(t, err)
	require.Len(t, claimsAfter, len(w.parts),
		"stable claims must survive past the handoff bucket's original MaxAge")

	// The worker still owns every partition — it MUST still deliver messages.
	w.publishAndExpectDelivery(ctx, t, "after-window")
}

// TestPullGating_MultiKeyPartition_DeliversMessages verifies pull gating works
// for partitions with more than one key.
//
// Partition.ID() joins keys with "-" while Partition.SubjectKey() joins with
// ".". The two-phase handoff coordinator must key its ownership claims by the
// same identity the consumer's pull gating (and processing gate) derive from
// the partition subject — SubjectKey(). If the claim is keyed by ID() instead,
// GetOwner returns not-found for every multi-key partition and pull gating
// permanently suppresses delivery — the same "pull gating resolve failed:
// partition not found" symptom. Single-key partitions are unaffected because
// ID() == SubjectKey() when there is only one key.
//
// This test FAILS on the buggy revision and PASSES once the claim key is
// aligned with SubjectKey().
func TestPullGating_MultiKeyPartition_DeliversMessages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	w := startPullGatingWorker(ctx, t, 5*time.Minute, 0, []types.Partition{
		{Keys: []string{"region", "us-east"}},
		{Keys: []string{"region", "us-west"}},
		{Keys: []string{"tenant", "acme"}},
	})

	claims, err := w.mgr.InspectHandoffClaims(ctx)
	require.NoError(t, err)
	require.Len(t, claims, len(w.parts),
		"two-phase handoff must write one stable claim per multi-key partition")

	// Let the claim resolver's watcher observe the claims.
	time.Sleep(2 * time.Second)

	w.publishAndExpectDelivery(ctx, t, "multikey")
}
