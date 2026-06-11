# Consumer Sweep Fixes Implementation Plan (Batch 2)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the consensus-confirmed defects from the pre-release
Queue/Static/Broadcast QA sweep and the zero-overlap doc audit
(tmp/consumer-sweep-findings-v1.md, ratified by codex in
tmp/codex-sweep-r1-verdict.md), accumulating on PR #47 before release.

**Architecture:** All fixes are local to the consumer layer (consumer/,
internal/ipartition/, internal/durable/) plus a docs-truth batch. No manager
core changes. One new exported sentinel (`ErrConsumerStopped`). Two behavior
changes (FetchTimeout floor, reject-restart) — changelog entries.

**Tech Stack:** Go, NATS JetStream, testify, embedded-NATS harness.

**Repo discipline (every task):** verify-first (run each new test before the
fix, record the failure), `make lint` before commit, no attribution trailers,
no plan jargon in commit messages. Anchor on symbols, not line numbers.

**Consensus decisions baked in:** validation floor (not clamp) for N1;
reject-restart (not restart-support) for N3/N5 with a shared
`types.ErrConsumerStopped` sentinel; mutual-exclusion (not drain-after-close)
for N6; wire (not document) backoff growth for N7, INCLUDING Dynamic's
`delayWithBackoffOrExit` (NF1); D1-D4 deferred.

---

### Task A: FetchTimeout 1s validation floor (N1, P1)

**Files:**
- Modify: `consumer/common.go` (CommonConfig.FetchTimeout tag + godoc)
- Test: `consumer/common_test.go`
- Modify: `CHANGELOG.md` (done in Task I)

- [ ] **Step 1: Write the failing test**

```go
// TestCommonConfig_FetchTimeout_FloorIsOneSecond pins the validation floor:
// nats.go rejects PullExpiry below 1s at iterator-creation time, so a
// sub-second FetchTimeout produced a consumer whose Start succeeded and then
// failed every iterator creation forever — a permanently dead consumer with
// a success return. Construction must reject it instead.
func TestCommonConfig_FetchTimeout_FloorIsOneSecond(t *testing.T) {
	cfg := CommonConfig{FetchTimeout: 500 * time.Millisecond}
	err := cfg.Validate()
	require.Error(t, err, "FetchTimeout below the 1s NATS PullExpiry floor must fail validation")

	cfg = CommonConfig{FetchTimeout: time.Second}
	require.NoError(t, cfg.Validate(), "FetchTimeout == 1s is the boundary and must pass")
}
```

Mirror neighboring tests in `consumer/common_test.go` for construction style
(Validate calls SetDefaults first — zero value gets the default and passes).

- [ ] **Step 2: Run, expect FAIL** — `go test ./consumer/ -run TestCommonConfig_FetchTimeout -v`
(sub-second currently passes validation).

- [ ] **Step 3: Implement** — in `consumer/common.go`, change the FetchTimeout
tag from `validate:"gt=0"` (find the exact current tag) to enforce >= 1s
(fuda supports `gte=1s`-style duration tags — check how other duration floors
in this repo express it, e.g. grep `validate:"gte=` for duration fields; if
tag syntax can't express 1s, add an explicit check in
`CommonConfig.Validate()` after fuda.Validate returning
`fmt.Errorf("%w: FetchTimeout must be at least 1s (NATS PullExpiry floor), got %v", ErrInvalidConfig, c.FetchTimeout)`).
Update the FetchTimeout godoc: document the 1s floor and why.

- [ ] **Step 4: Run all consumer + durable + ipartition tests** — fix any test
fixture using a sub-1s FetchTimeout (raise to 1s; do NOT weaken the check).
Note each fixture change in the report.

- [ ] **Step 5: Lint + commit** — `fix(consumer): reject FetchTimeout below the 1s NATS PullExpiry floor` (body: dead-consumer-with-successful-Start story; behavior change note).

---

### Task B: Signal the nil-recovery silent stall (N2, P1)

