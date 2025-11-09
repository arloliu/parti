# Basic Parti Example

This example demonstrates basic Parti usage with default settings.

## What It Shows

- Creating a runtime configuration with defaults
- Setting up a static partition source
- Initializing a Manager with minimal configuration
- Handling assignment changes via hooks
- Graceful startup and shutdown

## Prerequisites

- Go 1.25 or later
- Running NATS server (2.9.0+)

## Running NATS Server

```bash
# Using Docker
docker run -p 4222:4222 -p 8222:8222 nats:latest -js

# Or using nats-server binary
nats-server -js
```

## Running the Example

```bash
# From the repository root
go run ./examples/basic

# Or with custom NATS URL
NATS_URL=nats://localhost:4222 go run ./examples/basic
```

## Core Setup (JetStream-based)

Minimal initialization now requires a JetStream context:

```go
nc, _ := nats.Connect(nats.DefaultURL)
js, _ := jetstream.New(nc)

cfg := parti.Config{WorkerIDPrefix: "example-worker", WorkerIDMin: 0, WorkerIDMax: 9}
partitions := []parti.Partition{{Keys: []string{"partition-0"}, Weight: 100}}
src := source.NewStatic(partitions)
strat := strategy.NewConsistentHash()

mgr, err := parti.NewManager(&cfg, js, src, strat)
if err != nil { log.Fatal(err) }
```

## Expected Output

```
Starting Parti Manager...
State transition: 0 -> 1
State transition: 1 -> 2
State transition: 2 -> 3
State transition: 3 -> 4
Assignment changed:
  Added: 5 partitions
    + [partition-0]
    + [partition-1]
    + [partition-2]
    + [partition-3]
    + [partition-4]
  Removed: 0 partitions
Manager started successfully!
Worker ID: example-worker-0
Is Leader: true
Current assignment (version 1):
  - [partition-0] (weight: 100)
  - [partition-1] (weight: 100)
  - [partition-2] (weight: 100)
  - [partition-3] (weight: 100)
  - [partition-4] (weight: 100)

Manager running. Press Ctrl+C to stop.
```

## Testing Multi-Worker Scenario

Run multiple instances in separate terminals:

```bash
# Terminal 1
go run ./examples/basic

# Terminal 2
go run ./examples/basic

# Terminal 3
go run ./examples/basic
```

You'll see partitions automatically rebalanced across workers as new instances join.

## Configuration Highlights

Defaults applied via `parti.SetDefaults(&cfg)` (implicit inside `NewManager`). Key aspects:

- Worker IDs: 0–9 (`WorkerIDMin/Max`)
- Stabilization Windows: 30s cold start / 10s planned scale (tunable)
- Strategy: Consistent hash (cache affinity >80%)
- JetStream: Provided as `js` (explicit dependency; the old `*nats.Conn` constructor is deprecated)

## Next Steps

- Explore `examples/defender/` for failure-handling patterns
- See `examples/custom-strategy/` to implement a custom `AssignmentStrategy`
- Read `docs/USER_GUIDE.md` for JetStream best practices and tuning
