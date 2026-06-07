package vp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Exp9 asks the WorkQueue-specific version of the migration question. WorkQueue
// differs from Limits in three ways that matter here: (1) a message is DELETED on
// ack (consume-once); (2) the server ENFORCES disjoint consumer filters (Exp6);
// (3) it does NOT discard at publish time when no consumer covers a subject (unlike
// Interest) — it retains until consumed. The question: does delete-on-ack +
// retain-until-consumed save the migrating partition's in-flight backlog, or does
// the gaining bucket's advanced cursor still skip it (Exp1) — and is it recoverable
// given the disjoint-filter rule?
func TestExp9_WorkQueueMigrationGap(t *testing.T) {
	url, stop := startServer(t)
	defer stop()
	nc, js := connect(t, url)
	defer nc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "VP9",
		Subjects:  []string{"vp9.>"},
		Storage:   jetstream.MemoryStorage,
		Retention: jetstream.WorkQueuePolicy,
	})
	if err != nil {
		t.Fatalf("CreateStream: %v", err)
	}

	// "p" is the migrating partition. Publish its backlog BEFORE any consumer
	// covers it (the no-owner window). WorkQueue retains these (no publish-time
	// discard).
	pub(t, js, "vp9.p", 5)    // seq 1..5  -- retained, uncovered
	pub(t, js, "vp9.busy", 5) // seq 6..10 -- the gaining bucket's own traffic

	// bucketB owns only "busy" for now (disjoint from anything covering "p").
	bucketB, err := js.CreateOrUpdateConsumer(ctx, "VP9", jetstream.ConsumerConfig{
		Durable:        "bucketB",
		FilterSubjects: []string{"vp9.busy"},
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	})
	if err != nil {
		t.Fatalf("create bucketB: %v", err)
	}
	consumed := drain(t, bucketB, 5, 2*time.Second, true) // acks busy 6..10 -> deleted; cursor past 10
	t.Logf("Exp9 bucketB consumed+acked busy seqs=%v (deleted on ack); cursor now past seq 10", seqsOf(consumed, "vp9.busy"))

	// "Migrate" p onto bucketB (remove-before-add already satisfied: nothing else
	// covers p). This is the gaining side of a handoff.
	if _, err := js.UpdateConsumer(ctx, "VP9", jetstream.ConsumerConfig{
		Durable:        "bucketB",
		FilterSubjects: []string{"vp9.busy", "vp9.p"},
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	}); err != nil {
		t.Fatalf("migrate p onto bucketB: %v", err)
	}
	pub(t, js, "vp9.p", 3) // seq 11..13 (new traffic after migration)

	got := drain(t, bucketB, 50, 2*time.Second, true)
	pSeen := seqsOf(got, "vp9.p")
	t.Logf("Exp9 bucketB after gaining p saw p seqs=%v", pSeen)

	info, _ := js.Stream(ctx, "VP9")
	si, _ := info.Info(ctx)
	t.Logf("Exp9 stream VP9 resident msgs=%d (busy 6..10 deleted on ack; p backlog 1..5 + new 11..13 status?)", si.State.Msgs)

	// Did the gaining bucket's cursor skip the retained backlog (1..5)?
	skipped := true
	for _, s := range pSeen {
		if s <= 5 {
			skipped = false
		}
	}
	if !skipped {
		t.Errorf("Exp9 UNEXPECTED: WorkQueue delivered pre-migration backlog (1..5) — the gap thesis would be wrong; pSeen=%v", pSeen)
	}
	t.Logf("Exp9 FINDING: WorkQueue RETAINED the backlog but bucketB's advanced cursor SKIPPED seq 1..5 (only new 11..13 delivered) — same Exp1 gap")

	// Recovery attempt: can an overlapping consumer read the stranded backlog?
	_, recErr := js.CreateOrUpdateConsumer(ctx, "VP9", jetstream.ConsumerConfig{
		Durable:        "recovery",
		FilterSubjects: []string{"vp9.p"}, // overlaps bucketB which now covers vp9.p
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	})
	var apiErr *jetstream.APIError
	if recErr == nil {
		t.Errorf("Exp9 UNEXPECTED: overlapping recovery consumer was allowed on WorkQueue (disjoint rule should reject it)")
	} else if errors.As(recErr, &apiErr) {
		if apiErr.ErrorCode != 10100 {
			t.Errorf("Exp9 UNEXPECTED: recovery rejected with err_code=%d, expected 10100 (filtered consumer not unique)", apiErr.ErrorCode)
		}
		t.Logf("Exp9 recovery consumer (overlapping vp9.p) REJECTED: code=%d err_code=%d — stranded backlog NOT recoverable while bucket still owns the subject", apiErr.Code, apiErr.ErrorCode)
	} else {
		t.Errorf("Exp9 recovery consumer failed with non-API error: %v", recErr)
	}

	// After removing p from bucketB, recovery (now disjoint) can read what's left.
	if _, err := js.UpdateConsumer(ctx, "VP9", jetstream.ConsumerConfig{
		Durable: "bucketB", FilterSubjects: []string{"vp9.busy"},
		AckPolicy: jetstream.AckExplicitPolicy, DeliverPolicy: jetstream.DeliverAllPolicy,
	}); err != nil {
		t.Fatalf("shrink bucketB: %v", err)
	}
	rec, err := js.CreateOrUpdateConsumer(ctx, "VP9", jetstream.ConsumerConfig{
		Durable: "recovery", FilterSubjects: []string{"vp9.p"},
		AckPolicy: jetstream.AckExplicitPolicy, DeliverPolicy: jetstream.DeliverAllPolicy,
	})
	if err != nil {
		t.Fatalf("recovery after shrink: %v", err)
	}
	recovered := seqsOf(drain(t, rec, 50, 2*time.Second, true), "vp9.p")
	t.Logf("Exp9 after removing p from bucketB, fresh recovery consumer read p seqs=%v", recovered)
	if len(recovered) != 5 {
		t.Errorf("Exp9 UNEXPECTED: after removing p from bucket, recovery should read the 5 stranded backlog msgs, got %v", recovered)
	}
	t.Logf("Exp9 CONCLUSION: WorkQueue does NOT save migration — it eliminates DUPLICATES (acked msgs are deleted) and retains the no-owner window (vs Interest's publish-time discard), BUT the gaining bucket's advanced cursor still SKIPS the backlog (same gap), and the disjoint-filter rule BLOCKS an overlapping recovery consumer until the subject is removed from the bucket. Net: gap remains, recovery is harder, and any transient two-bucket overlap during handoff is a hard 10100 rejection.")
}
