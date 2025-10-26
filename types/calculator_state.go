package types

// CalculatorState represents the state of the assignment calculator.
//
// The calculator transitions through these states during partition assignment:
//
//	Idle → Scaling → Rebalancing → Idle (normal scaling)
//	Idle → Emergency → Idle (worker crash)
//
// This provides type-safe state management for the assignment calculator,
// preventing string comparison errors in the Manager.
type CalculatorState int

const (
	// CalcStateIdle indicates the calculator is idle (stable operation).
	// No active rebalancing or scaling in progress.
	CalcStateIdle CalculatorState = iota

	// CalcStateScaling indicates the calculator is in a stabilization window.
	// Waiting for worker topology to stabilize before rebalancing.
	// Used during planned scaling (workers joining gradually).
	CalcStateScaling

	// CalcStateRebalancing indicates active rebalancing is in progress.
	// Assignments are being calculated and published to workers.
	CalcStateRebalancing

	// CalcStateEmergency indicates emergency rebalancing (worker crash).
	// No stabilization window - immediate rebalancing required.
	CalcStateEmergency
)

// String returns the string representation of calculator state.
//
// Returns:
//   - string: Human-readable state name
func (s CalculatorState) String() string {
	switch s {
	case CalcStateIdle:
		return "Idle"
	case CalcStateScaling:
		return "Scaling"
	case CalcStateRebalancing:
		return "Rebalancing"
	case CalcStateEmergency:
		return "Emergency"
	default:
		return "Unknown"
	}
}
