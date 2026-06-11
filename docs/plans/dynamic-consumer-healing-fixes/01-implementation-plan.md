# Dynamic-Consumer Healing Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close four consensus-reviewed defects in the Dynamic consumer's
auto-healing / handoff interplay: (1) the manager exits Degraded while
partition consumers are permanently dead after stream-missing recovery
exhaustion; (2) `ErrMaxSubjectsExceeded` is documented but never returned —
the silent cap-skip can strand a partition unowned through a committed
handoff; (3) the subject-loop remove-timeout path returns success while loops
may still be processing; (4) an application `WithOnPermanentFailure` callback
silently disables the manager's stream-missing degraded route.

**Architecture:** All four fixes are small, local changes to existing
mechanisms — a new reason-scoped terminal gate in
`attemptRecoveryFromDegraded`, a pre-mutation cap check in
`WorkerConsumer.UpdateWorkerConsumer`, an error return on the remove-timeout
branch, and a dual-dispatch (user callback + manager observer) in
`Dynamic.onPermanentFailure` with an explicit suppress option. No new types,
no interface changes.

**Tech Stack:** Go, NATS JetStream, testify, embedded-NATS integration
harness (`partitest`).

**Provenance:** QA findings + 2-round codex consensus:
`tmp/dynamic-consumer-qa-findings-v2.md`, `tmp/codex-review-r1-verdict.md`,
`tmp/codex-review-r2-verdict.md`. Task order follows the consensus sequencing
constraint: the dispatcher change (Task 1) lands before the terminal-hold
tests (Task 2) because they share the stream-missing observer route.

**Repo discipline (applies to every task):**
- Verify-first: run each new test BEFORE the fix and confirm it fails for the
  expected reason.
- Run `make lint` before every commit; fix findings.
- Commit messages: no plan jargon (no "F1", "Task 2", review-round refs), no
  attribution trailers.
- This series touches `manager_degraded.go` (degraded routing) and
  `internal/durable/` — the AGENTS.md pre-PR gate (`make pre-pr`) is
  mandatory before opening the PR (Task 6).

---

### Task 1: Dual-dispatch stream-missing observer + explicit suppress option

The application callback currently suppresses the manager observer entirely
(`consumer/dynamic.go:446-454`). New contract: user callback fires first
(when set), THEN the manager observer is notified for stream-missing
exhaustion — unless the application explicitly opts out via the new
`WithSuppressManagerDegradeOnStreamMissing()` option.

**Files:**
- Modify: `consumer/dynamic.go` (struct field, dispatcher, `NewDynamic`
  wiring, `DynamicConfig` field + godoc on `OnPermanentFailure`)
- Modify: `consumer/options.go` (new option; rewrite `WithOnPermanentFailure`
  godoc "Interaction with the Parti manager's auto-degraded route" section)
- Modify: `consumer/options.go` — the `options` struct at line 150 (add the
  new bool field next to `onPermanentFailure` at line 198)
- Test: `consumer/dynamic_on_permanent_failure_test.go`

- [ ] **Step 1: Rewrite the dispatch-precedence test to pin the NEW contract**

In `consumer/dynamic_on_permanent_failure_test.go`, replace
`TestDynamic_onPermanentFailure_UserCallbackWinsOverManagerObserver` with:

```go
// TestDynamic_onPermanentFailure_UserCallbackAndManagerObserverBothFire
// pins the dual-dispatch contract: when an application has registered its
// own OnPermanentFailure via WithOnPermanentFailure, the manager-installed
// observer ALSO fires for stream-missing exhaustion (user callback first,
// then manager observer), so platform self-healing (Degraded -> rotation)
// is not silently coupled to an app-level observability hook. Applications
// that deliberately manage rotation themselves opt out explicitly via
// WithSuppressManagerDegradeOnStreamMissing.
func TestDynamic_onPermanentFailure_UserCallbackAndManagerObserverBothFire(t *testing.T) {
	var (
		userCalls    atomic.Int32
		managerCalls atomic.Int32
	)

	d := &Dynamic{
		streamName: "TEST_STREAM",
		userOnPermanentFailure: func(_ string, _ error) {
			userCalls.Add(1)
		},
	}
	d.SetOnStreamMissingError(func(_ string, err error) {
		managerCalls.Add(1)
		require.ErrorIs(t, err, types.ErrStreamMissing,
			"manager observer must receive the wrapped types.ErrStreamMissing chain")
	})

	wrapped := fmt.Errorf("stream %q: %w", "TEST_STREAM", types.ErrStreamMissing)
	d.onPermanentFailure("test.subject.p1", wrapped)

	require.Equal(t, int32(1), userCalls.Load(),
		"user callback must fire exactly once")
	require.Equal(t, int32(1), managerCalls.Load(),
		"manager observer must ALSO fire for stream-missing exhaustion when a user callback is registered")

	// Generic (non-stream-missing) exhaustion still reaches ONLY the user
	// callback — the manager observer remains scoped to stream-missing.
	d.onPermanentFailure("test.subject.p1", errors.New("generic exhaustion"))
	require.Equal(t, int32(2), userCalls.Load())
	require.Equal(t, int32(1), managerCalls.Load(),
		"manager observer must NOT fire for non-stream-missing exhaustion")
}

// TestDynamic_onPermanentFailure_SuppressOptionDisablesManagerObserver pins
// the explicit opt-out: with suppressManagerDegrade set, the manager
// observer never fires (stream-missing or otherwise) while the user
// callback still does.
func TestDynamic_onPermanentFailure_SuppressOptionDisablesManagerObserver(t *testing.T) {
	var (
		userCalls    atomic.Int32
		managerCalls atomic.Int32
	)

	d := &Dynamic{
		streamName:             "TEST_STREAM",
		suppressManagerDegrade: true,
		userOnPermanentFailure: func(_ string, _ error) {
			userCalls.Add(1)
		},
	}
	d.SetOnStreamMissingError(func(_ string, _ error) {
		managerCalls.Add(1)
	})

	wrapped := fmt.Errorf("stream %q: %w", "TEST_STREAM", types.ErrStreamMissing)
	d.onPermanentFailure("test.subject.p1", wrapped)

	require.Equal(t, int32(1), userCalls.Load(),
		"user callback must still fire when suppression is enabled")
	require.Equal(t, int32(0), managerCalls.Load(),
		"manager observer must NOT fire when the application explicitly suppressed the degrade route")
}
```

