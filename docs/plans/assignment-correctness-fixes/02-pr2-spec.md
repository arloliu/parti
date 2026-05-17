# PR-2 Implementation Spec — Audit Grace Windows Use Monotonic Observation Time

Implements **ISSUE-007** from
[`00-fix-plan.md`](./00-fix-plan.md), per
`tmp/assignment_review/07-verification-plan.md` §7.7.

**Revisions:**
- v1 (initial draft)
- v2 (this revision) — address Codex review `tmp/02-pr2-spec_pr2-impl-spec_v1_review.md`:
  - P1 Test 7.7 conflict with fixture migration: the new test must
    OVERRIDE `lastCommitObservedAtMono = time.Now()` after the
    fixture builds it (default fixture sets it to PublishedAt to
    preserve existing-test intent; the new test specifically wants
    "old wire timestamp but fresh local observation").
  - P2 Godoc gap: `Config.ApplyGracePeriod` and
    `Config.ExtendedApplyGracePeriod` (`config.go:60-67`) Godoc
    must be updated to reflect the new "from local observation"
    semantic instead of "from commit.PublishedAt".

---

## 1. The bug, in one paragraph

The leader-side apply audit (`internal/assignment/calculator_audit.go:70, :80`)
gates retry-pressure metrics and `audit_repair` escalation on
`time.Since(commit.PublishedAt) < c.ApplyGracePeriod` and
`< c.ExtendedApplyGracePeriod` respectively. `commit.PublishedAt` is the
WALL-CLOCK timestamp set by the LEADER THAT ISSUED the CAS
(`assignment_publisher.go:383`). On leader handoff to a new node with a
different system clock, or after container suspend/resume, the new leader's
wall clock can disagree with `PublishedAt` by minutes or hours — causing
either suppressed audit-repair (commit looks "future" to us; grace never
expires) or premature escalation (commit looks ancient; grace appears long
expired before the new leader's workers have had a chance to ACK to it).

The plan was Codex-upgraded from S3 to S2 because `maybeEscalateAudit`
gates *real recovery* (an `audit_repair` rebalance), not just retry-pressure
metrics.

---

## 2. Fix design

Track when **this leader** observed the commit, using Go's monotonic clock
reading (which is unaffected by wall-clock changes and immune to leader
handoff because we set it locally). Use that observation time for grace
window computation. The wire-format `PublishedAt` field is **unchanged** —
it remains the protocol carrier for audit/log/ops visibility.

### 2.1 Field placement

Add a private field on `AssignmentPublisher`:

```go
// lastCommitObservedAtMono records the local monotonic-clock instant at
// which THIS publisher observed lastCommit — either via a successful
// CAS (Publish step 9) or via BootstrapLastCommit. The audit uses this
// instead of commit.PublishedAt for grace-window math so wall-clock
// skew across leader handoff cannot suppress or prematurely trigger
// audit_repair escalation.
//
// Zero value means "no commit observed yet" (lastCommit is also nil in
// that case). Set under p.mu.
lastCommitObservedAtMono time.Time
```

Placement: in `AssignmentPublisher` struct, immediately after `lastCommit`
(`assignment_publisher.go:108`).

### 2.2 Set sites

Two sites, both already hold `p.mu`:

1. **Successful CAS** (`assignment_publisher.go:422-428`, just after `lastCommit = &commitCopy`):
   ```go
   p.lastCommit = &commitCopy
   p.lastCommitObservedAtMono = time.Now()  // captures monotonic reading
   ```

2. **Bootstrap** (`assignment_publisher.go:996-1005`, inside the `p.mu` block):
   ```go
   p.lastCommit = &commitCopy
   p.lastCommitObservedAtMono = time.Now()
   ```

`time.Now()` in Go since 1.9 includes a monotonic reading, used by
`time.Since` automatically. We do not need `runtime.nanotime` or anything
exotic — `time.Since(lastCommitObservedAtMono)` is monotonic.

### 2.3 Accessor

Single new method on `AssignmentPublisher`:

```go
// LastCommitObservedAt returns the local monotonic-clock instant at
// which this publisher observed the current lastCommit. Returns the
// zero time when lastCommit is nil.
//
// Callers use time.Since on the return value; subtracting wall-clock
// times against this value is meaningless and will silently lose the
// monotonic reading.
func (p *AssignmentPublisher) LastCommitObservedAt() time.Time {
    p.mu.Lock()
    defer p.mu.Unlock()
    return p.lastCommitObservedAtMono
}
```

Placement: immediately below `LastCommit()` (`assignment_publisher.go:960`).

**TOCTOU note:** the audit reads `LastCommit()` then `LastCommitObservedAt()`
in sequence. Between the two calls a fresh Publish could land, replacing
both `lastCommit` and `lastCommitObservedAtMono`. The mismatch is benign:
the audit would be classifying workers against commit V_n but gating
against observation time of V_{n+1} (later, so grace windows are
guaranteed not-yet-expired → no premature escalation). The alternative
(a combined accessor returning both) is heavier and the misalignment
never produces a false-positive escalation, only a one-tick deferral.
**Stick with two accessors; document the benign skew.**

### 2.4 Audit call-site changes

`internal/assignment/calculator_audit.go`:

```go
// At line 70 (was: time.Since(commit.PublishedAt)):
if time.Since(c.publisher.LastCommitObservedAt()) < c.ApplyGracePeriod {
    return
}
// ...
// At line 80 (was: time.Since(commit.PublishedAt)):
if time.Since(c.publisher.LastCommitObservedAt()) < c.ExtendedApplyGracePeriod {
    return
}
```

No other audit logic changes. `commit.PublishedAt` remains as the wire
field; the existing log line / metric that references it (if any) is
unchanged.

### 2.5 Config Godoc updates (PR2-V1-002)

`internal/assignment/config.go:60-67` Godoc for `ApplyGracePeriod` and
`ExtendedApplyGracePeriod` references `commit.PublishedAt`. Update both
to reflect the new semantic:

```go
// ApplyGracePeriod is the time after THIS leader observed the current
// commit (via successful CAS or BootstrapLastCommit) before the audit
// loop emits retry-pressure metrics for behind workers. Measured against
// a monotonic clock so cross-leader wall-clock skew does not influence
// the grace window. Default: 2 × HeartbeatTTL.
ApplyGracePeriod time.Duration

// ExtendedApplyGracePeriod is the time after THIS leader observed the
// current commit before the audit may escalate via two-phase handoff
// (audit_repair rebalance). Measured against a monotonic clock; see
// ApplyGracePeriod. Default: 5 × HeartbeatTTL.
ExtendedApplyGracePeriod time.Duration
```

Pure doc change; no Config field rename, no validation logic touched.

---

## 3. Test design (Test 7.7)

### 3.1 New test — `TestCalculatorAudit_GraceFromLocalObservedAt`

**File:** `internal/assignment/calculator_audit_test.go` (append).

**Intent:** prove the audit's grace windows depend on the new leader's
local observation time, not on the wire `PublishedAt`.

**Setup:**
- Build an `auditTestFixture` (existing helper at `:88`).
- Synthetic commit with `PublishedAt = time.Now().Add(-1*time.Hour)`
  (simulating an old wire timestamp from a former leader / clock-skewed peer).
- Heartbeats classify workers as `behind` (mismatched `AppliedVersion`).
- Configure `ApplyGracePeriod = 1 * time.Hour` and
  `ExtendedApplyGracePeriod = 1 * time.Hour` so the wall-clock check
  WOULD have elapsed but the local-observed check has NOT.

**Critical test setup detail (PR2-V1-001):** the default
`auditTestFixture` sets `lastCommitObservedAtMono = commit.PublishedAt`
to preserve existing-test intent. Test 7.7 specifically tests "old
wire timestamp BUT fresh local observation", so it must OVERRIDE
`calc.publisher.lastCommitObservedAtMono = time.Now()` AFTER the
fixture builds it. Without that override, `time.Since(observedAt) ≈ 1h`
which equals `ApplyGracePeriod=1h` and the gate appears elapsed —
defeating the test.

**Pre-fix behavior:** `time.Since(commit.PublishedAt) ≈ 1h ≥ 1h`, both
grace windows appear elapsed → `RecordWorkerBehind` fires AND
`maybeEscalateAudit` may emit metrics / call `EnterScaling`.

**Post-fix behavior:** test overrides `lastCommitObservedAtMono` to
`time.Now()` → `time.Since(observedAt) ≈ 0` → both grace gates
short-circuit → no `RecordWorkerBehind`, no escalation-skip metric,
no `EnterScaling`.

**Assertions:**
```go
// After fixture build:
f.calc.publisher.lastCommitObservedAtMono = time.Now()

f.calc.auditApplied(context.Background())

require.Empty(t, f.metrics.getBehindCalls(),
    "retry-pressure metric must NOT fire before local-observed ApplyGracePeriod elapses")
require.Empty(t, f.metrics.getEscalationSkipped(),
    "escalation-skip metrics must NOT fire before local-observed ExtendedApplyGracePeriod elapses")
```

The auditTestFixture must be tweaked to set `lastCommitObservedAtMono`
explicitly — see §3.2 below.

### 3.2 Fixture compat

Existing `auditTestFixture` (line 111) constructs:
```go
calc.publisher = &AssignmentPublisher{lastCommit: commit, logger: ...}
```

After my change, the fixture must also set
`lastCommitObservedAtMono`. To preserve the EXISTING tests' intent
(they use `PublishedAt = time.Now().Add(-time.Hour)` to express "grace
has elapsed"), the fixture should set:

```go
calc.publisher = &AssignmentPublisher{
    lastCommit:               commit,
    lastCommitObservedAtMono: commit.PublishedAt,  // match prior wall-clock semantics
    logger:                   logging.NewNop(),
}
```

This is a **fixture-only** change. It preserves the prior tests' verdicts
because they were *intent-encoding* "elapsed grace" via an old PublishedAt;
mirroring that on observedAt keeps them passing. Existing tests don't
need new assertions.

One exception: the existing
`TestCalculatorAudit_GracePeriod_WithinGrace` test (line 501) uses
`PublishedAt: time.Now()` to express "grace not yet elapsed". The
fixture change preserves this intent (observedAt == now), so the test
keeps passing without modification.

---

## 4. Compatibility

| Surface | Change | Compat |
|---|---|---|
| `types.AssignmentCommit` | Unchanged. `PublishedAt` remains the wire timestamp. | ✅ |
| `AssignmentPublisher.LastCommit()` | Unchanged signature; still returns a deep copy. | ✅ |
| `AssignmentPublisher.LastCommitObservedAt()` | New public method. Additive. | ✅ |
| Audit grace semantics | Behavior change: grace is now measured from THIS leader's observation, not from wire `PublishedAt`. Strict improvement; failure modes the old code had (wall-clock skew suppressing or prematurely firing escalation) are eliminated. | ✅ Semantic correction |
| Wire format / KV schema | Unchanged. | ✅ |
| Rollback | Clean revert; `lastCommitObservedAtMono` field is internal. | ✅ |

---

## 5. Risk audit

| Risk | Mitigation |
|---|---|
| TOCTOU between LastCommit() and LastCommitObservedAt() | Documented benign skew (§2.3). Misalignment always defers, never prematurely escalates. |
| Fixture migration breaks existing audit tests | The fixture change in §3.2 mirrors prior intent. All ~12 existing audit tests use `PublishedAt` as the grace-control knob; mapping that onto observedAt preserves their verdicts. |
| `time.Since` does NOT use monotonic reading if the time was constructed without it | `time.Now()` always includes a monotonic reading in Go 1.9+ (parti targets >=1.21). Synthetic times like `time.Now().Add(-1h)` retain the monotonic reading. Only times constructed via `time.Date` / `time.Unix` lose it; we never construct observedAt that way. |
| New leader takeover: prior leader's commit gets a fresh grace window | This IS the intended semantic. Workers may need to re-ACK to the new leader; restarting the grace window is correct (this is the bug ISSUE-007 names). |
| Audit suppressed indefinitely if Publish never observes a commit | `lastCommit` is also nil in that case; audit's existing `if commit == nil` early-return handles it. |
| Could affect the existing-leader steady-state grace timing | No. For a long-lived leader, observedAt advances with every successful CAS. `time.Since(observedAt)` from the new accessor equals `time.Since(PublishedAt)` in the absence of wall-clock movement — same behavior as before. |

---

## 6. LOC budget

| File | Estimated LOC |
|---|---|
| `assignment_publisher.go` (field + 2 set sites + accessor) | +12 |
| `calculator_audit.go` (2 line changes) | +0 / -0 net |
| `calculator_audit_test.go` (fixture init + 1 new test) | +50 |
| Total | ~15 LOC production + ~50 LOC tests |

Matches the plan's "~20 LOC + 1 test" estimate.

---

## 7. Out of scope

- ISSUE-001 (CAS-loss recovery, PR-4).
- ISSUE-006 / ISSUE-008 / ISSUE-004 (PR-3 housekeeping).
- Renaming `PublishedAt` or adding a new wire field — the wall-clock
  remains protocol-visible for log/ops; only the audit's local semantic
  changes.
- Other consumers of `commit.PublishedAt` if any — only the audit's two
  grace gates are wrong; everything else that uses `PublishedAt` does so
  for display / ordering and is correct against the wire field.
