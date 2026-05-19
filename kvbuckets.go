package parti

import (
	"time"

	"github.com/arloliu/parti/v2/internal/kvbuckets"
	"github.com/nats-io/nats.go/jetstream"
)

// BuildControlPlaneKVConfig returns the canonical jetstream.KeyValueConfig for
// a Parti control-plane KV bucket. It is a thin wrapper around
// internal/kvbuckets.BuildKeyValueConfig, re-exported here so that external
// packages (such as provision/) can construct byte-equivalent configurations
// without importing an internal package.
//
// See internal/kvbuckets.BuildKeyValueConfig for the full contract, including
// the byte-equivalence guarantee with (*Manager).ensureKVBucket.
func BuildControlPlaneKVConfig(bucket string, ttl time.Duration, storage jetstream.StorageType) jetstream.KeyValueConfig {
	return kvbuckets.BuildKeyValueConfig(bucket, ttl, storage)
}
