package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// WaitSoleConsumer waits until stream has a consumer and returns it.
// Use it when the consumer under test derives its durable name internally
// and is the only consumer on the stream.
func WaitSoleConsumer(t *testing.T, js jetstream.JetStream, stream string) jetstream.Consumer {
	t.Helper()
	var cons jetstream.Consumer
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		s, err := js.Stream(ctx, stream)
		if err != nil {
			return false
		}
		for name := range s.ConsumerNames(ctx).Name() {
			cons, err = s.Consumer(ctx, name)

			return err == nil
		}

		return false
	}, 5*time.Second, 20*time.Millisecond, "no consumer found on stream %s", stream)

	return cons
}

// WaitConsumerInfo polls cons.Info until pred accepts it and returns that info.
// Each fetch has a short deadline so a leaderless gap cannot eat the caller's timing window.
// Info without cluster details is skipped.
func WaitConsumerInfo(
	t *testing.T, cons jetstream.Consumer, timeout time.Duration, what string,
	pred func(*jetstream.ConsumerInfo) bool,
) *jetstream.ConsumerInfo {
	t.Helper()
	var last, got *jetstream.ConsumerInfo
	reached := func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
		defer cancel()
		info, err := cons.Info(ctx)
		if err != nil || info.Cluster == nil {
			return false
		}
		last = info
		if pred(info) {
			got = info
		}

		return got != nil
	}
	deadline := time.Now().Add(timeout)
	for !reached() {
		if time.Now().After(deadline) {
			waiting := -1
			if last != nil {
				waiting = last.NumWaiting
			}
			require.FailNowf(t, "consumer info condition not met",
				"%s: not reached within %v (last NumWaiting=%d)", what, timeout, waiting)
		}
		time.Sleep(20 * time.Millisecond)
	}

	return got
}

// ConsumerReplicasCurrent reports whether info lists followers replicas and all are caught up.
func ConsumerReplicasCurrent(info *jetstream.ConsumerInfo, followers int) bool {
	if len(info.Cluster.Replicas) != followers {
		return false
	}
	for _, replica := range info.Cluster.Replicas {
		if !replica.Current || replica.Lag != 0 {
			return false
		}
	}

	return true
}

// RequestConsumerLeaderStepdown asks the consumer's leader to step down and checks the reply.
// It does not wait for the new leader.
func RequestConsumerLeaderStepdown(t *testing.T, nc *nats.Conn, cons jetstream.Consumer) {
	t.Helper()
	cached := cons.CachedInfo()
	reply, err := nc.Request(
		fmt.Sprintf("$JS.API.CONSUMER.LEADER.STEPDOWN.%s.%s", cached.Stream, cached.Name), nil, 5*time.Second)
	require.NoError(t, err)
	var resp struct {
		Error   *jetstream.APIError `json:"error"`
		Success bool                `json:"success"`
	}
	require.NoError(t, json.Unmarshal(reply.Data, &resp))
	require.Nil(t, resp.Error, "stepdown reply: %s", reply.Data)
	require.True(t, resp.Success, "stepdown reply: %s", reply.Data)
}

// StepDownConsumerLeader asks the consumer's leader to step down,
// waits until a different server leads it, and returns the info that shows the new leader.
func StepDownConsumerLeader(t *testing.T, nc *nats.Conn, cons jetstream.Consumer, oldLeader string) *jetstream.ConsumerInfo {
	t.Helper()
	RequestConsumerLeaderStepdown(t, nc, cons)

	return WaitConsumerInfo(t, cons, 10*time.Second, "new consumer leader",
		func(info *jetstream.ConsumerInfo) bool {
			return info.Cluster.Leader != "" && info.Cluster.Leader != oldLeader
		})
}

// HeartbeatMissInjector injects one missed pull heartbeat into a real iterator.
// A real miss fires with an empty client buffer; this one fires after the server
// has delivered messages, to show what happens to them.
type HeartbeatMissInjector struct {
	claimed     atomic.Bool
	injected    atomic.Bool
	undelivered atomic.Bool
}

// Wrap returns iter with the miss injected on its second Next call,
// once cons reports wantDelivered messages delivered.
// Only one wrapper per injector ever injects, even if the loop under test builds a second iterator.
func (h *HeartbeatMissInjector) Wrap(
	iter jetstream.MessagesContext, cons jetstream.Consumer, wantDelivered uint64,
) jetstream.MessagesContext {
	return &missInjectIter{MessagesContext: iter, cons: cons, wantDelivered: wantDelivered, injector: h}
}

// Injected reports whether the miss has been returned.
func (h *HeartbeatMissInjector) Injected() bool { return h.injected.Load() }

// Undelivered reports whether the miss was injected although the server
// never reported wantDelivered messages delivered.
// Next may run off the test goroutine, so the test asserts on this itself.
func (h *HeartbeatMissInjector) Undelivered() bool { return h.undelivered.Load() }

type missInjectIter struct {
	jetstream.MessagesContext

	cons          jetstream.Consumer
	wantDelivered uint64
	calls         int
	injector      *HeartbeatMissInjector
}

func (w *missInjectIter) Next(opts ...jetstream.NextOpt) (jetstream.Msg, error) {
	w.calls++
	if w.calls != 2 || !w.injector.claimed.CompareAndSwap(false, true) {
		return w.MessagesContext.Next(opts...)
	}

	delivered := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		info, err := w.cons.Info(context.Background())
		if err == nil && info.Delivered.Consumer >= w.wantDelivered {
			delivered = true

			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	w.injector.undelivered.Store(!delivered)
	w.injector.injected.Store(true)

	return nil, jetstream.ErrNoHeartbeat
}
