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

## Configuration

The example uses default configuration. Key settings:

- **Worker ID Range**: 0-9
- **Worker ID Prefix**: "example-worker"
- **Stabilization Window**: 30s for cold start, 10s for planned scale
- **Assignment Strategy**: Weighted Consistent Hash (default)

## Next Steps

- See `examples/defender/` for a real-world use case
- See `examples/custom-strategy/` for advanced customization
