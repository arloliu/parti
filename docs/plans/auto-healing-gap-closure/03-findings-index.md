# Auto-Healing Gap Closure Findings Index

This index records the current investigation output and the boundary for the
next task. Implementation fixes are intentionally out of scope here; the next
task starts from the fix plans and failing proofs named below.

| Finding | Matrix rows | Status | Evidence | Follow-up |
|---|---|---|---|---|
| G1 wedged/read-only storage surface | M12 | Probe implemented and opt-in verified. Existing runtime KV/stream surfaces stayed OK after StoreDir chmod, while new file-backed stream and bucket creation returned JetStream API error 10049. Stronger runtime storage-fault injection remains open. | `PARTI_RUN_WEDGED_STORAGE_PROBE=1 go test ./test/integration/failure -run TestWedgedStorage_ReadOnlyFileStore_RecordsOperationSurfaces -count=1 -v` | Create a deeper storage-fault harness before widening classifiers. |
| G2 RF3 selective-peer quorum loss | M1, M7 | Five-node gated probe implemented and opt-in verified. The probe avoids stopping the JetStream meta leader and records KV get/put/status error surfaces under handoff bucket quorum loss. | `PARTI_RUN_QUORUM_LOSS_TIER2=1 go test ./test/integration/failure -run TestRF3SelectivePeerFault_HandoffQuorumLoss -count=1 -v` | Keep gated; use probe output when changing handoff quorum-loss classifiers. |
| G3 full NATS outage | M5, M6 | Unlimited reconnect and finite reconnect proofs implemented and verified. Unlimited reconnect recovers to Stable after restart; finite reconnect reaches Degraded after the client closes and must not claim Stable. | `go test ./test/integration/failure -run 'TestFullNATSOutage_' -count=1 -v` | No implementation fix planned from this investigation. |
| G4 handoff-only rebalance write fault | M7 | Opt-in known failing proof implemented. Current behavior protects the new worker from exposing uncommitted ownership, but old owners stop consuming some pre-fault partitions while handoff writes are failing. | `PARTI_RUN_HANDOFF_REBALANCE_PROOF=1 go test ./test/integration/failure -run TestHandoffOnlyWriteFault_RebalancePreservesOldOwners -count=1 -v` fails with old-owner deltas below the pre-fault owner counts. | Implement `02-g4-handoff-rebalance-fix-plan.md` in the next task. |
| G5 source bucket sustained timeout signaling | M9 | Implemented and verified. `context.DeadlineExceeded` now fires the source-unavailable hook and gauge without classifying the source as bucket deletion. | `go test ./source -run 'TestNatsKV_(F6A|SourceUnavailable)' -count=1` | No additional fix planned. |
| G6 dynamic generic permanent failure policy | M11 | Policy resolved as local durable-layer WARN/metric only. Manager Degraded remains reserved for stream-missing recovery exhaustion unless application callbacks choose a broader policy. | `go test ./consumer -run TestDynamic_onPermanentFailure -count=1` | No implementation fix planned. |
| G7 public degraded reason taxonomy | M2-M6, M8-M11 | Operator-facing taxonomy documented in the fault matrix, operations guide, API reference, and `StateDegraded` Godoc. | `make lint`; `git diff --check` | Keep synchronized when adding new `OnDegraded` reasons. |

## Commit Boundary

This investigation branch should stop after recording evidence and committing
the probes, taxonomy, and plans. The next task is implementation work, starting
with G4 unless a higher-priority finding is selected.
