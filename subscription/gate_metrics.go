package subscription

import "time"

// GateMetrics captures optional Processing Gate metrics.
//
// Implementations should be non-blocking. When nil, metrics are not recorded.
type GateMetrics interface {
	// IncGateNAK increments the NAK counter by reason
	// (e.g., "unknown_ownership", "non_owner", "owner_disallowed_state").
	IncGateNAK(reason string)

	// ObserveGateNakDelay records the NAK delay applied by the gate.
	ObserveGateNakDelay(d time.Duration)
}
