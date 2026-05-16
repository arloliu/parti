# Precision Pass Review

## Summary

The architecture is settled and I did not find a surprise P0. The full-pass cleanup is mostly applied: heartbeat dual decoding, post-alias leadership fencing, dual-read fallback, explicit source API signatures, and `AppliedSourceRevKnown` audit semantics are now present in the spec. The remaining issues are precision/documentation hazards that could make phase agents implement slightly different APIs or chase stale references. Fix the P1 items before implementation dispatch; the P2 items can be cleaned in the same pass.

## Findings

### P0 — None Found

No P0 found in this precision pass. The correctness design still holds at the spec level.

### P1 — Top-Level Rollout Text Still Treats Heartbeat As JSON-Additive

The authoritative heartbeat section is correct: legacy heartbeat payloads are raw RFC3339 timestamp bytes, and new readers must use `DecodeHeartbeat` to handle either legacy timestamp or v1 JSON. That is grounded in current code: [internal/heartbeat/publisher.go](internal/heartbeat/publisher.go#L228) formats a timestamp string and [internal/heartbeat/publisher.go](internal/heartbeat/publisher.go#L231) writes those bytes directly.

Two top-level summaries still use the old framing:

- [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L120) says internal KV schemas, including `heartbeat.*`, gain new fields with safe defaults.
- [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L847-L850) says heartbeat `SchemaVersion`, capabilities, and ack fields are additive and old peers treat unknown JSON fields as zero.

That contradicts the corrected bidirectional-tolerance section, which now correctly says `heartbeat.<W>` is dual-format, not JSON-additive.

Failure case: a phase 2 implementer skimming rollout/scope text could treat heartbeat migration as JSON-only additive and miss the legacy raw timestamp branch.

Recommended fix:

- Change the scope summary to say assignment schemas are JSON-additive, while heartbeat keys are dual-format.
- Change rollout/risk to say old peers do not consume the new heartbeat JSON for audit; new peers decode old timestamp bytes and v1 JSON.
- Keep the detailed §4.1 and migration-table wording as-is; those parts are now right.

No new test needed; tests #54-58 already encode the required behavior.

### P1 — Source API Surface Still Has Two Residual Ambiguities

The new API surface block is a big improvement, but two places still conflict with it.

First, the CAS retry migration section introduces `WithUpdateRetries` at [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L2036), but the API summary only lists `WithReconcileInterval` and `WithLeadershipProbe` at [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L439-L440). That leaves an implementer unsure whether `WithUpdateRetries` is required public API or stale text.

Second, the publish flow still starts with an unconditional `source.Snapshot()` at [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L1033), while the API summary correctly says `RevisionedPartitionSource` is optional and the calculator must type-assert/fallback at [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L455-L466).

Failure case: one phase agent adds `WithUpdateRetries` and tests around it while another omits it; or a publisher/calculator implementation calls `Snapshot` directly on `types.PartitionSource` and breaks `source.Static` or user custom sources.

Recommended fix:

- Either add `func WithUpdateRetries(n int) NatsKVOption` to the API surface summary and phase 1 scope, or remove the tuning bullet from the migration section and make retry count internal-only.
- Change publish-flow step 1 to reference the exact fallback helper, for example:

```text
1. Source snapshot:
   if source implements RevisionedPartitionSource, call Snapshot();
   otherwise call List() and set srcRev=0, srcKnown=false.
```

The existing tests #64 and any future retry-option test should then match whichever API choice is made.

### P2 — Implementation Strategy Has Stale Step And Test References

The strategy doc is operational, but implementing agents are told to consult it before each phase, so drift here will cost time.

Stale references found:

- Phase 3 says the heartbeat-aware alias barrier is publish step 5: [docs/plans/cache-freeze-improvement/02-implementation-strategy.md](docs/plans/cache-freeze-improvement/02-implementation-strategy.md#L30). In the current spec, step 5 is the pre-alias leadership fence and the alias barrier is step 6.
- Phase 6 still says the latest rolling-upgrade/dual-read/cap-wiring tests are tests #46-53: [docs/plans/cache-freeze-improvement/02-implementation-strategy.md](docs/plans/cache-freeze-improvement/02-implementation-strategy.md#L33). The current test plan now extends that area through #65, with end-to-end tests renumbered to #66-68.
- The worked Phase 1 dispatch points at §3.4 for `RevisionedPartitionSource`: [docs/plans/cache-freeze-improvement/02-implementation-strategy.md](docs/plans/cache-freeze-improvement/02-implementation-strategy.md#L148). There is no §3.4 in the spec; the optional interface now lives in the API surface summary, and Pillar 3 jumps from §3.3 to §3.5 at [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L927-L1027).
- The intro still says about 45 tests: [docs/plans/cache-freeze-improvement/02-implementation-strategy.md](docs/plans/cache-freeze-improvement/02-implementation-strategy.md#L12). The current plan lists 68 tests.

Recommended fix: update strategy references after the spec line-up is final. In particular, replace `publish step 5` with `publish step 6`, replace `tests #46-53` with the current ranges, and point Phase 1 at the API surface summary instead of nonexistent §3.4.

### P2 — Test Plan Numbering Still Duplicates `5`

The CAS/write-path list ends with test #5, and the next Reconcile/read-path list also starts at #5: [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L493-L501). Later numbers assume the old sequence and now continue through #68.

This is not a design bug, but it makes test inventory tracking awkward. Renumber once, after deciding whether `WithUpdateRetries` gets a test.

### P2 — Documentation Section Does Not Match The Expanded Public API

The API surface summary now lists `Snapshot`, `AddPartitions`, `RemovePartitions`, `WithLeadershipProbe`, `RevisionedPartitionSource`, `DecodeHeartbeat`, capability constants, `Manager.SetCapability`, and `Manager.Capabilities`. The documentation section still only calls out `Modify`, `WithReconcileInterval`, and a concurrent-update subsection at [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L823-L825).

Recommended fix: expand the documentation section so Phase 7 knows to document every additive public API, especially `AddPartitions` / `RemovePartitions`, `RevisionedPartitionSource`, `WithLeadershipProbe`, capability bits, and `SetCapability` / `Capabilities`.

### P2 — A Few Code-Grounding References Are Stale Or Wrong

The plan is mostly code-grounded, but these references are stale enough to slow an implementer:

- §4.2 cites `jsutil/consumer.go processing/pull gate` for the processing gate at [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L1778-L1780). Current gate wiring is in [internal/durable/worker_consumer.go](internal/durable/worker_consumer.go#L382-L387), and the gate itself is in [internal/durable/processing_gate.go](internal/durable/processing_gate.go).
- §4.4 says `applyHandoffAndHooks` stores `newAssignment`, but current code stores in `applyAssignmentUpdate` before calling `applyHandoffAndHooks`: [manager_assignment.go](manager_assignment.go#L365-L377), then `Apply` happens at [manager_assignment.go](manager_assignment.go#L385-L389).
- The initial path reference remains conceptually right, but the current sequence is clearer as [manager.go](manager.go#L374-L386): wait for assignment, emit events, call `applyInitialHandoffAsync`, then transition stable.

Recommended fix: update these references so phase 4/5 agents land on the right functions without hunting.

## Additional Tests To Add

No new invariant tests required from this precision pass. If `WithUpdateRetries` is kept as public API, add one small option test; otherwise remove it from the plan.

## Verdict

The plan is very close, but I would still do one short cleanup pass before phase 1 dispatch. The only P1 gates are wording/API precision: remove the remaining heartbeat JSON-additive summary text, and decide whether `WithUpdateRetries` is real API while making publish step 1 use the optional `RevisionedPartitionSource` fallback explicitly. The P2 items are strategy/test/doc/reference cleanup and can be handled in the same edit.
