// Package jsutil provides utilities for working with NATS JetStream streams and consumers.
//
// It offers helper functions for robust stream and consumer initialization, handling common
// race conditions and transient errors that can occur in distributed systems.
//
// Key features:
//   - Robust stream creation with retry logic and race condition handling
//   - Robust consumer creation with retry logic
//   - Consumer name validation utilities
//   - Simplified configuration for common use cases
//
// Example:
//
//	// Ensure a stream exists
//	stream, err := jsutil.EnsureStream(ctx, js, jetstream.StreamConfig{
//	    Name:     "EVENTS",
//	    Subjects: []string{"events.>"},
//	})
//
//	// Ensure a consumer exists
//	consumer, err := jsutil.EnsureConsumer(ctx, js, "EVENTS", jetstream.ConsumerConfig{
//	    Durable: "my-consumer",
//	})
package jsutil
