# CapProcessingGate Wiring Post-Implementation Review (v3)

## Summary

The v2 remaining P1 is closed. Tests #5 and #6 now pre-create the heartbeat bucket before `NewManager` / `Start`, and the manager's bucket helper opens that existing bucket instead of recreating it with `History: 1`, so the post-start watcher can replay retained heartbeat revisions. The ordering being tested is now code-grounded: `reportConsumerCapabilities` runs after `Apply` and before `SetAppliedAssignment` / `PublishNow`, and heartbeat snapshots are monotone in `AppliedVersion`. No P0/P1 findings remain; ready to merge.

## Spec Compliance

| Spec / review requirement | Status | Evidence |
| --- | --- | --- |
| Pre-create heartbeat bucket with larger history before `Manager.Start` | Compliant | Helper creates the bucket with `History: 64` (`manager_capability_reporter_test.go:131-137`); Test #5 calls it before `NewManager` and `Start` (`manager_capability_reporter_test.go:226-234`); Test #6 does the same (`manager_capability_reporter_test.go:288-293`). |
| Manager's bucket ensure opens an existing bucket before applying `History: 1` config | Compliant | `EnsureKVBucketWithRetry` calls `js.KeyValue(ctx, config.Bucket)` first and returns on success (`kvutil/bucket.go:48-53`); it only creates after `ErrBucketNotFound` (`kvutil/bucket.go:55-66`) and also opens on create races (`kvutil/bucket.go:69-74`). Manager's intended heartbeat config is `History: 1` (`manager_setup.go:148-160`) and is reached through `ensureCoreKVBuckets` (`manager_setup.go:63-83`). |
| Existing heartbeat bucket config survives manager startup | Compliant | Manager documentation explicitly says users can pre-create buckets and `EnsureKVBucketWithRetry` opens existing buckets without inspecting config (`manager_setup.go:51-54`); the helper relies on that path (`manager_capability_reporter_test.go:121-130`). |
| Watcher replays retained heartbeat history and selects first post-apply revision | Compliant | The helper watches the worker key with `jetstream.IncludeHistory()` (`manager_capability_reporter_test.go:152-156`) and returns only the first decoded `KeyValuePut` whose `AppliedVersion > 0` (`manager_capability_reporter_test.go:159-175`). NATS client docs for `IncludeHistory` state that it sends historical values up to `KeyValueMaxHistory`; `KeyValueConfig.History` docs state max history is 64. |
| Immediate post-apply heartbeat has newly sampled capability bit | Compliant | `applyAssignmentWithPrev` calls `handoffCoordinator.Apply`, then unconditionally samples consumer capabilities before any apply-error return and before heartbeat ack (`manager_assignment.go:745-755`); the success path then calls `SetAppliedAssignment` and `PublishNow` (`manager_assignment.go:794-807`). |
| `AppliedVersion` monotonicity supports "first `AppliedVersion > 0`" proof | Compliant | `Publisher.SetAppliedAssignment` ignores lower versions (`internal/heartbeat/publisher.go:168-195`); heartbeat build reads the applied snapshot plus live capabilities (`internal/heartbeat/publisher.go:329-353`); publish writes that payload to KV (`internal/heartbeat/publisher.go:356-367`). |
| No sleep polling in async test | Compliant | `waitForPostApplyHeartbeat` waits on the KV watcher channel and context/deadline cases (`manager_capability_reporter_test.go:159-180`), matching the async testing rule against `time.Sleep` polling (`.agents/rules/300-testing.md:18-24`). |

## Prior Finding Resolution Audit

| Prior finding | Status | Evidence |
| --- | --- | --- |
| v1 P1#1 — same-update partial failure did not prove the bit flips before later failure | Resolved | Test starts with the bit clear (`internal/durable/worker_consumer_capabilities_test.go:148-152`), performs one update containing `p1` and `p2` (`internal/durable/worker_consumer_capabilities_test.go:154-163`), requires the second subject failure (`internal/durable/worker_consumer_capabilities_test.go:164`), and asserts the bit is already set (`internal/durable/worker_consumer_capabilities_test.go:165-178`). Implementation flips `gateWired` immediately after wrapping a subject (`internal/durable/worker_consumer.go:387-399`) while `UpdateWorkerConsumer` adds subjects sequentially and returns on the first later add error (`internal/durable/worker_consumer.go:154-158`). |
| v1 P1#2 / v2 P1 — post-apply `PublishNow` ordering was not proven | Resolved | The bucket is now pre-created with `History: 64` before startup (`manager_capability_reporter_test.go:226-234`, `manager_capability_reporter_test.go:288-293`), the manager opens the existing bucket (`kvutil/bucket.go:48-53`), and the watcher replays retained values to return the first `AppliedVersion > 0` heartbeat (`manager_capability_reporter_test.go:152-175`). The production ordering is `Apply` -> sample capabilities -> `SetAppliedAssignment` -> `PublishNow` (`manager_assignment.go:745-807`). |
| v1 P2 — sleep polling in async tests | Resolved | The helper is event-driven on `w.Updates()` with deadline/context exits (`manager_capability_reporter_test.go:159-180`); no sleep-poll loop remains in the changed manager capability tests. |

