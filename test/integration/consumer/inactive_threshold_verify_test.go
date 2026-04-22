package consumer_test

// Verify_InactiveThreshold runs targeted probes against a live embedded NATS server
// to establish ground truth for docs/auto_recover_impl_plan.md.
//
// Run with:
//
//	go test ./test/integration/consumer/ -run TestVerify_InactiveThresholdSemantics -v -timeout 90s

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	partitesting "github.com/arloliu/parti/v2/partitest"
)

// nats-go enforces PullExpiry >= 1s. Use 1s so pulls expire frequently enough
// that the background goroutine must keep re-issuing them during the wait window.
// InactiveThreshold must be longer than PullExpiry to allow us to distinguish
// "background goroutine re-issued pulls" (consumer stays alive) from "no pulls
// issued" (consumer expires).
const (
	verifyPullExpiry        = 1 * time.Second
	verifyInactiveThreshold = 3 * time.Second             // > PullExpiry so expiry is detectable
	verifyExpiryWait        = 2 * verifyInactiveThreshold // 6s — well past threshold
)

// verifyConsumerExists checks if the named consumer still exists on the server.
func verifyConsumerExists(t *testing.T, js jetstream.JetStream, streamName, consName string) bool {
	t.Helper()
	_, err := js.Consumer(context.Background(), streamName, consName)
	if err == nil {
		return true
	}
	if errors.Is(err, jetstream.ErrConsumerNotFound) {
		return false
	}
	t.Logf("verifyConsumerExists: unexpected error: %v", err)

	return false
}

