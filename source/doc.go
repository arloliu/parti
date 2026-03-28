// Package source provides built-in partition source implementations.
//
// Partition sources discover available partitions for assignment.
// The package includes:
//
//   - Static: Fixed list of partitions defined at compile time.
//   - NatsKV: Dynamic partition list backed by a NATS KeyValue bucket,
//     supporting live updates via KV watches (implements PartitionSource,
//     PartitionUpdater, and WatchablePartitionSource).
//
// Custom sources can be implemented by satisfying the [types.PartitionSource] interface.
package source
