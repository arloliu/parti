package types_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/types"
)

// TestHeartbeat_DecodeLegacyTimestampString verifies that legacy timestamp-string
// payloads (RFC3339Nano and plain RFC3339) decode to SchemaVersion=0, Capabilities=0,
// with the Timestamp field populated correctly and all ack fields zero.
func TestHeartbeat_DecodeLegacyTimestampString(t *testing.T) {
	t.Run("RFC3339Nano with fractional seconds", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Nanosecond)
		payload := []byte(now.Format(time.RFC3339Nano))

		hb, err := types.DecodeHeartbeat(payload)
		require.NoError(t, err)
		require.Equal(t, uint8(0), hb.SchemaVersion)
		require.Equal(t, uint32(0), hb.Capabilities)
		require.True(t, hb.Timestamp.Equal(now), "timestamp mismatch: got %v, want %v", hb.Timestamp, now)
		require.Equal(t, int64(0), hb.AppliedVersion)
		require.Equal(t, uint64(0), hb.AppliedDigest)
		require.Equal(t, uint64(0), hb.AppliedSourceRevision)
		require.False(t, hb.AppliedSourceRevKnown)
		require.True(t, hb.AppliedAt.IsZero())
	})

	t.Run("RFC3339 without fractional seconds", func(t *testing.T) {
		// Plain RFC3339 (no sub-second precision) — the decoder must fall back from
		// RFC3339Nano to RFC3339 and succeed, not return a parse error.
		ts, err := time.Parse(time.RFC3339, "2025-01-15T12:30:45Z")
		require.NoError(t, err)
		payload := []byte("2025-01-15T12:30:45Z")

		hb, err := types.DecodeHeartbeat(payload)
		require.NoError(t, err)
		require.Equal(t, uint8(0), hb.SchemaVersion)
		require.Equal(t, uint32(0), hb.Capabilities)
		require.True(t, hb.Timestamp.Equal(ts), "timestamp mismatch: got %v, want %v", hb.Timestamp, ts)
		require.Equal(t, int64(0), hb.AppliedVersion)
		require.True(t, hb.AppliedAt.IsZero())
	})
}

// TestHeartbeat_DecodeV1JSON verifies that a v1 JSON payload decodes all fields
// correctly, including non-zero SchemaVersion and Capabilities.
func TestHeartbeat_DecodeV1JSON(t *testing.T) {
	appliedAt := time.Now().UTC().Add(-5 * time.Second).Truncate(time.Millisecond)
	ts := time.Now().UTC().Truncate(time.Millisecond)

	src := types.Heartbeat{
		WorkerID:              "worker-7",
		SchemaVersion:         1,
		Capabilities:          types.CapAckV1 | types.CapTwoPhaseHandoff,
		LeaderRevision:        42,
		AppliedVersion:        100,
		AppliedDigest:         0xdeadbeef,
		AppliedSourceRevision: 9,
		AppliedSourceRevKnown: true,
		AppliedAt:             appliedAt,
		Timestamp:             ts,
	}

	payload, err := json.Marshal(src)
	require.NoError(t, err)

	hb, err := types.DecodeHeartbeat(payload)
	require.NoError(t, err)
	require.Equal(t, uint8(1), hb.SchemaVersion)
	require.Equal(t, types.CapAckV1|types.CapTwoPhaseHandoff, hb.Capabilities)
	require.Equal(t, "worker-7", hb.WorkerID)
	require.Equal(t, uint64(42), hb.LeaderRevision)
	require.Equal(t, int64(100), hb.AppliedVersion)
	require.Equal(t, uint64(0xdeadbeef), hb.AppliedDigest)
	require.Equal(t, uint64(9), hb.AppliedSourceRevision)
	require.True(t, hb.AppliedSourceRevKnown)
	require.True(t, hb.AppliedAt.Equal(appliedAt))
	require.True(t, hb.Timestamp.Equal(ts))
}

// TestHeartbeat_DecodeLabels verifies the additive Labels field: a v1 payload
// carrying "labels" decodes to the set; a legacy timestamp payload and a v1
// payload without the field both decode to Labels=nil (distinct from error).
func TestHeartbeat_DecodeLabels(t *testing.T) {
	t.Run("v1 JSON with labels", func(t *testing.T) {
		hb, err := types.DecodeHeartbeat([]byte(`{"worker_id":"w1","schema_version":1,"labels":["vip"],"applied_at":"0001-01-01T00:00:00Z","timestamp":"0001-01-01T00:00:00Z"}`))
		require.NoError(t, err)
		require.Equal(t, []string{"vip"}, hb.Labels)
	})

	t.Run("legacy timestamp payload has nil labels", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Nanosecond)
		hb, err := types.DecodeHeartbeat([]byte(now.Format(time.RFC3339Nano)))
		require.NoError(t, err)
		require.Nil(t, hb.Labels, "pre-label workers decode with nil Labels")
	})

	t.Run("v1 JSON without labels field has nil labels", func(t *testing.T) {
		// A pre-label v1 worker's payload omits "labels"; it must decode to nil,
		// which is distinct from a decode error.
		src := types.Heartbeat{WorkerID: "w2", SchemaVersion: 1}
		payload, err := json.Marshal(src)
		require.NoError(t, err)
		require.NotContains(t, string(payload), "labels", "omitempty must drop the empty field")

		hb, err := types.DecodeHeartbeat(payload)
		require.NoError(t, err)
		require.Nil(t, hb.Labels)
	})
}

// TestHeartbeat_DecodeMalformed_ReturnsError verifies that a payload that is
// neither valid JSON nor a parseable RFC3339 timestamp is returned as an error,
// not silently degraded to an empty Heartbeat. Importantly, a payload that
// begins with '{' but is invalid JSON must also return an error — it must NOT
// silently fall back to legacy timestamp parsing.
func TestHeartbeat_DecodeMalformed_ReturnsError(t *testing.T) {
	t.Run("non-JSON non-timestamp string", func(t *testing.T) {
		_, err := types.DecodeHeartbeat([]byte("not json or timestamp"))
		require.Error(t, err)
	})

	t.Run("starts with brace but is invalid JSON", func(t *testing.T) {
		// This payload looks like a v1 JSON attempt but is malformed.
		// DecodeHeartbeat must return an error, not silently degrade to
		// SchemaVersion=0 as a "legacy" worker.
		_, err := types.DecodeHeartbeat([]byte("{not valid json"))
		require.Error(t, err, "malformed v1 JSON must not silently fall back to legacy parsing")
	})
}

// TestHeartbeat_DecodeEmpty_ReturnsError verifies that an empty payload returns
// an error.
func TestHeartbeat_DecodeEmpty_ReturnsError(t *testing.T) {
	_, err := types.DecodeHeartbeat([]byte{})
	require.Error(t, err)
}
