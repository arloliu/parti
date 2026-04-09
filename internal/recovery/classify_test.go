package recovery

import (
	"context"
	"errors"
	"fmt"
	"testing"

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