// TestVerify_InactiveThresholdSemantics runs six targeted sub-tests.
func TestVerify_InactiveThresholdSemantics(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	// ---------------------------------------------------------------
	// Shared embedded NATS server for all sub-tests.
	// ---------------------------------------------------------------
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// makeConsumer creates a fresh stream+consumer with verifyInactiveThreshold.
	makeConsumer := func(t *testing.T, streamName, consName string) jetstream.Consumer {
		t.Helper()
		_, err := js.CreateOrUpdateStream(context.Background(), jetstream.StreamConfig{
			Name:     streamName,
			Subjects: []string{streamName + ".>"},
		})
		require.NoError(t, err)

		cons, err := js.CreateOrUpdateConsumer(context.Background(), streamName, jetstream.ConsumerConfig{
			Durable:           consName,
			AckPolicy:         jetstream.AckExplicitPolicy,
			InactiveThreshold: verifyInactiveThreshold,
		})
		require.NoError(t, err)

		return cons
	}

	// ---------------------------------------------------------------
	// Case 1: Running Messages() iterator — never call Next().
	// Hypothesis: background goroutine re-issues pulls every PullExpiry,
	// keeping the consumer active indefinitely.
	// ---------------------------------------------------------------
	t.Run("RunningIteratorNeverDrained_PreventsExpiry", func(t *testing.T) {
		cons := makeConsumer(t, "VERIFYS1", "verify-c1")

		// PullExpiry=1s (minimum allowed). InactiveThreshold=3s.
		// Background goroutine must re-issue pulls every ~1s.
		// If it does, the consumer stays alive past 6s (verifyExpiryWait).
		iter, err := cons.Messages(
			jetstream.PullMaxMessages(1),
			jetstream.PullExpiry(verifyPullExpiry),
		)
		require.NoError(t, err)
		defer iter.Stop()

		t.Logf("Iterator started (PullExpiry=%v, InactiveThreshold=%v). "+
			"Sleeping %v WITHOUT ever calling Next()...",
			verifyPullExpiry, verifyInactiveThreshold, verifyExpiryWait)
		time.Sleep(verifyExpiryWait)

		alive := verifyConsumerExists(t, js, "VERIFYS1", "verify-c1")
		t.Logf("RESULT RunningIteratorNeverDrained: consumer alive after %v = %v", verifyExpiryWait, alive)
		require.False(t, alive, "consumer must expire when Messages() is created but Next() is never called")
	})

	// ---------------------------------------------------------------
	// Case 2: Stopped iterator immediately.
	// Hypothesis: iter.Stop() halts all pull requests → consumer expires.
	// ---------------------------------------------------------------
	t.Run("StoppedIterator_CausesExpiry", func(t *testing.T) {
		cons := makeConsumer(t, "VERIFYS2", "verify-c2")

		iter, err := cons.Messages(
			jetstream.PullMaxMessages(1),
			jetstream.PullExpiry(verifyPullExpiry),
		)
		require.NoError(t, err)
		iter.Stop() // immediate stop

		t.Logf("Iterator stopped immediately. Sleeping %v...", verifyExpiryWait)
		time.Sleep(verifyExpiryWait)

		alive := verifyConsumerExists(t, js, "VERIFYS2", "verify-c2")
		t.Logf("RESULT StoppedIterator: consumer alive after %v = %v", verifyExpiryWait, alive)
		require.False(t, alive, "consumer must expire after iter.Stop()")
	})

	// ---------------------------------------------------------------
	// Case 3: No iterator ever created (baseline).
	// ---------------------------------------------------------------
	t.Run("NoIterator_BaselineExpiry", func(t *testing.T) {
		makeConsumer(t, "VERIFYS3", "verify-c3")

		t.Logf("No iterator created. Sleeping %v...", verifyExpiryWait)
		time.Sleep(verifyExpiryWait)

		alive := verifyConsumerExists(t, js, "VERIFYS3", "verify-c3")
		t.Logf("RESULT NoIterator: consumer alive after %v = %v", verifyExpiryWait, alive)
		require.False(t, alive, "consumer must expire when no iterator is created")
	})

	// ---------------------------------------------------------------
	// Case 4: Slow handler gap — call Next() once, then sleep >> threshold.
	// Hypothesis: background goroutine keeps issuing pulls even while
	// the caller is "processing" (not calling Next() again).
	// ---------------------------------------------------------------
	t.Run("SlowHandler_GapBetweenNextCalls", func(t *testing.T) {
		cons := makeConsumer(t, "VERIFYS4", "verify-c4")

		_, err := js.Publish(context.Background(), "VERIFYS4.evt", []byte("hello"))
		require.NoError(t, err)

		iter, err := cons.Messages(
			jetstream.PullMaxMessages(1),
			jetstream.PullExpiry(verifyPullExpiry),
		)
		require.NoError(t, err)
		defer iter.Stop()

		msg, err := iter.Next()
		require.NoError(t, err)
		require.NoError(t, msg.Ack())

		t.Logf("Got+acked one message. Simulating slow handler: sleeping %v (>> InactiveThreshold=%v) without calling Next() again...",
			verifyExpiryWait, verifyInactiveThreshold)
		time.Sleep(verifyExpiryWait)

		alive := verifyConsumerExists(t, js, "VERIFYS4", "verify-c4")
		t.Logf("RESULT SlowHandler: consumer alive after %v slow-handler gap = %v", verifyExpiryWait, alive)
		require.False(t, alive, "consumer must expire when handler blocks longer than InactiveThreshold")
	})

	// ---------------------------------------------------------------
	// Case 5: Explicit delete with ACTIVE pending pull in flight.
	// Q: What error does iter.Next() return, and how fast?
	// Hypothesis (from plan doc): immediate ErrConsumerDeleted via 409.
	// ---------------------------------------------------------------
	t.Run("ExplicitDelete_DuringActivePull_DetectionLatency", func(t *testing.T) {
		cons := makeConsumer(t, "VERIFYS5", "verify-c5")

		// Short heartbeat so we detect deletion quickly if heartbeat-based.
		// PullHeartbeat defaults to min(PullExpiry/2, 30s) = 500ms.
		// After 2 missed heartbeats (2*500ms=1s) nats-go returns ErrNoHeartbeat.
		iter, err := cons.Messages(
			jetstream.PullMaxMessages(1),
			jetstream.PullExpiry(verifyPullExpiry),
			jetstream.PullHeartbeat(500*time.Millisecond),
		)
		require.NoError(t, err)
		defer iter.Stop()

		// Wait for the pull request to reach the server.
		time.Sleep(100 * time.Millisecond)

		type iterResult struct {
			err     error
			elapsed time.Duration
		}
		resultCh := make(chan iterResult, 1)
		go func() {
			start := time.Now()
			_, nextErr := iter.Next()
			resultCh <- iterResult{err: nextErr, elapsed: time.Since(start)}
		}()

		// Give the goroutine a moment to block on Next().
		time.Sleep(50 * time.Millisecond)

		deleteAt := time.Now()
		require.NoError(t, js.DeleteConsumer(context.Background(), "VERIFYS5", "verify-c5"))
		t.Logf("Consumer deleted at T+%v from iterator start.", time.Since(deleteAt))

		select {
		case r := <-resultCh:
			isDeleted := errors.Is(r.err, jetstream.ErrConsumerDeleted)
			isNoHB := errors.Is(r.err, jetstream.ErrNoHeartbeat)
			isClosed := errors.Is(r.err, jetstream.ErrMsgIteratorClosed)
			t.Logf("RESULT ExplicitDelete(DuringActivePull): iter.Next() returned %v after delete | "+
				"latency=%v | ErrConsumerDeleted=%v | ErrNoHeartbeat=%v | ErrMsgIteratorClosed=%v",
				r.err, r.elapsed.Round(time.Millisecond), isDeleted, isNoHB, isClosed)
			require.True(t, isDeleted, "active-pull deletion must surface as ErrConsumerDeleted, got %v", r.err)
		case <-time.After(10 * time.Second):
			t.Error("TIMEOUT: iter.Next() did not return within 10s.")
		}
	})

	// ---------------------------------------------------------------
	// Case 6: Explicit delete BETWEEN pulls (no active pull at delete time).
	// Q: Does the next pull after deletion return ErrConsumerDeleted?
	// ---------------------------------------------------------------
	t.Run("ExplicitDelete_BetweenPulls_DetectionLatency", func(t *testing.T) {
		cons := makeConsumer(t, "VERIFYS6", "verify-c6")

		// Publish enough messages to drain the first pull batch immediately
		// — this forces the iterator into "idle" right after and creates a
		// moment where the active pull has just completed and a new one
		// hasn't been issued yet.
		for range 5 {
			_, err := js.Publish(context.Background(), "VERIFYS6.evt", []byte("msg"))
			require.NoError(t, err)
		}

		iter, err := cons.Messages(
			jetstream.PullMaxMessages(1),
			jetstream.PullExpiry(verifyPullExpiry),
			jetstream.PullHeartbeat(500*time.Millisecond),
		)
		require.NoError(t, err)
		defer iter.Stop()

		// Drain all 5 messages; after acking the last one, the iterator
		// has no active pull but an idle one should be re-issued quickly.
		for range 5 {
			msg, err := iter.Next()
			require.NoError(t, err)
			_ = msg.Ack()
		}

		// Delete the consumer now: server should have a fresh pending
		// pull for the next (non-existent) message.
		deleteAt := time.Now()
		require.NoError(t, js.DeleteConsumer(context.Background(), "VERIFYS6", "verify-c6"))
		t.Logf("Consumer deleted after draining 5 messages, T+%v from iterator creation.", time.Since(deleteAt))

		type iterResult struct {
			err     error
			elapsed time.Duration
		}
		resultCh := make(chan iterResult, 1)
		go func() {
			start := time.Now()
			_, nextErr := iter.Next()
			resultCh <- iterResult{err: nextErr, elapsed: time.Since(start)}
		}()

		select {
		case r := <-resultCh:
			isDeleted := errors.Is(r.err, jetstream.ErrConsumerDeleted)
			isNoHB := errors.Is(r.err, jetstream.ErrNoHeartbeat)
			isClosed := errors.Is(r.err, jetstream.ErrMsgIteratorClosed)
			t.Logf("RESULT ExplicitDelete(BetweenPulls): iter.Next() returned %v after delete | "+
				"latency=%v | ErrConsumerDeleted=%v | ErrNoHeartbeat=%v | ErrMsgIteratorClosed=%v",
				r.err, r.elapsed.Round(time.Millisecond), isDeleted, isNoHB, isClosed)
			require.True(t, isNoHB, "between-pulls deletion must surface as ErrNoHeartbeat, got %v", r.err)
		case <-time.After(10 * time.Second):
			t.Error("TIMEOUT: iter.Next() did not return within 10s post-delete.")
		}
	})

	// ---------------------------------------------------------------
	// Case 7: ErrNoHeartbeat → ErrConsumerDeleted two-step sequence.
	//
	// Real recovery code pattern:
	//   1. iter.Next() returns ErrNoHeartbeat (heartbeat-based detection).
	//   2. Recovery code: iter.Stop(), try to re-create iterator.
	//   3. New iter.Next() — what error surfaces from the server?
	//
	// Q: After ErrNoHeartbeat, does a subsequent Next() call on the
	//    SAME iterator return ErrMsgIteratorClosed (self-stopped)?
	//    And does a brand-new iter.Next() on a stale consumer handle
	//    return ErrConsumerDeleted or ErrConsumerNotFound?
	// ---------------------------------------------------------------
	t.Run("NoHeartbeat_Then_NewIter_ErrorSequence", func(t *testing.T) {
		cons := makeConsumer(t, "VERIFYS7", "verify-c7")

		// Publish messages so we can drain them and reach the "between-pulls" state.
		for range 3 {
			_, err := js.Publish(context.Background(), "VERIFYS7.evt", []byte("msg"))
			require.NoError(t, err)
		}

		iter, err := cons.Messages(
			jetstream.PullMaxMessages(1),
			jetstream.PullExpiry(verifyPullExpiry),
			jetstream.PullHeartbeat(500*time.Millisecond),
		)
		require.NoError(t, err)

		// Drain messages.
		for range 3 {
			msg, err := iter.Next()
			require.NoError(t, err)
			_ = msg.Ack()
		}

		// Delete consumer while in between-pulls state.
		require.NoError(t, js.DeleteConsumer(context.Background(), "VERIFYS7", "verify-c7"))
		t.Log("Consumer deleted between pulls (between-pulls scenario).")

		// Step 1: First Next() should return ErrNoHeartbeat (heartbeat path).
		type iterResult struct {
			err     error
			elapsed time.Duration
		}
		step1Ch := make(chan iterResult, 1)
		go func() {
			start := time.Now()
			_, nextErr := iter.Next()
			step1Ch <- iterResult{err: nextErr, elapsed: time.Since(start)}
		}()

		var step1 iterResult
		select {
		case step1 = <-step1Ch:
		case <-time.After(10 * time.Second):
			t.Fatal("TIMEOUT waiting for step1 iter.Next().")
		}
		isNoHB1 := errors.Is(step1.err, jetstream.ErrNoHeartbeat)
		isDeleted1 := errors.Is(step1.err, jetstream.ErrConsumerDeleted)
		isClosed1 := errors.Is(step1.err, jetstream.ErrMsgIteratorClosed)
		t.Logf("Step1 (same iter, first Next() after delete): err=%v | latency=%v | "+
			"ErrNoHeartbeat=%v | ErrConsumerDeleted=%v | ErrMsgIteratorClosed=%v",
			step1.err, step1.elapsed.Round(time.Millisecond), isNoHB1, isDeleted1, isClosed1)
		require.True(t, isNoHB1, "first Next() after between-pulls delete must return ErrNoHeartbeat, got %v", step1.err)

		// Step 2: Call Next() AGAIN on the same (now errored) iterator.
		// Expect ErrMsgIteratorClosed because the iterator self-stopped after the error.
		step2Ch := make(chan iterResult, 1)
		go func() {
			start := time.Now()
			_, nextErr := iter.Next()
			step2Ch <- iterResult{err: nextErr, elapsed: time.Since(start)}
		}()

		var step2 iterResult
		select {
		case step2 = <-step2Ch:
		case <-time.After(3 * time.Second):
			t.Fatal("second Next() on stale iterator blocked after ErrNoHeartbeat")
		}
		isNoHB2 := errors.Is(step2.err, jetstream.ErrNoHeartbeat)
		isDeleted2 := errors.Is(step2.err, jetstream.ErrConsumerDeleted)
		isClosed2 := errors.Is(step2.err, jetstream.ErrMsgIteratorClosed)
		t.Logf("Step2 (same iter, second Next()): err=%v | latency=%v | "+
			"ErrNoHeartbeat=%v | ErrConsumerDeleted=%v | ErrMsgIteratorClosed=%v",
			step2.err, step2.elapsed.Round(time.Millisecond), isNoHB2, isDeleted2, isClosed2)
		require.True(t, isNoHB2, "stale iterator must continue returning ErrNoHeartbeat, got %v", step2.err)

		// Step 3: Stop old iterator, create a brand-new one on the stale consumer handle.
		// The consumer handle (cons) still has the deleted consumer's name.
		iter.Stop()
		iter2, err2 := cons.Messages(
			jetstream.PullMaxMessages(1),
			jetstream.PullExpiry(verifyPullExpiry),
			jetstream.PullHeartbeat(500*time.Millisecond),
		)
		require.NoError(t, err2, "creating a new iterator on a stale consumer handle should succeed")
		defer iter2.Stop()
		step3Ch := make(chan iterResult, 1)
		go func() {
			start := time.Now()
			_, nextErr := iter2.Next()
			step3Ch <- iterResult{err: nextErr, elapsed: time.Since(start)}
		}()

		var step3 iterResult
		select {
		case step3 = <-step3Ch:
		case <-time.After(10 * time.Second):
			t.Fatal("TIMEOUT waiting for step3 iter2.Next().")
		}
		isNoHB3 := errors.Is(step3.err, jetstream.ErrNoHeartbeat)
		isDeleted3 := errors.Is(step3.err, jetstream.ErrConsumerDeleted)
		isClosed3 := errors.Is(step3.err, jetstream.ErrMsgIteratorClosed)
		isNotFound3 := errors.Is(step3.err, jetstream.ErrConsumerNotFound)
		t.Logf("Step3 (new iter on stale handle, first Next()): err=%v | latency=%v | "+
			"ErrNoHeartbeat=%v | ErrConsumerDeleted=%v | ErrMsgIteratorClosed=%v | ErrConsumerNotFound=%v",
			step3.err, step3.elapsed.Round(time.Millisecond),
			isNoHB3, isDeleted3, isClosed3, isNotFound3)
		require.True(t, isNoHB3, "new iterator on stale handle must return ErrNoHeartbeat, got %v", step3.err)
	})

	// ---------------------------------------------------------------
	// Case 8: After ErrNoHeartbeat (between-pulls deletion), confirm
	//         deletion via consumer.Info() API call.
	//
	// Key question: is consumer.Info() the ONLY reliable confirmation
	// path when the deletion signal is ErrNoHeartbeat?
	// ---------------------------------------------------------------
	t.Run("NoHeartbeat_ConfirmViaConsumerInfo", func(t *testing.T) {
		cons := makeConsumer(t, "VERIFYS8", "verify-c8")

		// Publish and drain so we reach between-pulls state.
		_, err := js.Publish(context.Background(), "VERIFYS8.evt", []byte("msg"))
		require.NoError(t, err)

		iter, err := cons.Messages(
			jetstream.PullMaxMessages(1),
			jetstream.PullExpiry(verifyPullExpiry),
			jetstream.PullHeartbeat(500*time.Millisecond),
		)
		require.NoError(t, err)
		defer iter.Stop()

		msg, err := iter.Next()
		require.NoError(t, err)
		_ = msg.Ack()

		// Delete while between pulls.
		require.NoError(t, js.DeleteConsumer(context.Background(), "VERIFYS8", "verify-c8"))

		// Get ErrNoHeartbeat from iter.Next().
		step1Ch := make(chan error, 1)
		go func() {
			_, err := iter.Next()
			step1Ch <- err
		}()
		var noHBErr error
		select {
		case noHBErr = <-step1Ch:
		case <-time.After(5 * time.Second):
			t.Fatal("TIMEOUT waiting for ErrNoHeartbeat.")
		}
		t.Logf("Got from iter.Next() after delete: %v (ErrNoHeartbeat=%v)", noHBErr, errors.Is(noHBErr, jetstream.ErrNoHeartbeat))
		require.ErrorIs(t, noHBErr, jetstream.ErrNoHeartbeat)

		// Now confirm via consumer.Info().
		infoStart := time.Now()
		_, infoErr := cons.Info(context.Background())
		infoLatency := time.Since(infoStart)

		isConsumerNotFound := errors.Is(infoErr, jetstream.ErrConsumerNotFound)
		t.Logf("consumer.Info() after ErrNoHeartbeat: err=%v | latency=%v | ErrConsumerNotFound=%v",
			infoErr, infoLatency.Round(time.Millisecond), isConsumerNotFound)
		require.True(t, isConsumerNotFound, "consumer.Info() after ErrNoHeartbeat must confirm ErrConsumerNotFound, got %v", infoErr)
	})
}
