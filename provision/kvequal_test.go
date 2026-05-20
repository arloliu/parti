package provision

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestKVConfigsEqual(t *testing.T) {
	t.Parallel()

	base := jetstream.KeyValueConfig{
		Bucket:       "test-bucket",
		History:      1,
		Storage:      jetstream.FileStorage,
		Replicas:     1,
		MaxValueSize: -1, // server "no limit"
		TTL:          5 * time.Minute,
		Metadata:     map[string]string{"k": "v"},
	}

	tests := []struct {
		name  string
		a     jetstream.KeyValueConfig
		b     jetstream.KeyValueConfig
		equal bool
	}{
		{
			name:  "byte_equal",
			a:     base,
			b:     base,
			equal: true,
		},
		{
			// Replicas 0 and 1 are both "server default" — must be equal.
			name:  "replicas_zero_equals_one",
			a:     withReplicas(base, 0),
			b:     withReplicas(base, 1),
			equal: true,
		},
		{
			name:  "replicas_one_equals_zero",
			a:     withReplicas(base, 1),
			b:     withReplicas(base, 0),
			equal: true,
		},
		{
			name:  "replicas_mismatch",
			a:     withReplicas(base, 1),
			b:     withReplicas(base, 3),
			equal: false,
		},
		{
			// MaxValueSize 0 and -1 are both "no limit" — must be equal.
			name:  "max_value_size_zero_equals_minus_one",
			a:     withMaxValueSize(base, 0),
			b:     withMaxValueSize(base, -1),
			equal: true,
		},
		{
			name:  "max_value_size_minus_one_equals_zero",
			a:     withMaxValueSize(base, -1),
			b:     withMaxValueSize(base, 0),
			equal: true,
		},
		{
			name:  "max_value_size_mismatch",
			a:     withMaxValueSize(base, 512),
			b:     withMaxValueSize(base, 1024),
			equal: false,
		},
		{
			name:  "metadata_nil_equals_empty_map",
			a:     withMetadata(base, nil),
			b:     withMetadata(base, map[string]string{}),
			equal: true,
		},
		{
			name:  "metadata_empty_map_equals_nil",
			a:     withMetadata(base, map[string]string{}),
			b:     withMetadata(base, nil),
			equal: true,
		},
		{
			name:  "metadata_equal_maps",
			a:     withMetadata(base, map[string]string{"k": "v", "x": "y"}),
			b:     withMetadata(base, map[string]string{"x": "y", "k": "v"}),
			equal: true,
		},
		{
			name:  "metadata_value_mismatch",
			a:     withMetadata(base, map[string]string{"k": "v1"}),
			b:     withMetadata(base, map[string]string{"k": "v2"}),
			equal: false,
		},
		{
			name:  "metadata_key_missing",
			a:     withMetadata(base, map[string]string{"k": "v"}),
			b:     withMetadata(base, map[string]string{}),
			equal: false,
		},
		{
			name:  "ttl_equal",
			a:     withTTL(base, 10*time.Minute),
			b:     withTTL(base, 10*time.Minute),
			equal: true,
		},
		{
			name:  "ttl_mismatch",
			a:     withTTL(base, 10*time.Minute),
			b:     withTTL(base, 20*time.Minute),
			equal: false,
		},
		{
			name:  "history_equal",
			a:     withHistory(base, 2),
			b:     withHistory(base, 2),
			equal: true,
		},
		{
			name:  "history_mismatch",
			a:     withHistory(base, 1),
			b:     withHistory(base, 5),
			equal: false,
		},
		{
			name:  "storage_equal",
			a:     withStorage(base, jetstream.MemoryStorage),
			b:     withStorage(base, jetstream.MemoryStorage),
			equal: true,
		},
		{
			name:  "storage_mismatch",
			a:     withStorage(base, jetstream.FileStorage),
			b:     withStorage(base, jetstream.MemoryStorage),
			equal: false,
		},
		{
			// Bucket is NOT compared — it is the resource identity, not drift.
			name:  "bucket_name_difference_ignored",
			a:     withBucket(base, "bucket-a"),
			b:     withBucket(base, "bucket-b"),
			equal: true,
		},
		{
			// Description is preserved-from-live — not compared.
			name:  "description_difference_ignored",
			a:     withDescription(base, "notes"),
			b:     withDescription(base, ""),
			equal: true,
		},
		{
			// MaxBytes is preserved-from-live — not compared.
			name:  "max_bytes_difference_ignored",
			a:     withMaxBytes(base, 1024),
			b:     withMaxBytes(base, 0),
			equal: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := kvConfigsEqual(tc.a, tc.b)
			if tc.equal {
				require.True(t, got, "expected equal for %q", tc.name)
			} else {
				require.False(t, got, "expected not-equal for %q", tc.name)
			}
		})
	}
}

// helpers to produce modified copies.

func withReplicas(c jetstream.KeyValueConfig, r int) jetstream.KeyValueConfig {
	c.Replicas = r
	return c
}

func withMaxValueSize(c jetstream.KeyValueConfig, s int32) jetstream.KeyValueConfig {
	c.MaxValueSize = s
	return c
}

func withMetadata(c jetstream.KeyValueConfig, m map[string]string) jetstream.KeyValueConfig {
	c.Metadata = m
	return c
}

func withTTL(c jetstream.KeyValueConfig, d time.Duration) jetstream.KeyValueConfig {
	c.TTL = d
	return c
}

func withHistory(c jetstream.KeyValueConfig, h uint8) jetstream.KeyValueConfig {
	c.History = h
	return c
}

func withStorage(c jetstream.KeyValueConfig, s jetstream.StorageType) jetstream.KeyValueConfig {
	c.Storage = s
	return c
}

func withBucket(c jetstream.KeyValueConfig, b string) jetstream.KeyValueConfig {
	c.Bucket = b
	return c
}

func withDescription(c jetstream.KeyValueConfig, d string) jetstream.KeyValueConfig {
	c.Description = d
	return c
}

func withMaxBytes(c jetstream.KeyValueConfig, b int64) jetstream.KeyValueConfig {
	c.MaxBytes = b
	return c
}
