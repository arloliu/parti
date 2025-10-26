package types

// State represents the worker lifecycle state.
//
// States follow a defined progression during normal operation:
//
//	StateInit → StateClaimingID → StateElection → StateWaitingAssignment → StateStable
//
// During scaling or rebalancing:
//
//	StateStable → StateScaling/StateRebalancing → StateStable
//
// Emergency and shutdown are terminal states.
type State int

const (
	// StateInit is the initial state before any operations.
	StateInit State = iota

	// StateClaimingID indicates the worker is claiming a stable ID.
	StateClaimingID

	// StateElection indicates the worker is participating in leader election.
	StateElection

	// StateWaitingAssignment indicates waiting for initial partition assignment.
	StateWaitingAssignment

	// StateStable indicates normal operation with stable assignment.
	StateStable

	// StateScaling indicates dynamic scaling is in progress.
	StateScaling

	// StateRebalancing indicates partition rebalancing is in progress.
	StateRebalancing

	// StateEmergency indicates an error condition requiring intervention.
	StateEmergency

	// StateShutdown indicates graceful shutdown is in progress.
	StateShutdown
)

// String returns the string representation of the state.
func (s State) String() string {
	switch s {
	case StateInit:
		return "Init"
	case StateClaimingID:
		return "ClaimingID"
	case StateElection:
		return "Election"
	case StateWaitingAssignment:
		return "WaitingAssignment"
	case StateStable:
		return "Stable"
	case StateScaling:
		return "Scaling"
	case StateRebalancing:
		return "Rebalancing"
	case StateEmergency:
		return "Emergency"
	case StateShutdown:
		return "Shutdown"
	default:
		return "Unknown"
	}
}
