package coordinator

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// TestNetworkDisconnectLeaderEvent_ParamsAndStringer is the deterministic
// proof that the new chaos event's registration is complete. The audit's
// round-1 P1 was that probabilistic CI runs may not exercise the new
// event at all; this test bypasses the scheduler entirely and asserts:
//   - generateEventParams populates a duration in the expected range
//   - String() returns a human-readable name (not "Unknown Event")
//   - GetAvailableEvents would surface it if listed in config
//
// These three invariants guard against future refactors that miss one of
// the typed-switch surfaces (the round-1 review found I missed
// generateEventParams and String() in my original plan).
func TestNetworkDisconnectLeaderEvent_ParamsAndStringer(t *testing.T) {
	cfg := ChaosConfig{
		Enabled:     true,
		Events:      []string{string(NetworkDisconnectLeaderEvent)},
		MinInterval: 1 * time.Second,
		MaxInterval: 2 * time.Second,
	}
	cc := NewChaosController(cfg)

	// generateEventParams duration matches the random variant: 5–15s.
	params := cc.generateEventParams(NetworkDisconnectLeaderEvent)
	dur, ok := params["duration"].(time.Duration)
	if !ok {
		t.Fatalf("params[duration] missing or wrong type: %T %+v", params["duration"], params)
	}
	if dur < 5*time.Second || dur > 15*time.Second {
		t.Errorf("duration = %v, want 5s..15s", dur)
	}

	// String() must not fall through to "Unknown Event".
	got := NetworkDisconnectLeaderEvent.String()
	if got == "" || strings.Contains(strings.ToLower(got), "unknown") {
		t.Errorf("String() = %q, want a recognized human-readable name", got)
	}
	if !strings.Contains(strings.ToLower(got), "leader") {
		t.Errorf("String() = %q, want a name that mentions 'leader' to distinguish from the random variant", got)
	}

	// Random variant is unaffected — guards against accidentally clobbering it.
	if got := NetworkDisconnectEvent.String(); strings.Contains(strings.ToLower(got), "leader") {
		t.Errorf("random NetworkDisconnect String() should NOT mention 'leader'; got %q", got)
	}

	// Constants must be distinct.
	if NetworkDisconnectLeaderEvent == NetworkDisconnectEvent {
		t.Error("NetworkDisconnectLeaderEvent and NetworkDisconnectEvent must be distinct constants")
	}

	// Configured event survives the parse path (string → ChaosEvent cast).
	available := cc.GetAvailableEvents()
	if !slices.Contains(available, NetworkDisconnectLeaderEvent) {
		t.Errorf("NetworkDisconnectLeaderEvent not in GetAvailableEvents(); got %v", available)
	}
}
