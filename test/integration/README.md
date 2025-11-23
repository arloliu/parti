# Integration Test Suite

This directory contains end-to-end tests that exercise multiple components of the parti system using embedded NATS where needed. Tests are organized by domain using a consistent filename prefix to make navigation and intent clear.

## Naming Conventions

- assignment_*: Partition assignment, partition sources, strategy behavior, and invariants
- manager_*: Manager lifecycle, state machine transitions, leader election, watchers, and claimers
- failure_*: Failure and resilience scenarios (NATS outages, error handling, graceful shutdown)
- emergency_* / degraded_*: Emergency mode, hysteresis behavior, degraded operation
- timing_* / behavior_*: Cross-cutting scenario and timing-focused tests
- subscription_*: JetStream subscription helpers, durable consumer behavior, and worker consumer flows

Examples:
- assignment_invariants_smoke_test.go – quick invariant checks around assignment correctness
- manager_state_machine_test.go – state transitions and timing around the manager
- failure_nats_test.go – behavior under NATS-level failures
- subscription_durable_helper_test.go – durable consumer lifecycle and message handling

## Guidelines

- Keep files focused by domain; add new tests to the closest existing file when appropriate.
- If a scenario spans multiple domains, prefer the dominant concern (e.g., manager_ vs failure_). Add a short header comment clarifying scope if needed.
- Avoid long sleeps; prefer event-driven assertions and collectors to reduce flakiness.
- Integration tests should be runnable via:

```
go test -count=1 ./test/integration
```

For long-running performance tests, see `test/stress/` (gated via PARTI_STRESS). The `assignment_invariants_smoke_test.go` file provides a fast sanity check for critical invariants.
