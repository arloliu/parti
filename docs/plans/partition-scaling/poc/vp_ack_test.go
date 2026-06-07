// Ack-semantics / flow-control / retention experiments for the virtual-partition
// assessment. Reuses the harness in vp_test.go (same package).
//
//	Exp5  Subject removal w/ in-flight  -- a multi-filter consumer has UNACKED msgs
//	                                        on subject b; UpdateConsumer drops b from
//	                                        the filter set. Do the in-flight b msgs
//	                                        ever redeliver? Does acking them error?
//	                                        Run on Limits AND Interest retention.
//	Exp6  WorkQueue overlap rules        -- WorkQueuePolicy stream: can two consumers
//	                                        carry OVERLAPPING filters? DISJOINT filters?
//	                                        Is remove-before-add mandatory to move a
//	                                        subject between consumers?
//	Exp7  Interest publish-time interest -- InterestPolicy stream: a msg published to a
//	                                        subject that NO consumer's filter covers --
//	                                        is it retained or discarded at publish time?
package vp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// mkStreamRetention is mkStream with an explicit retention policy.
func mkStreamRetention(t *testing.T, js jetstream.JetStream, name, subjectWildcard string, ret jetstream.RetentionPolicy) jetstream.Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      name,
		Subjects:  []string{subjectWildcard},
		Storage:   jetstream.MemoryStorage,
		Retention: ret,
	})
	if err != nil {
		t.Fatalf("CreateStream %s (ret=%v): %v", name, ret, err)
	}
	return s
}

// fetchNoAck pulls up to max msgs within wait WITHOUT acking, returning the
// jetstream.Msg handles so the caller can ack/inspect them later.
func fetchNoAck(t *testing.T, cons jetstream.Consumer, max int, wait time.Duration) []jetstream.Msg {
	t.Helper()
	out := []jetstream.Msg{}
	deadline := time.Now().Add(wait)
	for len(out) < max && time.Now().Before(deadline) {
		batch, err := cons.Fetch(max-len(out), jetstream.FetchMaxWait(400*time.Millisecond))
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		n := 0
		for msg := range batch.Messages() {
			out = append(out, msg)
			n++
		}
		if err := batch.Error(); err != nil {
			t.Logf("batch error (non-fatal): %v", err)
		}
		if n == 0 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	return out
}

// ---- Exp5: subject removal with in-flight unacked messages ------------------

func TestExp5_SubjectRemovalInFlight(t *testing.T) {
	cases := []struct {
		name string
		ret  jetstream.RetentionPolicy
	}{
		{"Limits", jetstream.LimitsPolicy},
		{"Interest", jetstream.InterestPolicy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, stop := startServer(t)
			defer stop()
			nc, js := connect(t, url)
			defer nc.Close()
			mkStreamRetention(t, js, "VP5", "vp5.>", tc.ret)

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			// AckWait short so redelivery (if any) happens within the test window.
			cons, err := js.CreateOrUpdateConsumer(ctx, "VP5", jetstream.ConsumerConfig{
				Durable:        "slot",
				FilterSubjects: []string{"vp5.a", "vp5.b"},
				AckPolicy:      jetstream.AckExplicitPolicy,
				DeliverPolicy:  jetstream.DeliverAllPolicy,
				AckWait:        1 * time.Second,
				MaxDeliver:     -1,
			})
			if err != nil {
				t.Fatalf("create consumer: %v", err)
			}

			pub(t, js, "vp5.a", 2) // seq 1..2
			pub(t, js, "vp5.b", 3) // seq 3..5

			// Pull all 5 but do NOT ack: 2 on a, 3 on b are now in-flight (pending).
			inflight := fetchNoAck(t, cons, 5, 3*time.Second)
			bMsgs := []jetstream.Msg{}
			for _, m := range inflight {
				if m.Subject() == "vp5.b" {
					bMsgs = append(bMsgs, m)
				}
			}
			t.Logf("Exp5/%s pulled %d in-flight (b=%d), none acked", tc.name, len(inflight), len(bMsgs))

			ci, _ := cons.Info(ctx)
			t.Logf("Exp5/%s before removal: NumPending=%d NumAckPending=%d",
				tc.name, ci.NumPending, ci.NumAckPending)

			// REMOVE subject b from the filter set while its msgs are in-flight unacked.
			_, err = js.UpdateConsumer(ctx, "VP5", jetstream.ConsumerConfig{
				Durable:        "slot",
				FilterSubjects: []string{"vp5.a"},
				AckPolicy:      jetstream.AckExplicitPolicy,
				DeliverPolicy:  jetstream.DeliverAllPolicy,
				AckWait:        1 * time.Second,
				MaxDeliver:     -1,
			})
			if err != nil {
				t.Fatalf("UpdateConsumer remove b: %v", err)
			}
			t.Logf("Exp5/%s removed vp5.b from filter set (its msgs still unacked)", tc.name)

			ci2, _ := cons.Info(ctx)
			t.Logf("Exp5/%s after removal: NumPending=%d NumAckPending=%d",
				tc.name, ci2.NumPending, ci2.NumAckPending)

			st5, _ := js.Stream(ctx, "VP5")
			sti5, _ := st5.Info(ctx)
			t.Logf("Exp5/%s stream msg count after removal: %d (published 2 a + 3 b = 5)",
				tc.name, sti5.State.Msgs)

			// Wait well past AckWait. If removed-subject msgs are still "owned" by
			// the consumer they would redeliver; if dropped, nothing arrives.
			redelivered := drain(t, cons, 10, 3*time.Second, false)
			rsub := subjects(redelivered)
			t.Logf("Exp5/%s after AckWait expiry: redelivered subjects=%v total=%d",
				tc.name, rsub, len(redelivered))

			// Probe: does acking a now-removed-subject msg error?
			if len(bMsgs) > 0 {
				ackErr := bMsgs[0].Ack()
				t.Logf("Exp5/%s ack of a removed-subject in-flight msg: err=%v", tc.name, ackErr)
				if ackErr != nil && !errors.Is(ackErr, context.DeadlineExceeded) {
					t.Logf("Exp5/%s NOTE ack returned error after removal", tc.name)
				}
			}

			if rsub["vp5.b"] == 0 {
				t.Logf("Exp5/%s FINDING: removed-subject in-flight msgs were NOT redelivered (stranded)", tc.name)
				if tc.name == "Limits" {
					t.Errorf("Exp5/Limits UNEXPECTED: removed-subject unacked msgs should keep redelivering on Limits, got 0 (stranded)")
				}
			} else {
				t.Logf("Exp5/%s FINDING: removed-subject msgs STILL redelivered after removal (count=%d)", tc.name, rsub["vp5.b"])
				if tc.name == "Interest" {
					t.Errorf("Exp5/Interest UNEXPECTED: removed-subject msgs should be deleted/stranded on Interest, got %d redelivered", rsub["vp5.b"])
				}
			}
		})
	}
}

