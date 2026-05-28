package recovery

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestClassifyConfirm(t *testing.T) {
	leaderful := &jetstream.ConsumerInfo{Cluster: &jetstream.ClusterInfo{Leader: "n0"}}
	leaderless := &jetstream.ConsumerInfo{Cluster: &jetstream.ClusterInfo{Leader: ""}}
	r1 := &jetstream.ConsumerInfo{Cluster: nil} // non-replicated consumer

	tests := []struct {
		name string
		ci   *jetstream.ConsumerInfo
		err  error
		want ConfirmResult
	}{
		{"healthy leaderful", leaderful, nil, ConfirmHealthy},
		{"healthy R1 nil-cluster", r1, nil, ConfirmHealthy},
		{"healthy nil-info", nil, nil, ConfirmHealthy},
		{"leaderless -> unservable", leaderless, nil, ConfirmUnservable},
		{"consumer not found -> gone", nil, jetstream.ErrConsumerNotFound, ConfirmGone},
		{"stream not found -> degrading", nil, jetstream.ErrStreamNotFound, ConfirmDegrading},
		{"stream missing umbrella -> degrading", nil, types.ErrStreamMissing, ConfirmDegrading},
		{"timeout -> connectivity", nil, nats.ErrTimeout, ConfirmConnectivity},
		{"no-stream-response -> connectivity", nil, jetstream.ErrNoStreamResponse, ConfirmConnectivity},
		{"no-responders -> unservable", nil, nats.ErrNoResponders, ConfirmUnservable},
		{"api 503 text -> unservable", nil, errors.New("nats: API error: code=503 err_code=10008 description=JetStream system temporarily unavailable"), ConfirmUnservable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, ClassifyConfirm(tt.ci, tt.err))
		})
	}
}

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
