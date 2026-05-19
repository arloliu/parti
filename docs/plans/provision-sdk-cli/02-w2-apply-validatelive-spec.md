# W2 — `Apply` + `ValidateLive` Sub-Spec

This sub-spec resolves the implementation details the master plan
(`docs/plans/provision-sdk-cli/00-implementation-plan.md`) leaves implicit
for W2. It does not redesign anything; every behavioral contract it states
either cites the master plan by header or refines its "TBD"-style language
into a single deterministic algorithm.

Cross-references to the master plan use its headers verbatim. Cross-references
to nats.go are pinned at `v1.50.0` (`go.mod`).

---

## 1. `ValidateLive`

### 1.A Order of probes (deterministic)

`ValidateLive(ctx, js, cfg)` executes the following steps in order. Each step
is fail-fast: the first error short-circuits the rest of `ValidateLive` and is
returned. The function never mutates NATS.

1. **Reachability probe** — exactly once per call (see 1.B).
2. **Per-bucket info probes** for every named bucket in `cfg`, in the same
   deterministic order Plan visits them
   (`provision/plan.go:80-96` — control-plane component order, optional
   handoff appended last). PartitionSource follows the control-plane block
   (W3 wires its bucket here; this sub-spec specifies the seam, W3 lands
   the call site).
3. **Partition-source key probe** — only when `cfg.PartitionSource != nil`
   and step 2 succeeded for the partition-source bucket (see 1.D).
4. **Dynamic-consumer live checks** — only when `cfg.DynamicConsumers` is
   non-empty. W4 lands the call site; this sub-spec only fixes the
   ordering slot.

Steps that have no corresponding config block are **skipped silently** (1.F).
Step 1 always runs.

### 1.B Reachability probe

Single call: `js.AccountInfo(ctx)`
(`jetstream.go:1008-1031` in nats.go v1.50.0).

