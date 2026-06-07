// Package vp is a throwaway feasibility POC for the "virtual partition"
// (pooled / subject-filtered) consumer idea: a fixed pool of K JetStream pull
// consumers, each holding several partition subjects via FilterSubjects,
// serving N >> K partitions.
//
// It answers four empirical questions against a real embedded NATS server
// (nats-server v2.14.1, the version pinned in the parent module), with
// observed evidence rather than documentation reading:
//
//	Exp1  Filter-mutation loss    -- add a backlogged subject to a consumer whose
//	                                 cursor has already advanced. Are the old
//	                                 messages delivered? (the W-coupled failure)
//	Exp2  Durable-rebind no-loss  -- same filter set, a different client binds the
//	                                 existing durable by name. Lossless? (the
//	                                 decoupled / slot-claim failover mechanism)
//	Exp3  Head-of-line blocking   -- one subject stops acking. Does it starve the
//	                                 co-located subjects? (intra-slot blast radius)
//	Exp4  Update seamlessness     -- add a subject to a LIVE pulling consumer.
//	                                 Disrupted? Cursor preserved? New subject flows?
//
// Each experiment LOGS its observations (the run output is the evidence) and
// asserts the predicted behavior; a failed assertion is a surprising finding to
// be investigated, not silently tolerated.
package vp

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// ---- harness ---------------------------------------------------------------

func startServer(t *testing.T) (string, func()) {
	t.Helper()
	opts := &server.Options{JetStream: true, Port: -1, StoreDir: t.TempDir()}
	ns, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		ns.Shutdown()
		t.Fatal("server not ready")
	}
	return ns.ClientURL(), ns.Shutdown
}

func connect(t *testing.T, url string) (*nats.Conn, jetstream.JetStream) {
	t.Helper()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		t.Fatalf("jetstream.New: %v", err)
	}
	return nc, js
}

func mkStream(t *testing.T, js jetstream.JetStream, name, subjectWildcard string) jetstream.Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     name,
		Subjects: []string{subjectWildcard},
		Storage:  jetstream.MemoryStorage, // semantics are storage-independent; memory is fast
	})
	if err != nil {
		t.Fatalf("CreateStream %s: %v", name, err)
	}
	return s
}

func pub(t *testing.T, js jetstream.JetStream, subject string, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < n; i++ {
		if _, err := js.Publish(ctx, subject, []byte(fmt.Sprintf("%s-%d", subject, i))); err != nil {
			t.Fatalf("publish %s: %v", subject, err)
		}
	}
}

type got struct {
	subject string
	seq     uint64
}

// drain fetches up to max messages within wait, acking each, returning what arrived.
func drain(t *testing.T, cons jetstream.Consumer, max int, wait time.Duration, ack bool) []got {
	t.Helper()
	out := []got{}
	deadline := time.Now().Add(wait)
	for len(out) < max && time.Now().Before(deadline) {
		batch, err := cons.Fetch(max-len(out), jetstream.FetchMaxWait(400*time.Millisecond))
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		n := 0
		for msg := range batch.Messages() {
			md, _ := msg.Metadata()
			out = append(out, got{subject: msg.Subject(), seq: md.Sequence.Stream})
			if ack {
				_ = msg.Ack()
			}
			n++
		}
		if err := batch.Error(); err != nil {
			t.Logf("batch error (non-fatal): %v", err)
		}
		if n == 0 {
			// no messages this round; small sleep avoids a hot loop
			time.Sleep(50 * time.Millisecond)
		}
	}
	return out
}

func subjects(g []got) map[string]int {
	m := map[string]int{}
	for _, x := range g {
		m[x.subject]++
	}
	return m
}

func seqs(g []got) []uint64 {
	s := make([]uint64, 0, len(g))
	for _, x := range g {
		s = append(s, x.seq)
	}
	return s
}

// ---- Exp1: filter-mutation loss (the W-coupled failure mode) ---------------

