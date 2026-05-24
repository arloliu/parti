package natsutil

import (
	"errors"
	"fmt"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestIsConsumerGone(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"ErrConsumerDeleted", jetstream.ErrConsumerDeleted, true},
		{"wrapped ErrConsumerDeleted", fmt.Errorf("wrap: %w", jetstream.ErrConsumerDeleted), true},
		{"ErrNoHeartbeat", jetstream.ErrNoHeartbeat, false},
		{"ErrMsgIteratorClosed", jetstream.ErrMsgIteratorClosed, false},
		{"random error", errors.New("something"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, IsConsumerGone(tt.err))
		})
	}
}

func TestIsConsumerNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"nats.ErrConsumerNotFound", nats.ErrConsumerNotFound, true},
		{"jetstream.ErrConsumerNotFound", jetstream.ErrConsumerNotFound, true},
		{"wrapped nats", fmt.Errorf("wrap: %w", nats.ErrConsumerNotFound), true},
		{"wrapped jetstream", fmt.Errorf("wrap: %w", jetstream.ErrConsumerNotFound), true},
		{"random error", errors.New("something"), false},
		{"ErrConsumerDeleted", jetstream.ErrConsumerDeleted, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, IsConsumerNotFound(tt.err))
		})
	}
}

func TestIsBenignWatcherStopErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, true},
		{"nats.ErrBadSubscription", nats.ErrBadSubscription, true},
		{"wrapped ErrBadSubscription", fmt.Errorf("wrap: %w", nats.ErrBadSubscription), true},
		{"nats.ErrConsumerNotFound", nats.ErrConsumerNotFound, true},
		{"jetstream.ErrConsumerNotFound", jetstream.ErrConsumerNotFound, true},
		{"wrapped jetstream consumer not found", fmt.Errorf("wrap: %w", jetstream.ErrConsumerNotFound), true},
		{"random error", errors.New("something"), false},
		{"stream not found is NOT benign", jetstream.ErrStreamNotFound, false},
		{"timeout is NOT benign", nats.ErrTimeout, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, IsBenignWatcherStopErr(tt.err))
		})
	}
}

func TestIsStreamNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"nats.ErrStreamNotFound", nats.ErrStreamNotFound, true},
		{"jetstream.ErrStreamNotFound", jetstream.ErrStreamNotFound, true},
		{"wrapped nats", fmt.Errorf("wrap: %w", nats.ErrStreamNotFound), true},
		{"random error", errors.New("something"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, IsStreamNotFound(tt.err))
		})
	}
}

func TestIsDegradingJetStreamError(t *testing.T) {
	// Simulates the double-%w wrapping used by election.RenewLeadership
	// (fmt.Errorf("%w: %w", ErrLeadershipLost, err)) to confirm errors.Is
	// traversal still succeeds after that wrapping.
	sentinelLeadershipLost := errors.New("leadership was lost")

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"bucket not found", jetstream.ErrBucketNotFound, true},
		{"nats stream not found", nats.ErrStreamNotFound, true},
		{"jetstream stream not found", jetstream.ErrStreamNotFound, true},
		{"nats consumer not found", nats.ErrConsumerNotFound, true},
		{"jetstream consumer not found", jetstream.ErrConsumerNotFound, true},
		{"wrapped bucket not found", fmt.Errorf("wrap: %w", jetstream.ErrBucketNotFound), true},
		{"double-wrapped stream not found", fmt.Errorf("%w: %w", sentinelLeadershipLost, jetstream.ErrStreamNotFound), true},
		{"timeout is not degrading", nats.ErrTimeout, false},
		{"no servers is not degrading", nats.ErrNoServers, false},
		{"random error", errors.New("something"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, IsDegradingJetStreamError(tt.err))
		})
	}
}

func TestIsConnectivityError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"nats timeout", nats.ErrTimeout, true},
		{"no servers", nats.ErrNoServers, true},
		{"disconnected", nats.ErrDisconnected, true},
		{"connection closed", nats.ErrConnectionClosed, true},
		{"connection draining", nats.ErrConnectionDraining, true},
		{"connection reconnecting", nats.ErrConnectionReconnecting, true},
		{"no stream response", jetstream.ErrNoStreamResponse, true},
		{"connection refused string", errors.New("dial tcp: connection refused"), true},
		{"i/o timeout string", errors.New("read tcp: i/o timeout"), true},
		{"random error", errors.New("something else"), false},
		{"wrapped timeout", fmt.Errorf("wrap: %w", nats.ErrTimeout), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, IsConnectivityError(tt.err))
		})
	}
}