Rationale: this is the canonical "JetStream is reachable and the caller has
the minimum account-level grant" check and is the same call the master plan's
[Permissions](../../plans/provision-sdk-cli/00-implementation-plan.md#permissions)
table assigns to `$JS.API.INFO`. nats.go folds the no-responders case (which
includes both "JS not enabled" and "no permission on `$JS.API.INFO`") into
`ErrJetStreamNotEnabled` (`jetstream.go:1015-1017`), so the client cannot
discriminate JS-disabled from no-perm from the error alone.

**Classification of reachability errors → exit code 4:**

- `errors.Is(err, jetstream.ErrJetStreamNotEnabled)` → 4 (covers both
  "JS truly down" and "caller lacks `$JS.API.INFO`" — both are operationally
  pre-flight failures that prevent the worker from starting).
- `errors.Is(err, jetstream.ErrJetStreamNotEnabledForAccount)` → 4.
- `errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)`
  → 4 (cancellation).
- Any other error → 4 (conservative: if reachability is in question, we
  exit 4, not 3; the operator's first remediation is "can the client talk
  to NATS at all").

This is the **only** probe where `ErrNoResponders` / its
`ErrJetStreamNotEnabled` rewrap maps to exit code 4. Every probe **after**
this one (steps 2–4) treats the same surface error as **permission denied**
(exit code 3), because reachability has already been proven.

### 1.C Bucket info probe

For each named bucket `<bucket>`, call `js.Stream(ctx, "KV_" + bucket)` then
`stream.Info(ctx)`. (This matches Plan's existing lookup at
`provision/plan.go:104-117`.)

Classification:

- `errors.Is(err, jetstream.ErrStreamNotFound)` → **not an error**. The
  bucket is missing; `apply` will create it. ValidateLive records nothing
  and proceeds to the next bucket. (Step 3, partition-source key probe,
  short-circuits in this case for that bucket — there is no live stream to
  probe a key against.)
- `errors.Is(err, jetstream.ErrJetStreamNotEnabled)` after step 1 succeeded
  → permission denied on `$JS.API.STREAM.INFO.KV_<bucket>`. nats.go cannot
  distinguish "no responders" from "permission denied" at this level
  (NATS server silently drops API requests without the publish grant on
  the API subject). Because step 1 proved reachability, this surface
  error reclassifies as a **permission denied** ValidateLive error → exit
  code 3.
- `errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)`
  → exit code 4.
- Any other error → exit code 1 (unknown / generic).

### 1.D Partition-source key probe — `AllowDirect` retry protocol

The master plan's [Permissions](../../plans/provision-sdk-cli/00-implementation-plan.md#permissions)
section punts the exact retry algorithm to this sub-spec. The algorithm
below is the single source of truth.

**Why this is subtle:** `kv.Get(ctx, key)` goes through `stream.getMsg`
which branches on `s.info.Config.AllowDirect` **cached at stream-handle-open
time** (`jetstream/stream.go:539`). A handle opened against an
`AllowDirect=true` snapshot will keep using the direct path even if the
operator toggles the stream to `AllowDirect=false` between handle-open and
probe. We therefore do **not** use `kv.KeyValue(...).Get(...)`; we probe
through a freshly resolved `stream.GetLastMsgForSubject` so the
`AllowDirect` branch is re-evaluated on retry.

**Subject for the underlying call** (informational — nats.go computes it
internally from `info.Config.AllowDirect`):

- `AllowDirect=true`:
  `$JS.API.DIRECT.GET.KV_<bucket>.$KV.<bucket>.<key>`
  (`jetstream/stream.go:540-547`, format string `apiDirectMsgGetLastBySubjectT`).
- `AllowDirect=false`:
  `$JS.API.STREAM.MSG.GET.KV_<bucket>`
  (`jetstream/stream.go:557`, format string `apiMsgGetT`).

**Algorithm:**

```text
// Pre-condition: step 1.C succeeded for KV_<bucket> and we hold a `stream`
// handle whose info is the just-read STREAM.INFO response.
key := cfg.PartitionSource.Key
subject := "$KV." + cfg.PartitionSource.Bucket + "." + key

err := probeKey(ctx, stream, subject)   // stream.GetLastMsgForSubject
if err == nil || errors.Is(err, jetstream.ErrMsgNotFound) {
    return nil   // key missing is OK (Phase 3 manages records)
}
if !isPermissionDenied(err, ctxStillLive) {
    return classify(err)  // see 1.E
}

// One retry with refreshed STREAM.INFO.
freshInfo, err2 := stream.Info(ctx)
if err2 != nil {
    return classify(err2)
}
// `stream.Info` updates the cached info in nats.go; the next call to
// GetLastMsgForSubject re-evaluates AllowDirect from freshInfo.
err = probeKey(ctx, stream, subject)
if err == nil || errors.Is(err, jetstream.ErrMsgNotFound) {
    return nil
}
return classifyPermission(err)  // surface as permission-denied ValidateLive error
```

`isPermissionDenied(err, ctxStillLive)` is true when:
`errors.Is(err, jetstream.ErrJetStreamNotEnabled) ||
 errors.Is(err, nats.ErrNoResponders) ||
 errors.Is(err, nats.ErrPermissionViolation)` **and** `ctx.Err() == nil`.
(The ctx-live guard avoids misclassifying a cancellation as a
permission failure.)

Sentinel-error mapping:

- `jetstream.ErrMsgNotFound` (`jetstream/stream.go:564-565`) ← key not
  present. **Success.** Equivalent to `jetstream.ErrKeyNotFound`
  (`jetstream/kv.go:1004-1014`); we bypass `kv.Get` so the
  `ErrMsgNotFound` form is what surfaces.
- `nats.ErrPermissionViolation` (`nats.go:117`) ← rarely seen for JS
  request/reply (the server normally silently drops the request producing
  no-responders), but we accept it as a permission signal too.
- `nats.ErrNoResponders` / `jetstream.ErrJetStreamNotEnabled` after step
  1 passed → permission-denied surface (see 1.B rationale).

**No `kv.Get` fallback.** `kv.Get` would re-use a cached `AllowDirect`,
which is exactly what we are trying to avoid; we always probe through
`stream.GetLastMsgForSubject`.

### 1.E Error classification → exit code mapping

Single table for all `ValidateLive` probes, applied uniformly by the CLI's
exit-code precedence
(`docs/plans/provision-sdk-cli/00-implementation-plan.md#exit-code-precedence`):

| Probe surface error                                                                | Exit code | Notes                                                              |
|------------------------------------------------------------------------------------|-----------|--------------------------------------------------------------------|
| `ErrJetStreamNotEnabled` / `ErrJetStreamNotEnabledForAccount` **during step 1.B**  | 4         | Reachability/JS-account failure                                    |
| `context.Canceled` / `context.DeadlineExceeded` (any probe)                        | 4         | Cancellation always wins over permission classification            |
| Permission-denied surface (`ErrNoResponders`, `ErrJetStreamNotEnabled`, `ErrPermissionViolation`) **after step 1.B succeeded** | 3 | Live-validation permission error                                  |
| `ErrStreamNotFound` (1.C)                                                          | —         | Not an error; bucket is creatable                                  |
| `ErrMsgNotFound` (1.D, after probe)                                                | —         | Not an error; key is creatable in Phase 3                          |
| Any other error                                                                    | 1         | Unknown / generic                                                  |

The classifier returns a typed `*ValidateLiveError` (or wraps with a new
sentinel `ErrLiveValidation` — implementation choice; the CLI keys on the
exit code, not the error type identity). The error message must include
the resource Kind and Name for operator diagnostics.

### 1.F Per-resource skipping rules

- `cfg.ControlPlane == nil` → skip every control-plane bucket info probe.
  Step 1.B still runs.
- `cfg.PartitionSource == nil` → skip the partition-source bucket info
  probe **and** the key probe (1.D). Step 1.B still runs.
- `cfg.DynamicConsumers` empty → skip the dynamic-consumer live checks
  slot (W4 wires the actual call; W2 only needs the slot).

Step 1.B (`AccountInfo`) is unconditional even when `cfg` declares no
resources, because an empty Config is still a valid `validate -live`
invocation and the operator wants reachability confirmed.

---

## 2. `Apply`

### 2.A Pre-flight invocation

```go
func Apply(ctx context.Context, js jetstream.JetStream, cfg Config) (Report, error) {
    if err := Validate(cfg); err != nil {
        return Report{}, err            // static validation error → exit 3
    }
    if err := validateLive(ctx, js, cfg); err != nil {
        return Report{}, err            // live validation error → exit 3 or 4
    }
    plan, err := Plan(ctx, js, cfg)     // re-uses W1's plan; deterministic
    if err != nil {
        return Report{}, err
    }
    return applyPlan(ctx, js, cfg, plan)
}
```

Ordering matches the master plan's
[`Validate` / `ValidateLive` / `Plan` / `Apply` boundary](../../plans/provision-sdk-cli/00-implementation-plan.md#validate--validatelive--plan--apply-boundary)
and the
[Exit code precedence](../../plans/provision-sdk-cli/00-implementation-plan.md#exit-code-precedence)
table (static-validate → 3, JS/auth/cancel → 4, live-validate → 3,
operation failure → 1, drift → 2).

There is no skip-validation flag in v1.

### 2.B Apply algorithm (pseudocode)

```text
report := Report{APIVersion: "parti.io/provision/v1", Kind: "Report"}

for i, action := range plan.Actions {
    if err := ctx.Err(); err != nil {
        // Mid-mutation cancel (i > 0) or pre-mutation cancel (i == 0).
        report.Aborted = true
        for _, rem := range plan.Actions[i:] {
            report.Skipped = append(report.Skipped, SkippedAction{
                Kind: rem.Kind, Name: rem.Name, Reason: "context-cancelled",
            })
        }
        return report, err
    }

    switch action.Kind {
    case ActionCreateKV:
        kvCfg := action.Resource.(jetstream.KeyValueConfig)
        // kvCfg.Metadata is already stamped (provision/plan.go:130-138).
        _, err := js.CreateKeyValue(ctx, kvCfg)
        switch {
        case err == nil:
            report.Executed = append(report.Executed, ExecutedAction{
                Kind: action.Kind, Name: action.Name,
            })
        case errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) ||
             errors.Is(err, jetstream.ErrBucketExists):
            // Race: bucket appeared between Plan and Apply. See 2.E.
            report.Executed = append(report.Executed, ExecutedAction{
                Kind: action.Kind, Name: action.Name,
                // Implementations may add a "raced: pre-existing" note in
                // the JSON Detail map (additive field; not load-bearing
                // for v1 callers).
            })
        case errors.Is(err, context.Canceled) ||
             errors.Is(err, context.DeadlineExceeded):
            report.Aborted = true
            // current action plus rest go to Skipped/context-cancelled
            for _, rem := range plan.Actions[i:] {
                report.Skipped = append(report.Skipped, SkippedAction{
                    Kind: rem.Kind, Name: rem.Name, Reason: "context-cancelled",
                })
            }
            return report, err
        default:
            // Non-cancellation failure: fail-fast (2.C).
            report.Errors = append(report.Errors, ResourceError{
                Kind: action.Kind, Name: action.Name, Error: err.Error(),
            })
            for _, rem := range plan.Actions[i+1:] {
                report.Skipped = append(report.Skipped, SkippedAction{
                    Kind: rem.Kind, Name: rem.Name, Reason: "prior-error",
                })
            }
            return report, fmt.Errorf("provision: apply %s %q: %w",
                action.Kind, action.Name, err)
        }

    default:
        // v1 only emits "create-kv"; defensive guard.
        return report, fmt.Errorf("provision: apply: unsupported action kind %q", action.Kind)
    }
}

// All actions succeeded. Drift list from Plan is carried through unchanged.
return report, nil
```

**Why `js.CreateKeyValue`, not `kvutil.EnsureKVBucketWithRetry`:**
`EnsureKVBucketWithRetry` is get-first (`kvutil/bucket.go:48-54`) — on the
Plan→Apply race it would silently return the pre-existing bucket
**without applying the Parti marker**, and the resulting `Executed` entry
would not distinguish "we created it" from "we opened someone else's
bucket." Plan already proved the bucket was missing at Plan-time; Apply
should attempt to create exactly that bucket with the marker stamped in
one shot, so we use `js.CreateKeyValue(ctx, kvCfg)` directly. The race
case is handled explicitly (see 2.E).

**Marker stamping:** Plan stamps `kvCfg.Metadata` via `BuildMarker` at
plan-build time (`provision/plan.go:130-138`); `js.CreateKeyValue` writes
the marker as part of the create call. No separate update step.

**Drift findings:** `plan.Drift` is **not** echoed into `Report` by Apply
in v1. Apply's Report is mutation-outcome only; the CLI's `apply` command
prints `plan.Drift` separately (CLI concern, lands in W5). This matches
the master plan's "Apply does not mutate based on drift findings in v1"
clause under [Safety Rules](../../plans/provision-sdk-cli/00-implementation-plan.md#safety-rules).

### 2.C Non-cancellation failure semantics (fail-fast)

Verbatim from the master plan's
[Apply failure semantics (non-cancellation)](../../plans/provision-sdk-cli/00-implementation-plan.md#apply-failure-semantics-non-cancellation)
section, restated for the W2 implementer:

- Pre-error completed actions stay in `Report.Executed`.
- The failed action is appended **once** to `Report.Errors` as a
  `ResourceError` whose `Kind`/`Name` identify the resource and whose
  `Error` is the underlying NATS error string (`err.Error()`).
- All remaining planned actions (everything after the failing index) go
  to `Report.Skipped` with `Reason = "prior-error"`. The failing action
  itself does **not** appear in `Skipped`.
- `Report.Aborted` stays **false**. `Aborted` is reserved for context
  cancellation only.
- Apply returns the wrapped error
  (`fmt.Errorf("provision: apply %s %q: %w", kind, name, err)`) plus the
  partial Report.
- CLI exit code → **1** (runtime / generic).

### 2.D Cancellation semantics (restated)

Matches the master plan's
[Cancellation contract](../../plans/provision-sdk-cli/00-implementation-plan.md#cancellation-contract).

- **Pre-mutation cancel** (`ctx.Err()` observed before any
  `js.CreateKeyValue` call): returns `(Report{Aborted: true,
  Skipped: [every planned action with reason "context-cancelled"]},
  ctx.Err())`. `Executed` and `Errors` are empty. Exit code → **4**.
- **Mid-mutation cancel** (`ctx.Err()` observed between iterations, or a
  `js.CreateKeyValue` call returns `context.Canceled`/`DeadlineExceeded`):
  returns `(partial Report with Aborted=true; Executed lists completed
  creates; current+remaining actions in Skipped with reason
  "context-cancelled"; Errors empty)`, error = `ctx.Err()`. Exit
  code → **4**.

Cancellation is **never** recorded in `Report.Errors`. The `Aborted`
flag is the discriminator.

### 2.E Race conditions

Between `Plan(ctx, js, cfg)` and the `js.CreateKeyValue` call inside
`applyPlan`, another caller may create a stream named `KV_<bucket>`. The
NATS server returns `ErrStreamNameAlreadyInUse` (which wraps as
`ErrBucketExists` at the KV API layer); see
`jetstream/errors.go:91, jetstream/kv.go:581`.

**Resolution:** treat the race as a successful outcome of this Apply
action. The action is appended to `Report.Executed` (not `Report.Errors`).
Apply does **not** re-read live state to drift-check the now-existing
bucket; doing so would smear Plan's snapshot semantics across Apply and
complicate the failure-semantics contract in 2.C. The canonical drift
report for this run is `plan.Drift` (already computed in 2.A).

Operator-visible effect: re-running `partictl plan` after the race
surfaces the pre-existing bucket's marker/drift status. If the racing
creator did not stamp the Parti marker, the next `plan` reports `adopted`
drift, which is the standard W1 path.

### 2.F No update path in v1

When `plan.Actions` is empty — every named resource already exists and
matches, or exists with drift — Apply does nothing and returns
`(Report{APIVersion, Kind, Executed: nil, Skipped: nil, Errors: nil,
Aborted: false}, nil)`. The CLI exit code is **0** (or **2** if
`-fail-on-drift` is set and any drift is non-informational). Apply
itself never produces exit code 2 — that is a CLI concern.

---

## 3. Behavior table

| Scenario                                                | Validate | ValidateLive | Apply mutation | `Report.Aborted` | `Report.Executed`      | `Report.Errors`   | `Report.Skipped`               | Returned error          | CLI exit |
|---------------------------------------------------------|----------|--------------|----------------|------------------|------------------------|-------------------|--------------------------------|-------------------------|----------|
| Static-validate fails                                   | fail     | —            | —              | false            | empty                  | empty             | empty                          | `ErrInvalidConfig`-wrapped | 3        |
| AccountInfo fails (JS down / no `$JS.API.INFO`)         | ok       | fail (1.B)   | —              | false            | empty                  | empty             | empty                          | `ErrJetStreamNotEnabled` | 4        |
| AccountInfo ok; STREAM.INFO returns ErrNoResponders     | ok       | fail (1.C)   | —              | false            | empty                  | empty             | empty                          | live-validation error    | 3        |
| Partition-source key probe permission denied (after retry) | ok    | fail (1.D)   | —              | false            | empty                  | empty             | empty                          | live-validation error    | 3        |
| Partition-source key probe AllowDirect-flip retry succeeds | ok    | ok           | full           | false            | every action           | empty             | empty                          | nil                      | 0        |
| Live cancel during ValidateLive                         | ok      | fail (cancel)| —              | false (\*)       | empty                  | empty             | empty                          | `ctx.Err()`              | 4        |
| All planned creates succeed                             | ok      | ok           | full           | false            | every action           | empty             | empty                          | nil                      | 0        |
| Pre-mutation cancel (ctx already done)                  | ok      | ok           | none           | **true**         | empty                  | empty             | every action / `context-cancelled` | `ctx.Err()`           | 4        |
| Mid-mutation cancel                                     | ok      | ok           | partial        | **true**         | actions completed before cancel | empty    | current+remaining / `context-cancelled` | `ctx.Err()`         | 4        |
| Mid-mutation resource error (e.g. server rejection)     | ok      | ok           | partial        | false            | actions completed before failure | failing action × 1 | actions after failure / `prior-error` | wrapped NATS err  | 1        |
| Plan→Apply race (`ErrBucketExists`)                     | ok      | ok           | full           | false            | every action (raced row marked) | empty    | empty                          | nil                      | 0        |
| Empty plan (all desired resources exist)                | ok      | ok           | no-op          | false            | empty                  | empty             | empty                          | nil                      | 0 (or 2 with `-fail-on-drift`) |

(\*) Cancellation observed strictly inside `ValidateLive` does not produce
a populated Report because Apply has not yet reached `applyPlan`.
`Report{}` is returned (zero value: `Aborted = false`).

---

## 4. Test plan (W2-specific)

The tests below are additive to the master plan's
[Test Plan](../../plans/provision-sdk-cli/00-implementation-plan.md#test-plan)
embedded-NATS section. Names are illustrative.

1. **`TestValidateLive_AccountInfoFailure_ExitCode4`** — start the
   embedded NATS server without JetStream enabled (or with a user that
   has no `$JS.API.INFO` publish grant); assert ValidateLive returns an
   error that maps to exit code 4.
2. **`TestValidateLive_BucketInfoPermissionDenied_ExitCode3`** — create
   a user with `$JS.API.INFO` but **not** `$JS.API.STREAM.INFO.KV_*`;
   assert ValidateLive returns a permission-denied error mapping to
   exit code 3. (Cross-references the master plan's
   [Permissions](../../plans/provision-sdk-cli/00-implementation-plan.md#permissions)
   table row for `ValidateLive`.)
3. **`TestValidateLive_PartitionSourceKey_AllowDirectTrue_KeyMissing_OK`**
   — create the partition-source bucket with `AllowDirect=true` (nats.go
   default for `CreateKeyValue`), assert ValidateLive succeeds for the
   absent key.
4. **`TestValidateLive_PartitionSourceKey_AllowDirectFalse_KeyMissing_OK`**
   — create the bucket with `AllowDirect=false` via `js.CreateStream`
   with an explicit `StreamConfig`; assert ValidateLive succeeds for the
   absent key.
5. **`TestValidateLive_PartitionSourceKey_AllowDirectFlip_RetrySucceeds`**
   — open a bucket with `AllowDirect=true`, race-update it to
   `AllowDirect=false` between STREAM.INFO and the first probe (use
   `js.UpdateStream` to flip), assert the second probe succeeds. This
   exercises the §1.D retry protocol.
6. **`TestValidateLive_PartitionSourceKey_PermissionDenied_ExitCode3`** —
   user has bucket read but no `$JS.API.DIRECT.GET.*` and no
   `$JS.API.STREAM.MSG.GET.*`; assert ValidateLive returns a
   permission-denied error after the single retry, exit code 3.
7. **`TestApply_NonCancellationFailure_FailFast`** — pre-create a
   non-KV stream named `KV_parti-heartbeat` (e.g. via `js.CreateStream`
   with subjects `KV_parti-heartbeat.*` and `MaxMsgsPerSubject != 1` so
   the create-KV call's underlying create-stream returns a non-race
   conflict). Assert: first create-kv succeeds, second fails, the
   failing action lands in `Errors` with the heartbeat resource, the
   third (assignment) lands in `Skipped` with reason `prior-error`,
   `Aborted == false`, and the returned error wraps the NATS error.
8. **`TestApply_MidMutationCancel`** — install a stream-create hook
   that cancels `ctx` on the second action; assert first action in
   `Executed`, remaining in `Skipped` with reason `context-cancelled`,
   `Aborted == true`, error is `ctx.Err()`.
9. **`TestApply_PreMutationCancel`** — pass an already-cancelled `ctx`
   (post-`ValidateLive` — easiest to test by canceling after
   `validateLive` returns but before `applyPlan` starts; equivalent
   harness: a fake `js` that succeeds reachability + info but the
   harness cancels `ctx` before the create loop). Assert
   `Report{Aborted: true, Skipped: every-action/context-cancelled}`,
   `Executed/Errors` empty.
10. **`TestApply_PlanRaceBucketExists_Success`** — between Plan and
    Apply, create the second bucket out-of-band; assert Apply still
    returns success with all actions in `Executed`.
11. **`TestApply_EmptyPlan_NoOp`** — pre-create all desired buckets with
    matching markers; assert Apply returns `Report{}` (zero value
    except `APIVersion`/`Kind`) and `nil` error.

Tests 1–6 exercise ValidateLive; 7–11 exercise Apply. All tests use the
existing embedded-NATS harness.

---

## 5. Open questions

All resolved here; nothing carries forward as open.

- **Q: Does Apply re-read live state per action, or trust Plan's snapshot?**
  Resolution: **trust Plan**. Apply only inspects live state when
  `js.CreateKeyValue` returns an error, and only to classify
  `ErrBucketExists`/`ErrStreamNameAlreadyInUse` as a benign race. Re-doing
  drift classification inside Apply would smear Plan's snapshot
  semantics and complicate fail-fast.
- **Q: How is the Plan→Apply race recorded?**
  Resolution: **as `Executed` with no error**. The mutation outcome of
  "the desired bucket exists now" is achieved; the next `plan` reveals
  drift (including `adopted` drift if the racing creator was not Parti).
- **Q: Does `ValidateLive` short-circuit on the first missing-bucket
  case?** Resolution: **no**. `ErrStreamNotFound` is not an error for
  `ValidateLive`; it merely means "this bucket will be created by
  apply" and the per-bucket probe loop continues.

---

## Master plan amendments

None. The master plan delegated the retry algorithm (§1.D), the Apply
race semantics (§2.E), and the create-vs-ensure choice (§2.B) to this
sub-spec, and this sub-spec resolves all three. Every other behavior is
restated from existing master-plan sections by header reference.