Keep `TestDynamic_onPermanentFailure_ManagerObserverOnlyOnStreamMissing`,
`..._NoUserNoManager`, and `..._NilClearsAfterFire` unchanged — they pin
rows of the matrix that do not change.

- [ ] **Step 2: Run the new tests to verify they fail on the current code**

Run: `go test ./consumer/ -run 'TestDynamic_onPermanentFailure' -v`
Expected: `UserCallbackAndManagerObserverBothFire` FAILS (managerCalls == 0 —
old precedence suppresses the observer); `SuppressOption...` FAILS to compile
or fails (no `suppressManagerDegrade` field yet). Compile error counts as the
failing-reproducer evidence for the suppress test.

- [ ] **Step 3: Implement the dual dispatcher**

In `consumer/dynamic.go`, add the struct field (next to
`userOnPermanentFailure`):

```go
	// suppressManagerDegrade, when true, prevents the dispatcher from
	// notifying the manager-installed stream-missing observer. Set via
	// WithSuppressManagerDegradeOnStreamMissing for applications that
	// deliberately own rotation/degrade signaling themselves.
	suppressManagerDegrade bool
```

Replace `onPermanentFailure` (currently `dynamic.go:446-454`) and update its
doc comment:

```go
// onPermanentFailure is the indirection dispatcher installed as
// WorkerConsumerConfig.OnPermanentFailure for every Dynamic. It runs on
// the partition consumer's goroutine when iterator-creation envelope
// or Site B detour exhaustion fires.
//
// Dispatch rules:
//  1. An application-supplied OnPermanentFailure callback (captured from
//     WithOnPermanentFailure) fires first when registered.
//  2. The manager-installed observer (set via SetOnStreamMissingError) is
//     then ALSO notified for stream-missing exhaustion, so platform
//     self-healing (enterDegraded -> rotation) does not silently turn off
//     when an application adds its own observability callback. Applications
//     that own degrade signaling themselves opt out explicitly via
//     WithSuppressManagerDegradeOnStreamMissing.
//  3. Generic (non-stream-missing) exhaustion never reaches the manager
//     observer; the durable layer's WARN log + metric remain the
//     operator-visible signal for those.
func (d *Dynamic) onPermanentFailure(subject string, err error) {
	if d.userOnPermanentFailure != nil {
		d.userOnPermanentFailure(subject, err)
	}
	if d.suppressManagerDegrade {
		return
	}
	if fn := d.managerOnStreamMissing.Load(); fn != nil && errors.Is(err, types.ErrStreamMissing) {
		(*fn)(d.streamName, err)
	}
}
```

Add to `DynamicConfig` (after `OnPermanentFailure`):

```go
	// SuppressManagerDegradeOnStreamMissing disables the Parti manager's
	// auto-degraded route for stream-missing recovery exhaustion. By default
	// the manager observer is notified even when OnPermanentFailure is set
	// (both fire; application callback first). Set this only when the
	// application deliberately owns degrade/rotation signaling itself.
	SuppressManagerDegradeOnStreamMissing bool
```

Wire it in `NewDynamic`: set `SuppressManagerDegradeOnStreamMissing: o.suppressManagerDegradeOnStreamMissing`
in the `cfg := DynamicConfig{...}` literal, and
`suppressManagerDegrade: o.suppressManagerDegradeOnStreamMissing` in the
`d := &Dynamic{...}` literal. Update the `OnPermanentFailure` field godoc in
`DynamicConfig` (currently `dynamic.go:231-247`): replace the sentence
"Setting this disables the Parti manager's auto-degraded route ..." with:

```go
	// Registering this callback does NOT disable the Parti manager's
	// auto-degraded route: for stream-missing exhaustion the manager
	// observer is notified after this callback returns. Use
	// [DynamicConfig.SuppressManagerDegradeOnStreamMissing] (option form:
	// [WithSuppressManagerDegradeOnStreamMissing]) to opt out explicitly.
```

