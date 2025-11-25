// Package kvutil provides utilities for working with NATS JetStream KeyValue stores.
//
// It offers helper functions for robust KV bucket initialization, handling common
// race conditions and transient errors that can occur in distributed systems.
//
// Key features:
//   - Robust bucket creation with retry logic
//   - Handling of race conditions (e.g., ErrBucketExists)
//   - Simplified configuration for common use cases
package kvutil
