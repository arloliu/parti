package types

// StateProvider allows components to check manager state without circular dependencies.
//
// This interface enables the Calculator to check for degraded mode and recovery
// grace period without requiring a direct dependency on the Manager type.
type StateProvider interface {
	// State returns the current manager state.
	State() State

	// IsInRecoveryGrace returns true if in post-recovery grace period.
	// This prevents split-brain scenarios during recovery.
	IsInRecoveryGrace() bool
}
