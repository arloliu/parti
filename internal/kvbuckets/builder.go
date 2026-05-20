// Package kvbuckets provides shared builders for JetStream KeyValue bucket
// configurations used by the Parti control plane.
package kvbuckets

import (
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// BuildKeyValueConfig returns the canonical jetstream.KeyValueConfig for a
// Parti control-plane KV bucket.
//
// The returned config is byte-equivalent to the literal historically produced
// inside (*Manager).ensureKVBucket in manager_setup.go:
//
//	jetstream.KeyValueConfig{
//	    Bucket:  bucket,
//	    History: 1,
//	    Storage: storage,
//	    // TTL set only when ttl > 0
//	}
//
// This equivalence is a hard contract: the runtime manager and the
// provision plan emitter must produce the same KeyValueConfig from the
// same inputs. The provision package may stamp a Replicas value on the
// returned config when its operator-supplied control-plane Replicas is
// non-zero; that is the only field permitted to deviate.
//
// Rules:
//   - Bucket, History (always 1), and Storage are always set.
//   - TTL is set only when ttl > 0; zero and negative values leave cfg.TTL unset.
//   - Replicas is left zero — nats.go normalizes it to 1 server-side.
//   - This function performs no I/O.
func BuildKeyValueConfig(bucket string, ttl time.Duration, storage jetstream.StorageType) jetstream.KeyValueConfig {
	cfg := jetstream.KeyValueConfig{
		Bucket:  bucket,
		History: 1,
		Storage: storage,
	}

	if ttl > 0 {
		cfg.TTL = ttl
	}

	return cfg
}
