# Auto-Healing Fault Matrix

This matrix is the policy source of truth for the remaining auto-healing gap
closure work. Each row names the expected response before tests or production
classifiers are added.

| ID | Connection | JetStream state | Operation surface | Subsystem | Timing | Expected policy | Reason / signal | Proof |
|---|---|---|---|---|---|---|---|---|
| M1 | connected | RF3 bucket quorum lost | `Get` after `Keys`; real RF3 peer loss | handoff claims | stable | In-process self-heal / retry; do not tombstone listed-but-unreadable claims. | none | `TestResolverReadFault_ConsumerSurvivesQuorumLossWindow`; gated `TestRF3SelectivePeerFault_HandoffQuorumLoss` |
| M2 | connected | manager KV quorum lost | heartbeat / election / stableid KV op | manager | stable | Enter Degraded; rotate if sustained past operator SLO. | `kv-unavailable` | `TestManager_KVUnavailable_EntersDegraded` |
| M3 | connected | Parti-owned bucket deleted | KV op | manager | stable | Enter Degraded; live workers must not recreate coordination buckets. | `KV error threshold exceeded` | `TestManager_LiveNATSBucketLoss` |
| M4 | connected | Parti-owned bucket recreated | bucket epoch monitor | manager | stable | Enter Degraded because revision reset is ambiguous data loss. | `bucket-recreated:<bucket>` | `TestManager_BucketRecreated_EntersDegraded` |
| M5 | reconnecting | all NATS nodes stopped, reconnect budget unlimited | connection monitor | manager + dynamic consumer | stable | Enter Degraded, keep committed cached assignment where possible, recover to Stable when NATS returns. | `NATS connection down` | `TestFullNATSOutage_UnlimitedReconnects_RecoversFleet` |
| M6 | closed | all NATS nodes stopped, finite reconnect budget exhausted | connection monitor | manager | stable | Enter Degraded/readiness rotation; do not claim in-process self-heal after the client is closed. | `NATS connection down` or a future explicit closed-connection reason | `TestFullNATSOutage_FiniteReconnects_DegradesClosedConnection` |
| M7 | connected | handoff bucket write timeout only | `Create` / `Update` claim | handoff apply | rebalance | Keep old data-plane ownership, retry, and do not expose new ownership until claims commit. **Holds for a fully-upgraded fleet that wires `CapProcessingGate` (fences the gaining worker before commit). Mixed-version safety is a rollout contract — finish the upgrade before relying on M7; during the upgrade window an un-upgraded worker is no worse than current `main`.** | retry log + `parti_handoff_removal_pending` metric | `TestHandoffOnlyWriteFault_RebalancePreservesOldOwners` (passes; opt-in via `PARTI_RUN_HANDOFF_REBALANCE_PROOF=1` pending promotion); see `02-g4-handoff-rebalance-fix-plan.md` |
| M8 | connected | source bucket missing/deleted | `Get` / `Watch` | `source.NatsKV` | reconcile | Caller/operator-owned recovery; Parti must not recreate caller-owned source buckets. | `source-unavailable:<bucket>` hook + gauge | `TestNatsKV_F6A_BucketMissing_FiresHookAndSetsMetric` |
| M9 | connected | source bucket quorum lost | `Get` deadline/no responders | `source.NatsKV` | reconcile | Sustained source-unavailable signal without treating the bucket as deleted or recreating it. | `source-unavailable:<bucket>` hook + gauge | `TestNatsKV_SourceUnavailable_DeadlineExceededThreshold` |
| M10 | connected | stream missing | consumer info / pull | dynamic consumer | stable | Manager Degraded if no application callback recovers or overrides. | `stream-missing-recovery-exhausted` | `TestStreamMissingNoHook_RoutesPermanentFailureToManager` |
| M11 | connected | generic durable permanent failure | durable retry envelope | dynamic consumer | stable | Local durable-layer WARN/metric only. Applications that need readiness impact must install their own permanent-failure callback. | durable permanent-failure log/metric | `TestDynamic_onPermanentFailure_ManagerObserverOnlyOnStreamMissing` |
| M12 | connected | read-only/wedged disk | write / read / watch / stream-info / consumer-info / file-backed create | all | any | Fail loud according to the observed operation surface; update this matrix before widening classifiers. | matrix-specific | gated `TestWedgedStorage_ReadOnlyFileStore_RecordsOperationSurfaces` (existing surfaces stayed OK; new file-backed stream/bucket create returned API error 10049) |

## Reason Taxonomy

| Reason | Class | Operator action |
|---|---|---|
| `NATS connection down` | ride-through if reconnecting | Keep readiness degraded until NATS is stable; rotate only if the connection is closed or the outage exceeds policy. |
| `kv-unavailable` | connected but KV quorum unavailable | Keep readiness degraded; rotation is acceptable if the outage exceeds SLO. |
| `KV error threshold exceeded` | Parti-owned coordination data missing/lost | Restart or rotate workers after confirming bucket loss. |
| `bucket-recreated:<bucket>` | ambiguous Parti-owned data loss | Restart or rotate workers; inspect JetStream storage before trusting the recreated bucket. |
| `startup-timeout` | startup apply/wait did not reach Stable in budget | Readiness rotation unless the runner recovers before the pod is replaced. |
| `assignment-watcher-exhausted` | assignment watcher retry envelope exhausted | Restart or rotate the worker; inspect the assignment bucket and NATS logs. |
| `stream-missing-recovery-exhausted` | dynamic consumer stream missing and no app hook recovered it | Recover the stream or rotate workers according to application ownership. |
| `source-unavailable:<bucket>` | caller-owned source bucket unavailable | Caller/operator recovers the source bucket; Parti does not recreate it. |

## Open Proof Rows

The remaining executable work is intentionally split by cost:

1. M9 is deterministic and should be closed first with a synthetic source KV
   timeout seam.
2. M7 has an executable failing proof and a dedicated fix plan.
3. M12 and the RF3 selective-peer proof are gated probes because they require
   environment-specific storage or five-node cluster conditions. The read-only
   StoreDir probe observed API error 10049 for new file-backed stream/bucket
   creation, while existing stream/KV operations stayed OK; a stronger
   wedged-storage mechanism is still needed for runtime write failure proof.
