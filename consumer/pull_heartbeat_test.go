package consumer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	partitesting "github.com/arloliu/parti/v2/partitest"
)

// TestValidatePullHeartbeatCap pins the accept/reject boundary shared by every
// consumer type's Validate path: 0 (disabled) is always accepted; a nonzero
// value is accepted only within nats.go's PullHeartbeat range [500ms, 30s].
func TestValidatePullHeartbeatCap(t *testing.T) {
	tests := []struct {
		name         string
		heartbeatCap time.Duration
		wantErr      bool
	}{
		{"zero disables", 0, false},
		{"499ms below floor", 499 * time.Millisecond, true},
		{"500ms at floor", 500 * time.Millisecond, false},
		{"2s mid-range", 2 * time.Second, false},
		{"30s at ceiling", 30 * time.Second, false},
		{"31s above ceiling", 31 * time.Second, true},
		{"negative", -time.Second, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePullHeartbeatCap(tt.heartbeatCap)
			if tt.wantErr {
				require.Error(t, err)
				require.ErrorIs(t, err, ErrInvalidConfig)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestQueue_IteratorFactoryInjection_TakesPrecedenceOverPullHeartbeatCap pins
// that a caller-supplied IteratorFactory still wins over the
// PullHeartbeatCap-derived default at the Queue seam — the same precedence
// rule proven at the internal/durable seam.
func TestQueue_IteratorFactoryInjection_TakesPrecedenceOverPullHeartbeatCap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	sentinel := errors.New("sentinel: injected factory called")
	injected := func(_ jetstream.Consumer, _ int, _ time.Duration) (jetstream.MessagesContext, error) {
		return nil, sentinel
	}

	handler := MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	q, err := NewQueue(js, "S", "c", "s.>", handler,
		WithPullHeartbeatCap(2*time.Second),
		WithIteratorFactory(injected),
	)
	require.NoError(t, err)

	_, err = q.iterFactory(nil, 1, 30*time.Second)
	require.ErrorIs(t, err, sentinel, "custom IteratorFactory must take precedence over the PullHeartbeatCap-derived default")
}
