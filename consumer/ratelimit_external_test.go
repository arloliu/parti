package consumer_test

import (
	"context"
	"testing"

	"github.com/arloliu/parti/v2/consumer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// externalLimiter is a user-supplied ConsumerCreateLimiter implemented WITHOUT
// importing any internal package — proving external code can satisfy the public
// interface (the whole point of exporting ConsumerCreateLimiter). The compile
// of this file in package consumer_test, importing only consumer/, is itself the
// proof that the limiter API is usable from outside the module.
type externalLimiter struct{}

func (externalLimiter) Wait(ctx context.Context) error { return ctx.Err() }

var _ consumer.ConsumerCreateLimiter = externalLimiter{}

func TestNewConsumerCreateLimiter_Validation(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		lim, err := consumer.NewConsumerCreateLimiter(100, 10)
		require.NoError(t, err)
		require.NotNil(t, lim)
	})
	t.Run("zero perSec rejected", func(t *testing.T) {
		_, err := consumer.NewConsumerCreateLimiter(0, 10)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "perSec")
	})
	t.Run("negative perSec rejected", func(t *testing.T) {
		_, err := consumer.NewConsumerCreateLimiter(-1, 10)
		require.Error(t, err)
	})
	t.Run("burst below one rejected", func(t *testing.T) {
		_, err := consumer.NewConsumerCreateLimiter(100, 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "burst")
	})
}

// TestNewConsumerCreateLimiter_WaitGrantsAndHonoursCtx proves the constructed
// limiter both grants the burst token immediately and honours ctx cancellation
// on a subsequent paced wait.
func TestNewConsumerCreateLimiter_WaitGrantsAndHonoursCtx(t *testing.T) {
	lim, err := consumer.NewConsumerCreateLimiter(1000, 1) // burst 1
	require.NoError(t, err)

	// First call draws the single burst token immediately.
	require.NoError(t, lim.Wait(context.Background()))

	// The bucket is now empty; the next token is ~1ms out. A cancelled ctx must
	// make Wait return promptly with the ctx error rather than block.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, lim.Wait(ctx), context.Canceled)
}

// TestWithConsumerCreateLimiter_AcceptsPublicAndCustom proves the option accepts
// both a constructed shared limiter and a user-implemented one, using only the
// public surface — the exact path an external caller takes to share one budget
// across multiple Dynamic consumers.
func TestWithConsumerCreateLimiter_AcceptsPublicAndCustom(t *testing.T) {
	shared, err := consumer.NewConsumerCreateLimiter(50, 5)
	require.NoError(t, err)

	// The option must accept a constructed shared limiter, a user-implemented
	// one, and nil — all referenced through the public surface alone.
	opts := []consumer.DynamicOption{
		consumer.WithConsumerCreateLimiter(shared),
		consumer.WithConsumerCreateLimiter(externalLimiter{}),
		consumer.WithConsumerCreateLimiter(nil),
	}
	require.Len(t, opts, 3)
}
