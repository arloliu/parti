---
name: qa-review
description: Perform a critical review focused on correctness, fault tolerance, and performance implications of a Go library from the perspective of external users.
---

# QA Review - Go Library Robustness and Correctness

**Assumed Role:** Quality Assurance (QA) Engineer.

**Testing Premise:** Your testing plan relies on the public API (Godoc) and the README as the primary specifications. You need to ensure the library is robust, reliable, and compliant with its published contract.

When executing this skill, perform a critical review focused on **correctness, fault tolerance, and performance implications**, specifically addressing the following points from the perspective of a user who intends to misuse the library:

## Scope

When reviewing specific packages, specify them by name. Default scope for Parti:
- Root package (`parti`): `Manager`, `Config`, `Hooks`, `Options`
- `consumer/`: `Queue`, `Static`, `Dynamic`, `Broadcast` consumer types
- `partition/`: `Publisher`, `JSPublisher`, `Subscriber`, routing config
- `strategy/`: `ConsistentHash`, `WeightedConsistentHash`, `RoundRobin`
- `source/`: `Static`, `NatsKV` partition sources
- `types/`: Shared interfaces and error sentinels

## 1. Functional Correctness and Compliance Testing

1. **Public API Contract Gaps:**
    * Identify any ambiguity where the Godoc describes behavior but provides insufficient detail, such as **data structure ordering, subject pattern syntax, or exact side effects** upon function calls.
    * Are there **implicit, undocumented limitations** on input values (e.g., maximum partition count, required non-zero values, partition key format constraints) that are enforced by the code but not specified in the documentation?

2. **Edge Case Identification & Initialization:**
    * Identify critical **zero-value or nil-pointer dereference** risks in public methods, especially when optional parameters or configurations are not provided during initialization.
    * List required fields or settings in `Config`, `PartitionConfig`, or consumer options that **will panic or return a non-idiomatic error** if omitted during construction.
    * Check that `Partition.HashID()` correctly handles all edge cases for its key set.

## 2. Fault Tolerance and Error Handling

1. **Error Propagation and Inspection:**
    * Analyze the **error propagation strategy**. Does the library consistently use Go's standard error wrapping (`fmt.Errorf` with `%w`) to preserve the error chain, or does it discard original error information?
    * Does the library define and export **sentinel errors** (in `types/errors.go` and re-exported via root `errors.go`) for common failures, allowing `errors.Is()` usage?
    * Are all raw string errors replaced with proper sentinel errors?

2. **Resource Management Safety:**
    * For types requiring **cleanup** (`Manager.Stop()`, consumer `.Stop()`, subscriber `.Close()`), are these methods safe to call multiple times? Is there documentation on the consequences if cleanup is *not* called?
    * Are **NATS reconnection, JetStream timeouts, and context cancellation** handled gracefully without leaking goroutines?

## 3. Non-Functional Concerns (Concurrency and Performance)

1. **Concurrency Guarantees:**
    * Verify all **thread-safety guarantees** for exported types. `Manager` is used concurrently — are all state mutations properly synchronized?
    * Check that assignment callbacks (`Hooks.OnAssignmentChanged`) document whether they are called from a dedicated goroutine or the caller's goroutine.
    * Verify that `CurrentAssignment()` return values document read-only constraints (no deep copy = shared slice headers).

2. **Performance and Memory:**
    * Identify functions involving **deep copying of large data structures** (partition lists, assignment maps) that could cause GC pressure under high partition counts.
    * Check that consistent hash ring operations scale appropriately with the configured number of virtual nodes.
    * Look for **unbounded growth** in internal maps/slices (e.g., heartbeat tracking, election state).