## Findings

### P0 — None

### P1 — None

### P2 — Test helper comments overstate "full sequence"

The comments say `History: 64` lets the watcher replay the "FULL sequence" / "every heartbeat ever published" (`manager_capability_reporter_test.go:125-128`, `manager_capability_reporter_test.go:142-145`), but the configured history is capped at 64 (`manager_capability_reporter_test.go:133-136`). This does not reopen the v2 P1 because the watcher is called immediately after `Start` and only needs to retain the immediate post-apply revision (`manager_capability_reporter_test.go:234-245`), while `AppliedVersion` cannot regress (`internal/heartbeat/publisher.go:168-195`). Recommended fix: soften the comments to "retained test-window sequence" / "retained heartbeat history." Suggested test: none; wording-only.

## Test Coverage Audit

| Test | Coverage status | Evidence |
| --- | --- | --- |
| `TestManager_CapProcessingGate_SampledAfterUpdaterOnApplyError` | Present-and-meaningful | Stub stores `CapProcessingGate` during updater call and returns an error (`manager_capability_reporter_test.go:44-56`); test asserts `applyAssignmentWithPrev` propagates the error and manager capabilities include the bit (`manager_capability_reporter_test.go:68-88`). |
| `TestManager_CapProcessingGate_ReportsAfterFirstApply` | Present-and-meaningful | Uses real `consumer.Dynamic` with processing gate enabled (`manager_capability_reporter_test.go:217-224`), pre-creates heartbeat history before startup (`manager_capability_reporter_test.go:226-234`), and asserts the first post-apply heartbeat plus manager state contain the bit (`manager_capability_reporter_test.go:241-253`). |
| `TestManager_CapProcessingGate_StaysClearWithoutGate` | Present-and-meaningful | Uses real `consumer.Dynamic` without gate config (`manager_capability_reporter_test.go:283-286`), pre-creates heartbeat history before startup (`manager_capability_reporter_test.go:288-293`), and asserts the first post-apply heartbeat plus manager state keep the bit clear (`manager_capability_reporter_test.go:300-309`). |
| `TestManager_CapProcessingGate_EmptyAssignmentStaysClear` | Present-and-meaningful | Drives empty apply then non-empty apply through `applyAssignmentWithPrev`; asserts clear after empty and set after first non-empty assignment (`manager_capability_reporter_test.go:344-363`). |
| `TestWorkerConsumer_GateWiredReportsCapProcessingGate` | Present-and-meaningful | Verifies enabled gate reports the bit after update and disabled gate stays clear (`internal/durable/worker_consumer_capabilities_test.go:20-97`). |
| `TestWorkerConsumer_GateBitMonotonic_StaysSetAfterLaterSubjectError` | Present-and-meaningful | Forces same-update partial failure with `MaxConsumers: 1`, asserts error, then asserts the bit is already and remains set (`internal/durable/worker_consumer_capabilities_test.go:111-179`). |
| `TestDynamic_ImplementsCapabilityReporter` | Present-and-meaningful | Compile-time assertion covers interface satisfaction (`consumer/dynamic_test.go:17-19`); runtime test asserts `Dynamic.Capabilities()` forwards the bit after update (`consumer/dynamic_test.go:25-66`). |
| `TestCompositeConsumerUpdater_CapabilitiesORs` | Present-and-meaningful | Covers no-reporters, one reporter, and multiple reporter OR semantics (`composite_updater_test.go:155-189`), matching composite implementation (`composite_updater.go:89-112`). |

## Interactions Outside Phase Scope

The test-only pre-create intentionally changes more than history: because manager startup opens existing buckets without updating config (`kvutil/bucket.go:48-53`, `manager_setup.go:51-54`), the pre-created heartbeat bucket also keeps the helper's omitted TTL/storage defaults instead of manager's heartbeat TTL and memory storage (`manager_setup.go:148-156`). That is acceptable for these single-worker capability tests, but downstream tests should not copy this helper when they need heartbeat expiration or storage-policy coverage.

## Lint / Build / Test Status

`make lint`:

```text
Checking golangci-lint version...
✓ golangci-lint 2.11.4 is installed
Running linters...
0 issues.
```

`go test . ./consumer/ ./internal/durable/ ./partitest/ -race -count=1 -timeout=300s`:

```text
ok  	github.com/arloliu/parti/v2	14.930s
ok  	github.com/arloliu/parti/v2/consumer	1.651s
ok  	github.com/arloliu/parti/v2/internal/durable	23.751s
ok  	github.com/arloliu/parti/v2/partitest	1.312s
```

`go vet ./...`:

```text
(no output; exit code 0)
```

No flakes or failures observed.

## Verdict

**merge**. The v2 P1 is closed by the heartbeat-history pre-create plus existing-bucket open-first behavior, and there are no blocking P0/P1 findings.
