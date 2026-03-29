# 400 - Documentation Standards

## General
- **Godoc:** All exported symbols MUST have doc comments.
- **First Line:** Start with the symbol name. One-line summary.
- **README:** Keep updated with install/usage.

## Godoc Template (MANDATORY)

```go
// FunctionName one-line summary.
//
// Detailed description (optional but recommended).
//
// Parameters:
//   - param1: Description and constraints
//   - param2: Expected values
//
// Returns:
//   - Type: What it represents
//   - error: Conditions that cause errors
//
// Example:
//
//	result, err := FunctionName(input)
//	if err != nil { ... }
func FunctionName(param1 T1, param2 T2) (Result, error) { }
```

## Examples by Type

**Constructor:**
```go
// NewConsistentHash creates a consistent hash assignment strategy.
//
// Parameters:
//   - opts: Functional options (e.g., WithVirtualNodes)
//
// Returns:
//   - *ConsistentHash: Ready-to-use strategy instance
func NewConsistentHash(opts ...Option) *ConsistentHash { }
```

**Method with Multiple Returns:**
```go
// Assign calculates partition assignments for the given workers.
//
// Parameters:
//   - workers: Worker IDs to assign partitions to
//   - partitions: Available partitions
//
// Returns:
//   - map[string][]Partition: Worker ID to assigned partitions
//   - error: If assignment calculation fails
func (c *ConsistentHash) Assign(workers []string, partitions []Partition) (map[string][]Partition, error) { }
```

## Omit When Appropriate
- No params → Omit Parameters section.
- No returns → Omit Returns section.
- Simple getters → Minimal doc is OK.
