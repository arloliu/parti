# Simulation KV and Handoff Faults Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add simulation coverage for connected-but-KV-unavailable timeouts and handoff claim write-fault self-healing.

**Architecture:** Add one reusable simulation fault controller in `test/simulation/cmd/simulation` that wraps the JetStream handle handed to workers. Chaos events arm the controller for bounded windows. Existing degraded/source/oracle gates remain the final authority, with small new counters only where the current gates cannot express non-vacuity.

**Tech Stack:** Go, NATS JetStream `jetstream.JetStream` / `jetstream.KeyValue` wrappers, existing simulation YAML scenarios, existing coordinator oracles.

---

## File Structure

- Create `test/simulation/cmd/simulation/kv_fault_chaos.go`: reusable fault controller, JetStream/KV wrappers, and chaos handlers.
- Modify `test/simulation/internal/coordinator/chaos.go`: add `kv_unavailable` and `handoff_claim_write_fault` chaos event constants and default params.
- Modify `test/simulation/cmd/simulation/main.go`: install the wrapper before worker construction, route new chaos events, and include new final-gate counters.
- Modify `test/simulation/internal/config/config.go`: add optional `chaos.faults` knobs for startup-armed faults.
- Create configs:
  - `test/simulation/configs/chaos_kv_unavailable.yaml`
  - `test/simulation/configs/chaos_handoff_startup_write_fault.yaml`
  - `test/simulation/configs/chaos_handoff_version_write_fault.yaml`
- Add focused unit tests in `test/simulation/cmd/simulation/kv_fault_chaos_test.go`.

## Design

Use a single `simKVFaultController` shared by all workers in all-in-one mode. The wrapper faults only selected KV bucket operations while leaving the underlying NATS connection connected. This mirrors the integration seams in `test/integration/manager/manager_kv_read_unavailable_test.go` and `test/integration/failure/startup_writefault_test.go`, but applies through the real simulation worker/manager path.

The controller supports three fault selectors:

- `kv-unavailable`: fault all active read/write ops on selected buckets with `context.DeadlineExceeded`.
- `handoff-claims-write`: fault only `Create`, `Update`, and `Put` for keys under `claims/` in the handoff bucket.

The final gates will remain positive:

- `kv_unavailable_expected_missing == 0` through `DegradedReasonOracle` expectations for `kv-unavailable`.
- `handoff_claim_write_fault_injected > 0` for non-vacuity.
- Handoff scenarios must still satisfy the existing message, ownership, overlap, degraded, and gap gates.

## Tasks

### Task 1: Add the fault controller and unit tests

**Files:**
- Create: `test/simulation/cmd/simulation/kv_fault_chaos.go`
- Create: `test/simulation/cmd/simulation/kv_fault_chaos_test.go`

- [ ] **Step 1: Write failing tests**

Add tests that construct a real embedded NATS KV bucket, wrap it with `newSimKVFaultJetStream`, and assert:

```go
func TestSimKVFaultController_KVUnavailableFaultsSelectedBucket(t *testing.T)
func TestSimKVFaultController_HandoffClaimWriteFaultOnlyClaimsWrites(t *testing.T)
func TestSimKVFaultController_DisarmRestoresKV(t *testing.T)
```

- [ ] **Step 2: Run red test**

Run: `go test ./test/simulation/cmd/simulation -run 'TestSimKVFaultController'`

Expected: compile failure because the controller does not exist.

- [ ] **Step 3: Implement minimal wrapper**

Implement:

```go
type simKVFaultMode string
type simKVFaultController struct { ... }
type simKVFaultJetStream struct { jetstream.JetStream; fc *simKVFaultController }
type simKVFaultKeyValue struct { jetstream.KeyValue; bucket string; fc *simKVFaultController }
```

Override `KeyValue`, `CreateKeyValue`, `CreateOrUpdateKeyValue`, and KV `Get`, `Put`, `Create`, `Update`.

- [ ] **Step 4: Run green test**

