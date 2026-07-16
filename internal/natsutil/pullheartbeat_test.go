package natsutil

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestDerivePullHeartbeat_Table pins the derivation formula across the full
// expiry x cap matrix: expiry in {1s, 5s, 30s, 60s, 61s, 90s} x cap in
// {0, 500ms, 2s, 5s, 30s}. The expiry=30s/cap=0 case (want=15s) is the
// documented pre-knob default heartbeat that WithPullHeartbeatCap leaves
// unchanged when unset.
//
// Expected values are hardcoded (not recomputed via the production formula)
// so this test can't pass by mirroring a bug in derivePullHeartbeat itself.
func TestDerivePullHeartbeat_Table(t *testing.T) {
	tests := []struct {
		expiry       time.Duration
		heartbeatCap time.Duration
		want         time.Duration
	}{
		{time.Second, 0, 500 * time.Millisecond},
		{time.Second, 500 * time.Millisecond, 500 * time.Millisecond},
		{time.Second, 2 * time.Second, 500 * time.Millisecond},
		{time.Second, 5 * time.Second, 500 * time.Millisecond},
		{time.Second, 30 * time.Second, 500 * time.Millisecond},

		{5 * time.Second, 0, 2500 * time.Millisecond},
		{5 * time.Second, 500 * time.Millisecond, 500 * time.Millisecond},
		{5 * time.Second, 2 * time.Second, 2 * time.Second},
		{5 * time.Second, 5 * time.Second, 2500 * time.Millisecond},
		{5 * time.Second, 30 * time.Second, 2500 * time.Millisecond},

		{30 * time.Second, 0, 15 * time.Second},
		{30 * time.Second, 500 * time.Millisecond, 500 * time.Millisecond},
		{30 * time.Second, 2 * time.Second, 2 * time.Second},
		{30 * time.Second, 5 * time.Second, 5 * time.Second},
		{30 * time.Second, 30 * time.Second, 15 * time.Second},

		{60 * time.Second, 0, 30 * time.Second},
		{60 * time.Second, 500 * time.Millisecond, 500 * time.Millisecond},
		{60 * time.Second, 2 * time.Second, 2 * time.Second},
		{60 * time.Second, 5 * time.Second, 5 * time.Second},
		{60 * time.Second, 30 * time.Second, 30 * time.Second},

		{61 * time.Second, 0, 30 * time.Second},
		{61 * time.Second, 500 * time.Millisecond, 500 * time.Millisecond},
		{61 * time.Second, 2 * time.Second, 2 * time.Second},
		{61 * time.Second, 5 * time.Second, 5 * time.Second},
		{61 * time.Second, 30 * time.Second, 30 * time.Second},

		{90 * time.Second, 0, 30 * time.Second},
		{90 * time.Second, 500 * time.Millisecond, 500 * time.Millisecond},
		{90 * time.Second, 2 * time.Second, 2 * time.Second},
		{90 * time.Second, 5 * time.Second, 5 * time.Second},
		{90 * time.Second, 30 * time.Second, 30 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.expiry.String()+"/cap="+tt.heartbeatCap.String(), func(t *testing.T) {
			got := DerivePullHeartbeat(tt.expiry, tt.heartbeatCap)
			require.Equal(t, tt.want, got)
			require.GreaterOrEqual(t, got, MinPullHeartbeat, "result must respect nats.go's PullHeartbeat floor")
			require.LessOrEqual(t, got, MaxPullHeartbeat, "result must respect nats.go's PullHeartbeat ceiling")
			if tt.expiry >= time.Second {
				require.LessOrEqual(t, got, tt.expiry/2,
					"result must never exceed expiry/2 for an in-range FetchTimeout (nats.go rejects Heartbeat > 50%% of Expires)")
			}
		})
	}
}