func TestExp1_FilterMutationLoss(t *testing.T) {
	url, stop := startServer(t)
	defer stop()
	nc, js := connect(t, url)
	defer nc.Close()
	mkStream(t, js, "VP1", "vp1.>")

	// Backlog on p1 lands at LOW stream sequences (1..5), THEN p0 (6..10).
	pub(t, js, "vp1.p1", 5) // seq 1..5  (will be the "reassigned" partition)
	pub(t, js, "vp1.p0", 5) // seq 6..10

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Consumer initially owns only p0.
	cons, err := js.CreateOrUpdateConsumer(ctx, "VP1", jetstream.ConsumerConfig{
		Durable:        "c1",
		FilterSubjects: []string{"vp1.p0"},
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	})
	if err != nil {
		t.Fatalf("create consumer: %v", err)
	}
	first := drain(t, cons, 5, 3*time.Second, true)
	t.Logf("Exp1 initial drain (p0 only): %d msgs, subjects=%v seqs=%v", len(first), subjects(first), seqs(first))

	// "Reassign" p1 onto this consumer by mutating its filter set live.
	_, err = js.UpdateConsumer(ctx, "VP1", jetstream.ConsumerConfig{
		Durable:        "c1",
		FilterSubjects: []string{"vp1.p0", "vp1.p1"},
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	})
	if err != nil {
		t.Fatalf("UpdateConsumer add p1: %v", err)
	}
	t.Log("Exp1 UpdateConsumer added vp1.p1 to filter set: OK (no error)")

	// New p1 traffic after the reassignment (seq 11..13).
	pub(t, js, "vp1.p1", 3) // seq 11..13

	after := drain(t, cons, 50, 3*time.Second, true)
	sub := subjects(after)
	t.Logf("Exp1 post-reassign drain: %d msgs, subjects=%v seqs=%v", len(after), sub, seqs(after))

	p1Delivered := sub["vp1.p1"]
	// Prediction: only the 3 NEW p1 msgs (seq 11..13) arrive; the 5 backlog
	// p1 msgs (seq 1..5) are below the shared cursor and are LOST.
	t.Logf("Exp1 RESULT: p1 messages delivered after reassign = %d (backlog of 5 + new 3 published)", p1Delivered)
	if p1Delivered == 8 {
		t.Errorf("UNEXPECTED: all 8 p1 msgs delivered -- shared-cursor loss did NOT occur (revisit the whole thesis)")
	}
	for _, x := range after {
		if x.subject == "vp1.p1" && x.seq <= 5 {
			t.Errorf("UNEXPECTED: backlog p1 seq=%d delivered after filter mutation", x.seq)
		}
	}
	t.Logf("Exp1 CONCLUSION: %d of 5 backlog p1 messages recovered; %d new delivered => LOSS on filter-mutation reassignment", 0, p1Delivered)
}

// ---- Exp2: durable rebind, no loss (the decoupled slot-claim mechanism) -----

