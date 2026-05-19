# 100 - Project Map

## Identity
- **Project:** Parti (NATS-based work partitioning)
- **Module:** `github.com/arloliu/parti/v2`
- **Language:** Go >=1.25.0
- **Linting:** `golangci-lint` v2.11.4 (via `make lint`)

## What Parti Does
Parti dynamically assigns work partitions across worker instances using NATS JetStream for coordination. It provides stable worker IDs, leader-based assignment, and cache-affinity-aware rebalancing.

## Project Structure
```
parti/                        # Root = main public package (Manager, Config, Hooks)
├── consumer/                 # Unified JetStream consumer API (Queue, Static, Dynamic, Broadcast)
├── partition/                # Static partition routing (publish + core-NATS subscribe)
├── strategy/                 # Assignment strategies (ConsistentHash, WeightedConsistentHash, RoundRobin)
├── source/                   # Partition sources (Static, NatsKV)
├── types/                    # Shared contracts — leaf package (interfaces, errors, state)
├── partitest/                # Public test helpers (embedded NATS, logger)
├── jsutil/                   # JetStream utilities (stream/consumer helpers)
├── kvutil/                   # KV bucket utilities
├── internal/                 # Private implementation
│   ├── assignment/           # Assignment calculation
│   ├── durable/              # JetStream durable consumers
│   ├── election/             # Leader election (NATS KV)
│   ├── heartbeat/            # Heartbeat publisher
│   ├── hooks/                # Hook dispatch
│   ├── ipartition/           # Internal partition types (JSConsumer, KeyExtractor)
│   ├── stableid/             # Stable ID claiming
│   ├── hash/                 # Hash utilities
│   ├── logging/              # Logger helpers
│   ├── metrics/              # Metrics helpers
│   ├── natsutil/             # NATS connection utilities
│   ├── partutil/             # Shared pattern/validation utilities
│   └── testutil/             # Internal test utilities
├── test/                     # Integration, simulation & stress tests
│   ├── integration/
│   ├── simulation/
│   └── stress/
├── examples/                 # Example programs (basic, kv-watcher)
├── scripts/                  # Operational scripts (inspect_consumers, gap_timeline)
└── docs/                     # Design & user documentation
```

## Architecture Notes
- **Import cycle prevention:** The `types/` package is a leaf. Root `parti` package re-exports types via aliases (`parti.Partition`, `parti.State`, etc.). Internal packages import `types/`, never the root.
- **Interface assertions:** Use `var _ Interface = (*Type)(nil)` in `internal/` packages only. In public packages (`strategy/`, `source/`, `consumer/`), use assertions in `_test.go` files to avoid import cycles.

## Dependency Policy
- Check `go.mod` before adding dependencies.
- Prefer the standard library.
- Ask before adding a new dependency.
- Blocked dependencies are enforced by `gomodguard`:
    - `github.com/golang/protobuf` -> use `google.golang.org/protobuf`
    - `github.com/satori/go.uuid` -> use `github.com/google/uuid`
    - `github.com/gofrs/uuid` -> use `github.com/google/uuid`

## Key Dependencies
- **NATS:** `github.com/nats-io/nats.go` (core + JetStream)
- **Hashing:** `github.com/zeebo/xxh3` (xxh3 for partition routing)
- **Metrics:** `github.com/prometheus/client_golang`
- **Config/Validation:** `github.com/arloliu/fuda`, `github.com/go-playground/validator/v10`
- **Testing:** `github.com/stretchr/testify`
