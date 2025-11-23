package types

import "testing"

func TestStateString(t *testing.T) {
	tests := []struct {
		state State
		want  string
	}{
		{StateInit, "Init"},
		{StateClaimingID, "ClaimingID"},
		{StateElection, "Election"},
		{StateWaitingAssignment, "WaitingAssignment"},
		{StateStable, "Stable"},
		{StateScaling, "Scaling"},
		{StateRebalancing, "Rebalancing"},
		{StateEmergency, "Emergency"},
		{StateDegraded, "Degraded"},
		{StateShutdown, "Shutdown"},
		{State(999), "Unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.state.String(); got != tt.want {
				t.Errorf("State.String() = %v, want %v", got, tt.want)
			}
		})
	}
}
