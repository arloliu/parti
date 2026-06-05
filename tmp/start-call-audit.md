# Start Call-Site Audit (Task 2)

Classification of every `mgr.Start(` / `manager.Start(` / `w.manager.Start(`
call site, ahead of the Manager.Start async refactor. `OK` means the caller
already waits for assignment via `WaitState` / `require.Eventually` / tests
Start-error behaviour. `MIGRATE` means the caller reads `CurrentAssignment()`
or otherwise assumes Stable immediately after Start returns. `REVIEW` means
human inspection done below.

`k8s/cmd/manager/main.go:112` is **NOT** Parti (controller-runtime); excluded.

## MIGRATE (must add WaitState(StateStable, ...) after Start)

| File:line | Notes |
| --- | --- |
| `doc.go:38` | Godoc example. Migrated in Task 11. |
| `manager.go:394` | Manager.Start Godoc example. Migrated in Task 11. |
| `examples/basic/main.go:133` | Basic example reads `mgr.CurrentAssignment()` (line 154). MIGRATE. |
| `test/simulation/internal/worker/worker.go:433` | Simulation worker — needs Stable before producing load. MIGRATE. |
| `test/perf-measurement/cmd/harness/harness.go:493` | IOPS harness. MIGRATE. |
| `internal/testutil/nats.go:313` (`StartWorkers`) | Used by clusters that expect ready managers. MIGRATE inside helper. |
| `internal/testutil/manager_helpers.go` (`StartManagerWithHandoffRecorder`) | Explicitly migrated by Task 10 Step 3. |
| `test/integration/handoff/handoff_sweeper_integration_test.go:67` | Sweeper test reads assignment. MIGRATE. |
| `test/integration/assignment/assignment_helpers_test.go:138, 150` | Assignment helpers used by tests reading CurrentAssignment. MIGRATE. |
| `test/integration/durable/durable_worker_consumer_update_test.go:73` | Drives updates immediately after Start; needs Stable. MIGRATE. |
| `test/integration/durable/durable_helper_test.go:153` | REVIEW → MIGRATE (helper builds the worker fixture expected ready). |
| `test/integration/consumer/dynamic_test.go:277` | Reads assignment & subscribes immediately. MIGRATE. |
| `test/integration/manager/manager_empty_partitions_test.go:51` | Asserts empty assignment after Start — needs Stable to settle. MIGRATE. |
| `test/integration/manager/manager_flag_two_phase_test.go:56` | Reads consumer reports after Start. MIGRATE. |
| `test/integration/handoff/*_test.go` (multiple, see below) | All read handoff state shortly after Start. MIGRATE. |
| `test/integration/provision/provision_e2e_test.go:137, 184, 309` | Reads provision results. MIGRATE. |
| `test/integration/manager/manager_lifecycle_idempotency_test.go:47` | Tests Start idempotency — needs Stable. Line 57 = second Start (error expected); OK already. |
| `test/integration/manager/manager_live_bucket_loss_test.go:195` | Drives bucket loss right after Start. MIGRATE. |

Handoff suite (all MIGRATE):
- `test/integration/handoff/handoff_bucket_ttl_test.go:45`
- `test/integration/handoff/handoff_crash_recovery_test.go:90, 151`
- `test/integration/handoff/handoff_idempotence_test.go:56`
- `test/integration/handoff/handoff_intermediate_states_test.go:76`
- `test/integration/handoff/handoff_lifecycle_test.go:46`
- `test/integration/handoff/handoff_resume_finalize_test.go:60`
- `test/integration/handoff/handoff_startup_hygiene_test.go:63`

## OK (no migration needed)

| File:line | Reason |
| --- | --- |
| `examples/degraded-readiness/main.go:97` | Example demonstrates degraded readiness; does not require Stable. |
| `example_test.go:40` | Godoc-style example; followed by Stop. |
| `manager_audit_wireup_test.go:51, 91` | Wire-up audit; checks audit fields, no immediate assignment read. |
| `manager_capability_reporter_test.go:238, 297` | Already uses `require.Eventually` for capability flag. |
| `manager_claimer_error_test.go:151` | Tests claim path; no assignment read. |
| `manager_handoff_bucket_test.go:71, 110, 188` | Bucket setup tests; line 154 is Start-error path. |
| `manager_handoff_bucket_test.go:154` | Tests Start error. |
| `manager_hooks_test.go:80, 176, 268` | Drives hooks; uses Eventually or expected-error patterns. |
| `manager_initial_bootstrap_test.go:100` | Initial bootstrap inspection — drives via watchers. |
| `manager_max_reconnects_warning_test.go:94` | Asserts warning log; no assignment read. |
| `manager_resolver_reconcile_warning_test.go:112` | Asserts warning log; no assignment read. |
| `manager_stableid_bucket_test.go:54, 91, 125, 221, 263` | StableID bucket setup paths (125 = Start error). |
| `manager_test.go:221, 239` | Already uses WaitState / Eventually patterns. |
| `pull_gating_repro_test.go:158` | Reproducer test; drives via Eventually. |
| `test/integration/assignment/assignment_correctness_test.go:66` | Already drives via Eventually loops. |
| `test/integration/failure/claim_resolver_nats_restart_test.go:419` | Tests restart resilience; uses Eventually. |
| `test/integration/failure/degraded_mode_test.go:46, 110, 197, 256, 333` | Tests degraded; intentionally does not require Stable. |
| `test/integration/failure/failure_error_handling_test.go` (all entries) | Tests error wrapping / failure modes. |
| `test/integration/failure/failure_graceful_shutdown_test.go` (all) | Tests Stop semantics. |
| `test/integration/failure/failure_nats_test.go` (all entries) | Tests NATS failure modes. |
| `test/integration/failure/failure_patterns_test.go` (all) | Tests recovery patterns. |
| `test/integration/manager/manager_behavior_timing_test.go` (all entries) | Already drives via Eventually / explicit waits. |
| `test/integration/manager/manager_kv_size_limit_test.go:80` | Asserts KV-size error path. |
| `test/integration/manager/manager_leader_election_test.go` (all) | Drives via Eventually for leader election. |
| `test/integration/manager/manager_lifecycle_test.go:59, 127` | Lifecycle test; uses Eventually. |
| `test/integration/manager/manager_state_machine_test.go:135` | State-machine drive via Eventually. |
| `test/integration/manager/manager_watcher_test.go:36, 146, 216` | Drives via watcher Eventually. |

## Strategy

Task 10 implements migration for the MIGRATE entries. The two
load-bearing helpers (`internal/testutil/nats.go::StartWorkers` and
`internal/testutil/manager_helpers.go::StartManagerWithHandoffRecorder`)
absorb the bulk of the migration via WaitState inside the helper, so
many test files don't need direct edits. Per-file migration is only
required for callers that bypass these helpers.
