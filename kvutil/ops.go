package kvutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nats-io/nats.go/jetstream"
)

// GetJSON retrieves a value from KV and unmarshals it into the target.
//
// Parameters:
//   - ctx: Context for cancellation.
//   - kv: The KeyValue bucket.
//   - key: The key to retrieve.
//
// Returns:
//   - *T: Pointer to the unmarshaled value, or nil if key not found.
//   - uint64: The revision of the key, or 0 if not found.
//   - error: Error if retrieval or unmarshaling fails.
func GetJSON[T any](ctx context.Context, kv jetstream.KeyValue, key string) (*T, uint64, error) {
	entry, err := kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("kv get %q: %w", key, err)
	}

	var target T
	if err := json.Unmarshal(entry.Value(), &target); err != nil {
		return nil, entry.Revision(), fmt.Errorf("kv unmarshal %q: %w", key, err)
	}

	return &target, entry.Revision(), nil
}

// PutJSON marshals the value and puts it into KV.
//
// Parameters:
//   - ctx: Context for cancellation.
//   - kv: The KeyValue bucket.
//   - key: The key to set.
//   - value: The value to marshal and store.
//
// Returns:
//   - uint64: The new revision of the key.
//   - error: Error if marshaling or put fails.
func PutJSON[T any](ctx context.Context, kv jetstream.KeyValue, key string, value T) (uint64, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return 0, fmt.Errorf("kv marshal %q: %w", key, err)
	}

	rev, err := kv.Put(ctx, key, data)
	if err != nil {
		return 0, fmt.Errorf("kv put %q: %w", key, err)
	}

	return rev, nil
}

// UpdateJSON atomically updates a key using CAS (compare-and-swap) semantics.
//
// The update only succeeds if the key's current revision matches the provided
// revision. This enables optimistic locking patterns where you:
//  1. Get the current value and revision with GetJSON
//  2. Modify the value
//  3. Update with UpdateJSON using the original revision
//
// If another process modified the key between steps 1 and 3, the update fails
// and you should retry the read-modify-write cycle.
//
// Parameters:
//   - ctx: Context for cancellation.
//   - kv: The KeyValue bucket.
//   - key: The key to update.
//   - value: The new value to marshal and store.
//   - revision: The expected current revision (from a prior GetJSON call).
//
// Returns:
//   - uint64: The new revision of the key.
//   - error: Error if revision doesn't match (CAS failure), or other errors.
//
// Example:
//
//	// Read-modify-write pattern
//	val, rev, _ := kvutil.GetJSON[MyStruct](ctx, kv, "key")
//	val.Counter++
//	newRev, err := kvutil.UpdateJSON(ctx, kv, "key", val, rev)
//	if errors.Is(err, jetstream.ErrWrongLastSequence) {
//	    // Retry: another process modified the key
//	}
func UpdateJSON[T any](ctx context.Context, kv jetstream.KeyValue, key string, value T, revision uint64) (uint64, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return 0, fmt.Errorf("kv marshal %q: %w", key, err)
	}

	rev, err := kv.Update(ctx, key, data, revision)
	if err != nil {
		return 0, fmt.Errorf("kv update %q: %w", key, err)
	}

	return rev, nil
}

// ListKeys returns all keys in the bucket that match the given prefix.
//
// Parameters:
//   - ctx: Context for cancellation.
//   - kv: The KeyValue bucket.
//   - prefix: The prefix to filter keys by.
//   - stripPrefix: If true, the prefix is removed from the returned keys.
//
// Returns:
//   - []string: List of matching keys.
//   - error: Error if listing keys fails.
func ListKeys(ctx context.Context, kv jetstream.KeyValue, prefix string, stripPrefix bool) ([]string, error) {
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("kv keys: %w", err)
	}

	if prefix == "" {
		return keys, nil
	}

	var out []string
	for _, k := range keys {
		if strings.HasPrefix(k, prefix) {
			if stripPrefix {
				out = append(out, strings.TrimPrefix(k, prefix))
			} else {
				out = append(out, k)
			}
		}
	}

	return out, nil
}

// DeleteKey deletes a key from the KV bucket, ignoring ErrKeyNotFound.
//
// Parameters:
//   - ctx: Context for cancellation.
//   - kv: The KeyValue bucket.
//   - key: The key to delete.
//
// Returns:
//   - error: Error if delete fails (other than key not found).
func DeleteKey(ctx context.Context, kv jetstream.KeyValue, key string) error {
	err := kv.Delete(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil
		}
		return fmt.Errorf("kv delete %q: %w", key, err)
	}

	return nil
}