**Files:**
- Modify: `consumer/queue.go` (the runLoop iterator-error branch where
  `q.recovery` is nil / Classify short-circuits — find the Debug "iterator
  error" log)
- Modify: `internal/ipartition/consumer.go` (same — Debug log ~`run` loop)
- Modify: `internal/durable/broadcast_consumer.go` (same)
- Test: `consumer/queue_test.go`, plus the equivalent test files for the other two

- [ ] **Step 1: Write the failing tests (one per consumer)**

Queue (adapt for the other two — each file has an injectable IteratorFactory
and a recorded-metrics stub; mirror the file's existing recovery tests):

```go
// TestQueue_RecoveryDisabled_IteratorErrorIsSignaled pins the operator
// signal for the default configuration: with RecoveryDisabled (nil
// controller), a non-graceful iterator error (e.g. the durable was deleted)
// must emit a Warn log and the iterator-restart metric. Pre-fix the only
// artifact was a Debug log — at production log levels a deleted durable was
// a zero-signal permanent stall.
func TestQueue_RecoveryDisabled_IteratorErrorIsSignaled(t *testing.T) {
	// Arrange a Queue with RecoveryDisabled (default), an IteratorFactory
	// whose iterator returns jetstream.ErrNoHeartbeat from Next(), and the
	// package's recording metrics stub. Run one loop cycle (or Start +
	// brief settle + Stop, matching this file's loop-test pattern).
	// Assert: metrics recorded IncrementWorkerConsumerIteratorRestart with
	// a non-empty reason, and (if the file has a recording logger) a Warn
	// was emitted. Mirror TestQueue_RunLoop_FailedRecoveryUsesBackoff for
	// the harness shape.
}
```

Write the full test bodies against the harness each file actually has (read
the neighboring tests first); the assertion contract is fixed: Warn-level +
iterator-restart metric on the nil-recovery path.

- [ ] **Step 2: Run, expect FAIL** (no metric, Debug-only).

- [ ] **Step 3: Implement** — in each consumer's iterator-error handling,
when the recovery controller is nil (Queue: `q.recovery == nil`; Static/
Broadcast: equivalent), before the backoff: log Warn (message:
"iterator error with recovery disabled; consumer will retry but cannot
recreate a deleted durable" + error + subject/stream fields) and emit
`IncrementWorkerConsumerIteratorRestart("recovery_disabled")` when a metrics
collector is configured. Do NOT change the retry semantics. Keep the Debug
log if it carries extra fields, or fold into the Warn.

- [ ] **Step 4: Run tests, expect PASS**; run the three packages fully.

- [ ] **Step 5: Lint + commit** — `fix(consumer): surface iterator errors when recovery is disabled` (body: default-config deleted-durable = Debug-only zero-metric stall story).

---

### Task C: Terminal reject-restart for Broadcast and Static (N3+N5, P1/P2)

**Files:**
- Modify: `types/errors.go` (new sentinel), `consumer/errors.go` (re-export)
- Modify: `internal/durable/broadcast_consumer.go`
- Modify: `internal/ipartition/consumer.go`
- Modify: `consumer/broadcast.go`, `consumer/static.go` (godoc)
- Test: `internal/durable/broadcast_consumer_test.go`, `internal/ipartition/consumer_test.go` (or the files where lifecycle tests live — locate with grep)

- [ ] **Step 1: Add the sentinel**

`types/errors.go` (mirror the file's existing sentinel style):

```go
// ErrConsumerStopped is returned when an operation requires a running
// consumer but the consumer has been stopped/closed. Stop is terminal for
// Static and Broadcast consumers: create a new instance to consume again.
// Pre-fix, a stopped Broadcast reported success ("active") on subsequent
// Start/UpdateWorkerConsumer calls while its pull loop was permanently dead.
var ErrConsumerStopped = errors.New("consumer is stopped")
```

Re-export in `consumer/errors.go` alongside the existing re-exports.

- [ ] **Step 2: Write the failing tests**

```go
// TestBroadcastConsumer_UpdateAfterClose_ReturnsErrConsumerStopped pins the
// terminal-Stop contract: Close cancels the pull loop permanently
// (loopStarted was never reset; loopDone cannot be reused), so a subsequent
// UpdateWorkerConsumer/Start must FAIL with the sentinel instead of logging
// "active" and returning nil — which let a manager-driven apply report
// success with a dead fan-out loop and the worker Stable.
func TestBroadcastConsumer_UpdateAfterClose_ReturnsErrConsumerStopped(t *testing.T) {
	// Construct via the file's existing harness (live embedded NATS or stub
	// — mirror the existing Close test), Start/Update once, Close, then:
	err := bc.UpdateWorkerConsumer(ctx, "w1", nil)
	require.ErrorIs(t, err, types.ErrConsumerStopped)
}
```

Static equivalent: Start → Stop → Start must return ErrConsumerStopped
(pre-fix returns nil; godoc already promises Start-once). Also pin the
DispatchByKey variant if cheap: restart returns the error BEFORE any
fetch-never-process loop can exist.

- [ ] **Step 3: Run, expect FAIL** (both currently return nil).

- [ ] **Step 4: Implement**

Broadcast (`internal/durable/broadcast_consumer.go`): add `closed bool` under
the existing mutex; `Close` sets it (idempotent: repeated Close stays nil per
current contract — verify current double-Close behavior and keep it);
`UpdateWorkerConsumer` (and any Start entry) returns
`fmt.Errorf("broadcast consumer: %w", types.ErrConsumerStopped)` when closed.

Static (`internal/ipartition/consumer.go`): add `stopped bool` set in `Stop`
(keep `cancel=nil` reset); `Start` returns
`fmt.Errorf("js consumer: %w", types.ErrConsumerStopped)` when stopped.
`Stop` stays idempotent.

Godoc: `consumer/broadcast.go` Start/Stop and `consumer/static.go` Start/Stop
— state Stop is terminal; create a new instance to consume again; name the
sentinel.

- [ ] **Step 5: Run tests + both packages; lint + commit** — `fix(consumer): make Stop terminal for Static and Broadcast` (body: dead-loop-reports-success story; behavior change note).

---

### Task D: keyDispatcher idle-exit/Dispatch mutual exclusion (N6, P2)

**Files:**
- Modify: `internal/ipartition/key_dispatcher.go`
- Test: `internal/ipartition/key_dispatcher_test.go`

**The race (confirmed):** worker idle path runs `len(worker.msgCh)==0` then
`close(worker.closeCh); return` with NO synchronization against `Dispatch`'s
send (`worker.msgCh <- km` in a select). A send can commit between the
len-check and the close; the worker exits without draining; the message is
stranded unacked until AckWait, and later same-key messages process first —
breaking the documented per-key ordering. NOTE: a closeCh pre-check alone is
insufficient — Go's select picks randomly when both the send and a closed
closeCh are ready.

**The fix (consensus: mutual exclusion):** serialize the send-commit and the
exit-decision under `kd.mu`:

1. `Dispatch` fast path: under `kd.mu.RLock()`, look up the worker AND
   attempt a non-blocking send in the same critical section:

```go
	for {
		kd.mu.RLock()
		worker, exists := kd.workers[key]
		if exists {
			select {
			case worker.msgCh <- keyMessage{ctx: ctx, msg: msg}:
				kd.mu.RUnlock()
				return true
			default: // buffer full — fall through to the wait below
			}
		}
		kd.mu.RUnlock()

		if !exists {
			if w := kd.getOrCreateWorker(key); w == nil {
				return false // dispatcher closed
			}
			continue // retry the locked fast path against the fresh worker
		}

		// Backpressure: buffer full. Wait for drain progress, worker close,
		// or dispatcher shutdown, then RETRY THE LOCKED FAST PATH — never
		// commit a send outside the lock (an unlocked blocking send is the
		// exact race this fix removes).
		select {
		case <-worker.closeCh:
			select {
			case <-worker.done:
			case <-kd.ctx.Done():
				return false
			}
		case <-kd.ctx.Done():
			return false
		case <-time.After(time.Millisecond):
		}
	}
```

2. Worker idle-exit: take the WRITE lock for the decision, remove from the
   map inside it, close outside:

```go
		case <-timer.C:
			kd.mu.Lock()
			if len(worker.msgCh) == 0 {
				// No send can commit concurrently (sends hold RLock) and no
				// future sender can find this worker (removed under the same
				// lock) — the stranded-message window is closed.
				delete(kd.workers, worker.key)
				kd.mu.Unlock()
				close(worker.closeCh)
				return
			}
			kd.mu.Unlock()
			timer.Reset(kd.idleTimeout)
```

   Adjust the deferred `kd.removeWorker(worker.key)` so it cannot delete a
   SUCCESSOR worker for the same key (delete only if the map still holds this
   exact worker pointer; check removeWorker's current shape and make it
   pointer-conditional).

3. Audit the dispatcher-shutdown drain path (`<-kd.ctx.Done()` case in
   runWorker) against the same invariant — it drains, so it is exempt, but
   confirm its map removal also cannot clobber a successor.

- [ ] **Step 1: Write the failing reproducer**

```go
// TestKeyDispatcher_IdleExitDoesNotStrandMessages hammers the idle-exit
// window: a razor-thin idle timeout with paced single-key dispatches makes
// the worker's exit decision race Dispatch's send. Every message Dispatch
// accepted (returned true) must be processed exactly once and in dispatch
// order — pre-fix, a send committing between the worker's len-check and its
// close left the message stranded in a dead worker's channel (unprocessed
// here; redelivered only after AckWait in production, breaking per-key
// ordering).
func TestKeyDispatcher_IdleExitDoesNotStrandMessages(t *testing.T) {
	// Construct keyDispatcher directly (same package) with idleTimeout =
	// 1*time.Millisecond, channelBuf >= 4, a handler that appends the
	// message's sequence to a mutex-guarded []int.
	// Loop i := 0; i < 2000; i++ { Dispatch(msg with key "k", seq i);
	// time.Sleep(timing jittered around 1ms — alternate 0/1/2ms so dispatches
	// straddle the idle boundary) }.
	// After a final settle (Close or drain-wait), assert the recorded
	// sequence equals exactly 0..1999 in order (no gaps = no stranding,
	// no inversions = ordering preserved). Run with -race.
}
```

Use the file's existing test scaffolding for constructing the dispatcher and
fake messages (read it first; there are existing keyDispatcher tests —
locate with `grep -n "func Test" internal/ipartition/key_dispatcher_test.go`).

- [ ] **Step 2: Run with -race -count=5, expect FAIL** (gaps in the sequence).
If 2000 iterations don't trip it, raise iterations / tighten pacing before
concluding anything; record the failing output.

- [ ] **Step 3: Implement the fix above.**

- [ ] **Step 4: Run with -race -count=5, expect PASS; run the whole
ipartition package -race** (the locked fast path touches every Dispatch).

- [ ] **Step 5: Lint + commit** — `fix(consumer): serialize key-worker idle exit with dispatch sends` (body: stranded-message/ordering story).

---

### Task E: Wire real backoff growth in Broadcast and Dynamic loops (N7+NF1, P2)

**Files:**
- Modify: `internal/durable/broadcast_consumer.go` (`delayOrExit` + a
  persistent prev-backoff field + seeded rng)
- Modify: `internal/durable/partition_consumer.go` (`delayWithBackoffOrExit`
  — same constant-Base bug: `jitterBackoff(0, ...)` with nil rng)
- Test: `internal/durable/broadcast_consumer_test.go`, `internal/durable/partition_consumer_test.go`

- [ ] **Step 1: Write the failing tests**

For each: a unit test that invokes the delay helper repeatedly and asserts
the produced delays GROW toward Max (mirror how `consumer/queue.go` threads
`prev` — `queue.go` around `delayWithBackoffOrExit(ctx, &backoff)` and its
seeded rng at ~`:500-506`; and mirror whatever existing backoff tests exist —
grep `jitterBackoff` in *_test.go). Test shape: with Base=10ms, Multiplier=2,
Max=80ms, Seed fixed, call the helper N times capturing delays (inject a
clock or assert monotonic non-decreasing until cap; if delays are slept, keep
Base tiny and measure via the captured values not wall time — prefer
refactoring the helper to RETURN the chosen delay so the test reads it).

```go
// TestBroadcastDelayOrExit_BackoffGrows pins that consecutive retry delays
// grow per Multiplier up to Max. Pre-fix delayOrExit passed prev=0 on every
// call, so jitterBackoff returned Base unconditionally: Multiplier, Max and
// Seed in BroadcastConfig.Retry were dead config.
```

- [ ] **Step 2: Run, expect FAIL** (constant Base).

- [ ] **Step 3: Implement** — give broadcastConsumer a `retryPrev
time.Duration` (reset to 0 on successful iterate, mirroring Queue's
`backoff=0` reset) and a seeded `*rand.Rand` built once from
`config.Retry.Seed` (Queue's pattern); thread both through `delayOrExit` →
`jitterBackoff(bc.retryPrev, ...)` and store the result back. Same for
`partitionConsumer.delayWithBackoffOrExit`: add `retryPrev` field + seeded
rng, reset on successful iterator episode (where the loop currently resets
escalation counters / on ActionContinue), thread through. Check
`startConsumerLoop`'s zero-delay 3-attempt loop: add the same jittered delay
between attempts OR document it relies on jsutil.EnsureConsumer's internal
retry — decide by reading EnsureConsumer; prefer wiring (consensus: wire).
Update the `Retry` godoc in `consumer/broadcast.go` /
`internal/durable/broadcast_config.go` to match what is now true.

- [ ] **Step 4: Run both packages -race; lint + commit** — `fix(consumer): grow retry backoff in broadcast and dynamic consume loops` (body: constant-Base story, dead Multiplier/Max/Seed).

---

### Task F: Queue compat-check before durable creation (N4, P2)

**Files:**
- Modify: `consumer/queue.go` (Start: move `CheckWorkQueueRecoveryCompat`
  BEFORE `ensureConsumer` — mirror `consumer/static.go`'s order)
- Test: `consumer/queue_test.go` or the live test file used by queue tests

- [ ] **Step 1: Failing test** — on a WorkQueuePolicy stream with
RecoverFromNew, `Start` must fail AND leave no durable behind:

```go
// TestQueue_Start_IncompatibleConfig_LeavesNoDurable pins startup hygiene:
// the WorkQueue/recovery compatibility check must run BEFORE the durable is
// created. Pre-fix, a failed Start left an exclusive durable on the
// WorkQueuePolicy stream that blocked every other consumer for
// InactiveThreshold (default 24h).
// Harness: embedded NATS (mirror this package's live tests), WorkQueuePolicy
// stream, NewQueue(WithRecoveryStrategy(RecoverFromNew)), Start → require
// ErrorIs ErrInvalidConfig → js.Consumer lookup for the durable name must
// return not-found.
```

- [ ] **Step 2: Run, expect FAIL** (durable exists after the failed Start).

- [ ] **Step 3: Implement the reorder (two statements); Step 4: PASS + package green; Step 5: lint + commit** — `fix(consumer): validate WorkQueue compatibility before creating the queue durable`.

---

### Task G: Queue closed-iterator hot loop (N9, P3)

**Files:**
- Modify: `consumer/queue.go` (runLoop / processIterator interplay)
- Test: `consumer/queue_test.go` (reuse `queueErrorIter`)

- [ ] **Step 1: Failing test** — IteratorFactory returning an iterator whose
`Next` always returns `jetstream.ErrMsgIteratorClosed`, loop ctx alive:
assert the iterator-factory call count stays bounded (e.g. < 20) over 200ms.
Pre-fix it spins unboundedly (processIterator returns nil → continue with
backoff reset to 0).

- [ ] **Step 2: Run, expect FAIL** (hundreds+ of factory calls).

- [ ] **Step 3: Implement** — distinguish "iterator closed but loop ctx
alive": in `processIterator`, return a non-nil retryable signal (or a bool)
when `ErrMsgIteratorClosed` arrives while `ctx.Err() == nil`, and take the
existing backoff path in runLoop. Graceful shutdown (ctx done) keeps the
current nil return. Keep the change minimal and inside queue.go.

- [ ] **Step 4: PASS + package green; Step 5: lint + commit** — `fix(consumer): back off when the queue iterator closes without shutdown`.

---

### Task H: ErrInvalidConfig wrapping across constructors (N8, P2)

**Files:**
- Modify: `consumer/queue.go` (name `:158-160`, filter-subject `:161-163`),
  `consumer/static.go` (name `~:294`), `consumer/broadcast.go` (prefix
  `~:254`), `internal/ipartition/consumer.go` (subject parse errors `~:86-94`
  — wrap at the consumer/static.go boundary, NOT inside internal, if internal
  lacks the sentinel; decide by import direction: types owns no such sentinel,
  consumer owns ErrInvalidConfig — so wrap where errors cross into the public
  constructor), and the fuda validation returns in each `Validate()`
  (`fmt.Errorf("%w: %w", ErrInvalidConfig, err)`).
- Test: a table test per constructor asserting `errors.Is(err, ErrInvalidConfig)`
  for: missing required field, out-of-range field, invalid name/prefix,
  invalid subject/filter.

- [ ] **Step 1: Write the failing table tests; Step 2: run, record which rows
fail (expect: fuda rows + name/subject rows). Step 3: wrap consistently.
Step 4: PASS + all four consumer constructors' packages green. Step 5: lint +
commit** — `fix(consumer): wrap all constructor validation failures with ErrInvalidConfig`.

---

### Task I: Docs-truth batch (N10 + N11 + stale comment), changelog

**Files:**
- Modify: `docs/LIFECYCLE.md` (two-phase section rewrite), `docs/REFERENCE.md:363`,
  `docs/CONSUMERS.md` (stream-missing per-type behavior; thread-safety notes),
  `consumer/dynamic.go` (ProcessingGate field godoc + DrainOnRemove mechanism
  wording), `consumer/common.go` (ManualAck godoc), `consumer/queue.go`
  (Stop-timeout godoc, RecoveryStrategy guard wording, defensive-exit log line),
  `test/integration/durable/processing_gate_exclusivity_test.go:88` (stale
  comment: durables are shared, not per-worker), `CHANGELOG.md`.

- [ ] **Step 1: LIFECYCLE rewrite** — replace the "zero overlap" claims
(`:219`, `:276`, `:283`) and the fictional leader-ACK protocol diagram
(`:224-249`) with: (a) an accurate worker-driven KV-CAS description; (b) a
per-tier guarantee table (no two-phase / two-phase / +gate / +gate+pull-gating)
stating two-phase orders RELEASE (no unowned gap) while the gate provides
per-message admission control; (c) the irreducible window: an in-flight
handler invocation plus AckWait-expiry redelivery of that message via the
SHARED per-partition durable — mitigation `consumer.NewWIPHandler`
(in-progress keepalive); (d) the resolver stale-positive bound
(≤ ReconcileInterval, default 30s, self-correcting). Use "minimizes overlap";
delivery is at-least-once at every tier. Align `docs/REFERENCE.md:363` to one
honest line.
- [ ] **Step 2: Godoc/docs sweep** — ProcessingGate "ensure ... *only* active
processor" → "per-message admission control; bounds, does not eliminate,
overlap" (consumer/dynamic.go + gate_config.go); DrainOnRemove mechanism
wording (drain polls acks while pulls continue, then stop); ManualAck "still
logged" → match reality (errors discarded — either add the missing log in
recovery.Dispatch/keyDispatcher.processMessage [decide: doc-fix only this
batch]; fix the doc); CONSUMERS.md stream-missing section: add the
Queue/Static/Broadcast paragraph (Warn + backoff forever; self-heals only if
the stream returns AND a recovery strategy is enabled; no exhaustion/degrade
tier); Queue Stop-timeout note; queue.go defensive nil-consumer exit gets a
Warn log line. Global-grep discipline: `grep -rn "zero overlap\|only active processor\|never processed" docs/ consumer/ internal/` and fix every parallel instance.
- [ ] **Step 3: CHANGELOG** — Unreleased additions, matching house style:
Fixed: N2 (silent stall signal), N3/N5 (terminal Stop), N4 (durable-before-
compat), N6 (ordering race), N7/NF1 (backoff growth), N9 (hot loop), N8
(sentinel wrapping). Changed: N1 (FetchTimeout floor — configs in (0,1s) now
fail construction), reject-restart contract + new `ErrConsumerStopped`.
Docs: zero-overlap claims corrected.
- [ ] **Step 4: lint + commit** — `docs: replace zero-overlap claims with the per-tier overlap contract` (+ the changelog/godoc sweep).

---

### Task J: Validation gate + review loop + push

- [ ] **Step 1:** `make pre-pr` (full: lint + unit -race + integration -race).
- [ ] **Step 2:** `/simplify` over the batch range; apply; re-lint/test.
- [ ] **Step 3:** post-impl review (codex) against THIS plan, v1 → iterate to
merge-clean (0 P0/P1).
- [ ] **Step 4:** push to PR #47 (history: keep one commit per task — they are
already shaped that way; no squash needed beyond fix-round folding).

---

## Deferred (decided, do not implement in this batch)

D1 retry-stash version-only family (next planned correctness item — needs own
plan + reproducer via testHookHandleCommitValue/testHookHandleAssignment);
D2 resolver stale-positive hardening (documented in Task I instead);
D3 orphan stable-claim GC; D4 overlap chaos test (spec preserved in the
zero-overlap audit transcript + findings doc).

## Implementation addenda (review-found, accepted during execution)

- **Task C addendum — terminal Close extended to Dynamic's WorkerConsumer.**
  Review of Task C found the same hazard in internal/durable/worker_consumer.go,
  aggravated: Close nils the gate resolver (constructor-only init), so a
  post-Close UpdateWorkerConsumer restarted pull loops WITHOUT the configured
  processing gate — a silent safety downgrade. Same reject-restart fix
  (`closed` flag under updateMu, ErrConsumerStopped), pinned by
  `TestUpdateWorkerConsumer_AfterClose_ReturnsErrConsumerStopped`. Manager
  suite verified unaffected (no Update-after-Stop in shutdown paths).
  Stop-before-Start is also terminal (pinned by never-started subtests).
- **Task A addendum — floor duplicated into per-consumer Validates.** The
  four consumer configs call fuda.Validate directly (no delegation to
  CommonConfig.Validate), so the 1s floor lives in all five Validate methods.

## Self-review notes

- Consensus fidelity: N1 floor-not-clamp; N3/N5 reject-restart via shared
  types.ErrConsumerStopped; N6 mutual-exclusion with the select-random-pick
  hazard documented; N7 includes NF1 (Dynamic) per codex; D1-D4 deferred.
- Tasks B, C, E Step-1 test bodies are contract-complete but harness-adaptive
  (each file's existing scaffolding differs); the assertion contracts are
  fixed and the implementer must record verify-first failures for each.
- Behavior changes called out: Task A (validation floor), Task C (terminal
  Stop). Both changelog'd in Task I.
