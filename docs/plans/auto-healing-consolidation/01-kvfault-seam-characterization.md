# 2.7 kvfault-toolkit — six-seam characterization (for the consolidate/decline decision)

Captured 2026-06-02 from a parallel read of all six KV/JetStream fault-injection seams. This is the
input to the 2.7 go/no-go: the plan's one-line `Controller + []Rule{Buckets, Ops, KeyPrefix, err}`
model does NOT capture the seams' real divergence (below). Preserve this analysis even if 2.7 is
declined or deferred.

## Common shape (all 6)
- A `*FaultJetStream` wrapping `jetstream.JetStream`, overriding **exactly** `KeyValue`,
  `CreateKeyValue`, `CreateOrUpdateKeyValue`; wraps the returned handle by bucket (membership/equality),
  wrap-on-success-only (raw `(kv,err)` on error). All other JS methods pass through (embedding).
- A `*FaultKeyValue` wrapping `jetstream.KeyValue`, overriding a SUBSET of ops, returning
  **`context.DeadlineExceeded`** (timeout-shaped, classified as neither connectivity nor
  degrading-JetStream — drives the F-D1 `kv-unavailable` path). Non-overridden ops pass through.
- An `injected atomic.Int64`, incremented **only on the fault path** (counts injected faults, not
  intercepted calls). `arm()` resets the counter to 0 then sets armed; faults are **always-on while
  armed** (no countdown / probability). Proofs assert `injected > 0` for non-vacuity + the connection
  stays `IsConnected()`.

## Per-seam divergence (the essence of each — NOT incidental)

| Seam | File | Faulted ops | Scoping | Arming | Unique |
|---|---|---|---|---|---|
| **ku** | test/integration/manager/manager_kv_read_unavailable_test.go | Put/Update/Create/**Get** | buckets {election,heartbeat,stableid}; no prefix | 1 `armed` bool | Watch/WatchAll/Keys **pass through** so the assignment watcher's OnPermanent can't race the asserted `kv-unavailable` reason |
| **np9** | test/integration/failure/np9_full_quorum_loss_arbitration_test.go | Put/Update/Create/Get/**Watch** | 4 coord buckets; no prefix | 1 `armed` + `disarm()` | faults **Watch** too (full quorum loss); Watch passes through when disarmed so the watcher re-arms on recovery |
| **np10** | test/integration/failure/np10_enumeration_stall_test.go | **Keys only** | heartbeat bucket only; no prefix | 1 `armed` + `disarm()` | enumeration-stall: single-key ops MUST keep succeeding; only the stream-wide `Keys` scan stalls. Reason `heartbeat-enumeration-stall` |
| **wf** | test/integration/failure/startup_writefault_test.go | Create/Update/Put | **2 modes**: claims/-prefix on handoff bucket; ALL keys on heartbeat bucket | **2 independent** flags `writeArmed`/`heartbeatArmed` (armed at different lifecycle points), 2 counters | `ArmWrites` resets its counter; `DisarmWrites` disarms BOTH; **empty prefix = match NONE** (explicit footgun guard), not all |
| **rf** | test/integration/failure/resolver_readfault_test.go | **Get only** | claims/-prefix on handoff bucket | 1 `readArmed` bool | Keys/Watch/WatchAll **stay live** (the Keys-ok/Get-fail asymmetry IS the test); read-back uses an UNWRAPPED handle |
| **simKV** | test/simulation/cmd/simulation/kv_fault_chaos.go | huge set: Get/GetRevision/Delete/Purge/Watch/WatchAll/WatchFiltered/Keys/History/Status + Put/PutString/Create/Update | 2 classes: kvUnavailable (bucket set) + handoffClaimWrite (parti-handoff + claims/) | **2 classes, each with a token/generation disarm** | see below |

## simKV token-generation contract (the crux — pinned by 2 tests)
- Each class has a monotonic `token`. `arm*()` **bumps** the token, **replaces** the class's match set
  (not merge), and **returns** the token. A timed `disarm(token)` clears the class **only if** the
  captured token still equals the current one → overlapping arm windows **extend** (newest wins, older
  timer no-ops). `disarm()`-all bumps BOTH tokens so all pending timers no-op.
  Tests: `TestHandleKVUnavailableFaultOverlappingTimersKeepNewestFaultArmed` + handoff twin.
- **Delete/Purge are gated by kvUnavailable ONLY, not the write class** — handoff-claim-write must NOT
  fault claim cleanup. Pinned by `TestSimKVFaultController_HandoffClaimWriteDoesNotFaultClaimCleanup`.
- Oracle integration: arms register `DegradedReasonOracle().ExpectAfter("kv_unavailable:...",
  ["kv-unavailable"], ...)`. Config is param-driven (`map[string]any`) + `config.FaultsConfig` for the
  startup handoff fault — NOT a `[]Rule` model today.

## Why the full engine is a poor fit (the decision rationale)
1. **Op-sets are the essence, and conflict:** Keys-only (np10) vs Get-only (rf) vs write-only (wf) vs
   write+Get (ku) vs +Watch (np9) vs the simKV superset with a Delete/Purge carve-out. The generic
   `Ops` must encode all + the carve-out, or a proof goes vacuous.
2. **Arming diverges:** 1 flag (ku/np9/np10/rf) vs 2 independent flags with coupled disarm (wf) vs 2
   token-generation classes (simKV). A generic Controller needs per-rule independent arming + optional
   token-generation — i.e. it must absorb simKV's whole mechanism.
3. **Prefix convention conflicts:** empty = ALL (ku/np9/np10) vs empty = NONE w/ explicit guard (wf/rf).
4. **Counters:** global single (ku/np9/np10) vs 2 per-class (wf/simKV), reset-on-arm coupling.
5. **Net:** a ~250-line engine encoding all the above replaces ~6×30 lines of honest, readable
   per-seam wrappers — marginal/negative net complexity, large blast radius (6 files across
   failure/manager/simulation + their exact-count/reason proofs).
6. **Test-only inverts the usual calculus:** for a fault harness the dominant risk is a subtly-wrong
   shared injector making the auto-healing **proofs vacuously pass** — strictly worse than duplication.
   The consolidation bar for fault injectors is therefore HIGHER than for production code.

## Options
- **Decline + document** (recommended): keep the 6 wrappers; this file is the record. Phase 3 (the
  named finish line) is the deliverable.
- **Minimal**: extract only the behavior-free plumbing shared verbatim — the `*FaultJetStream` that
  wraps KV handles by bucket across the 3 JS methods (wrap-on-success-only). Divergent fault logic
  stays per-seam. Marginal dedup, low risk.
- **Full**: the ~250-line engine + 6 ports, accepting the cost and the vacuous-proof risk.