Run: `go test ./test/simulation/cmd/simulation -run 'TestSimKVFaultController'`

Expected: pass.

### Task 2: Wire `kv_unavailable` into simulation

**Files:**
- Modify: `test/simulation/internal/coordinator/chaos.go`
- Modify: `test/simulation/cmd/simulation/main.go`
- Create: `test/simulation/configs/chaos_kv_unavailable.yaml`

- [ ] **Step 1: Add failing dispatch/scenario tests**

Add tests that verify `scenarioHasKVUnavailable` detects both random and scheduled events, and that `kv_unavailable` is recognized as all-in-one only.

- [ ] **Step 2: Run red test**

Run: `go test ./test/simulation/cmd/simulation -run 'TestScenarioHasKVUnavailable|TestKVUnavailable'`

Expected: compile or assertion failure.

- [ ] **Step 3: Implement event wiring**

Add `KVUnavailableEvent ChaosEvent = "kv_unavailable"`, generate default `duration`, `buckets`, and `expect_degraded` params, install `aioKVFaults`, and route the event to `handleKVUnavailableFault`.

- [ ] **Step 4: Add scenario config**

Create `chaos_kv_unavailable.yaml` with three workers, short duration, scheduled sustained `kv_unavailable` against election/heartbeat/stableid buckets, and an expected `kv-unavailable` degraded reason.

- [ ] **Step 5: Run focused tests**

Run: `go test ./test/simulation/cmd/simulation -run 'TestSimKVFaultController|TestScenarioHasKVUnavailable|TestKVUnavailable'`

Expected: pass.

### Task 3: Wire handoff claim write-fault scenarios

**Files:**
- Modify: `test/simulation/internal/coordinator/chaos.go`
- Modify: `test/simulation/cmd/simulation/main.go`
- Create: `test/simulation/configs/chaos_handoff_startup_write_fault.yaml`
- Create: `test/simulation/configs/chaos_handoff_version_write_fault.yaml`

- [ ] **Step 1: Add failing tests**

Add tests that verify startup-armed claim fault config arms the controller before workers start, and that `handoff_claim_write_fault` is detected by a scenario helper.

- [ ] **Step 2: Run red test**

Run: `go test ./test/simulation/cmd/simulation -run 'TestHandoffClaimWriteFault|TestScenarioHasHandoffClaimWriteFault'`

Expected: compile or assertion failure.

- [ ] **Step 3: Implement startup and scheduled wiring**

Add `HandoffClaimWriteFaultEvent`, startup config under `chaos.faults`, and event handling that arms claim-write fault plus optional heartbeat-write fault for a bounded duration.

- [ ] **Step 4: Add scenario configs**

Add a startup scenario that arms claim-write fault at process start and disarms automatically, plus a version-advance scenario that schedules `scale_down` while claim writes are faulted.

- [ ] **Step 5: Run focused tests**

Run: `go test ./test/simulation/cmd/simulation -run 'TestSimKVFaultController|TestHandoffClaimWriteFault|TestScenarioHasHandoffClaimWriteFault'`

Expected: pass.

### Task 4: Validation

**Files:** all touched Go/config files.

- [ ] **Step 1: Format**

Run: `gofmt -w test/simulation/cmd/simulation/kv_fault_chaos.go test/simulation/cmd/simulation/kv_fault_chaos_test.go test/simulation/cmd/simulation/main.go test/simulation/internal/coordinator/chaos.go test/simulation/internal/config/config.go`

- [ ] **Step 2: Go fix affected packages**

Run: `go fix ./test/simulation/cmd/simulation ./test/simulation/internal/coordinator ./test/simulation/internal/config`

- [ ] **Step 3: Targeted tests**

Run: `go test ./test/simulation/cmd/simulation ./test/simulation/internal/coordinator ./test/simulation/internal/config`

- [ ] **Step 4: Simulation package tests**

Run: `go test ./test/simulation/...`

- [ ] **Step 5: Lint**

Run: `make lint`

Expected: all commands pass before the work is called complete.
