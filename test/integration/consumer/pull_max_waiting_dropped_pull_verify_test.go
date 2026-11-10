package consumer_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestVerify_PullMaxWaitingDropsOverlappingPull characterizes how a low
// consumer MaxWaiting strands a nats.go Messages() iterator.
// It reproduces, on a single server, the production signature
// "messages sit undelivered in the stream until ErrNoHeartbeat fires".
//
// Observed on the pinned nats-server (v2.14.x) and nats.go:
//
//   - nats.go issues a new pull whenever its pending count drops below half the batch,
//     so one iterator routinely has overlapping pull requests on the server.
//   - The server serves waiting requests round-robin,
//     so two overlapping requests both stay partially filled and a third overlaps them.
//   - When the waiting queue is full and the request carries a heartbeat,
//     the server drops the request without any reply.
//     The client still counts it as outstanding.
//   - Once the two accepted requests are fulfilled the client's pending count rests at
//     batch/2+1, which is not below the re-pull threshold, so it never pulls again.
//     No request waits on the server, so no heartbeat is sent.
//     New messages stay in the stream until Next returns ErrNoHeartbeat.
//
// Calling Next again on the same iterator recovers: nats.go has reset its pending count.
func TestVerify_PullMaxWaitingDropsOverlappingPull(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	const (
		batch     = 100
		heartbeat = 500 * time.Millisecond
		// Long enough that no request expires during the test.
		expiry = 30 * time.Second
	)

	tests := []struct {
		name       string
		maxWaiting int
		wantStrand bool
	}{
		{"max_waiting_2_strands_the_iterator", 2, true},
		{"max_waiting_512_keeps_pulling", 512, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nc, cleanup := testutil.StartEmbeddedNATS(t)
			defer cleanup()

			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()

			js, err := jetstream.New(nc)
			require.NoError(t, err)

			streamName := fmt.Sprintf("MAXWAIT_%d", tt.maxWaiting)
			subject := fmt.Sprintf("maxwait.%d", tt.maxWaiting)
			_, err = js.CreateStream(ctx, jetstream.StreamConfig{
				Name:     streamName,
				Subjects: []string{subject},
				Storage:  jetstream.MemoryStorage,
			})
			require.NoError(t, err)

			cons, err := js.CreateOrUpdateConsumer(ctx, streamName, jetstream.ConsumerConfig{
				Durable:       "dur",
				AckPolicy:     jetstream.AckExplicitPolicy,
				MaxWaiting:    tt.maxWaiting,
				MaxAckPending: 1000,
			})
			require.NoError(t, err)

			iter, err := cons.Messages(
				jetstream.PullMaxMessages(batch),
				jetstream.PullExpiry(expiry),
				jetstream.PullHeartbeat(heartbeat),
			)
			require.NoError(t, err)
			defer iter.Stop()

			// One goroutine drives Next for the whole test and never stops the iterator on an error.
			type result struct {
				err error
			}
			results := make(chan result, 512)
			go func() {
				for {
					msg, nextErr := iter.Next()
					if errors.Is(nextErr, jetstream.ErrMsgIteratorClosed) {
						return
					}
					if nextErr == nil {
						_ = msg.Ack()
					}
					results <- result{err: nextErr}
				}
			}()

			publish := func(n int) {
				t.Helper()
				for range n {
					_, pubErr := js.Publish(ctx, subject, []byte("x"))
					require.NoError(t, pubErr)
				}
			}
			// receive waits for n messages and fails on any iterator error.
			receive := func(n int, phase string) {
				t.Helper()
				for i := range n {
					select {
					case r := <-results:
						require.NoError(t, r.err, "%s: unexpected iterator error at message %d", phase, i+1)
					case <-time.After(10 * time.Second):
						t.Fatalf("%s: message %d of %d not delivered", phase, i+1, n)
					}
				}
			}
			numWaiting := func() int {
				t.Helper()
				info, infoErr := cons.Info(ctx)
				require.NoError(t, infoErr)

				return info.NumWaiting
			}

			// Phase 1: a backlog larger than half the batch.
			// Pull 1 (100) takes all 60; after 51 are consumed the client issues pull 2 (51).
			publish(60)
			receive(60, "backlog")
			require.Eventually(t, func() bool { return numWaiting() == 2 }, 5*time.Second, 20*time.Millisecond,
				"two overlapping pull requests must be waiting")

			// Phase 2: 42 more messages, served round-robin, leave both requests partially filled
			// (49 outstanding in total) and take the client's pending count to 49.
			// The client then issues pull 3.
			publish(42)
			receive(42, "second wave")

			// Phase 3: exactly the 49 messages the two accepted requests can still take.
			publish(49)
			receive(49, "drain")

			if !tt.wantStrand {
				// With room in the waiting queue pull 3 was accepted, so delivery simply continues.
				publish(1)
				receive(1, "after drain")
				require.GreaterOrEqual(t, numWaiting(), 1, "a pull request must still be waiting")

				return
			}

			// Pull 3 was dropped without a reply: nothing waits on the server,
			// yet the client believes 51 messages are still on their way.
			require.Eventually(t, func() bool { return numWaiting() == 0 }, 5*time.Second, 20*time.Millisecond,
				"both accepted pull requests are fulfilled and the third was dropped")

			// Phase 4: a new message is stranded in the stream.
			publish(1)
			strandedAt := time.Now()

			// While stranded the message is pending in the stream and was never delivered,
			// so this is not a redelivery wait.
			info, err := cons.Info(ctx)
			require.NoError(t, err)
			require.Equal(t, uint64(1), info.NumPending, "the message must still be pending in the stream")
			require.Equal(t, 0, info.NumAckPending, "nothing was delivered, so nothing awaits an ack")
			require.Equal(t, 0, info.NumWaiting, "no pull request is waiting on the server")

			select {
			case r := <-results:
				require.ErrorIs(t, r.err, jetstream.ErrNoHeartbeat,
					"the stranded iterator must surface as a missed heartbeat, not a delivery")
			case <-time.After(10 * time.Second):
				t.Fatal("neither a message nor ErrNoHeartbeat arrived")
			}
			t.Logf("stranded for %s before ErrNoHeartbeat (2x heartbeat = %s)", time.Since(strandedAt), 2*heartbeat)

			// The same iterator recovers on the next Next call.
			receive(1, "recovery on the same iterator")
		})
	}
}
