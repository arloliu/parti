package vp

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Exp8 answers: when a migration "loses" a message (the gaining consumer's shared
// cursor has advanced past it), is the message DESTROYED, or merely skipped by
// that one consumer? It reproduces the Exp1 loss, then points a SECOND, fresh
// consumer at the same subject and checks whether the "lost" messages are still
// retrievable from the stream.
//
// Result distinguishes two very different meanings of "loss":
//   - DELIVERY GAP (Limits): the message stays in the stream; only the migrated
//     consumer skipped it. Recoverable by a different consumer / a rewind — but the
//     normal delivery path never re-feeds it to the handler.
//   - PHYSICAL DELETION (Interest/WorkQueue): the message is gone for good.
func TestExp8_LossIsDeliveryGapNotDeletion(t *testing.T) {
	url, stop := startServer(t)
	defer stop()
	nc, js := connect(t, url)
	defer nc.Close()

	// LimitsPolicy is parti's recommended/common retention (ack-independent).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "VP8",
		Subjects:  []string{"vp8.>"},
		Storage:   jetstream.MemoryStorage,
		Retention: jetstream.LimitsPolicy,
	})
	if err != nil {
		t.Fatalf("CreateStream: %v", err)
	}

	// Same setup as Exp1: p1 backlog at low seq, p0 at higher seq.
	pub(t, js, "vp8.p1", 5) // seq 1..5  -- the "to-be-skipped" backlog
	pub(t, js, "vp8.p0", 5) // seq 6..10

	// "bucket" = the consumer that will gain p1 via a live filter mutation.
	bucket, err := js.CreateOrUpdateConsumer(ctx, "VP8", jetstream.ConsumerConfig{
		Durable:        "bucket",
		FilterSubjects: []string{"vp8.p0"},
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	})
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	drain(t, bucket, 5, 2*time.Second, true) // cursor advances past seq 10

	if _, err := js.UpdateConsumer(ctx, "VP8", jetstream.ConsumerConfig{
		Durable:        "bucket",
		FilterSubjects: []string{"vp8.p0", "vp8.p1"},
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	}); err != nil {
		t.Fatalf("UpdateConsumer add p1: %v", err)
	}
	pub(t, js, "vp8.p1", 3) // seq 11..13 (new traffic after migration)

	onBucket := drain(t, bucket, 50, 2*time.Second, true)
	t.Logf("Exp8 'bucket' (migrated consumer) saw p1 seqs=%v  (backlog 1..5 SKIPPED)", seqsOf(onBucket, "vp8.p1"))

	// Now the decisive check: a FRESH, independent consumer on the same subject.
	recovery, err := js.CreateOrUpdateConsumer(ctx, "VP8", jetstream.ConsumerConfig{
		Durable:        "recovery",
		FilterSubjects: []string{"vp8.p1"},
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	})
	if err != nil {
		t.Fatalf("create recovery consumer: %v", err)
	}
	onRecovery := drain(t, recovery, 50, 3*time.Second, true)
	recSeqs := seqsOf(onRecovery, "vp8.p1")
	t.Logf("Exp8 fresh 'recovery' consumer saw p1 seqs=%v", recSeqs)

	// Stream still physically holds the messages?
	info, err := js.Stream(ctx, "VP8")
	if err != nil {
		t.Fatalf("stream lookup: %v", err)
	}
	si, _ := info.Info(ctx)
	t.Logf("Exp8 stream VP8 total msgs still resident: %d (8 p1 + 5 p0 = 13 expected on Limits)", si.State.Msgs)

	// The backlog the migrated consumer skipped (seq 1..5) must still be readable.
	for want := uint64(1); want <= 5; want++ {
		found := false
		for _, s := range recSeqs {
			if s == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("UNEXPECTED: skipped backlog seq=%d is NOT retrievable even by a fresh consumer (physically gone)", want)
		}
	}
	if len(recSeqs) == 8 {
		t.Logf("Exp8 CONCLUSION: the 5 'lost' backlog msgs are NOT destroyed -- they remain in the stream (Limits) and a DIFFERENT consumer reads all 8 p1 msgs. 'Loss' = the migrated consumer's shared cursor skipped them; the normal delivery path never re-feeds them to the handler, and there is no per-partition ack floor to rewind to.")
	}
}

func seqsOf(g []got, subject string) []uint64 {
	out := []uint64{}
	for _, x := range g {
		if x.subject == subject {
			out = append(out, x.seq)
		}
	}
	return out
}
