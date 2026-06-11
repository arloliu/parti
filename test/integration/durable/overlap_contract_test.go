package durable_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/internal/durable"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/types"
)

// TestDynamicHandoff_AckWaitRedelivery_OverlapContract pins, in BOTH directions,
// the irreducible per-partition overlap window that the lifecycle docs describe.
//
// # What the per-tier contract says
//
// docs/LIFECYCLE.md ("Two-Phase Handoff") and docs/CONSUMERS.md ("WIPHandler —
// Long-Running Processing") together describe the strongest gating tier for a
// dynamic per-partition consumer: two-phase handoff orders the RELEASE of a
// partition, the processing gate is a per-message PRE-HANDLER admission check
// (internal/durable.processingGate.Wrap evaluates ownership BEFORE calling the
// underlying handler — "an invocation that has started is not revoked"), pull
// gating suppresses pulls on the non-owner, and DrainOnRemove waits for the old
// owner's pending acks before cancelling its loop.
//
// Even with all four engaged, ONE overlap window remains and cannot be closed
// by ownership coordination alone:
//
//  1. The old owner (w1) PASSES the pre-handler gate for a message and enters
//     its handler. The gate's verdict is taken once, at admission; it cannot
//     retroactively abort an invocation already running.
//  2. Ownership moves to the new stable owner (w2) while w1's handler is still
//     mid-invocation. w1 cannot be interrupted: the pull loop is single
//     threaded and only re-checks ctx after the handler returns, so a remove +
//     drain cannot stop w1 until the handler finishes. removeSubjectLoops waits
//     only up to DrainOnRemoveTimeout, then SURFACES a timeout error to the
//     caller (UpdateWorkerConsumer returns non-nil) while the in-flight handler
//     keeps running — pinned by the remove-error assertion below.
//  3. AckWait expires before w1's handler acks. JetStream redelivers the SAME
//     stream sequence. Because the durable is SHARED per partition — its name
//     is prefix_partition_hash with NO workerID (internal/dynamicbuild.
//     PerSubjectDurableName) — the redelivery is eligible for whichever worker
//     pulls next. With pull gating, only the current owner (now w2) pulls, so
//     w2 receives it. w2's gate correctly PASSES (w2 IS the stable owner) and
//     w2 invokes the handler for that sequence while w1's invocation is still
//     running.
//
// The net effect is at-least-once delivery with a temporally OVERLAPPING pair
// of handler invocations for one sequence: w1 (old, draining) and w2 (new,
// legitimately admitted). This is NOT a gate bug — at w2's admission ownership
// is genuinely w2/stable. It is the documented residual window.
//
// # The documented mitigation
//
// consumer.NewWIPHandler wraps the handler and periodically calls
// msg.InProgress(), extending AckWait so the redelivery in step 3 never fires
// while the handler is still running. The negative subtest proves that with the
// WIPHandler keepalive in place, no second invocation of the poison sequence
// starts before the first ends.
//
// # Why this test exists
//
// If the POSITIVE subtest fails to reproduce the overlap, the docs OVERCLAIM
// the window and that is a major finding (the test fails loudly, it does not
// silently pass). If a future hardening narrows or closes the window, the
// positive subtest will surface that as an intentional contract change.

const (
	overlapPoisonPayload = "POISON"
	// overlapW1Sleep must outlast AckWait + the ownership flip + w2's redelivery
	// so the two invocations genuinely overlap. AckWait is 2s; this gives a wide
	// margin for CI load.
	overlapW1Sleep = 8 * time.Second
	// overlapW2Sleep only needs to be long enough for the collector poll to
	// observe the overlap; it keeps total runtime bounded.
	overlapW2Sleep = 300 * time.Millisecond
)

// invocationRecord captures one handler invocation of a stream sequence.
type invocationRecord struct {
	streamSeq uint64
	workerID  string
	start     time.Time
	end       time.Time
}

// overlapCollector is a mutex-guarded sink for handler invocation records.
type overlapCollector struct {
	mu      sync.Mutex
	records []invocationRecord
}

func (c *overlapCollector) begin(seq uint64, worker string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, invocationRecord{streamSeq: seq, workerID: worker, start: time.Now()})

	return len(c.records) - 1
}

func (c *overlapCollector) finish(idx int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records[idx].end = time.Now()
}

