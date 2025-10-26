# Project Structure

**Status**: Approved
**Date**: October 26, 2025
**Version**: 1.0

---

## Overview

This document defines the final project structure for the Parti library. The structure follows Go idioms with a root-level public API, strategic subpackages for domain-specific functionality, and private implementation details in `internal/`.

## Design Principles

1. **Root-level public API** - Main `parti` package at repository root for simple imports
2. **Strategic subpackages** - Separate domains (strategy, source, subscription) into subpackages
3. **Factory functions** - Root package provides convenience factories wrapping subpackages
4. **Internal isolation** - All implementation details hidden in `internal/`
5. **3-file maximum rule** - Each component: implementation + test + optional benchmark

## Directory Structure

```
parti/                                  # Root = main public package
├── doc.go                              # Package documentation
├── manager.go                          # Manager interface + NewManager()
├── interfaces.go                       # Core interfaces (PartitionSource, AssignmentStrategy, ElectionAgent)
├── config.go                           # Config types and parsing
├── partition.go                        # Partition, Assignment types
├── state.go                            # State enum and constants
├── options.go                          # Functional options + factory functions
├── errors.go                           # Sentinel errors
├── manager_test.go
├── config_test.go
├── partition_test.go
│
├── strategy/                           # Assignment strategies subpackage
│   ├── doc.go                          # Package documentation
│   ├── consistent_hash.go              # WeightedConsistentHash implementation
│   ├── consistent_hash_test.go
│   ├── round_robin.go                  # RoundRobin implementation
│   └── round_robin_test.go
│
├── source/                             # Partition sources subpackage
│   ├── doc.go                          # Package documentation
│   ├── static.go                       # Static source implementation
│   └── static_test.go
│
├── subscription/                       # Subscription helper subpackage
│   ├── doc.go                          # Package documentation
│   ├── helper.go                       # Subscription helper implementation
│   └── helper_test.go
│
├── internal/                           # Private implementation (not importable)
│   ├── manager/                        # Manager implementation
│   │   ├── manager.go                  # Core manager logic
│   │   ├── manager_test.go
│   │   └── state_machine.go            # State machine transitions
│   │
│   ├── election/                       # Election implementations
│   │   ├── nats.go                     # NATS KV-based election (default)
│   │   ├── nats_test.go
│   │   ├── agent.go                    # External election-agent wrapper
│   │   └── agent_test.go
│   │
│   ├── heartbeat/                      # Heartbeat publisher
│   │   ├── publisher.go
│   │   └── publisher_test.go
│   │
│   ├── assignment/                     # Assignment calculation
│   │   ├── calculator.go               # Assignment version tracking
│   │   ├── calculator_test.go
│   │   └── affinity.go                 # Cache affinity scoring
│   │
│   ├── stableid/                       # Stable ID management
│   │   ├── claimer.go                  # ID claiming logic
│   │   └── claimer_test.go
│   │
│   └── hash/                           # Hash utilities
│       ├── ring.go                     # Consistent hash ring
│       └── ring_test.go
│
├── examples/                           # Example programs (not cmd/)
│   ├── basic/                          # Simple usage example
│   │   └── main.go
│   ├── defender/                       # Defender use case example
│   │   └── main.go
│   └── custom-strategy/                # Advanced customization example
│       └── main.go
│
├── docs/                               # Design documentation
│   ├── library-specification.md
│   ├── migration-module-discussion.md
│   ├── README.md
│   └── design/
│       ├── 01-requirements/
│       ├── 02-problem-analysis/
│       ├── 03-architecture/
│       ├── 04-components/
│       ├── 05-operational-scenarios/
│       └── 06-implementation/
│           └── project-structure.md    # This file
│
├── .github/
│   └── copilot-instructions.md         # Coding standards
│
├── go.mod                              # Module definition
├── go.sum                              # Dependency checksums
└── README.md                           # Main project README
```

## Package Responsibilities

### Root Package (`parti`)

**Purpose**: Main public API with high-level interfaces and factory functions.

**Contents**:
- `Manager` interface and lifecycle management
- Core interfaces: `PartitionSource`, `AssignmentStrategy`, `ElectionAgent`
- Configuration types and parsing
- Functional options pattern (`WithStrategy`, `WithHooks`, etc.)
- Convenience factory functions wrapping subpackages
- Common types: `Partition`, `Assignment`, `State`

**Import**: `import "github.com/arlolib/parti"`

### Subpackage: `strategy`

**Purpose**: Built-in assignment strategy implementations.

**Contents**:
- `ConsistentHash` - Weighted consistent hashing with virtual nodes
- `RoundRobin` - Simple round-robin distribution

**Import**: `import "github.com/arlolib/parti/strategy"` (only for advanced customization)

**Factory Access**: `parti.ConsistentHashStrategy(virtualNodes)` (recommended)

### Subpackage: `source`

**Purpose**: Built-in partition source implementations.

**Contents**:
- `Static` - Fixed list of partitions

**Import**: `import "github.com/arlolib/parti/source"` (only for advanced customization)

**Factory Access**: `parti.StaticSource(partitions)` (recommended)

### Subpackage: `subscription`

**Purpose**: NATS subscription management utilities.

**Contents**:
- `Helper` - Automatic subscription reconciliation and retry logic

**Import**: `import "github.com/arlolib/parti/subscription"`

**Factory Access**: `parti.NewSubscriptionHelper(conn, cfg)`

### Internal Packages

**Purpose**: Private implementation details, not importable by external code.

**Packages**:
- `internal/manager` - Core manager implementation coordinating all components
- `internal/election` - Leader election (NATS KV and external agent)
- `internal/heartbeat` - Background heartbeat publisher
- `internal/assignment` - Assignment calculation and versioning
- `internal/stableid` - Stable ID claiming/release
- `internal/hash` - Hash ring and utilities

