// Package partcodec implements the wire-format codec for partition tables stored
// in NATS JetStream KV. It is shared by the source runtime and the provision SDK
// so that both paths write byte-identical output.
//
// Wire format: JSON-encoded []types.Partition, gzip-compressed.
// The decoder is dual-format: it accepts both gzip-compressed and plain-JSON
// payloads for backward compatibility.
package partcodec

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"

	"github.com/arloliu/parti/v2/types"
)

// Encode marshals the partition list to JSON and gzip-compresses it.
// It performs no validation; callers must validate before encoding.
func Encode(partitions []types.Partition) ([]byte, error) {
	data, err := json.Marshal(partitions)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal partitions: %w", err)
	}

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		return nil, fmt.Errorf("failed to compress partitions: %w", err)
	}
	if err := gw.Close(); err != nil {
		return nil, fmt.Errorf("failed to close gzip writer: %w", err)
	}

	return buf.Bytes(), nil
}

// Decode decodes partition data, accepting both gzip-compressed and plain-JSON
// payloads. Corrupt data is rejected, never silently filtered:
//   - An invalid partition (failing Validate) returns an error.
//   - A duplicate CanonicalID returns an error.
//
// On any error the returned slice is nil; callers must not apply a partial
// list. This keeps data corruption visible rather than silently dropped.
func Decode(data []byte) ([]types.Partition, error) {
	if len(data) == 0 {
		return []types.Partition{}, nil
	}

	// Gzip magic bytes (0x1f, 0x8b) — the wire-format detection contract.
	isGzip := len(data) > 2 && data[0] == 0x1f && data[1] == 0x8b

	var jsonData []byte
	if isGzip {
		gr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			// Fallback to treating as plain JSON if gzip reader fails immediately.
			jsonData = data
		} else {
			defer gr.Close()
			decompressed, ioErr := io.ReadAll(gr)
			if ioErr != nil {
				return nil, fmt.Errorf("failed to decompress data: %w", ioErr)
			}
			jsonData = decompressed
		}
	} else {
		jsonData = data
	}

	var raw []types.Partition
	if err := json.Unmarshal(jsonData, &raw); err != nil {
		return nil, fmt.Errorf("failed to unmarshal partitions: %w", err)
	}

	// Validate and dedupe by CanonicalID — return an error on first violation.
	// Callers must not apply partial lists when corruption is detected.
	seen := make(map[string]struct{}, len(raw))
	result := make([]types.Partition, 0, len(raw))
	for i, p := range raw {
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("invalid partition at index %d in KV data: %w", i, err)
		}
		cid := p.CanonicalID()
		if _, dup := seen[cid]; dup {
			return nil, fmt.Errorf("duplicate partition at index %d (canonical_id=%q) in KV data", i, cid)
		}
		seen[cid] = struct{}{}
		result = append(result, p)
	}

	return result, nil
}