// poisonOverlap scans for a stream sequence processed by two distinct workers
// whose invocations overlap in time (the second starts before the first ends).
// It returns the two records and true when such an overlap exists.
func (c *overlapCollector) poisonOverlap(seq uint64) (first, second invocationRecord, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var forSeq []invocationRecord
	for _, r := range c.records {
		if r.streamSeq == seq {
			forSeq = append(forSeq, r)
		}
	}

	for i := range forSeq {
		for j := range forSeq {
			if i == j || forSeq[i].workerID == forSeq[j].workerID {
				continue
			}
			a, b := forSeq[i], forSeq[j]
			// a started first; b started while a was still running (or a never ended).
			if a.start.Before(b.start) {
				aEnd := a.end
				if aEnd.IsZero() || b.start.Before(aEnd) {
					return a, b, true
				}
			}
		}
	}

	return invocationRecord{}, invocationRecord{}, false
}

// distinctWorkersStarted reports how many distinct workers have started an
// invocation of seq (regardless of overlap). Used by the negative control to
// detect any redelivery to a second worker.
func (c *overlapCollector) distinctWorkersStarted(seq uint64) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	seen := map[string]struct{}{}
	for _, r := range c.records {
		if r.streamSeq == seq {
			seen[r.workerID] = struct{}{}
		}
	}

	return len(seen)
}

// switchableResolver is a thread-safe OwnershipResolver whose owner/state can be
// flipped at runtime to drive ownership deterministically (no KV watch latency).
// It mirrors the switchingResolver pattern in processing_gate_exclusivity_test.go.
// Both the processing gate and pull gating consult the same resolver instance.
type switchableResolver struct {
	owner atomic.Value // string
	state atomic.Value // types.HandoffState
}

func newSwitchableResolver(owner string, st types.HandoffState) *switchableResolver {
	r := &switchableResolver{}
	r.owner.Store(owner)
	r.state.Store(st)

	return r
}

func (r *switchableResolver) GetOwner(string) (string, types.HandoffState, int64, bool) { //nolint:revive // interface requires 4 results
	return r.owner.Load().(string), r.state.Load().(types.HandoffState), 1, true //nolint:errcheck,forcetypeassert
}

func (r *switchableResolver) ForceRefreshPartition(context.Context, string) error { return nil }

func (r *switchableResolver) set(owner string, st types.HandoffState) {
	r.owner.Store(owner)
	r.state.Store(st)
}

// overlapHarness bundles the two worker consumers, the shared resolver, and the
// instrumentation channels for one subtest.
type overlapHarness struct {
	w1, w2  *durable.WorkerConsumer
	res     *switchableResolver
	subject string
	pid     string
	js      jetstream.JetStream

	collector *overlapCollector
	// firstStarted fires once when the very first poison invocation begins (on
	// w1) so the driver can flip ownership precisely mid-invocation.
	firstStarted chan struct{}
	firstOnce    sync.Once
	// poisonSeq is set to the poison message's stream sequence on first delivery.
	poisonSeq atomic.Uint64
	// firstInvocation gates the long sleep to w1 only; redeliveries sleep briefly.
	firstInvocation atomic.Bool
}

// makePoisonHandler builds the instrumented handler. The poison message (by
// payload) records start/end into the collector; its FIRST invocation signals
// firstStarted and sleeps overlapW1Sleep, later invocations sleep only
// overlapW2Sleep so total runtime stays bounded. Non-poison messages return
// immediately.
func (h *overlapHarness) makePoisonHandler(workerID string) consumer.MessageHandlerFunc {
	return func(_ context.Context, msg jetstream.Msg) error {
		meta, err := msg.Metadata()
		if err != nil {
			return err
		}
		seq := meta.Sequence.Stream

		if string(msg.Data()) != overlapPoisonPayload {
			return nil
		}

		h.poisonSeq.CompareAndSwap(0, seq)

		idx := h.collector.begin(seq, workerID)
		defer h.collector.finish(idx)

		if h.firstInvocation.CompareAndSwap(false, true) {
			h.firstOnce.Do(func() { close(h.firstStarted) })
			time.Sleep(overlapW1Sleep)
		} else {
			time.Sleep(overlapW2Sleep)
		}

		return nil
	}
}

