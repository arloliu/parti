package jsutil_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/arloliu/parti/v2/jsutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeJS embeds jetstream.JetStream (nil) and overrides only CreateOrUpdateConsumer.
type fakeJS struct {
	jetstream.JetStream
	outcomes []error
	idx      int32
}

func newFakeJS(outcomes ...error) *fakeJS {
	return &fakeJS{outcomes: outcomes}
}

func (f *fakeJS) CreateOrUpdateConsumer(_ context.Context, _ string, _ jetstream.ConsumerConfig) (jetstream.Consumer, error) {
	i := int(atomic.AddInt32(&f.idx, 1)) - 1
	if i < len(f.outcomes) {
		if err := f.outcomes[i]; err != nil {
			return nil, err
		}
	}
	return &fakeConsumer{}, nil
}

type fakeConsumer struct{ jetstream.Consumer }

// --- Test: EnsureConsumer (thin wrapper) signature preserved ---

func TestEnsureConsumer_SignaturePreserved(t *testing.T) {
	js := newFakeJS(nil)
	cons, err := jsutil.EnsureConsumer(t.Context(), js, "s", jetstream.ConsumerConfig{})
	require.NoError(t, err)
	require.NotNil(t, cons)
}

// --- Test: EnsureConsumerWithOptions without options equals EnsureConsumer ---

func TestEnsureConsumerWithOptions_NoOptions(t *testing.T) {
	js := newFakeJS(nil)
	cons, err := jsutil.EnsureConsumerWithOptions(t.Context(), js, "s", jetstream.ConsumerConfig{})
	require.NoError(t, err)
	require.NotNil(t, cons)
}

// --- Test: beforeAttempt called once per physical RPC attempt ---
// Two transient errors then success = 3 RPC attempts; beforeAttempt called 3×.

func TestEnsureConsumerWithOptions_BeforeAttemptCalledPerAttempt(t *testing.T) {
	transient := errors.New("transient error")
	js := newFakeJS(transient, transient, nil) // fail, fail, succeed

	var hookCalls atomic.Int32
	hook := func(_ context.Context) error {
		hookCalls.Add(1)
		return nil
	}

	cons, err := jsutil.EnsureConsumerWithOptions(
		t.Context(), js, "s", jetstream.ConsumerConfig{},
		jsutil.WithBeforeAttempt(hook),
	)
	require.NoError(t, err)
	require.NotNil(t, cons)

	// Key: hook must be called before EVERY physical attempt, including retries.
	assert.Equal(t, int32(3), hookCalls.Load(), "beforeAttempt must be called once per physical RPC attempt")
}

// --- Test: beforeAttempt error aborts and is returned ---

func TestEnsureConsumerWithOptions_BeforeAttemptErrorAborts(t *testing.T) {
	hookErr := errors.New("hook-abort")
	js := newFakeJS(nil) // JS would succeed but hook aborts first

	hook := func(_ context.Context) error {
		return hookErr
	}

	_, err := jsutil.EnsureConsumerWithOptions(
		t.Context(), js, "s", jetstream.ConsumerConfig{},
		jsutil.WithBeforeAttempt(hook),
	)
	require.Error(t, err)
	require.ErrorIs(t, err, hookErr, "beforeAttempt error must propagate")
	// JS must NOT have been called (hook fires before RPC).
	assert.Equal(t, int32(0), js.idx, "no RPC should be issued when beforeAttempt errors")
}

// --- Test: ctx cancellation from beforeAttempt propagates ---

func TestEnsureConsumerWithOptions_CtxCancelFromHook(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	callCount := 0

	hook := func(ctx context.Context) error {
		callCount++
		if callCount == 2 {
			cancel()
			return ctx.Err()
		}
		return nil
	}

	// First attempt fails (transient), second attempt's hook cancels.
	transient := errors.New("transient")
	js := newFakeJS(transient, nil)

	_, err := jsutil.EnsureConsumerWithOptions(ctx, js, "s", jetstream.ConsumerConfig{},
		jsutil.WithBeforeAttempt(hook),
	)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}
