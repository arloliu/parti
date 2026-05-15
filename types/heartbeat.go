package types

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Heartbeat represents a worker's heartbeat payload published to NATS KV.
//
// SchemaVersion=0 indicates a legacy worker that publishes only a timestamp
// string; all fields beyond Timestamp are zero for these workers. Callers must
// treat legacy workers as "alive but not ack-capable" — do not escalate
// reassignment for them via the audit path.
//
// SchemaVersion>=1 indicates an ack-capable worker. Capability bits in the
// Capabilities field reflect runtime wire-up state, not config intent.
//
// v1 publishers always emit JSON. Legacy timestamp-byte payloads are parsed
// for backwards-compatibility with pre-v1 workers.
type Heartbeat struct {
	WorkerID              string    `json:"worker_id"`
	SchemaVersion         uint8     `json:"schema_version,omitempty"`
	Capabilities          uint32    `json:"capabilities,omitempty"`
	LeaderRevision        uint64    `json:"leader_revision,omitempty"`
	AppliedVersion        int64     `json:"applied_version,omitempty"`
	AppliedDigest         uint64    `json:"applied_digest,omitempty"`
	AppliedSourceRevision uint64    `json:"applied_source_revision,omitempty"`
	AppliedSourceRevKnown bool      `json:"applied_source_revision_known,omitempty"`
	AppliedAt             time.Time `json:"applied_at,omitempty"`
	Timestamp             time.Time `json:"timestamp"`
}

// Capability bitmask values.
//
// A worker sets a bit only when the corresponding safety mechanism is actually
// wired up and active in this process, not merely configured. The leader's audit
// uses this to decide whether reassignment escalation is safe.
const (
	// CapAckV1 indicates the worker publishes apply receipts (AppliedVersion etc.).
	CapAckV1 uint32 = 1 << 0
	// CapTwoPhaseHandoff indicates the manager runs the two-phase handoff coordinator.
	CapTwoPhaseHandoff uint32 = 1 << 1
	// CapProcessingGate indicates consumer handlers are wrapped with the processing gate.
	CapProcessingGate uint32 = 1 << 2
)

// DecodeHeartbeat decodes a heartbeat payload from NATS KV.
//
// Accepts both v1 JSON (new workers) and legacy RFC3339/RFC3339Nano timestamp
// strings (pre-v1 workers). The two formats are distinguished by the first byte:
// JSON payloads begin with '{'; timestamp strings do not.
//
// Legacy payloads decode to a Heartbeat with SchemaVersion=0, Capabilities=0,
// and Timestamp set to the parsed time. All other fields are zero.
//
// Malformed payloads (neither valid JSON nor a parseable timestamp) return an
// error. Do NOT silently degrade to an empty Heartbeat — a malformed payload
// is a distinct failure signal.
//
// Parameters:
//   - b: Raw bytes from the KV entry value
//
// Returns:
//   - Heartbeat: Decoded heartbeat
//   - error: Parse error for malformed payloads
//
// Example:
//
//	entry, err := kv.Get(ctx, key)
//	if err != nil { ... }
//	hb, err := types.DecodeHeartbeat(entry.Value())
func DecodeHeartbeat(b []byte) (Heartbeat, error) {
	if len(b) == 0 {
		return Heartbeat{}, errors.New("heartbeat decode: empty payload")
	}

	// v1 JSON — new payloads always start with '{'.
	if b[0] == '{' {
		var hb Heartbeat
		if err := json.Unmarshal(b, &hb); err != nil {
			return Heartbeat{}, fmt.Errorf("v1 JSON parse: %w", err)
		}
		return hb, nil
	}

	// Fallback: legacy timestamp string (RFC3339Nano then RFC3339).
	ts, err := time.Parse(time.RFC3339Nano, string(b))
	if err != nil {
		ts, err = time.Parse(time.RFC3339, string(b))
	}
	if err != nil {
		return Heartbeat{}, fmt.Errorf("legacy timestamp parse: %w", err)
	}

	return Heartbeat{
		SchemaVersion: 0,
		Capabilities:  0,
		Timestamp:     ts,
	}, nil
}
