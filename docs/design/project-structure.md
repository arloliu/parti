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
- Functional options (`WithHooks`, `WithLogger`, `WithMetrics`, `WithElectionAgent`, `WithWorkerConsumerUpdater`, etc.)
- Common types: `Partition`, `Assignment`, `State`

**Import**: `import "github.com/arloliu/parti"`

### Subpackage: `strategy`

**Purpose**: Built-in assignment strategy implementations.

**Contents**:
- `ConsistentHash` - Weighted consistent hashing with virtual nodes
- `RoundRobin` - Simple round-robin distribution

**Import**: `import "github.com/arloliu/parti/strategy"`

**Constructors**:
- `strategy.NewConsistentHash(...)`
- `strategy.NewWeightedConsistentHash(...)`
- `strategy.NewRoundRobin()`

### Subpackage: `source`

**Purpose**: Built-in partition source implementations.

**Contents**:
- `Static` - Fixed list of partitions

**Import**: `import "github.com/arloliu/parti/source"`

**Constructors**:
- `source.NewStatic(partitions)`
- `source.NewNatsKV(js, bucket)`

### Subpackage: `subscription`

**Purpose**: NATS subscription management utilities.

**Contents**:
- `WorkerConsumer` - Per-worker durable consumer with partition-scoped filters
- `BroadcastConsumer` - Wildcard consumer for fan-out streams
- `ProcessingGate` - Optional ownership/state-based processing enforcement

**Import**: `import "github.com/arloliu/parti/subscription"`

**Constructors**:
- `subscription.NewWorkerConsumer(js, cfg, handler)`
- `subscription.NewBroadcastConsumer(js, cfg, handler)`

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

Users import the root package plus built-in strategy/source packages:

```go
import (
    "github.com/arloliu/parti"
    "github.com/arloliu/parti/source"
    "github.com/arloliu/parti/strategy"
    "github.com/nats-io/nats.go/jetstream"
)

cfg := parti.Config{
    WorkerIDPrefix: "defender",
    WorkerIDMin:    0,
    WorkerIDMax:    63,
}

partitions := []parti.Partition{{ID: "0"}, {ID: "1"}, {ID: "2"}}
src := source.NewStatic(partitions)
strat := strategy.NewConsistentHash()
js, _ := jetstream.New(natsConn)
mgr, err := parti.NewManager(&cfg, js, src, strat)
if err != nil {
    log.Fatal(err)
}

if err := mgr.Start(ctx); err != nil {
    log.Fatal(err)
}
```

### Advanced Usage (10% Case)

Users import subpackages directly for customization:

```go
import (
    "context"

    "github.com/arloliu/parti"
    "github.com/arloliu/parti/source"
    "github.com/arloliu/parti/strategy"
    "github.com/arloliu/parti/subscription"
    "github.com/nats-io/nats.go/jetstream"
)

cfg := parti.Config{
    WorkerIDPrefix: "defender",
    WorkerIDMin:    0,
    WorkerIDMax:    63,
}

partitions := []parti.Partition{{ID: "0"}, {ID: "1"}, {ID: "2"}}
src := source.NewStatic(partitions)

js, _ := jetstream.New(natsConn)

// Direct subpackage access for customization
strat := strategy.NewConsistentHash(
    strategy.WithVirtualNodes(300),
    strategy.WithHashSeed(12345),
)

// Consumer helper wired to the manager for automatic filter updates.
wc, _ := subscription.NewWorkerConsumer(js, subscription.WorkerConsumerConfig{
    StreamName:      "events",
    ConsumerPrefix:  "worker",
    SubjectTemplate: "events.{{.PartitionID}}",
}, subscription.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
    // Handle message
    return nil
}))

hooks := &parti.Hooks{
    OnAssignmentChanged: func(ctx context.Context, oldPartitions, newPartitions []parti.Partition) error {
        // Called with full assignment sets.
        return nil
    },
}

mgr, err := parti.NewManager(&cfg, js, src, strat,
    parti.WithHooks(hooks),
    parti.WithWorkerConsumerUpdater(wc),
)
if err != nil {
    log.Fatal(err)
}
```

## Factory Functions Pattern

Parti keeps strategy/source implementations in subpackages (`strategy/`, `source/`).
Use their constructors directly (for example, `strategy.NewConsistentHash()` and `source.NewStatic(...)`).

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
- Simpler import path: `import "github.com/arloliu/parti"`
- Matches repository name
- Common for single-purpose libraries (net/http, database/sql, context)
- All core interfaces visible in one godoc page

**Disadvantages**: None significant for a library this size

### Why Subpackages for strategy/source/subscription?

**Advantages**:
- Domain separation without polluting root namespace
- Advanced users can import directly for customization
- Built-ins live in subpackages while the root package provides core interfaces and types
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