## Usage Patterns

### Simple Usage (90% Case)

Users import only the root package and use factory functions:

```go
import "github.com/arlolib/parti"

cfg := parti.Config{
    WorkerIDPrefix: "defender",
    WorkerIDMin:    0,
    WorkerIDMax:    63,
}

// Factory functions hide subpackage details
src := parti.StaticSource(partitions)
mgr := parti.NewManager(&cfg, natsConn, src)

if err := mgr.Start(ctx); err != nil {
    log.Fatal(err)
}
```

### Advanced Usage (10% Case)

Users import subpackages directly for customization:

```go
import (
    "github.com/arlolib/parti"
    "github.com/arlolib/parti/strategy"
    "github.com/arlolib/parti/subscription"
)

// Direct subpackage access for customization
strat := strategy.NewConsistentHash(
    strategy.WithVirtualNodes(300),
    strategy.WithHashSeed(12345),
)

subHelper := subscription.NewHelper(natsConn, subscription.Config{
    MaxRetries:        5,
    RetryBackoff:      2 * time.Second,
    ReconcileInterval: 30 * time.Second,
})

hooks := &parti.Hooks{
    OnAssignmentChanged: func(ctx context.Context, added, removed []parti.Partition) error {
        return subHelper.UpdateSubscriptions(ctx, added, removed, handler)
    },
}

mgr := parti.NewManager(&cfg, natsConn, src,
    parti.WithStrategy(strat),
    parti.WithHooks(hooks),
)
```

## Factory Functions Pattern

Root package provides convenience factories that wrap subpackage constructors:

```go
// parti/options.go

// ConsistentHashStrategy creates a weighted consistent hash strategy.
func ConsistentHashStrategy(virtualNodes int) AssignmentStrategy {
    return strategy.NewConsistentHash(strategy.WithVirtualNodes(virtualNodes))
}

// RoundRobinStrategy creates a round-robin assignment strategy.
func RoundRobinStrategy() AssignmentStrategy {
    return strategy.NewRoundRobin()
}

// StaticSource creates a partition source with a fixed list of partitions.
func StaticSource(partitions []Partition) PartitionSource {
    return source.NewStatic(partitions)
}

// NewSubscriptionHelper creates a subscription helper with automatic reconciliation.
func NewSubscriptionHelper(conn *nats.Conn, cfg subscription.Config) *subscription.Helper {
    return subscription.NewHelper(conn, cfg)
}
```

## Examples Organization

The `examples/` directory contains runnable example programs demonstrating library usage:

```bash
# Run basic example
go run ./examples/basic

# Run defender use case example
go run ./examples/defender

# Run custom strategy example
go run ./examples/custom-strategy
```

Each example is self-contained and shows different aspects:
- **basic**: Minimal setup with defaults
- **defender**: Real-world Defender application scenario
- **custom-strategy**: Advanced customization patterns

## Rationale

### Why Root-Level Package?

**Advantages**:
- Simpler import path: `import "github.com/arlolib/parti"`
- Matches repository name
- Common for single-purpose libraries (net/http, database/sql, context)
- All core interfaces visible in one godoc page

**Disadvantages**: None significant for a library this size

### Why Subpackages for strategy/source/subscription?

**Advantages**:
- Domain separation without polluting root namespace
- Advanced users can import directly for customization
- Simple users use factory functions and never see subpackages
- Allows independent evolution of strategy implementations

**Real-World Examples**:
- `net/http` (root) + `net/http/httputil` (utilities)
- `database/sql` (root) + `database/sql/driver` (implementations)
- `encoding/json` (root) - simple, no subpackages needed
- `github.com/nats-io/nats.go` - flat structure with factories

### Why examples/ Instead of cmd/?

**Rationale**:
- `cmd/` is for CLI tools and binaries the library provides
- `examples/` is for demonstration code
- Parti is a **library**, not a tool
- Matches Go ecosystem conventions (nats.go, redis, prometheus client)

### Why internal/?

**Rationale**:
- Hides implementation details from public API
- Allows refactoring without breaking users
- Enforces interface-based design
- Go compiler prevents imports of `internal/` from external packages

## Migration Path

The current temporary structure `internal/manager/{doc.go,interfaces.go,config.go}` will be reorganized:

1. Move `internal/manager/*.go` → root `parti/*.go`
2. Create `strategy/`, `source/`, `subscription/` subpackages
3. Add factory functions in `parti/options.go`
4. Create stub implementations in `internal/manager/manager.go`
5. Add working examples in `examples/basic/`

## File Organization Rules

Following `.github/copilot-instructions.md`:

1. **3-file maximum per component**:
   - `component.go` (implementation)
   - `component_test.go` (unit tests)
   - `component_bench_test.go` (benchmarks, optional)

2. **File content order**:
   - Package declaration
   - Imports (grouped: stdlib, external, internal)
   - Constants (exported first)
   - Variables (exported first)
   - Types (exported first)
   - Factory functions (immediately after type)
   - Exported functions
   - Unexported functions
   - Exported methods (grouped by receiver)
   - Unexported methods (grouped by receiver)

3. **Documentation**:
   - All exported items must have godoc comments
   - Follow standardized format from copilot-instructions.md
   - Include Parameters, Returns, and Example sections

## Next Steps

1. ✅ Document project structure (this file)
2. ⏳ Reorganize files from `internal/manager` to root `parti/`
3. ⏳ Create `strategy/`, `source/`, `subscription/` subpackages
4. ⏳ Implement factory functions in `parti/options.go`
5. ⏳ Create working example in `examples/basic/`
6. ⏳ Add unit test stubs for core interfaces

---

**Approved By**: Design Review
**Implementation Status**: Ready to proceed
