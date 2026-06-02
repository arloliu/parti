package parti

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

// TestClassifyKVError table-tests the pure KV-error router extracted from
// recordKVError, locking the precedence and the transient/whole-bucket split
// (the latter selects DegradeReasonKVUnavailable vs DegradeReasonKVErrorThreshold).
func TestClassifyKVError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		err       error
		route     kvErrorRoute
		transient bool
	}{
		{"nil drops", nil, kvRouteDrop, false},
		{"stream-missing is observer-owned", types.ErrStreamMissing, kvRouteStreamMissing, false},
		{"wrapped stream-missing is observer-owned", fmt.Errorf("recovery exhausted: %w", types.ErrStreamMissing), kvRouteStreamMissing, false},
		{"connectivity is whole-bucket window", nats.ErrTimeout, kvRouteWindow, false},
		{"degrading-jetstream is whole-bucket window", jetstream.ErrBucketNotFound, kvRouteWindow, false},
		{"kv-unavailable-marked timeout is transient window", errors.Join(ErrKVUnavailable, context.DeadlineExceeded), kvRouteWindow, true},
		{"unclassified non-timeout drops", errors.New("some unrelated failure"), kvRouteDrop, false},
		{"bare deadline (unmarked) drops", context.DeadlineExceeded, kvRouteDrop, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyKVError(tc.err)
			require.Equal(t, tc.route, got.route, "route for %v", tc.err)
			require.Equal(t, tc.transient, got.transient, "transient for %v", tc.err)
		})
	}
}
