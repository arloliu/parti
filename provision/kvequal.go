package provision

import (
	"maps"
	"strings"

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

// streamConfigToKVConfig projects a live jetstream.StreamConfig onto a
// jetstream.KeyValueConfig covering every field nats.go's KeyValueConfig
// exposes, so a round-trip through UpdateKeyValue preserves
// preserved-from-live fields. This is the full projection; the
// comparison-subset-only callers (drift classifiers) read only the
// comparison-subset fields, for which the extra fields are inert.
//
// LimitMarkerTTL is backed by StreamConfig.SubjectDeleteMarkerTTL in
// nats.go v1.50.0 (confirmed via KeyValueBucketStatus.LimitMarkerTTL,
// jetstream/kv.go:840). Compression is the StoreCompression enum on the
// stream side and a bool on the KeyValueConfig side.
func streamConfigToKVConfig(sc jetstream.StreamConfig) jetstream.KeyValueConfig {
	return jetstream.KeyValueConfig{
		Bucket:         strings.TrimPrefix(sc.Name, kvStreamPrefix),
		Description:    sc.Description,
		MaxValueSize:   sc.MaxMsgSize,
		History:        historyFromStream(sc.MaxMsgsPerSubject),
		TTL:            sc.MaxAge,
		MaxBytes:       sc.MaxBytes,
		Storage:        sc.Storage,
		Replicas:       sc.Replicas,
		Placement:      sc.Placement,
		RePublish:      sc.RePublish,
		Mirror:         sc.Mirror,
		Sources:        sc.Sources,
		Compression:    sc.Compression != jetstream.NoCompression,
		LimitMarkerTTL: sc.SubjectDeleteMarkerTTL,
		Metadata:       sc.Metadata,
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