- [ ] **Step 4: Add the option and update option godoc**

In the `options` struct (`consumer/options.go:150`, field block around line
198), add `suppressManagerDegradeOnStreamMissing bool`. In
`consumer/options.go`, next to `WithOnPermanentFailure`:

```go
// WithSuppressManagerDegradeOnStreamMissing disables the Parti manager's
// auto-degraded route for stream-missing recovery exhaustion on this
// Dynamic consumer.
//
// By default, when a [Dynamic] is wired into a [parti.Manager], stream-missing
// exhaustion notifies the manager observer (entering Degraded with reason
// "stream-missing-recovery-exhausted" so the readiness probe rotates the
// pod) IN ADDITION to any application callback registered via
// [WithOnPermanentFailure]. Suppress this only when the application
// deliberately owns degrade/rotation signaling itself — e.g. it forwards
// stream-missing events to its own readiness wiring inside its
// OnPermanentFailure callback.
//
// Applies only to [Dynamic].
func WithSuppressManagerDegradeOnStreamMissing() DynamicOption {
	return dynamicOpt(func(o *options) {
		o.suppressManagerDegradeOnStreamMissing = true
	})
}
```

Rewrite the "# Interaction with the Parti manager's auto-degraded route"
section of the `WithOnPermanentFailure` godoc (`options.go` around line 489)
to describe the new both-fire contract and point at the suppress option
(delete the "fires ONLY when no application callback is registered" text and
the two-bullet workaround list; state: callback first, manager observer
second for stream-missing, opt out via WithSuppressManagerDegradeOnStreamMissing).

- [ ] **Step 5: Run the tests and verify they pass**

Run: `go test ./consumer/ -run 'TestDynamic_onPermanentFailure|TestDynamic_SetOnStreamMissingError' -v`
Expected: ALL PASS.

- [ ] **Step 6: Lint and commit**

```bash
make lint
git add consumer/
git commit -m "feat(consumer): notify manager stream-missing observer alongside user callback

The application OnPermanentFailure callback previously suppressed the
manager's stream-missing observer entirely, so adding a logging callback
silently disabled the Degraded-entry route that drives pod rotation.
Now both fire (application callback first); applications that own
rotation signaling themselves opt out explicitly via the new
WithSuppressManagerDegradeOnStreamMissing option."
```

---

### Task 2: Terminal Degraded hold for stream-missing recovery exhaustion

`attemptRecoveryFromDegraded` has no exit gate for
`DegradeReasonStreamMissingRecoveryExhausted`; the connection never dropped,
so the worker exits back to Stable within ~one monitor tick while its
partition-consumer loop is permanently dead (the dead subject stays in the
worker-consumer subject map; re-applies compute an empty diff). Hold
terminally Degraded for restart/rotation.

**Files:**
- Modify: `manager_degraded.go` (gate in `attemptRecoveryFromDegraded`,
  placed immediately after `reason := rec.reason` — i.e. after the refresh +
  commitment guard so `scheduleApplyRetry` still re-arms an unapplied
  assignment, and before the Family B gate)
- Test: `manager_recovery_conjuncts_test.go`
- Test: `test/integration/failure/stream_missing_no_hook_test.go`

- [ ] **Step 1: Write the failing unit test (both directions)**

Append to `manager_recovery_conjuncts_test.go`:

```go
// TestAttemptRecovery_StreamMissingExhausted_StaysDegraded pins the terminal
// hold for the dynamic-consumer stream-missing route: once a partition
// consumer's recovery envelope has exhausted, its loop has exited and cannot
// restart in-process (the dead subject remains in the worker-consumer's
// subject map, so a re-apply computes an empty diff), and operator stream
// recreation cannot revive it either. Recovery must therefore never exit
// this reason — rotation is the only recovery, matching the
// heartbeat-bucket backstop's terminal contract. Without the gate, the
// connection monitor exits back to Stable within ~one tick (the NATS
// connection never dropped) and the worker reports Stable while assigned
// partitions are silently not consumed.
func TestAttemptRecovery_StreamMissingExhausted_StaysDegraded(t *testing.T) {
	t.Parallel()
	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	m, _ := armDegraded(t, &snap, snap)
	plantAssignment(t, m, snap)
	m.markDegraded(time.Now().UnixNano(), DegradeReasonStreamMissingRecoveryExhausted)

	// Even with every recovery signal healthy (commitment guard satisfied by
	// armDegraded, fresh heartbeat far in the future), the hold is terminal.
	m.lastHeartbeatSuccessAt.Store(time.Now().UnixNano() + int64(time.Hour))

	m.attemptRecoveryFromDegraded()

	require.Equal(t, StateDegraded, m.State(),
		"stream-missing-recovery-exhausted must hold the worker terminally Degraded for rotation")
}

// TestAttemptRecovery_StreamMissingHold_IsReasonScoped proves the new gate
// is NOT accidentally global: a reason with no blocking gate (startup
// timeout) still exits to Stable through the same pipeline. This is the
// negative-space direction the boundary-test discipline requires.
func TestAttemptRecovery_StreamMissingHold_IsReasonScoped(t *testing.T) {
	t.Parallel()
	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	m, _ := armDegraded(t, &snap, snap)
	plantAssignment(t, m, snap)
	m.markDegraded(time.Now().UnixNano(), DegradeReasonStartupTimeout)

	m.attemptRecoveryFromDegraded()
	m.wg.Wait()

	require.Equal(t, StateStable, m.State(),
		"a non-stream-missing reason with healthy signals must still exit; the terminal hold must be reason-scoped")
}
```

