package recovery

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestClassifyError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want ErrorClass
	}{
		{"nil", nil, ErrorGracefulExit},
		{"ErrMsgIteratorClosed", jetstream.ErrMsgIteratorClosed, ErrorGracefulExit},
		{"context.Canceled", context.Canceled, ErrorGracefulExit},
		{"ErrConsumerDeleted", jetstream.ErrConsumerDeleted, ErrorConsumerGone},
		{"wrapped ErrConsumerDeleted", fmt.Errorf("wrap: %w", jetstream.ErrConsumerDeleted), ErrorConsumerGone},
		{"ErrNoHeartbeat", jetstream.ErrNoHeartbeat, ErrorNeedsConfirm},
		{"wrapped ErrNoHeartbeat", fmt.Errorf("wrap: %w", jetstream.ErrNoHeartbeat), ErrorNeedsConfirm},
		{"random error", errors.New("something"), ErrorTransient},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, ClassifyError(tt.err))
		})
	}
}

// --- merged from classify_streammissing_test.go ---

// streamGoneInfo simulates consumer.Info() against a DELETED STREAM: nats.go
// surfaces jetstream.ErrStreamNotFound (server error 10059), not
// ErrConsumerNotFound. This is the case the T4 integration test's
// MaxAttempts=1 workaround documents (test/integration/failure/
// stream_missing_no_hook_test.go): the whole stream is gone, so the
// consumer-scoped probe answers with the stream-scoped error.
func streamGoneInfo(_ context.Context) (*jetstream.ConsumerInfo, error) {
	return nil, jetstream.ErrStreamNotFound
}

// TestClassify_NoHeartbeat_StreamDeleted_RoutesStreamMissing pins the
// stream-deleted half of the ErrNoHeartbeat confirmation: when the burst
// threshold is reached and the Info() probe surfaces stream-not-found, the
// condition is PERMANENT (the stream is gone), so Classify must route it to
// ActionStreamMissing — the bounded detour that ends in the operator hook or
// OnPermanentFailure exhaustion.
//
// Pre-fix, confirmConsumerGone answered only IsConsumerNotFound, so
// stream-not-found returned false and the branch fell through to
// ActionBackoff: a permanent condition classified as transient-forever. The
// outer consumption loop's backoff path is unbounded by design, so with
// recovery enabled a deleted stream could leave the partition consumer
// ping-ponging between ErrNoHeartbeat and backoff indefinitely — stream-
// missing exhaustion never fires, the manager observer never sees it, and
// the worker reports Stable with a stalled consumer (the silent-stall class
// the terminal Degraded hold exists to prevent, defeated one layer up).
func TestClassify_NoHeartbeat_StreamDeleted_RoutesStreamMissing(t *testing.T) {
	c := NewController(ControllerConfig{
		Strategy:       FromLastProcessed,
		BurstThreshold: 2,
		BurstWindow:    10 * time.Second,
		Logger:         nopLog,
	})

	// 1st: below threshold — backoff without probing.
	action, _, _ := c.Classify(context.Background(), jetstream.ErrNoHeartbeat, streamGoneInfo, baseCfg, alwaysSucceedRecreate)
	require.Equal(t, ActionBackoff, action)

	// 2nd: threshold reached; the Info() probe surfaces stream-not-found.
	action, newCons, classifyErr := c.Classify(context.Background(), jetstream.ErrNoHeartbeat, streamGoneInfo, baseCfg, alwaysSucceedRecreate)
	require.Equal(t, ActionStreamMissing, action,
		"a stream-not-found Info probe after a heartbeat burst is a permanent condition and must route to the bounded stream-missing detour, not unbounded backoff")
	require.Nil(t, newCons)
	require.ErrorIs(t, classifyErr, types.ErrStreamMissing,
		"the companion error must wrap types.ErrStreamMissing so the detour and OnPermanentFailure chain can route on the sentinel")
}