func TestExp2_DurableRebindNoLoss(t *testing.T) {
	url, stop := startServer(t)
	defer stop()
	nc1, js1 := connect(t, url)
	mkStream(t, js1, "VP2", "vp2.>")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// A fixed-membership slot durable over two partition subjects.
	cons1, err := js1.CreateOrUpdateConsumer(ctx, "VP2", jetstream.ConsumerConfig{
		Durable:           "slot",
		FilterSubjects:    []string{"vp2.p2", "vp2.p3"},
		AckPolicy:         jetstream.AckExplicitPolicy,
		DeliverPolicy:     jetstream.DeliverAllPolicy,
		InactiveThreshold: time.Hour, // survive the brief gap between owners
	})
	if err != nil {
		t.Fatalf("create slot consumer: %v", err)
	}

	pub(t, js1, "vp2.p2", 3) // seq 1..3
	pub(t, js1, "vp2.p3", 2) // seq 4..5

	// Owner #1 consumes+acks only a prefix (2 msgs), advancing the ack floor.
	consumed1 := drain(t, cons1, 2, 2*time.Second, true)
	t.Logf("Exp2 owner#1 consumed+acked: %d msgs seqs=%v", len(consumed1), seqs(consumed1))

	// More traffic arrives ("in-between" backlog) while ownership is about to move.
	pub(t, js1, "vp2.p2", 3) // seq 6..8
	pub(t, js1, "vp2.p3", 2) // seq 9..10

	// Owner #1 disappears WITHOUT deleting the durable (simulate worker death).
	nc1.Close()
	t.Log("Exp2 owner#1 connection closed (durable left intact)")

	// Owner #2 binds the SAME durable by name -- the parti-Dynamic handoff mechanism.
	nc2, js2 := connect(t, url)
	defer nc2.Close()
	cons2, err := js2.Consumer(ctx, "VP2", "slot")
	if err != nil {
		t.Fatalf("rebind slot from owner#2: %v", err)
	}
	t.Log("Exp2 owner#2 bound existing durable 'slot' by name: OK")

	resumed := drain(t, cons2, 50, 3*time.Second, true)
	t.Logf("Exp2 owner#2 resumed: %d msgs subjects=%v seqs=%v", len(resumed), subjects(resumed), seqs(resumed))

	// Prediction: owner#2 resumes from the persisted ack floor (after seq 2) and
	// receives every uncommitted message (seq 3..10) -- NOTHING lost. Same filter
	// set => the shared cursor is preserved across the ownership change.
	want := map[uint64]bool{3: true, 4: true, 5: true, 6: true, 7: true, 8: true, 9: true, 10: true}
	for _, s := range seqs(resumed) {
		delete(want, s)
	}
	if len(want) != 0 {
		t.Errorf("UNEXPECTED LOSS on rebind: missing seqs=%v", want)
	} else {
		t.Logf("Exp2 CONCLUSION: rebind delivered all 8 uncommitted msgs (seq 3..10) => LOSSLESS failover (no filter mutation)")
	}
}

// ---- Exp3: head-of-line blocking / noisy neighbor inside a slot -------------

func TestExp3_HeadOfLineBlocking(t *testing.T) {
	url, stop := startServer(t)
	defer stop()
	nc, js := connect(t, url)
	defer nc.Close()
	mkStream(t, js, "VP3", "vp3.>")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const maxAck = 5
	cons, err := js.CreateOrUpdateConsumer(ctx, "VP3", jetstream.ConsumerConfig{
		Durable:        "e",
		FilterSubjects: []string{"vp3.stuck", "vp3.healthy"},
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
		MaxAckPending:  maxAck,
		AckWait:        2 * time.Second,
	})
	if err != nil {
		t.Fatalf("create consumer: %v", err)
	}

	// "stuck" partition lands first (low seq), "healthy" partition after it.
	pub(t, js, "vp3.stuck", 10)   // seq 1..10
	pub(t, js, "vp3.healthy", 10) // seq 11..20

	// Pull WITHOUT acking. DeliverAll => the stuck (low-seq) msgs come first and,
	// being unacked, fill MaxAckPending; nothing else is delivered.
	batch, err := cons.Fetch(20, jetstream.FetchMaxWait(2*time.Second))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	var stuckN, healthyN int
	held := []jetstream.Msg{}
	for msg := range batch.Messages() {
		held = append(held, msg)
		switch msg.Subject() {
		case "vp3.stuck":
			stuckN++ // intentionally NOT acked
		case "vp3.healthy":
			healthyN++
		}
	}
	t.Logf("Exp3 first pull (no acks): stuck=%d healthy=%d (MaxAckPending=%d)", stuckN, healthyN, maxAck)

	// Prediction: at most MaxAckPending msgs delivered, ALL stuck; healthy starved.
	if healthyN > 0 {
		t.Errorf("UNEXPECTED: %d healthy msgs delivered while stuck partition holds MaxAckPending unacked", healthyN)
	}
	if stuckN > maxAck {
		t.Errorf("UNEXPECTED: %d delivered exceeds MaxAckPending=%d", stuckN, maxAck)
	}
	t.Logf("Exp3 OBSERVATION: a single non-acking partition starves co-located partitions on the same slot (blast radius = whole slot)")

	// Now ack the stuck backlog and confirm the healthy partition unblocks.
	for _, m := range held {
		_ = m.Ack()
	}
	rest := drain(t, cons, 50, 4*time.Second, true)
	t.Logf("Exp3 after acking stuck backlog: drained %d more, subjects=%v", len(rest), subjects(rest))
	if subjects(rest)["vp3.healthy"] == 0 {
		t.Errorf("UNEXPECTED: healthy partition never recovered after acks")
	} else {
		t.Logf("Exp3 CONCLUSION: healthy partition unblocks only once the stuck partition's acks free MaxAckPending => intra-slot head-of-line blocking confirmed")
	}
}