- [ ] **Step 2: Run the unit tests to verify direction**

Run: `go test . -run 'TestAttemptRecovery_StreamMissing' -v`
Expected: `StaysDegraded` FAILS (state is Stable — recovery exited);
`IsReasonScoped` PASSES (it pins existing behavior; it exists to catch an
over-broad gate after Step 4).

- [ ] **Step 3: Write the failing integration assertion**

In `test/integration/failure/stream_missing_no_hook_test.go`, immediately
after the `firstDegradedReason` assertions (after the
`degradedMu.Unlock()` that closes the reason-sequence check), insert:

```go
	// Terminal hold: the worker must STAY Degraded — recovery must not exit
	// this reason. Pre-fix, the connection monitor (connection up the whole
	// time) exited back to Stable within ~one tick, defeating the
	// rotate-on-Degraded operator contract. 5s spans multiple ExitThreshold
	// windows at this test's cadence.
	require.Never(t, func() bool {
		return mgr.State() != types.StateDegraded
	}, 5*time.Second, 100*time.Millisecond,
		"stream-missing-recovery-exhausted must hold the worker terminally Degraded for rotation; "+
			"an exit back to Stable leaves dead partition consumers reported as healthy")
```

(Adjust the 5s window down only if the test's configured
`DegradedBehavior.ExitThreshold` makes a shorter window conclusive — the
window must comfortably exceed one ExitThreshold plus one monitor tick.)

- [ ] **Step 4: Run the integration test to verify it fails on the unfixed code**

Run: `go test ./test/integration/failure/ -run TestStreamMissingNoHook -race -v -timeout 300s`
(confirm the exact test name with `grep -n "func Test" test/integration/failure/stream_missing_no_hook_test.go`)
Expected: FAIL at the new `require.Never` — state flips back to `Stable`
within the window. This is the verify-first evidence for the bug.

- [ ] **Step 5: Implement the terminal hold**

In `manager_degraded.go`, `attemptRecoveryFromDegraded`, immediately after
`reason := rec.reason` (currently line ~504) and BEFORE the Family B gate:

```go
	// Terminal hold — stream-missing recovery exhaustion means at least one
	// partition-consumer loop in this process has exited permanently. The
	// loop cannot restart in-process (the dead subject remains in the
	// worker-consumer's subject map, so a re-apply computes an empty diff),
	// and operator stream recreation cannot revive it either. No recovery
	// signal exists that could stamp this reason healthy; hold the worker
	// terminally Degraded for restart/rotation, matching the
	// heartbeat-bucket backstop's terminal contract. Placed after the
	// commitment guard so an unapplied refreshed assignment still re-arms
	// scheduleApplyRetry above.
	if reason == DegradeReasonStreamMissingRecoveryExhausted {
		m.logger.Warn("recovery: stream-missing recovery exhausted is terminal; staying Degraded for restart/rotation")
		return
	}
```

- [ ] **Step 6: Run unit + integration tests to verify they pass**

Run: `go test . -run 'TestAttemptRecovery' -v`
Expected: ALL PASS (including the pre-existing kv-unavailable /
heartbeat-backstop gate tests — the new gate must not disturb them).

Run: `go test ./test/integration/failure/ -run TestStreamMissingNoHook -race -v -timeout 300s`
Expected: PASS, including the new `require.Never`.

- [ ] **Step 7: Lint and commit**

```bash
make lint
git add manager_degraded.go manager_recovery_conjuncts_test.go test/integration/failure/stream_missing_no_hook_test.go
git commit -m "fix: hold Degraded terminally after stream-missing recovery exhaustion

A dynamic consumer's exhausted stream-missing recovery permanently kills
its partition-consumer loop, but the recovery monitor had no exit gate
for this reason: the NATS connection never dropped, so the worker exited
back to Stable within one tick while its dead partitions were still
assigned, heartbeated as applied, and silently not consumed. Readiness
probes never saw a lasting Degraded state, so the documented
rotate-on-Degraded recovery could not trigger. Hold the worker
terminally Degraded (like the heartbeat-bucket backstop) so rotation
happens."
```

---

### Task 3: Return `ErrMaxSubjectsExceeded` before any mutation

The sentinel is exported and documented on `Dynamic.Update` but never
returned; `addSubjectLoop` silently skips subjects over the cap
(`internal/durable/worker_consumer.go:356-373`), which lets a two-phase
handoff commit ownership of a partition no loop was started for. Move the
check to `UpdateWorkerConsumer`, before any mutation, on the deduped subject
count.

**Files:**
- Modify: `internal/durable/worker_consumer.go`
- Modify: `internal/durable/config.go:257-259` (godoc)
- Modify: `consumer/dynamic.go:149-153` (`MaxConcurrentSubjects` godoc)
- Test: `internal/durable/worker_consumer_test.go`

