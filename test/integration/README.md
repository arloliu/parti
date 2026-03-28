# Integration Test Suite

This directory contains end-to-end tests that exercise multiple components of the parti system using embedded NATS where needed. Tests are organized by domain into sub-packages to allow for parallel execution and better organization.

## Directory Structure

- **consumer/**: Integration tests for the unified consumer API (`Queue`, `Static`, `Dynamic`, `Broadcast`, `WIPHandler`). Tests the public `consumer` package that users import directly.
- **partition/**: Static partition-based publish/subscribe (`JSConsumer`, `Publisher`, `JSPublisher`, `Subscriber`).
- **subscription/**: Internal subscription-layer tests (`WorkerConsumer` internals, `ProcessingGate`, durable helpers, iterator recovery).
- **assignment/**: Partition assignment, partition sources, strategy behavior, and invariants.
- **manager/**: Manager lifecycle, state machine transitions, leader election, watchers, and claimers.
- **failure/**: Failure and resilience scenarios (NATS outages, error handling, graceful shutdown, emergency mode).
- **handoff/**: Partition handoff scenarios.
- **stableid/**: Stable ID generation and management.
- **misc/**: Miscellaneous tests that don't fit into other categories.

## Guidelines

- Keep files focused by domain; add new tests to the appropriate subdirectory.
- If a scenario spans multiple domains, prefer the dominant concern.
- Avoid long sleeps; prefer event-driven assertions and collectors to reduce flakiness.
- Integration tests should be runnable via:

```bash
go test -count=1 ./test/integration/...
```

For long-running performance tests, see `test/stress/` (gated via PARTI_STRESS).