// newOverlapHarness builds embedded NATS, a LimitsPolicy stream with ONE
// partition, and two Dynamic worker consumers (w1, w2) at the strongest gating
// tier. The handler is wrapped by wrap (identity for the positive test,
// WIPHandler for the negative control). Streams/subjects are uniquely suffixed
// so the two subtests never share state.
func newOverlapHarness(
	t *testing.T,
	tag string,
	wrap func(consumer.MessageHandlerFunc) func(context.Context, jetstream.Msg) error,
) *overlapHarness {
	t.Helper()

	ctx := context.Background()
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	t.Cleanup(cleanup)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	suffix := fmt.Sprintf("%s_%d", tag, time.Now().UnixNano())
	streamName := "OVERLAP_" + suffix
	subjectRoot := "overlap" + tag

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{subjectRoot + ".*"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	pid := "p1"
	subject := subjectRoot + "." + pid

	// Shared resolver: starts owner=w1/stable so w1 admits and processes; the
	// driver flips it to w2/stable mid-invocation.
	res := newSwitchableResolver("w1", types.HandoffStateStable)

	h := &overlapHarness{
		res:          res,
		subject:      subject,
		pid:          pid,
		js:           js,
		collector:    &overlapCollector{},
		firstStarted: make(chan struct{}),
	}

	cfg := durable.WorkerConsumerConfig{
		StreamName:      streamName,
		ConsumerPrefix:  "worker",
		SubjectTemplate: subjectRoot + ".{{.PartitionID}}",
		ProcessingGate: &durable.ProcessingGateConfig{
			Enabled:       true,
			NakDelay:      50 * time.Millisecond,
			AllowedStates: []types.HandoffState{types.HandoffStateStable},
		},
		Resolver:             durable.ResolverConfig{OwnershipResolver: res},
		PullGatingEnabled:    true,
		DrainOnRemove:        true,
		DrainOnRemoveTimeout: 500 * time.Millisecond,
		AckWait:              2 * time.Second,
		MaxDeliver:           4,
		BatchSize:            1,
		FetchTimeout:         1 * time.Second,
	}

	w1, err := durable.NewWorkerConsumer(js, cfg, wrap(h.makePoisonHandler("w1")))
	require.NoError(t, err)
	t.Cleanup(func() { _ = w1.Close(context.Background()) })

	w2, err := durable.NewWorkerConsumer(js, cfg, wrap(h.makePoisonHandler("w2")))
	require.NoError(t, err)
	t.Cleanup(func() { _ = w2.Close(context.Background()) })

	h.w1, h.w2 = w1, w2

	part := types.Partition{Keys: []string{pid}}
	require.NoError(t, w1.UpdateWorkerConsumer(ctx, "w1", []types.Partition{part}))
	require.NoError(t, w2.UpdateWorkerConsumer(ctx, "w2", []types.Partition{part}))

	return h
}

func TestDynamicHandoff_AckWaitRedelivery_OverlapContract(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	t.Run("overlap-happens", func(t *testing.T) {
		// Positive direction: NO WIPHandler. The handler runs bare.
		identity := func(fn consumer.MessageHandlerFunc) func(context.Context, jetstream.Msg) error {
			return fn
		}
		h := newOverlapHarness(t, "pos", identity)
		ctx := context.Background()

		// Publish the poison message while w1 owns the partition.
		_, err := h.js.Publish(ctx, h.subject, []byte(overlapPoisonPayload))
		require.NoError(t, err)

		// Wait for w1 to enter the poison handler (gate passed, now sleeping).
		select {
		case <-h.firstStarted:
		case <-time.After(10 * time.Second):
			t.Fatal("w1 never started the poison handler")
		}

		// Flip ownership to w2/stable while w1's handler is mid-invocation. This
		// opens w2's pull gate and makes w2's gate admit the redelivery.
		h.res.set("w2", types.HandoffStateStable)

		// Invariant proof: at the moment ownership moved, the resolver — which
		// both the gate and pull gating consult — reports owner=w2/stable. So
		// w2's later admission is the genuine stable owner, not a gate bug.
		owner, state, _, ok := h.res.GetOwner(h.pid)
		require.True(t, ok)
		require.Equal(t, "w2", owner)
		require.Equal(t, types.HandoffStateStable, state)

		// Drive the remove on w1 (partition leaves w1's assignment) in the
		// background: it cannot complete while w1's handler sleeps. removeSubject-
		// Loops drains+waits up to DrainOnRemoveTimeout then SURFACES a timeout
		// error. Assert that below (the Task-4 drain-timeout contract).
		removeStart := time.Now()
		removeErrCh := make(chan error, 1)
		go func() {
			removeErrCh <- h.w1.UpdateWorkerConsumer(context.Background(), "w1", []types.Partition{})
		}()

		// Poll the collector for the overlap. The redelivery retries every AckWait
		// (2s) up to MaxDeliver, so the window is wide; give a generous deadline
		// under CI load (~3*AckWait).
		deadline := time.Now().Add(3 * 2 * time.Second)
		var first, second invocationRecord
		var found bool
		for time.Now().Before(deadline) {
			seq := h.poisonSeq.Load()
			if seq != 0 {
				if a, b, ok := h.collector.poisonOverlap(seq); ok {
					first, second, found = a, b, true
					break
				}
			}
			time.Sleep(25 * time.Millisecond)
		}

		require.True(t, found,
			"expected the poison stream sequence to be processed by two workers with "+
				"overlapping invocations (the documented AckWait-redelivery window); if this "+
				"never reproduces the lifecycle docs OVERCLAIM the window — see test doc comment")

		require.NotEqual(t, first.workerID, second.workerID, "overlap must span two distinct workers")
		require.True(t, second.start.After(first.start), "second invocation starts after the first")
		overlapDur := first.end.Sub(second.start)
		if first.end.IsZero() {
			overlapDur = time.Since(second.start)
		}
		t.Logf("OVERLAP observed: seq=%d first=%s(start=%s) second=%s(start=%s) overlap=%s",
			first.streamSeq, first.workerID, first.start.Format("15:04:05.000"),
			second.workerID, second.start.Format("15:04:05.000"), overlapDur)

		// Task-4 / drain-timeout contract: w1's remove cannot stop the loop while
		// the handler sleeps. removeSubjectLoops waits up to DrainOnRemoveTimeout
		// (500ms), then returns a non-nil timeout error WITHOUT waiting the full
		// 8s handler and without aborting the in-flight invocation.
		select {
		case rerr := <-removeErrCh:
			elapsed := time.Since(removeStart)
			t.Logf("w1 remove returned after %s, err=%v (DrainOnRemoveTimeout=500ms)", elapsed, rerr)
			require.Error(t, rerr,
				"remove must surface the drain timeout while the in-flight handler still runs")
			require.Less(t, elapsed, overlapW1Sleep,
				"remove must give up at the bounded drain timeout, not wait the full handler sleep")
		case <-time.After(overlapW1Sleep):
			t.Fatal("w1 remove did not return within the handler sleep window")
		}
	})

	t.Run("wip-handler-prevents", func(t *testing.T) {
		// Negative control: wrap the handler with WIPHandler. The keepalive
		// extends AckWait so the redelivery in the positive test never fires
		// while w1's handler is still running, so no second invocation starts.
		withWIP := func(fn consumer.MessageHandlerFunc) func(context.Context, jetstream.Msg) error {
			wrapped := consumer.NewWIPHandler(fn, consumer.WIPConfig{
				Interval:    500 * time.Millisecond, // << AckWait (2s); ~AckWait/4
				MinInterval: -1,                     // disable clamping (match wip_handler_test.go)
			})

			return wrapped.Handle
		}
		h := newOverlapHarness(t, "neg", withWIP)
		ctx := context.Background()

		_, err := h.js.Publish(ctx, h.subject, []byte(overlapPoisonPayload))
		require.NoError(t, err)

		select {
		case <-h.firstStarted:
		case <-time.After(10 * time.Second):
			t.Fatal("w1 never started the poison handler")
		}

		// Same ownership flip as the positive test.
		h.res.set("w2", types.HandoffStateStable)

		// Over a window comfortably longer than AckWait (so redelivery WOULD have
		// fired without the keepalive), assert no second worker ever starts the
		// poison sequence and no overlap is recorded.
		require.Never(t, func() bool {
			seq := h.poisonSeq.Load()
			if seq == 0 {
				return false
			}
			if h.collector.distinctWorkersStarted(seq) > 1 {
				return true
			}
			_, _, ok := h.collector.poisonOverlap(seq)

			return ok
		}, 3*2*time.Second, 50*time.Millisecond,
			"WIPHandler keepalive must prevent AckWait-redelivery, so no second worker "+
				"invokes the poison sequence while w1's handler runs")

		// Sanity: exactly one worker processed the poison sequence with WIPHandler.
		seq := h.poisonSeq.Load()
		require.NotZero(t, seq, "poison must have been delivered to w1")
		require.Equal(t, 1, h.collector.distinctWorkersStarted(seq),
			"exactly one worker processes the poison sequence with WIPHandler")
	})
}