- [ ] **Step 1: Write the failing tests (both directions of the boundary)**

Append to `internal/durable/worker_consumer_test.go`:

```go
// TestUpdateWorkerConsumer_OverCap_ErrorsBeforeMutation pins the
// MaxConcurrentSubjects contract: a deduped subject set larger than the cap
// must return ErrMaxSubjectsExceeded BEFORE any mutation — no workerID
// store, no removals, no new loops. The pre-fix behavior (silently skipping
// excess subjects inside the add loop and returning nil) let the two-phase
// handoff commit ownership of a partition no loop was started for,
// stranding it unowned while the worker reported the assignment applied.
func TestUpdateWorkerConsumer_OverCap_ErrorsBeforeMutation(t *testing.T) {
	wc := &WorkerConsumer{
		logger: logging.NewNop(),
		config: WorkerConsumerConfig{
			SubjectTemplate:       "orders.{{.PartitionID}}.events",
			MaxConcurrentSubjects: 2,
		},
		subjects: map[string]*partitionConsumer{
			// Pre-existing subject NOT in the new set: over-cap must not remove it.
			"orders.p9.events": nil,
		},
	}

	// types.Partition is keyed by Keys; PartitionID in the subject template
	// is the dot-joined HashID, so a single key "p0" yields "orders.p0.events".
	parts := []types.Partition{
		{Keys: []string{"p0"}}, {Keys: []string{"p1"}}, {Keys: []string{"p2"}},
	}
	err := wc.UpdateWorkerConsumer(context.Background(), "worker-1", parts)

	require.ErrorIs(t, err, ErrMaxSubjectsExceeded,
		"3 deduped subjects over cap 2 must surface the documented sentinel")
	require.Contains(t, wc.subjects, "orders.p9.events",
		"over-cap update must not perform removals — error must precede all mutation")
	require.Len(t, wc.subjects, 1,
		"over-cap update must not start any new subject loops")
	wc.mu.RLock()
	gotWorkerID := wc.workerID
	wc.mu.RUnlock()
	require.Empty(t, gotWorkerID,
		"over-cap update must not store the workerID — the check runs before setWorkerIDAndSnapshot")
}

// TestUpdateWorkerConsumer_AtCap_Succeeds pins the positive direction of the
// boundary: a deduped subject count EQUAL to the cap passes the check. The
// target subjects are pre-populated so the update is a no-op diff and no
// JetStream client is needed.
func TestUpdateWorkerConsumer_AtCap_Succeeds(t *testing.T) {
	wc := &WorkerConsumer{
		logger: logging.NewNop(),
		config: WorkerConsumerConfig{
			SubjectTemplate:       "orders.{{.PartitionID}}.events",
			MaxConcurrentSubjects: 2,
		},
		subjects: map[string]*partitionConsumer{
			"orders.p0.events": nil,
			"orders.p1.events": nil,
		},
	}

	parts := []types.Partition{{Keys: []string{"p0"}}, {Keys: []string{"p1"}}}
	err := wc.UpdateWorkerConsumer(context.Background(), "worker-1", parts)

	require.NoError(t, err, "subject count == cap must pass; the boundary is len(subjects) > cap")
	require.Len(t, wc.subjects, 2)
}
```

Notes for the implementer: `logging.NewNop()` is the no-op logger already
used by `internal/durable/partition_consumer_test.go:18` — copy its import.
The `Partition{Keys: ...}` literal shape matches
`internal/dynamicbuild/builder_test.go` usage (`{{.PartitionID}}` renders
the dot-joined keys).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/durable/ -run 'TestUpdateWorkerConsumer_OverCap|TestUpdateWorkerConsumer_AtCap' -v`
Expected: `OverCap` FAILS — current code returns nil and stores the workerID
(and would remove `orders.p9.events`). `AtCap` PASSES (pins existing
behavior; it exists to catch an off-by-one `>=` in Step 3).

- [ ] **Step 3: Implement the pre-mutation check; delete the silent skip**

In `internal/durable/worker_consumer.go`, `UpdateWorkerConsumer`, insert
between `subjects, err := wc.buildSubjects(partitions)` and
`existing := wc.setWorkerIDAndSnapshot(workerID)`:

```go
	// Enforce the subject cap BEFORE any mutation (workerID store, removals,
	// adds). Failing the whole update keeps the manager's apply pipeline
	// honest: the apply fails pre-commit and retries with backoff, the
	// two-phase removal guard keeps the previous owner consuming, and the
	// un-acked heartbeat makes the over-capped worker visible to the leader.
	// The pre-fix per-add silent skip returned success while ownership of
	// the skipped partition could commit with no loop started — a silently
	// unowned partition.
	if maxSubjects := wc.config.MaxConcurrentSubjects; maxSubjects > 0 && len(subjects) > maxSubjects {
		if wc.config.Metrics != nil {
			wc.config.Metrics.IncrementWorkerConsumerSubjectThresholdWarning()
		}
		return fmt.Errorf("%d subjects exceed MaxConcurrentSubjects %d: %w",
			len(subjects), maxSubjects, ErrMaxSubjectsExceeded)
	}
