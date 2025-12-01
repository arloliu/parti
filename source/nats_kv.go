package source

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go/jetstream"
)

// NatsKV implements a partition source backed by a NATS KeyValue bucket.
//
// It watches a specific key in the KV bucket for updates to the partition list.
// This allows dynamic partition updates without restarting workers.
type NatsKV struct {
	kv     jetstream.KeyValue
	key    string
	logger types.Logger

	mu         sync.RWMutex
	partitions []types.Partition
	watcher    jetstream.KeyWatcher
	ctx        context.Context
	cancel     context.CancelFunc
	running    bool
}

var (
	_ types.PartitionSource  = (*NatsKV)(nil)
	_ types.PartitionUpdater = (*NatsKV)(nil)
)

// NewNatsKV creates a new NATS KV-based partition source.
//
// Parameters:
//   - kv: The NATS KeyValue bucket to use
//   - key: The key where partitions are stored (JSON encoded)
//   - logger: Optional logger
func NewNatsKV(kv jetstream.KeyValue, key string, logger types.Logger) *NatsKV {
	return &NatsKV{
		kv:     kv,
		key:    key,
		logger: logger,
	}
}

// Start initializes the source and starts watching for updates.
func (s *NatsKV) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return nil
	}

	// Initial fetch
	entry, err := s.kv.Get(ctx, s.key)
	if err != nil {
		if !errors.Is(err, jetstream.ErrKeyNotFound) {
			return fmt.Errorf("failed to fetch initial partitions: %w", err)
		}
		// Initialize with empty list if not found
		s.partitions = []types.Partition{}
	} else {
		partitions, err := s.decodePartitions(entry.Value())
		if err != nil {
			return fmt.Errorf("failed to decode partitions: %w", err)
		}
		s.partitions = partitions
	}

	// Start watcher
	// Use object-level context for watcher lifecycle
	s.ctx, s.cancel = context.WithCancel(context.Background())
	watcher, err := s.kv.Watch(s.ctx, s.key)
	if err != nil {
		s.cancel()
		return fmt.Errorf("failed to start watcher: %w", err)
	}
	s.watcher = watcher
	s.running = true

	go s.watchLoop(s.ctx, s.watcher)

	return nil
}

// Stop stops the watcher.
func (s *NatsKV) Stop(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return nil
	}

	if s.cancel != nil {
		s.cancel()
	}

	var err error
	if s.watcher != nil {
		err = s.watcher.Stop()
	}
	s.running = false

	return err
}

// List returns the current list of partitions.
func (s *NatsKV) List(_ context.Context) ([]types.Partition, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Return a copy
	result := make([]types.Partition, len(s.partitions))
	copy(result, s.partitions)

	return result, nil
}

// Update updates the partition list in the KV bucket.
//
// Note: NATS KV buckets have a default value size limit of 1MB (MaxMsgSize).
// To support large partition lists (e.g., 2000+ partitions), this method
// automatically compresses the data using Gzip before storing it.
//
// If the compressed size still exceeds the limit, the update will fail.
func (s *NatsKV) Update(ctx context.Context, partitions []types.Partition) error {
	data, err := json.Marshal(partitions)
	if err != nil {
		return fmt.Errorf("failed to marshal partitions: %w", err)
	}

	// Compress data
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		return fmt.Errorf("failed to compress partitions: %w", err)
	}
	if err := gw.Close(); err != nil {
		return fmt.Errorf("failed to close gzip writer: %w", err)
	}
	compressedData := buf.Bytes()

	_, err = s.kv.Put(ctx, s.key, compressedData)
	if err != nil {
		if len(compressedData) > 1024*1024 {
			return fmt.Errorf("failed to update partitions in KV (compressed size %.2f MB exceeds default 1MB limit): %w", float64(len(compressedData))/(1024*1024), err)
		}

		return fmt.Errorf("failed to update partitions in KV: %w", err)
	}

	return nil
}

func (s *NatsKV) watchLoop(ctx context.Context, watcher jetstream.KeyWatcher) {
	for {
		select {
		case <-ctx.Done():
			return
		case entry := <-watcher.Updates():
			if entry == nil {
				continue
			}

			// Handle delete
			if entry.Operation() == jetstream.KeyValueDelete || entry.Operation() == jetstream.KeyValuePurge {
				s.mu.Lock()
				s.partitions = []types.Partition{}
				s.mu.Unlock()

				continue
			}

			partitions, err := s.decodePartitions(entry.Value())
			if err != nil {
				if s.logger != nil {
					s.logger.Error("failed to decode partitions update", "error", err)
				}

				continue
			}

			s.mu.Lock()
			s.partitions = partitions
			s.mu.Unlock()
		}
	}
}

// decodePartitions decodes partition data, handling both compressed (Gzip) and uncompressed (JSON) formats.
func (s *NatsKV) decodePartitions(data []byte) ([]types.Partition, error) {
	if len(data) == 0 {
		return []types.Partition{}, nil
	}

	// Check for Gzip magic bytes (0x1f, 0x8b)
	isGzip := len(data) > 2 && data[0] == 0x1f && data[1] == 0x8b

	var jsonData []byte
	if isGzip {
		gr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			// Fallback to treating as plain JSON if gzip reader fails immediately
			// This handles edge cases where data might start with magic bytes but not be valid gzip
			jsonData = data
		} else {
			defer gr.Close()
			decompressed, err := io.ReadAll(gr)
			if err != nil {
				return nil, fmt.Errorf("failed to decompress data: %w", err)
			}
			jsonData = decompressed
		}
	} else {
		jsonData = data
	}

	var partitions []types.Partition
	if err := json.Unmarshal(jsonData, &partitions); err != nil {
		return nil, fmt.Errorf("failed to unmarshal partitions: %w", err)
	}

	return partitions, nil
}
