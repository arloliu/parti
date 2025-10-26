package parti

import "github.com/arloliu/parti/types"

// Re-export types from the internal types package.
//
// This file provides a stable, backward-compatible public API for the library's
// core types and interfaces. It uses type aliases to re-export definitions
// from the `types` subpackage, which contains the actual implementations.
//
// This pattern solves the "import cycle" problem by allowing internal packages
// to depend on `types` without depending on the root `parti` package, while
// still providing a convenient `parti.State`, `parti.Logger`, etc. for users.
type (
	State      = types.State
	Partition  = types.Partition
	Assignment = types.Assignment
)

// Re-export interfaces from the internal types package for convenience.
type (
	AssignmentStrategy = types.AssignmentStrategy
	PartitionSource    = types.PartitionSource
	ElectionAgent      = types.ElectionAgent
	MetricsCollector   = types.MetricsCollector
	Logger             = types.Logger
	Hooks              = types.Hooks
)

// Re-export State constants from the internal types package.
const (
	StateInit              = types.StateInit
	StateClaimingID        = types.StateClaimingID
	StateElection          = types.StateElection
	StateWaitingAssignment = types.StateWaitingAssignment
	StateStable            = types.StateStable
	StateScaling           = types.StateScaling
	StateRebalancing       = types.StateRebalancing
	StateEmergency         = types.StateEmergency
	StateShutdown          = types.StateShutdown
)