```

Delete the cap block at the top of `addSubjectLoop` (the
`if wc.config.MaxConcurrentSubjects > 0 { ... return nil }` block,
currently lines 357-373). `addSubjectLoop` is reached only from
`UpdateWorkerConsumer`'s add loop (verified: broadcast has its own
`UpdateWorkerConsumer`), so the new check fully covers it.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/durable/ -run 'TestUpdateWorkerConsumer_OverCap|TestUpdateWorkerConsumer_AtCap' -v`
Expected: BOTH PASS.

Run: `go test ./internal/durable/ ./consumer/`
Expected: PASS — no existing test depended on the silent-skip behavior (if
one did, it pinned the bug; update it to expect the error and note that in
the commit message).

- [ ] **Step 5: Fix the contradicting godoc**

`consumer/dynamic.go:149-153` — replace:

```go
	// MaxConcurrentSubjects limits the number of partitions (subjects) processed concurrently.
	//
	// If the manager assigns more partitions than this limit, excess partitions
	// will be ignored (and logged/warned).
	MaxConcurrentSubjects int `validate:"gte=0"`
```

with:

```go
	// MaxConcurrentSubjects limits the number of partitions (subjects) processed concurrently.
	//
	// If an assignment's deduped subject count exceeds this limit,
	// [Dynamic.Update] rejects the whole update with [ErrMaxSubjectsExceeded]
	// before making any changes; the manager retries the apply with backoff
	// and the previous owners keep consuming. Size this above the worst-case
	// per-worker partition count (e.g. after scale-down) or leave it 0
	// (unlimited).
	MaxConcurrentSubjects int `validate:"gte=0"`
```

Update `internal/durable/config.go:257-259` similarly (cap rejects the whole
update with `ErrMaxSubjectsExceeded` pre-mutation).

- [ ] **Step 6: Lint and commit**

```bash
make lint
git add internal/durable/ consumer/dynamic.go
git commit -m "fix(consumer): reject over-cap assignments instead of silently skipping subjects

ErrMaxSubjectsExceeded was documented on Dynamic.Update but never
returned: the cap was enforced per-subject inside the add loop as a
silent skip that still reported success. A committed handoff could then
release the previous owner while no loop was started for the skipped
partition, leaving it silently unowned. The cap is now enforced on the
deduped subject count before any mutation, failing the apply so the
manager retries and the previous owner keeps consuming. Behavior
change: operators relying on the silent skip must raise the cap."
```

---

### Task 4: Surface the remove-timeout instead of silent success

`removeSubjectLoops`' `<-time.After(waitTimeout)` branch logs a warning but
returns nil and deletes the map entries, so a handoff commits while an old
loop's in-flight handler may still be processing. Return an error so the
apply fails and the commit is deferred to the retry cycle. (Per consensus:
this delays and surfaces the overlap risk — it does not prove handler
quiescence. Entries must still be deleted: keeping a stopped entry would
make a later re-add of the same subject a silent no-op.)

**Files:**
- Modify: `internal/durable/worker_consumer.go:332-353`
- Modify: `consumer/dynamic.go:143-147` (`DrainOnRemoveTimeout` godoc)
- Test: `internal/durable/worker_consumer_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/durable/worker_consumer_test.go`:

```go
// TestUpdateWorkerConsumer_RemoveTimeout_SurfacesError pins the
// remove-timeout contract: when subject loops fail to stop within
// DrainOnRemoveTimeout, UpdateWorkerConsumer must return an error (so the
// manager's apply fails pre-commit and retries) instead of reporting silent
// success while a loop may still be processing. Map entries are still
// deleted — a retained-but-stopped entry would make a later re-add of the
// same subject a silent no-op (the dead-subject hazard) — so the follow-up
// update converges.
//
// The never-stopping loop is simulated by a partitionConsumer whose done
// channel never closes: Stop() tolerates a never-started consumer (cancel
// is nil-checked) and Wait() blocks forever.
func TestUpdateWorkerConsumer_RemoveTimeout_SurfacesError(t *testing.T) {
	stuck := &partitionConsumer{done: make(chan struct{})}
	wc := &WorkerConsumer{
		logger: logging.NewNop(),
		config: WorkerConsumerConfig{
			SubjectTemplate:      "orders.{{.PartitionID}}.events",
			DrainOnRemoveTimeout: 50 * time.Millisecond,
		},
		subjects: map[string]*partitionConsumer{
			"orders.p0.events": stuck,
		},
	}

	// Empty set removes everything; the stuck loop forces the wait to time out.
	err := wc.UpdateWorkerConsumer(context.Background(), "worker-1", nil)
	require.Error(t, err,
		"a remove that times out waiting for loops to stop must fail the update, not report success")
	require.NotErrorIs(t, err, context.DeadlineExceeded,
		"the timeout is the internal wait bound, not the caller context")
	require.Empty(t, wc.subjects,
		"entries must still be deleted on timeout so the retry converges and re-adds are not silent no-ops")

	// Convergence: the retry (same target set) finds nothing to remove.
	err = wc.UpdateWorkerConsumer(context.Background(), "worker-1", nil)
	require.NoError(t, err, "the follow-up update must converge once entries are gone")
}
```

