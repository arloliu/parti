package provision

import (
	"maps"

	"github.com/nats-io/nats.go/jetstream"
)

// kvConfigsEqual reports whether two jetstream.KeyValueConfig values
// agree on the comparison subset: operator-expressible fields
// (Metadata, TTL, MaxValueSize, Replicas) and drift-detection-only
// fields (History, Storage). Preserved-from-live fields (Description,
// MaxBytes, Placement, RePublish, Mirror, Sources, Compression,
// LimitMarkerTTL, plus any future field) are NOT compared.
//
// Normalizations applied to match nats.go server-side defaults:
//   - Replicas: 0 == 1 (server default)
//   - MaxValueSize: 0 == -1 ("no limit")
//   - Metadata: nil == empty map
//
// Bucket is NOT compared — bucket name is the resource identity used
// by exact-name lookup, not an in-resource drift field.
func kvConfigsEqual(a, b jetstream.KeyValueConfig) bool {
	if normalizeReplicas(a.Replicas) != normalizeReplicas(b.Replicas) {
		return false
	}
	if normalizeMaxValueSize(a.MaxValueSize) != normalizeMaxValueSize(b.MaxValueSize) {
		return false
	}
	if a.TTL != b.TTL {
		return false
	}
	if !maps.Equal(a.Metadata, b.Metadata) {
		return false
	}
	if a.History != b.History {
		return false
	}
	if a.Storage != b.Storage {
		return false
	}

	return true
}

// normalizeReplicas maps the desired Replicas value through the NATS
// server-default rule: a zero (the KeyValueConfig zero value, meaning
// "omit") is equivalent to 1, the server's default replica count.
// Used by kvConfigsEqual and by every drift classifier that needs to
// compare a config-side Replicas against a live stream's Replicas.
func normalizeReplicas(r int) int {
	if r == 0 {
		return 1
	}

	return r
}

// normalizeMaxValueSize maps the desired MaxValueSize through the NATS
// server-default rule: a zero (meaning "no limit" on the caller side)
// is equivalent to -1, which is how the server stores "no limit".
func normalizeMaxValueSize(s int32) int32 {
	if s == 0 {
		return -1
	}

	return s
}

// extractLiveKVConfig projects a live jetstream.StreamConfig onto the
// subset of KeyValueConfig fields kvConfigsEqual compares. Fields outside
// that subset (Bucket, Description, MaxBytes, Placement, RePublish,
// Mirror, Sources, Compression, LimitMarkerTTL) are omitted because the
// equality contract excludes them; the returned value is suitable as
// input to kvConfigsEqual but not for round-tripping to UpdateKeyValue.
func extractLiveKVConfig(sc *jetstream.StreamConfig) jetstream.KeyValueConfig {
	return jetstream.KeyValueConfig{
		TTL:          sc.MaxAge,
		MaxValueSize: sc.MaxMsgSize,
		Replicas:     sc.Replicas,
		Storage:      sc.Storage,
		History:      historyFromStream(sc.MaxMsgsPerSubject),
		Metadata:     sc.Metadata,
	}
}

// historyFromStream clamps the stream-side MaxMsgsPerSubject to the
// uint8 range of jetstream.KeyValueConfig.History. NATS KV enforces
// History <= 64 (KeyValueMaxHistory), so any live value above 255 is
// non-KV data; the clamp keeps drift comparison deterministic without
// pretending to round-trip arbitrary stream configurations.
func historyFromStream(maxMsgsPerSubject int64) uint8 {
	switch {
	case maxMsgsPerSubject < 0:
		return 0
	case maxMsgsPerSubject > 255:
		return 255
	default:
		return uint8(maxMsgsPerSubject)
	}
}