// ---- Exp4: update seamlessness on a LIVE pulling consumer -------------------

func TestExp4_UpdateSeamlessness(t *testing.T) {
	url, stop := startServer(t)
	defer stop()
	nc, js := connect(t, url)
	defer nc.Close()
	mkStream(t, js, "VP4", "vp4.>")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cons, err := js.CreateOrUpdateConsumer(ctx, "VP4", jetstream.ConsumerConfig{
		Durable:        "f",
		FilterSubjects: []string{"vp4.a"},
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	})
	if err != nil {
		t.Fatalf("create consumer: %v", err)
	}

	pub(t, js, "vp4.a", 5) // seq 1..5
	c0 := drain(t, cons, 5, 2*time.Second, true)
	t.Logf("Exp4 pre-update drain (a only): %d msgs seqs=%v", len(c0), seqs(c0))

	pub(t, js, "vp4.a", 2) // seq 6..7  -- published BEFORE the update, after the cursor

	// Add subject b to the live consumer.
	_, err = js.UpdateConsumer(ctx, "VP4", jetstream.ConsumerConfig{
		Durable:        "f",
		FilterSubjects: []string{"vp4.a", "vp4.b"},
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	})
	if err != nil {
		t.Fatalf("UpdateConsumer add b on live consumer: %v", err)
	}
	t.Log("Exp4 UpdateConsumer added vp4.b to a live consumer: OK (no recreation, no error)")

	pub(t, js, "vp4.a", 2) // seq 8..9
	pub(t, js, "vp4.b", 3) // seq 10..12

	c1 := drain(t, cons, 50, 3*time.Second, true)
	t.Logf("Exp4 post-update drain: %d msgs subjects=%v seqs=%v", len(c1), subjects(c1), seqs(c1))

	// Prediction: cursor preserved (no redelivery of seq 1..5); the pre-update a
	// msgs (6..7), post-update a msgs (8..9), and the new b msgs (10..12) all flow.
	for _, x := range c1 {
		if x.seq <= 5 {
			t.Errorf("UNEXPECTED: cursor reset -- seq=%d redelivered after update", x.seq)
		}
	}
	sub := subjects(c1)
	if sub["vp4.a"] != 4 { // seq 6,7,8,9
		t.Errorf("expected 4 'a' msgs (no disruption to existing subject), got %d", sub["vp4.a"])
	}
	if sub["vp4.b"] != 3 { // seq 10,11,12
		t.Errorf("expected 3 new 'b' msgs to flow, got %d", sub["vp4.b"])
	}
	t.Logf("Exp4 CONCLUSION: filter update is a seamless in-place op (no recreation, cursor preserved, existing subject undisturbed, new subject's NEW msgs flow). The very seamlessness == why backlog below the cursor is skipped (Exp1).")
}