// ---- Exp6: WorkQueue overlapping/disjoint filter rules ----------------------

func TestExp6_WorkQueueFilterRules(t *testing.T) {
	url, stop := startServer(t)
	defer stop()
	nc, js := connect(t, url)
	defer nc.Close()
	mkStreamRetention(t, js, "VP6", "vp6.>", jetstream.WorkQueuePolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Consumer A holds p0.
	_, err := js.CreateOrUpdateConsumer(ctx, "VP6", jetstream.ConsumerConfig{
		Durable:        "slotA",
		FilterSubjects: []string{"vp6.p0"},
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	})
	if err != nil {
		t.Fatalf("create slotA[p0]: %v", err)
	}
	t.Log("Exp6 slotA created with filter [vp6.p0]")

	// Attempt B with OVERLAPPING filter (also p0). Expect server rejection.
	_, errOverlap := js.CreateOrUpdateConsumer(ctx, "VP6", jetstream.ConsumerConfig{
		Durable:        "slotB_overlap",
		FilterSubjects: []string{"vp6.p0"},
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	})
	t.Logf("Exp6 slotB OVERLAPPING filter [vp6.p0]: err=%v", errOverlap)
	if errOverlap == nil {
		t.Errorf("Exp6 UNEXPECTED: WorkQueue allowed two consumers on the same subject (disjoint rule should reject)")
	}
	t.Logf("Exp6 FINDING: WorkQueue REJECTS overlapping consumer filters (as expected)")

	// Attempt B with DISJOINT filter (p1). Expect success.
	_, errDisjoint := js.CreateOrUpdateConsumer(ctx, "VP6", jetstream.ConsumerConfig{
		Durable:        "slotB_disjoint",
		FilterSubjects: []string{"vp6.p1"},
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	})
	t.Logf("Exp6 slotB DISJOINT filter [vp6.p1]: err=%v", errDisjoint)
	if errDisjoint != nil {
		t.Errorf("Exp6 UNEXPECTED: disjoint filter rejected on WorkQueue: %v", errDisjoint)
	}

	// Rebalance: move p1 from slotB_disjoint to slotA.
	// First try ADD-before-remove: grow slotA to [p0,p1] while slotB still owns p1.
	_, errAddFirst := js.UpdateConsumer(ctx, "VP6", jetstream.ConsumerConfig{
		Durable:        "slotA",
		FilterSubjects: []string{"vp6.p0", "vp6.p1"},
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	})
	t.Logf("Exp6 rebalance ADD-before-remove (slotA grows to [p0,p1] while slotB owns p1): err=%v", errAddFirst)
	if errAddFirst == nil {
		t.Errorf("Exp6 UNEXPECTED: WorkQueue permitted transient overlap during add-before-remove (should force remove-before-add)")
	}
	t.Logf("Exp6 FINDING: WorkQueue forces REMOVE-before-add ordering for cross-consumer subject moves")

	// Now do it correctly: remove p1 from slotB first, then add to slotA.
	_, err = js.UpdateConsumer(ctx, "VP6", jetstream.ConsumerConfig{
		Durable:        "slotB_disjoint",
		FilterSubjects: []string{"vp6.unused"},
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	})
	if err != nil {
		t.Fatalf("Exp6 remove p1 from slotB: %v", err)
	}
	_, errAddAfter := js.UpdateConsumer(ctx, "VP6", jetstream.ConsumerConfig{
		Durable:        "slotA",
		FilterSubjects: []string{"vp6.p0", "vp6.p1"},
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	})
	t.Logf("Exp6 rebalance REMOVE-then-add (slotB drops p1, then slotA adds p1): err=%v", errAddAfter)
	if errAddAfter != nil {
		t.Errorf("Exp6 UNEXPECTED: remove-then-add still failed: %v", errAddAfter)
	}
}

// ---- Exp7: Interest-policy publish-time interest hazard ---------------------

func TestExp7_InterestPublishTimeInterest(t *testing.T) {
	url, stop := startServer(t)
	defer stop()
	nc, js := connect(t, url)
	defer nc.Close()
	mkStreamRetention(t, js, "VP7", "vp7.>", jetstream.InterestPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// One consumer covering ONLY p0. p1 is covered by NO consumer filter.
	consA, err := js.CreateOrUpdateConsumer(ctx, "VP7", jetstream.ConsumerConfig{
		Durable:        "slotA",
		FilterSubjects: []string{"vp7.p0"},
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	})
	if err != nil {
		t.Fatalf("create slotA[p0]: %v", err)
	}

	// Publish to p1 (no interest at publish time) and p0 (interest exists).
	pub(t, js, "vp7.p1", 5) // no consumer filter covers p1
	pub(t, js, "vp7.p0", 3) // covered by slotA

	si, _ := js.Stream(ctx, "VP7")
	info, _ := si.Info(ctx)
	t.Logf("Exp7 stream msgs after publish (InterestPolicy): %d (published 5 p1 + 3 p0 = 8)", info.State.Msgs)
	if info.State.Msgs != 3 {
		t.Errorf("Exp7 UNEXPECTED: InterestPolicy should retain only the 3 covered p0 msgs at publish time, stream holds %d", info.State.Msgs)
	}

	// Now create a consumer for p1 AFTER the fact and see if the 5 p1 msgs exist.
	consB, err := js.CreateOrUpdateConsumer(ctx, "VP7", jetstream.ConsumerConfig{
		Durable:        "slotB",
		FilterSubjects: []string{"vp7.p1"},
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	})
	if err != nil {
		t.Fatalf("create slotB[p1]: %v", err)
	}

	gotP1 := drain(t, consB, 10, 2*time.Second, true)
	gotP0 := drain(t, consA, 10, 2*time.Second, true)
	t.Logf("Exp7 slotB (p1, created AFTER publish) received %d msgs", len(gotP1))
	t.Logf("Exp7 slotA (p0, existed at publish) received %d msgs", len(gotP0))

	if len(gotP1) != 0 {
		t.Errorf("Exp7 UNEXPECTED: p1 msgs survived despite no interest at publish time (count=%d); publish-time-discard thesis would be wrong", len(gotP1))
	}
	t.Logf("Exp7 FINDING: InterestPolicy DISCARDED p1 msgs published when no consumer filter covered p1 (publish-time-interest loss)")
}
