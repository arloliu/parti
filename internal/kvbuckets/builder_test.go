package kvbuckets

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

func TestBuildKeyValueConfig_TTL(t *testing.T) {
	t.Run("positive TTL is set", func(t *testing.T) {
		ttl := 5 * time.Second
		cfg := BuildKeyValueConfig("test-bucket", ttl, jetstream.FileStorage)
		if cfg.TTL != ttl {
			t.Errorf("expected TTL=%v, got %v", ttl, cfg.TTL)
		}
	})

	t.Run("zero TTL leaves cfg.TTL unset", func(t *testing.T) {
		cfg := BuildKeyValueConfig("test-bucket", 0, jetstream.FileStorage)
		if cfg.TTL != 0 {
			t.Errorf("expected TTL=0 (unset), got %v", cfg.TTL)
		}
	})

	t.Run("negative TTL leaves cfg.TTL unset", func(t *testing.T) {
		cfg := BuildKeyValueConfig("test-bucket", -1*time.Second, jetstream.FileStorage)
		if cfg.TTL != 0 {
			t.Errorf("expected TTL=0 (unset), got %v", cfg.TTL)
		}
	})
}

func TestBuildKeyValueConfig_Storage(t *testing.T) {
	t.Run("FileStorage is propagated", func(t *testing.T) {
		cfg := BuildKeyValueConfig("test-bucket", 0, jetstream.FileStorage)
		if cfg.Storage != jetstream.FileStorage {
			t.Errorf("expected FileStorage, got %v", cfg.Storage)
		}
	})

	t.Run("MemoryStorage is propagated", func(t *testing.T) {
		cfg := BuildKeyValueConfig("test-bucket", 0, jetstream.MemoryStorage)
		if cfg.Storage != jetstream.MemoryStorage {
			t.Errorf("expected MemoryStorage, got %v", cfg.Storage)
		}
	})
}

func TestBuildKeyValueConfig_HistoryAlwaysOne(t *testing.T) {
	for _, storage := range []jetstream.StorageType{jetstream.FileStorage, jetstream.MemoryStorage} {
		cfg := BuildKeyValueConfig("test-bucket", 10*time.Second, storage)
		if cfg.History != 1 {
			t.Errorf("expected History=1, got %d (storage=%v)", cfg.History, storage)
		}
	}
}

func TestBuildKeyValueConfig_ReplicasUnset(t *testing.T) {
	cfg := BuildKeyValueConfig("test-bucket", 5*time.Second, jetstream.FileStorage)
	if cfg.Replicas != 0 {
		t.Errorf("expected Replicas=0 (unset), got %d", cfg.Replicas)
	}
}

func TestBuildKeyValueConfig_BucketPropagated(t *testing.T) {
	const bucketName = "parti-control-plane-elections-v2"
	cfg := BuildKeyValueConfig(bucketName, 0, jetstream.FileStorage)
	if cfg.Bucket != bucketName {
		t.Errorf("expected Bucket=%q, got %q", bucketName, cfg.Bucket)
	}
}
