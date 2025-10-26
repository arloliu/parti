// Package types provides core type definitions and interfaces for the Parti library.package types

// This package contains shared types that are used across multiple packages in the
// Parti library. By keeping these types in a separate package, we avoid import cycles
// between the main parti package and its internal implementations.
//
// Key types:
//   - State: Worker lifecycle state
//   - Partition: Logical work partition
//   - Assignment: Versioned partition assignment
//   - Logger: Structured logging interface
//   - MetricsCollector: Metrics recording interface
package types
