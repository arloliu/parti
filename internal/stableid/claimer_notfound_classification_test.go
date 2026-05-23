package stableid

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestClaimer_renew_BucketDeleted_ClassifiesAsClaimLost is the primary
// F3 reproducer: end-to-end against a real embedded NATS, delete the
// stableID bucket under a claimed worker, and assert the next renew
// returns ErrClaimLost (not a generic "failed to renew ID" wrap).
//
// Empirical surface: kv.Update on a cached handle after
// js.DeleteKeyValue returns jetstream.ErrNoStreamResponse. See
// [[project-nats-kv-delete-surface]]. On parent commit the classifier
// only recognizes ErrKeyExists, so the wrapped ErrNoStreamResponse
// falls through to the generic branch and this test fails.
func TestClaimer_renew_BucketDeleted_ClassifiesAsClaimLost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	bucket := "unit-stableid-bucket-deleted"
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  bucket,
		Storage: jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	c := NewClaimer(kv, "worker", 0, 0, time.Second, nil)
	wid, err := c.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, "worker-0", wid)

	// Bucket vanishes under the worker.
	require.NoError(t, js.DeleteKeyValue(ctx, bucket))

	renewErr := c.renew(ctx)
	require.Error(t, renewErr)
	require.ErrorIs(t, renewErr, ErrClaimLost,
		"a vanished stableID bucket must surface as ErrClaimLost so claimLostShutdown can rotate the pod; got %v", renewErr)
}

// TestClaimer_renew_DefenseInDepth_NotFoundSentinels covers the
// defense-in-depth branches the plan named (ErrBucketNotFound /
// ErrStreamNotFound). Empirically kv.Update does not surface these
// from the production embedded server today, but a future nats.go
// version or a different transport may, and the classifier must
// stay correct.
func TestClaimer_renew_DefenseInDepth_NotFoundSentinels(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
	}{
		{"ErrBucketNotFound", jetstream.ErrBucketNotFound},
		{"ErrStreamNotFound", jetstream.ErrStreamNotFound},
		{"ErrNoStreamResponse", jetstream.ErrNoStreamResponse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kv := &mockKV{
				createCalls: []func(context.Context, string, []byte) (uint64, error){
					func(_ context.Context, _ string, _ []byte) (uint64, error) { return 7, nil },
				},
				updateCalls: []func(context.Context, string, []byte, uint64) (uint64, error){
					func(_ context.Context, _ string, _ []byte, _ uint64) (uint64, error) {
						return 0, tc.err
					},
				},
			}
			c := NewClaimer(kv, "worker", 0, 0, time.Second, nil)
			_, err := c.Claim(context.Background())
			require.NoError(t, err)

			renewErr := c.renew(context.Background())
			require.Error(t, renewErr)
			require.ErrorIs(t, renewErr, ErrClaimLost,
				"%s must classify as ErrClaimLost; got %v", tc.name, renewErr)
		})
	}
}

// TestClaimer_renew_ContextDeadlineNotClaimLost guards the negative
// side: a context-timeout error MUST NOT classify as ErrClaimLost.
// Timeouts retry on the next tick; mis-classifying them as claim-loss
// would rotate pods on every transient NATS slowdown.
func TestClaimer_renew_ContextDeadlineNotClaimLost(t *testing.T) {
	t.Parallel()

	kv := &mockKV{
		createCalls: []func(context.Context, string, []byte) (uint64, error){
			func(_ context.Context, _ string, _ []byte) (uint64, error) { return 7, nil },
		},
		updateCalls: []func(context.Context, string, []byte, uint64) (uint64, error){
			func(_ context.Context, _ string, _ []byte, _ uint64) (uint64, error) {
				return 0, context.DeadlineExceeded
			},
		},
	}
	c := NewClaimer(kv, "worker", 0, 0, time.Second, nil)
	_, err := c.Claim(context.Background())
	require.NoError(t, err)

	renewErr := c.renew(context.Background())
	require.Error(t, renewErr)
	require.NotErrorIs(t, renewErr, ErrClaimLost,
		"context.DeadlineExceeded MUST stay generic — timeouts retry, they do not trigger pod rotation")
	require.True(t, errors.Is(renewErr, context.DeadlineExceeded),
		"the wrapped error must preserve the original sentinel for operator diagnostics")
}