(`logging.NewNop()` import as in Task 3. `stuck` needs only the `done`
channel — `Stop()` nil-checks `cancel` (`partition_consumer.go:406-414`),
and `Drain` is skipped because `DrainOnRemove` is false.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/durable/ -run TestUpdateWorkerConsumer_RemoveTimeout -v`
Expected: FAIL on `require.Error` — current code returns nil from the
timeout branch.

- [ ] **Step 3: Implement the error return**

In `internal/durable/worker_consumer.go`, `removeSubjectLoops`, change the
timeout case:

```go
	case <-time.After(waitTimeout):
		// Surface the timeout so the caller's apply fails pre-commit and is
		// retried with backoff. This delays the handoff commit by at least
		// one retry cycle and makes the stall observable; it does NOT
		// guarantee the old handler has finished — Stop only cancels the
		// loop context, and an in-flight handler invocation runs to
		// completion. Entries are still deleted below: a retained-but-
		// stopped entry would make a later re-add a silent no-op.
		wc.logger.Warn("timeout waiting for subject loops to stop",
			"count", len(loops),
			"timeout", waitTimeout,
		)
		err = fmt.Errorf("timed out after %v waiting for %d subject loops to stop", waitTimeout, len(loops))
	}
```

(The existing Warn moves inside the case body unchanged; only the `err`
assignment is new. The entry deletion below the select stays as-is.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/durable/ -run TestUpdateWorkerConsumer -v`
Expected: ALL PASS (including Task 3's tests).

- [ ] **Step 5: Sharpen the `DrainOnRemoveTimeout` godoc**

`consumer/dynamic.go:143-147` — replace:

```go
	// DrainOnRemoveTimeout caps the time spent draining a revoked partition.
	//
	// If draining takes longer than this timeout, the consumer is forcibly closed.
	// Default: 10s.
	DrainOnRemoveTimeout time.Duration `default:"10s" validate:"gte=0"`
```

with:

```go
	// DrainOnRemoveTimeout caps the time spent draining a revoked partition
	// and bounds the wait for its pull loop to stop.
	//
	// The wait is best-effort: if loops have not stopped within this bound,
	// [Dynamic.Update] returns an error (the manager retries the apply with
	// backoff) while an already-in-flight handler invocation may still run
	// to completion. Default: 10s.
	DrainOnRemoveTimeout time.Duration `default:"10s" validate:"gte=0"`
```

- [ ] **Step 6: Lint and commit**

```bash
make lint
git add internal/durable/ consumer/dynamic.go
git commit -m "fix(consumer): surface timeout when removed subject loops fail to stop

The remove path's bounded wait logged a warning on timeout but returned
success, letting a handoff commit while an old loop's in-flight handler
could still be processing the partition. The timeout now fails the
update so the manager's apply retries and the commit is deferred; map
entries are still cleared so the retry converges."
```

---

### Task 5: Docs, changelog, and informational godoc

**Files:**
- Modify: `consumer/common.go` (`CheckWorkQueueRecoveryCompat` godoc)
- Modify: `docs/CONSUMERS.md`, `docs/OPERATIONS.md`, `docs/LIFECYCLE.md`
- Modify: `CHANGELOG.md`

- [ ] **Step 1: Add the cached-pass caveat to `CheckWorkQueueRecoveryCompat`**

In `consumer/common.go`, append to the "best-effort" godoc paragraph
(currently ending "...continues as if the check passed."):

```go
// For [Dynamic], the per-consumer outcome is cached: a pass recorded during a
// transient fetch failure is not re-evaluated until a stream-recreate resets
// the check, so a genuinely incompatible configuration may go undetected
// until recovery first misbehaves.
```

- [ ] **Step 2: Sync the operator docs**

Search-and-update (use grep to find every instance — fix all parallel
occurrences, not just the first):

- `docs/OPERATIONS.md`, degraded-reason taxonomy: mark
  `stream-missing-recovery-exhausted` as **terminal** — the worker stays
  Degraded until restarted/rotated; recovery never exits this reason because
  the dead partition-consumer loop cannot restart in-process. Operator
  action: recreate the stream, then rotate the worker.
- `docs/CONSUMERS.md` (Dynamic sections): `MaxConcurrentSubjects` — excess
  partitions now reject the whole `Update` with `ErrMaxSubjectsExceeded`
  (was: "ignored"); `OnPermanentFailure` — manager observer now also fires
  for stream-missing unless `WithSuppressManagerDegradeOnStreamMissing` is
  set.
- `docs/LIFECYCLE.md` (degraded-mode section): note the terminal reason in
  whatever list/table enumerates recovery behavior, if one exists. Grep for
  `stream-missing` across `docs/` to catch every mention.

- [ ] **Step 3: Add changelog entries**

In `CHANGELOG.md`, under a new Unreleased (or next-version) heading, follow
the existing entry style:

```markdown
### Fixed
- Manager now stays Degraded permanently after a dynamic consumer's
  stream-missing recovery exhausts, so readiness-driven rotation can
  occur; previously it returned to Stable within seconds while dead
  partition consumers were still assigned and silently not consuming.
- `Dynamic.Update` now returns `ErrMaxSubjectsExceeded` (as documented)
  when an assignment exceeds `MaxConcurrentSubjects`, instead of
  silently skipping excess partitions — which could strand a partition
  unowned after a committed handoff.
- `Dynamic.Update` now returns an error when removed partition loops
  fail to stop within `DrainOnRemoveTimeout`, instead of reporting
  success while a handler could still be processing.

### Changed
- Registering `WithOnPermanentFailure` no longer disables the manager's
  auto-degraded route for stream-missing exhaustion: both the
  application callback and the manager observer now fire. Use the new
  `WithSuppressManagerDegradeOnStreamMissing()` option to restore the
  previous opt-out behavior explicitly.
```

- [ ] **Step 4: Lint and commit**

```bash
make lint
git add consumer/common.go docs/ CHANGELOG.md
git commit -m "docs: sync consumer healing and subject-cap contracts

Document the terminal stream-missing Degraded hold, the
ErrMaxSubjectsExceeded rejection (replacing silent skip), the bounded
best-effort remove wait, the dual-dispatch OnPermanentFailure contract,
and the cached best-effort WorkQueue compatibility check."
```

---

### Task 6: Full validation gate and review loop

This series touches `manager_degraded.go` and `internal/durable/` — the
pre-PR gate is mandatory, and the AGENTS.md cross-feature contracts (1)–(4)
must be re-verified since this changes degraded-exit routing.

- [ ] **Step 1: Run the full pre-PR gate**

Run: `make pre-pr`
Expected: lint clean, unit suite (`-race`) green, integration suite
(`-race`) green. The integration suite covers the cross-feature contracts:
`TestManager_LiveNATSBucketLoss*` (whole-bucket → Degraded; the new gate
must not block kv-threshold recovery), `TestStableID_StaleKeyTakeover_Reclaim`
(claim-lost routing), and the OnDegraded-once contract. Known load-flakes
(`TestLeaderElection_ColdStart`, `TestHandoffConflictStress`,
`TestFullNATSOutage_Unlimited`) pass in isolation if they trip under full
suite load — rerun individually before concluding regression.

- [ ] **Step 2: Quality pass**

Run `/simplify` over the changed files; apply cleanups; re-run
`make lint && make test`; commit any cleanup as
`refactor: simplify consumer healing fixes` (or fold into the relevant fix
commits if amending is cleaner pre-PR).

- [ ] **Step 3: External post-implementation review loop**

Run `/post-impl-review <this-plan-path> v1` (codex route). Iterate
fix→review (v2, v3, …) until the verdict is merge-clean with 0 P0/P1.
Re-run `make pre-pr` after any fix round that touches code.

- [ ] **Step 4: Open the PR**

Squash to one commit per fix (4 fix commits + 1 docs commit is fine;
follow the repo's squash-on-merge-verdict practice). PR description: state
the four user-visible behavior changes (terminal Degraded hold, over-cap
rejection, remove-timeout error, dual-dispatch callback) and link the two
behavior changes to the changelog. Base: `main`.

---

## Implementation addenda (review-found, accepted during execution)

- **Task 2 addendum — exhaustion latch for reason overlap.** Code-quality
  review found that the terminal condition carried only by the
  degradedRecord reason string is lost when exhaustion fires while the
  worker is ALREADY Degraded for another reason (enterDegraded CAS no-op);
  the other reason's recovery could then exit to Stable with a dead loop.
  Accepted fix: `Manager.streamMissingExhausted atomic.Bool`, set
  unconditionally in `onStreamMissingError`, OR-ed into the terminal-hold
  gate; never cleared in-process. Pinned by
  `TestAttemptRecovery_StreamMissingExhausted_LatchSurvivesReasonOverlap`.
- **Task 2 addendum — integration window 5s → 8s.** The plan's 5s
  `require.Never` window cannot catch the pre-fix flip (default
  ExitThreshold 5s + 1s monitor tick ⇒ flip at ~6.2s). Verified failing
  pre-fix at 8s.
- **Task 1 addendum — review fixes.** Dispatch-order assertion added to the
  both-fire test; `Hooks.OnError` clause restored in the rewritten godoc;
  orphaned comment block relocated.

## Self-review notes

- Spec coverage: F1→Task 2, F2→Task 3, F3→Task 4, F4→Task 1, F5→Task 5;
  consensus sequencing (dispatcher before terminal-hold tests) honored by
  task order; codex placement constraint (gate after commitment guard)
  honored in Task 2 Step 5; codex wording constraint (no zero-overlap claim)
  honored in Task 4's comment and godoc.
- Out of scope (recorded, deliberate): reap/revive of dead subjects;
  apply-retry age metric/escalation; version-only retry-stash coalescing.
- Test-shape details verified against source: `types.Partition{Keys: ...}`
  literals (no ID field; `{{.PartitionID}}` renders dot-joined keys),
  `logging.NewNop()` logger (as in `partition_consumer_test.go:18`),
  `options` struct at `consumer/options.go:150`,
  `partitionConsumer.Stop()` nil-checks `cancel` so a never-Run stub is safe.
