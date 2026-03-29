package consumer

import (
	"time"

	"github.com/arloliu/parti/v2/types"
)

// ProcessingGateConfig configures the optional exclusive processing gate.
//
// When enabled, a Dynamic consumer uses a distributed lock (via KV) to ensure
// that it is the *only* active processor for its assigned partitions. This
// prevents split-brain processing during rebalances.
//
// The gate NAKs messages when the worker is not the owner or when the handoff
// state is not in the allowed set.
type ProcessingGateConfig struct {
	// Enabled toggles the gate.
	// Default: false (gate disabled).
	Enabled bool

	// AllowedStates defines which handoff states permit message processing.
	// If empty, defaults to [types.StateStable].
	//
	// Common configurations:
	//  - [types.StateStable]: Strict consistency (default). Messages only processed when ownership is stable.
	//  - [types.StateStable, types.StateHandoff]: Higher availability. Allows processing during handoff.
	AllowedStates []types.HandoffState `validate:"omitempty,min=1"` //nolint:revive // struct-tag: omitempty is valid for go-playground/validator

	// WarmupDuration, when >0, enables a warm-up phase during which only
	// WarmupAllowedStates are permitted. After the duration elapses, AllowedStates
	// take effect for steady-state processing.
	WarmupDuration time.Duration `validate:"gte=0"`

	// WarmupAllowedStates defines which states are permitted during the warm-up phase.
	// If empty and WarmupDuration>0, defaults to Stable-only.
	WarmupAllowedStates []types.HandoffState

	// NakDelay is the base delay for NAK when the worker is not the owner or
	// the state is disallowed.
	// Default: 100ms.
	NakDelay time.Duration `default:"100ms" validate:"gte=0"`

	// NakJitter is a fractional jitter in [0.0, 1.0] applied to NakDelay.
	// Default: 0.0 (no jitter).
	NakJitter float64 `validate:"gte=0,lte=1"`

	// Debug enables verbose logging for NAK decisions (non-owner, disallowed state, etc).
	Debug bool

	// Metrics optionally records processing gate metrics (NAK reasons, delays).
	Metrics GateMetrics
}
