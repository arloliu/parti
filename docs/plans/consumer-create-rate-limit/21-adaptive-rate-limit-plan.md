# Adaptive (fleet-size-aware) rate limiting — Implementation Plan

> **For agentic workers:** Implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the two v2.8.0 opt-in rate limits (consumer-create, claim-write) fleet-size-aware so each worker enforces `effective_rate = min(perWorkerMax, clusterRate / observed_N)`, bounding the steady-state cluster-wide aggregate instead of letting it scale as `N × perWorkerMax`.

**Architecture:** Each worker observes the committed worker-count `N = len(commit.Workers)` at the pre-debounce commit-decode point (freshness-fenced so a stale commit cannot regress N), then live-retunes one shared token-bucket limiter via a new `SetRate`. Claim-write is fully manager-local. Consumer-create config + limiter live on the consumer, so the manager pushes `N` through a new optional `FleetSizeObserver` interface (mirror of the existing `CapabilityReporter`) and the consumer recomputes its own rate.

**Tech Stack:** Go, `golang.org/x/time/rate` (wrapped in `internal/ratelimit`), NATS JetStream, testify (`require`/`assert`), `make test` (`-race`), `make test-integration` (`-race`).

**Design spec:** `docs/plans/consumer-create-rate-limit/20-adaptive-rate-limit-design.md` (read it first — this plan implements it).

**Conventions (this repo):**
- Reproducer/test-first: write the failing test, confirm it fails, then implement.
- Commit messages: conventional-commit prefix, **no plan jargon** (no "P0", "task N", "spec"), **no attribution trailers**.
- Run `make lint` and fix findings **before every commit**.
- Pre-PR gate (touches `manager/`, `config.go`, `internal/...`): run `make pre-pr` before opening the PR (Task 10).
- Branch: continue on `feat/claim-write-ratelimit` (unreleased v2.8.0 work). These symbols are unreleased and freely shapeable.

---

## File structure

| File | Responsibility | Change |
|---|---|---|
| `internal/ratelimit/ratelimit.go` | token-bucket primitive | add `RateSetter` interface, `SetRate`, `Limit()` |
| `config.go` | `HandoffConfig` + validation | add `ClaimWriteClusterRate`, Validate rule, inert WARN |
| `fleet_size_observer.go` (new) | manager→consumer N push interface | new `FleetSizeObserver` |
| `manager.go` | manager state + construction | `fleetMu`+tuple, `fleetSizeObserver`, `asFleetSizeObserver` |
| `manager_assignment.go` | commit-watch + N observation | `observeFleetSize`, hook in `runCommitWatchSession` |
| `composite_updater.go` | composite updater | forward `ObserveWorkerCount` + late-add replay |
| `consumer/options.go` | Dynamic options | `consumerCreateClusterRate` + `WithConsumerCreateClusterRate` |
| `consumer/dynamic.go` | Dynamic consumer | capture `RateSetter`, `ObserveWorkerCount`, cluster validation |

Metrics (spec §9) are intentionally **out of scope** for this plan (optional follow-up).

---

## Task 1: ratelimit primitive — `SetRate` + `RateSetter`

**Files:**
- Modify: `internal/ratelimit/ratelimit.go`
- Test: `internal/ratelimit/ratelimit_setrate_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `internal/ratelimit/ratelimit_setrate_test.go`:

```go
package ratelimit

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTokenBucketLimiter_SetRate_ChangesSteadyState(t *testing.T) {
	l := New(100, 1, nil) // 100/s, burst 1
	require.InEpsilon(t, 100.0, l.Limit(), 1e-9)

	l.SetRate(10) // tighten to 10/s
	require.InEpsilon(t, 10.0, l.Limit(), 1e-9)

	l.SetRate(1000) // loosen
	require.InEpsilon(t, 1000.0, l.Limit(), 1e-9)
}

func TestTokenBucketLimiter_SetRate_PreservesBurst(t *testing.T) {
	l := New(100, 7, nil)
	l.SetRate(5)
	require.Equal(t, 7, l.rl.Burst(), "SetRate must not change burst")
}

func TestTokenBucketLimiter_RateSetter_Assertion(t *testing.T) {
	var lim Limiter = New(100, 1, nil)
	rs, ok := lim.(RateSetter)
	require.True(t, ok, "*TokenBucketLimiter must satisfy RateSetter")
	rs.SetRate(50)
	require.InEpsilon(t, 50.0, lim.(*TokenBucketLimiter).Limit(), 1e-9)
}

func TestTokenBucketLimiter_SetRate_ConcurrentWithWait(t *testing.T) {
	l := New(1000, 100, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				_ = l.Wait(ctx)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			l.SetRate(500)
			l.SetRate(2000)
		}
	}()
	wg.Wait() // -race must stay clean
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ratelimit/ -run SetRate -v`
Expected: FAIL — `l.SetRate undefined`, `l.Limit undefined`, `RateSetter` undefined.

- [ ] **Step 3: Implement `SetRate`, `Limit`, and `RateSetter`**

In `internal/ratelimit/ratelimit.go`, after the `Limiter` interface block (around line 35) add:

```go
// RateSetter is an optional capability implemented by limiters whose
// steady-state rate can be retuned at runtime (the built-in
// [TokenBucketLimiter]). Adaptive, fleet-size-aware callers type-assert a
// [Limiter] to RateSetter; a limiter that does not implement it (e.g. a
// user-injected custom limiter) keeps its constructed rate.
//
// Kept separate from [Limiter] deliberately: widening Limiter would force every
// implementation (including injected, Wait-only ones) to add SetRate.
type RateSetter interface {
	// SetRate changes the steady-state rate (events/second), leaving burst
	// unchanged. Safe to call concurrently with Wait.
	SetRate(perSec float64)
}
```

Then, after the `Wait` method on `*TokenBucketLimiter` (around line 116) add:

```go
// SetRate changes the steady-state rate to perSec events/second, leaving the
// burst unchanged. It is safe to call concurrently with Wait: the underlying
// rate.Limiter guards its limit with an internal mutex. A rate decrease does
// not retroactively cancel reservations already granted by an in-flight Wait
// (golang.org/x/time/rate semantics); the effect is a brief, self-correcting
// transient.
func (l *TokenBucketLimiter) SetRate(perSec float64) {
	l.rl.SetLimit(rate.Limit(perSec))
}

// Limit returns the current steady-state rate (events/second).
func (l *TokenBucketLimiter) Limit() float64 {
	return float64(l.rl.Limit())
}
```

Add the compile-time assertion next to the existing `var _ Limiter = (*TokenBucketLimiter)(nil)` (line 44):

```go
var _ RateSetter = (*TokenBucketLimiter)(nil)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/ratelimit/ -run SetRate -race -v`
Expected: PASS (all four tests, race-clean).

- [ ] **Step 5: Lint + commit**

```bash
make lint
git add internal/ratelimit/ratelimit.go internal/ratelimit/ratelimit_setrate_test.go
git commit -m "feat(ratelimit): add runtime SetRate and RateSetter capability"
```

---

## Task 2: claim-write cluster-rate config + validation + inert warning

**Files:**
- Modify: `config.go` (HandoffConfig struct ~148-156; `Validate` ~706; `ValidateWithWarnings` ~817)
- Test: `config_test.go`

- [ ] **Step 1: Write the failing test**

Add to `config_test.go`:

```go
func TestValidate_ClaimWriteClusterRate_RequiresPerWorker(t *testing.T) {
	cfg := DefaultConfig()
	cfg.EnableTwoPhaseHandoff = true
	cfg.Handoff.ClaimWriteClusterRate = 1000 // set
	cfg.Handoff.ClaimWritePerSec = 0          // but no per-worker ceiling
	cfg.Handoff.ClaimWriteBurst = 10
	err := cfg.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "ClaimWriteClusterRate")
}

func TestValidate_ClaimWriteClusterRate_NegativeRejected(t *testing.T) {
	cfg := DefaultConfig()
	cfg.EnableTwoPhaseHandoff = true
	cfg.Handoff.ClaimWriteClusterRate = -1
	err := cfg.Validate()
	require.Error(t, err)
}

func TestValidate_ClaimWriteClusterRate_ValidWithPerWorker(t *testing.T) {
	cfg := DefaultConfig()
	cfg.EnableTwoPhaseHandoff = true
	cfg.Handoff.ClaimWritePerSec = 200
	cfg.Handoff.ClaimWriteBurst = 50
	cfg.Handoff.ClaimWriteClusterRate = 1000
	require.NoError(t, cfg.Validate())
}

func TestValidateWithWarnings_ClaimWriteClusterRateWithoutTwoPhase(t *testing.T) {
	const warnPrefix = "Handoff.ClaimWriteClusterRate is set but EnableTwoPhaseHandoff is false"

	t.Run("warns when cluster rate set but two-phase off", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.EnableTwoPhaseHandoff = false
		cfg.Handoff.ClaimWritePerSec = 200
		cfg.Handoff.ClaimWriteBurst = 50
		cfg.Handoff.ClaimWriteClusterRate = 1000
		log := &warnCapture{}
		cfg.ValidateWithWarnings(log)
		require.True(t, log.contains(warnPrefix), "got %v", log.warns)
	})

	t.Run("no warning when two-phase on", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.EnableTwoPhaseHandoff = true
		cfg.Handoff.ClaimWritePerSec = 200
		cfg.Handoff.ClaimWriteBurst = 50
		cfg.Handoff.ClaimWriteClusterRate = 1000
		log := &warnCapture{}
		cfg.ValidateWithWarnings(log)
		require.False(t, log.contains(warnPrefix), "got %v", log.warns)
	})
}
```

> `warnCapture` (with `.contains(prefix)` and `.warns`) is the existing helper in `config_test.go` used by `TestValidateWithWarnings_ClaimWriteRateWithoutTwoPhase`. The warning message in Step 5 must start with exactly `warnPrefix`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run 'ClaimWriteClusterRate' -v`
Expected: FAIL — field `ClaimWriteClusterRate` undefined.

- [ ] **Step 3: Add the field**

In `config.go`, in `HandoffConfig`, immediately after the `ClaimWriteBurst` field:

```go
	// ClaimWriteClusterRate, when > 0, makes claim-write rate limiting
	// fleet-size-aware: each worker enforces
	// min(ClaimWritePerSec, ClaimWriteClusterRate/N), where N is the observed
	// committed worker count. It bounds the STEADY-STATE cluster-wide
	// claim-write rate to ClaimWriteClusterRate instead of N*ClaimWritePerSec.
	// Requires ClaimWritePerSec > 0 (the per-worker ceiling and burst source)
	// and EnableTwoPhaseHandoff. Default 0 = static per-worker rate only.
	ClaimWriteClusterRate float64 `yaml:"claimWriteClusterRate" default:"0" validate:"gte=0"`
```

> Match the tag shape of the adjacent `ClaimWritePerSec`/`ClaimWriteBurst` fields exactly (`yaml:"..." default:"0" validate:"gte=0"`) — this repo loads config via YAML + `SetDefaults`, not JSON. `SetDefaults` is a package function, not a method: add a quick default-applied assertion to one of the Task 2 tests (`var cfg Config; require.NoError(t, SetDefaults(&cfg))` leaves `ClaimWriteClusterRate` at 0 and still validates).

- [ ] **Step 4: Add the Validate cross-field rule**

In `config.go` `Validate`, beside the existing `ClaimWriteBurst >= 1 when ClaimWritePerSec > 0` check (~706):

```go
	if cfg.Handoff.ClaimWriteClusterRate > 0 && cfg.Handoff.ClaimWritePerSec <= 0 {
		return fmt.Errorf("Handoff.ClaimWriteClusterRate > 0 requires Handoff.ClaimWritePerSec > 0 (the per-worker ceiling and burst source)")
	}
```

(The `validate:"gte=0"` tag already rejects negatives; the explicit test above documents it.)

- [ ] **Step 5: Add the inert-config warning**

In `config.go` `ValidateWithWarnings`, beside the existing `ClaimWritePerSec`-without-two-phase warning (~817):

```go
	if !cfg.EnableTwoPhaseHandoff && cfg.Handoff.ClaimWriteClusterRate > 0 {
		logger.Warn("Handoff.ClaimWriteClusterRate is set but EnableTwoPhaseHandoff is false; cluster-rate claim-write limiting has no effect")
	}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test . -run 'ClaimWriteClusterRate' -v`
Expected: PASS.

- [ ] **Step 7: Lint + commit**

```bash
make lint
git add config.go config_test.go
git commit -m "feat(config): add ClaimWriteClusterRate for fleet-aware claim-write limiting"
```

---

## Task 3: `FleetSizeObserver` interface + manager fleet state + `observeFleetSize`

**Files:**
- Create: `fleet_size_observer.go`
- Modify: `manager.go` (struct fields; `asFleetSizeObserver`)
- Modify: `manager_assignment.go` (`observeFleetSize`)
- Test: `fleet_size_observer_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `fleet_size_observer_test.go`:

```go
package parti

import (
	"testing"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/ratelimit"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

type recordingObserver struct{ ns []int }

func (r *recordingObserver) ObserveWorkerCount(n int) { r.ns = append(r.ns, n) }

func newFleetTestManager(lim ratelimit.Limiter, obs FleetSizeObserver, clusterRate, perSec float64) *Manager {
	m := &Manager{logger: logging.NewNop(), claimWriteLimiter: lim, fleetSizeObserver: obs}
	m.cfg.Handoff.ClaimWriteClusterRate = clusterRate
	m.cfg.Handoff.ClaimWritePerSec = perSec
	return m
}

func commit(version int64, lr uint64, workers ...string) *types.AssignmentCommit {
	return &types.AssignmentCommit{Version: version, LeaderRevision: lr, Workers: workers}
}

func TestObserveFleetSize_RetunesClaimWriteAndPushes(t *testing.T) {
	lim := ratelimit.New(200, 50, nil) // perSec ceiling 200
	obs := &recordingObserver{}
	m := newFleetTestManager(lim, obs, 1000, 200)

	m.observeFleetSize(commit(1, 1, "a", "b", "c", "d", "e")) // N=5
	require.InEpsilon(t, 200.0, lim.Limit(), 1e-9, "min(200, 1000/5=200) = 200")
	require.Equal(t, []int{5}, obs.ns)

	m.observeFleetSize(commit(2, 1, "a", "b", "c", "d", "e", "f", "g", "h", "i", "j")) // N=10
	require.InEpsilon(t, 100.0, lim.Limit(), 1e-9, "min(200, 1000/10=100) = 100")
	require.Equal(t, []int{5, 10}, obs.ns)
}

func TestObserveFleetSize_StaleCommit_NoRegression(t *testing.T) {
	lim := ratelimit.New(200, 50, nil)
	obs := &recordingObserver{}
	m := newFleetTestManager(lim, obs, 1000, 200)

	m.observeFleetSize(commit(5, 2, "a", "b", "c", "d", "e", "f", "g", "h", "i", "j")) // v5, N=10 -> 100
	require.InEpsilon(t, 100.0, lim.Limit(), 1e-9)

	// Stale: lower version. Must NOT retune back to N=2.
	m.observeFleetSize(commit(3, 2, "a", "b")) // v3 < v5
	require.InEpsilon(t, 100.0, lim.Limit(), 1e-9, "stale commit must not regress rate")
	require.Equal(t, []int{10}, obs.ns, "stale commit must not push")

	// Stale by leader-revision at same version.
	m.observeFleetSize(commit(5, 1, "a", "b")) // same v5, lower lr
	require.Equal(t, []int{10}, obs.ns)
}

func TestObserveFleetSize_NoOpWhenNUnchanged(t *testing.T) {
	lim := ratelimit.New(200, 50, nil)
	obs := &recordingObserver{}
	m := newFleetTestManager(lim, obs, 1000, 200)
	m.observeFleetSize(commit(1, 1, "a", "b")) // N=2
	m.observeFleetSize(commit(2, 1, "a", "b")) // version advanced, N still 2
	require.Equal(t, []int{2}, obs.ns, "unchanged N must not push again")
}

func TestObserveFleetSize_NoClusterRate_NoRetune(t *testing.T) {
	lim := ratelimit.New(200, 50, nil)
	m := newFleetTestManager(lim, nil, 0, 200) // clusterRate=0
	m.observeFleetSize(commit(1, 1, "a", "b", "c"))
	require.InEpsilon(t, 200.0, lim.Limit(), 1e-9, "clusterRate=0 leaves limiter at perSec")
}

func TestObserveFleetSize_ClampsNToOne(t *testing.T) {
	lim := ratelimit.New(200, 50, nil)
	m := newFleetTestManager(lim, nil, 50, 200)
	m.observeFleetSize(commit(1, 1)) // empty Workers -> clamp N=1
	require.InEpsilon(t, 50.0, lim.Limit(), 1e-9, "min(200, 50/1=50) = 50")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run ObserveFleetSize -v`
Expected: FAIL — `FleetSizeObserver` undefined, `fleetSizeObserver` field undefined, `observeFleetSize` undefined.

- [ ] **Step 3: Create the `FleetSizeObserver` interface**

Create `fleet_size_observer.go`:

```go
package parti

// FleetSizeObserver is an optional interface a [WorkerConsumerUpdater] MAY
// implement to receive the observed cluster worker-count (N) for fleet-size-
// aware (adaptive) rate limiting.
//
// When the registered updater (or any child of a composite updater) satisfies
// this interface, the Manager calls ObserveWorkerCount whenever the committed
// worker-set size changes — and once during startup, before the first apply —
// so the consumer can retune its consumer-create rate to
// min(perWorkerCeiling, clusterRate/N).
//
// Implementations MUST be:
//   - Non-blocking. ObserveWorkerCount runs on the Manager's commit-watch
//     goroutine while the Manager holds an internal lock; it must not perform
//     I/O or block.
//   - Safe for concurrent use.
//   - Non-reentrant: it MUST NOT call back into the Manager or any apply/update
//     path (mirrors the [CapabilityReporter] contract and the D5 lock-order
//     rule).
//
// n is always >= 1.
type FleetSizeObserver interface {
	// ObserveWorkerCount reports the current committed cluster worker-count.
	ObserveWorkerCount(n int)
}
```

- [ ] **Step 4: Add manager fleet state + `asFleetSizeObserver`**

In `manager.go`, add fields to the `Manager` struct (near `capReporter`):

```go
	// fleetSizeObserver is the consumer updater cast to FleetSizeObserver at
	// construction (nil when the updater does not implement it). Used to push
	// the observed worker-count N for adaptive consumer-create rate limiting.
	fleetSizeObserver FleetSizeObserver

	// fleetMu guards the last-observed commit identity and worker-count so the
	// freshness fence and the recorded N never split. Held across the limiter
	// retune + the FleetSizeObserver push (both non-blocking, non-reentrant).
	fleetMu            sync.Mutex
	lastFleetVersion   int64
	lastFleetLeaderRev uint64
	lastObservedN      int
```

Add the constructor helper near `asCapabilityReporter` (manager.go:981):

```go
// asFleetSizeObserver returns u as a FleetSizeObserver when it implements the
// interface; otherwise nil. Called once at construction so the commit-watch
// hot path does not repeat the type assertion. Mirrors asCapabilityReporter.
func asFleetSizeObserver(u WorkerConsumerUpdater) FleetSizeObserver {
	fo, _ := u.(FleetSizeObserver)
	return fo
}
```

- [ ] **Step 5: Implement `observeFleetSize`**

In `manager_assignment.go`, add (near the commit-watch helpers, e.g. after `workerAssignmentChanged`):

```go
// observeFleetSize records the cluster worker-count from a committed assignment
// and live-retunes the fleet-size-aware rate limiters. It runs at the
// pre-debounce commit-decode point so it sees every commit the watcher
// delivers — including those the apply debounce suppresses when this worker's
// own slice is unchanged.
//
// It is freshness-fenced: only a commit that strictly supersedes the last
// observed one on (Version, LeaderRevision) lex order retunes, so a stale or
// out-of-order commit (e.g. a reconcile snapshot racing a newer watcher event)
// can never regress N. Guarded by fleetMu; the retune and push are non-blocking
// and non-reentrant by contract.
func (m *Manager) observeFleetSize(commit *types.AssignmentCommit) {
	if commit == nil {
		return
	}
	n := len(commit.Workers)
	if n < 1 {
		n = 1 // defensive: the publisher never emits an empty worker-set
	}

	m.fleetMu.Lock()
	defer m.fleetMu.Unlock()

	// Freshness fence: same (Version, LeaderRevision) lex PREFIX the apply stale
	// gate uses (commitSupersedesForStash also compares BatchDigest/source at
	// equal (V,LR), but N is fixed for a given (V,LR), so the lex prefix is
	// sufficient and correct here). Drop non-superseding commits so a stale or
	// out-of-order reconcile snapshot can never regress N.
	if commit.Version < m.lastFleetVersion ||
		(commit.Version == m.lastFleetVersion && commit.LeaderRevision <= m.lastFleetLeaderRev) {
		return
	}
	m.lastFleetVersion = commit.Version
	m.lastFleetLeaderRev = commit.LeaderRevision

	if n == m.lastObservedN {
		return // version advanced but N unchanged: no retune
	}
	m.lastObservedN = n

	// Claim-write (manager-local): retune the shared limiter in place.
	if m.cfg.Handoff.ClaimWriteClusterRate > 0 {
		if rs, ok := m.claimWriteLimiter.(ratelimit.RateSetter); ok {
			rs.SetRate(effectiveRate(m.cfg.Handoff.ClaimWritePerSec, m.cfg.Handoff.ClaimWriteClusterRate, n))
		}
	}

	// Consumer-create: push N; the consumer owns its rate policy.
	if m.fleetSizeObserver != nil {
		m.fleetSizeObserver.ObserveWorkerCount(n)
	}
}

// effectiveRate returns min(perWorkerMax, clusterRate/n). n must be >= 1.
func effectiveRate(perWorkerMax, clusterRate float64, n int) float64 {
	r := clusterRate / float64(n)
	if perWorkerMax > 0 && perWorkerMax < r {
		return perWorkerMax
	}
	return r
}
```

Ensure `manager_assignment.go` imports `"github.com/arloliu/parti/v2/internal/ratelimit"` (add to the import block if absent).

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test . -run 'ObserveFleetSize' -race -v`
Expected: PASS (all five observe tests).

- [ ] **Step 7: Lint + commit**

```bash
make lint
git add fleet_size_observer.go fleet_size_observer_test.go manager.go manager_assignment.go
git commit -m "feat(manager): add FleetSizeObserver and freshness-fenced observeFleetSize"
```

---

## Task 4: wire `observeFleetSize` into the commit watcher + bootstrap + construction

**Files:**
- Modify: `manager.go` (construction: set `fleetSizeObserver`; bootstrap observe ~692-701)
- Modify: `manager_assignment.go` (`runCommitWatchSession` `onUpdate`/`onReconcile`)
- Test: `manager_fleet_wiring_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `manager_fleet_wiring_test.go`:

```go
package parti

import (
	"testing"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/ratelimit"
	"github.com/stretchr/testify/require"
)

// Pins the observeFleetSize contract used by the watcher seam: sequential fresh
// commits observe in order. This is NOT a test of runCommitWatchSession itself —
// the production wiring (onUpdate/onReconcile actually call observeFleetSize) is
// proven end-to-end by the integration test in Task 8, where a real fleet change
// flows through the watcher. A pure-unit assertion on the watcher closure is
// brittle, so the integration test is the real wiring proof.
func TestObserveFleetSize_SequentialFreshCommits(t *testing.T) {
	lim := ratelimit.New(200, 50, nil)
	obs := &recordingObserver{}
	m := newFleetTestManager(lim, obs, 1000, 200)

	m.observeFleetSize(commit(1, 1, "a", "b")) // N=2
	m.observeFleetSize(commit(2, 1, "a", "b", "c", "d")) // N=4
	require.Equal(t, []int{2, 4}, obs.ns)
}
```

- [ ] **Step 2: Run test to verify it passes (uses Task 3 code)**

Run: `go test . -run TestObserveFleetSize_SequentialFreshCommits -v`
Expected: PASS — this task adds the production wiring that the Task 8 integration test exercises end-to-end.

- [ ] **Step 3: Hook the watcher decode points**

In `manager_assignment.go` `runCommitWatchSession`, update the `onUpdate` and `onReconcile` handlers to observe N before staging:

```go
		onUpdate: func(entry jetstream.KeyValueEntry) {
			if commit, ok := m.decodeCommitEntry(entry); ok {
				m.observeFleetSize(commit)
				db.stage(commit)
			}
		},
		flush:  db.flush,
		timerC: db.timerC,
		onReconcile: func() {
			if kv == nil {
				return
			}
			current, _, gerr := kvutil.GetJSON[types.AssignmentCommit](ctx, kv, key)
			if gerr != nil || current == nil {
				return
			}
			m.observeFleetSize(current)
			db.stage(current)
		},
```

- [ ] **Step 4: Hook the bootstrap path**

In `manager.go`, in the initial-bootstrap commit path (lines 692-701), the
structural skeleton is `newAsg, ok := m.buildAssignmentFromCommit(...)`, then
`if ok {` (with an existing explanatory comment), then
`if err := m.applyAssignmentWithPrev(Assignment{}, newAsg); err != nil`. Insert
the observe right after `if ok {` and **before** `applyAssignmentWithPrev` (after
the existing comment block; keep that comment), so the consumer receives N before
the first create storm:

```go
		newAsg, ok := m.buildAssignmentFromCommit(commit, m.WorkerID())
		if ok {
			// (existing comment block stays here, unchanged)
			m.observeFleetSize(commit) // retune before the first consumer-create storm
			if err := m.applyAssignmentWithPrev(Assignment{}, newAsg); err != nil {
				return err
			}
```

For the **alias-fallback** apply path — the real code at line 759 is
`if err := m.applyAssignmentWithPrev(Assignment{}, initial); err != nil {` where
`initial := m.CurrentAssignment()` (line 680) carries `TotalWorkers` from the
legacy alias — insert right before that apply:

```go
		if initial.TotalWorkers > 0 {
			m.observeFleetSizeN(initial.Version, initial.LeaderRevision, initial.TotalWorkers)
		}
```

Add a small sibling helper next to `observeFleetSize` in `manager_assignment.go` so the alias path (which has an `Assignment`, not a `*AssignmentCommit`) shares the fenced logic:

```go
// observeFleetSizeN is observeFleetSize for callers that already hold the
// (version, leaderRevision, N) triple (e.g. the legacy-alias bootstrap path)
// rather than a *AssignmentCommit. n is clamped to >= 1.
func (m *Manager) observeFleetSizeN(version int64, leaderRev uint64, n int) {
	if n < 1 {
		n = 1
	}
	m.fleetMu.Lock()
	defer m.fleetMu.Unlock()
	if version < m.lastFleetVersion ||
		(version == m.lastFleetVersion && leaderRev <= m.lastFleetLeaderRev) {
		return
	}
	m.lastFleetVersion = version
	m.lastFleetLeaderRev = leaderRev
	if n == m.lastObservedN {
		return
	}
	m.lastObservedN = n
	if m.cfg.Handoff.ClaimWriteClusterRate > 0 {
		if rs, ok := m.claimWriteLimiter.(ratelimit.RateSetter); ok {
			rs.SetRate(effectiveRate(m.cfg.Handoff.ClaimWritePerSec, m.cfg.Handoff.ClaimWriteClusterRate, n))
		}
	}
	if m.fleetSizeObserver != nil {
		m.fleetSizeObserver.ObserveWorkerCount(n)
	}
}
```

Then refactor `observeFleetSize` to delegate (DRY):

```go
func (m *Manager) observeFleetSize(commit *types.AssignmentCommit) {
	if commit == nil {
		return
	}
	m.observeFleetSizeN(commit.Version, commit.LeaderRevision, len(commit.Workers))
}
```

(Delete the now-duplicated body from Task 3 step 5; `effectiveRate` stays.)

- [ ] **Step 5: Wire `fleetSizeObserver` at construction**

In `manager.go` where `capReporter` is set (line 447), add the sibling assignment:

```go
		capReporter:       asCapabilityReporter(options.consumerUpdater),
		fleetSizeObserver: asFleetSizeObserver(options.consumerUpdater),
```

- [ ] **Step 6: Run unit tests**

Run: `go test . -run 'ObserveFleetSize' -race -v`
Expected: PASS (all observe tests, including the sequential-fresh-commits wiring contract).

- [ ] **Step 7: Build + lint + commit**

```bash
go build ./...
make lint
git add manager.go manager_assignment.go manager_fleet_wiring_test.go
git commit -m "feat(manager): observe committed worker-count and retune limiters live"
```

---

## Task 5: consumer `WithConsumerCreateClusterRate` option + validation

**Files:**
- Modify: `consumer/options.go` (options struct ~193; new option ~900)
- Modify: `consumer/dynamic.go` (`resolveConsumerCreateLimiter` ~735)
- Test: `consumer/consumer_create_clusterrate_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `consumer/consumer_create_clusterrate_test.go`:

```go
package consumer

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWithConsumerCreateClusterRate_RequiresPerWorker(t *testing.T) {
	_, err := buildDynamic(t, WithConsumerCreateClusterRate(1000)) // no WithConsumerCreateRate
	require.Error(t, err)
	require.Contains(t, err.Error(), "WithConsumerCreateClusterRate")
}

func TestWithConsumerCreateClusterRate_RejectedWithInjectedLimiter(t *testing.T) {
	inj, err := NewConsumerCreateLimiter(50, 5)
	require.NoError(t, err)
	_, err = buildDynamic(t,
		WithConsumerCreateLimiter(inj),
		WithConsumerCreateClusterRate(1000),
	)
	require.Error(t, err)
}

func TestWithConsumerCreateClusterRate_NegativeRejected(t *testing.T) {
	t.Run("with per-worker rate", func(t *testing.T) {
		_, err := buildDynamic(t, WithConsumerCreateRate(200, 50), WithConsumerCreateClusterRate(-1))
		require.Error(t, err)
	})
	t.Run("alone (no per-worker rate)", func(t *testing.T) {
		_, err := buildDynamic(t, WithConsumerCreateClusterRate(-1))
		require.Error(t, err)
	})
	t.Run("with injected limiter", func(t *testing.T) {
		inj, err := NewConsumerCreateLimiter(50, 5)
		require.NoError(t, err)
		_, err = buildDynamic(t, WithConsumerCreateLimiter(inj), WithConsumerCreateClusterRate(-1))
		require.Error(t, err)
	})
}

func TestWithConsumerCreateClusterRate_ValidWithPerWorker(t *testing.T) {
	d, err := buildDynamic(t,
		WithConsumerCreateRate(200, 50),
		WithConsumerCreateClusterRate(1000),
	)
	require.NoError(t, err)
	require.NotNil(t, d.inner.ConsumerCreateLimiter())
}
```

> `buildDynamic(t, ...opts)` is the existing test helper in `consumer/consumer_create_ratelimit_test.go`. Reuse it.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./consumer/ -run ClusterRate -v`
Expected: FAIL — `WithConsumerCreateClusterRate` undefined.

- [ ] **Step 3: Add the option field**

In `consumer/options.go`, in the options struct beside `consumerCreatePerSec`:

```go
	consumerCreateClusterRate float64
```

- [ ] **Step 4: Add the option**

In `consumer/options.go`, after `WithConsumerCreateRate`:

```go
// WithConsumerCreateClusterRate makes consumer-create rate limiting
// fleet-size-aware. Given a cluster-wide target (events/second), each worker
// enforces min(perWorkerCeiling, clusterPerSec/N), where perWorkerCeiling is
// the rate from [WithConsumerCreateRate] and N is the cluster worker-count the
// Parti Manager observes and pushes to this consumer.
//
// This bounds the STEADY-STATE cluster-wide create rate to clusterPerSec
// instead of N*perWorkerCeiling. The per-worker ceiling still caps any single
// worker during fleet-size transitions.
//
// Requires [WithConsumerCreateRate] (which supplies burst and the ceiling) and
// the built-in limiter; it is rejected at [NewDynamic] when used alone or with
// an injected [WithConsumerCreateLimiter] (an injected, possibly shared limiter
// is not adaptively retuned). clusterPerSec must be >= 0; 0 disables the
// overlay (static per-worker behaviour).
//
// Applies only to [Dynamic].
func WithConsumerCreateClusterRate(clusterPerSec float64) DynamicOption {
	return dynamicOpt(func(o *options) {
		o.consumerCreateClusterRate = clusterPerSec
	})
}
```

- [ ] **Step 5: Add validation in `resolveConsumerCreateLimiter`**

In `consumer/dynamic.go`, replace the head of `resolveConsumerCreateLimiter` (the injected-limiter and no-rate early returns at ~735-743) so the negative-cluster-rate check runs **before all branches** (otherwise `WithConsumerCreateClusterRate(-1)` slips through the injected / `perSec == 0` early returns):

```go
func resolveConsumerCreateLimiter(o options) (ratelimit.Limiter, error) {
	// Validate the cluster-rate overlay up front — independent of which limiter
	// wins — so a negative value is rejected on every path.
	if o.consumerCreateClusterRate < 0 {
		return nil, fmt.Errorf("WithConsumerCreateClusterRate: clusterPerSec must be >= 0, got %v", o.consumerCreateClusterRate)
	}

	// A non-nil injected limiter always wins; it is not adaptively retuned.
	if o.consumerCreateLimiter != nil {
		if o.consumerCreateClusterRate > 0 {
			return nil, fmt.Errorf("WithConsumerCreateClusterRate cannot be combined with an injected WithConsumerCreateLimiter (an injected limiter is not adaptively retuned)")
		}
		return o.consumerCreateLimiter, nil
	}

	// No per-worker rate configured. A cluster rate alone is invalid (it needs
	// the per-worker ceiling and burst); otherwise return unlimited (nil).
	if o.consumerCreatePerSec == 0 {
		if o.consumerCreateClusterRate > 0 {
			return nil, fmt.Errorf("WithConsumerCreateClusterRate requires WithConsumerCreateRate (the per-worker ceiling and burst source)")
		}
		return ratelimit.Limiter(nil), nil //nolint:nilnil // nil limiter is the intended "unlimited" sentinel
	}
```

Leave the remainder of the function (the `perSec < 0` check, the `burst < 1` check, the throttle-observer wiring, and the `return ratelimit.New(...)`) unchanged.

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./consumer/ -run ClusterRate -v`
Expected: PASS.

- [ ] **Step 7: Lint + commit**

```bash
make lint
git add consumer/options.go consumer/dynamic.go consumer/consumer_create_clusterrate_test.go
git commit -m "feat(consumer): add WithConsumerCreateClusterRate option and validation"
```

---

## Task 6: consumer `Dynamic.ObserveWorkerCount` + `RateSetter` capture

**Files:**
- Modify: `consumer/dynamic.go` (`Dynamic` struct fields; `NewDynamic` capture; new method)
- Test: `consumer/consumer_create_clusterrate_test.go` (extend)

- [ ] **Step 1: Write the failing test**

Add to `consumer/consumer_create_clusterrate_test.go`:

```go
func TestDynamic_ObserveWorkerCount_RetunesBuiltInLimiter(t *testing.T) {
	d, err := buildDynamic(t,
		WithConsumerCreateRate(200, 50),
		WithConsumerCreateClusterRate(1000),
	)
	require.NoError(t, err)

	d.ObserveWorkerCount(5) // min(200, 1000/5=200) = 200
	lim := d.inner.ConsumerCreateLimiter()
	tb, ok := lim.(interface{ Limit() float64 })
	require.True(t, ok)
	require.InEpsilon(t, 200.0, tb.Limit(), 1e-9)

	d.ObserveWorkerCount(20) // min(200, 1000/20=50) = 50
	require.InEpsilon(t, 50.0, tb.Limit(), 1e-9)
}

func TestDynamic_ObserveWorkerCount_NoClusterRate_NoOp(t *testing.T) {
	d, err := buildDynamic(t, WithConsumerCreateRate(200, 50)) // no cluster rate
	require.NoError(t, err)
	d.ObserveWorkerCount(20) // must not change the fixed 200/s
	lim := d.inner.ConsumerCreateLimiter()
	tb := lim.(interface{ Limit() float64 })
	require.InEpsilon(t, 200.0, tb.Limit(), 1e-9)
}

func TestDynamic_ObserveWorkerCount_InjectedLimiter_NoOp(t *testing.T) {
	inj, err := NewConsumerCreateLimiter(50, 5)
	require.NoError(t, err)
	d, err := buildDynamic(t, WithConsumerCreateLimiter(inj))
	require.NoError(t, err)
	require.NotPanics(t, func() { d.ObserveWorkerCount(10) }) // no RateSetter captured -> no-op
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./consumer/ -run ObserveWorkerCount -v`
Expected: FAIL — `d.ObserveWorkerCount` undefined.

- [ ] **Step 3: Add fields to the `Dynamic` struct**

In `consumer/dynamic.go`, in the `Dynamic` struct:

```go
	// Adaptive consumer-create rate (fleet-size-aware). createRateSetter is the
	// built-in limiter cast to RateSetter, non-nil only when WithConsumerCreateRate
	// AND WithConsumerCreateClusterRate are set. createPerSec is the per-worker
	// ceiling; createClusterRate is the cluster target.
	createRateSetter  ratelimit.RateSetter
	createPerSec      float64
	createClusterRate float64
```

- [ ] **Step 4: Capture the handle in `NewDynamic`**

In `consumer/dynamic.go` `NewDynamic`, after `resolvedLimiter, err := resolveConsumerCreateLimiter(o)` succeeds and after `d` is constructed (and before/after `d.inner = inner`), capture the adaptive handle:

```go
	if o.consumerCreateClusterRate > 0 {
		if rs, ok := resolvedLimiter.(ratelimit.RateSetter); ok {
			d.createRateSetter = rs
			d.createPerSec = o.consumerCreatePerSec
			d.createClusterRate = o.consumerCreateClusterRate
		}
	}
```

> `resolveConsumerCreateLimiter` has already rejected cluster-without-rate and cluster-with-injected, so when `consumerCreateClusterRate > 0` the resolved limiter is the built-in `*TokenBucketLimiter`, which implements `RateSetter`.

- [ ] **Step 5: Add `ObserveWorkerCount`**

In `consumer/dynamic.go` (near `Capabilities`, the other manager-facing optional method):

```go
// ObserveWorkerCount implements parti.FleetSizeObserver. The Parti Manager calls
// it with the current committed cluster worker-count N so the Dynamic consumer
// can retune its consumer-create limiter to min(perWorkerCeiling, clusterRate/N).
//
// It is a no-op unless both WithConsumerCreateRate and WithConsumerCreateClusterRate
// were configured. Non-blocking and non-reentrant per the FleetSizeObserver
// contract: it only calls SetRate on the built-in limiter.
func (d *Dynamic) ObserveWorkerCount(n int) {
	if d.createRateSetter == nil || d.createClusterRate <= 0 {
		return
	}
	if n < 1 {
		n = 1
	}
	r := d.createClusterRate / float64(n)
	if d.createPerSec > 0 && d.createPerSec < r {
		r = d.createPerSec
	}
	d.createRateSetter.SetRate(r)
}
```

Add a compile-time assertion (in `consumer/dynamic.go`, package-level) that documents the optional-interface satisfaction without importing `parti` (avoid an import cycle — assert structurally):

```go
var _ interface{ ObserveWorkerCount(int) } = (*Dynamic)(nil)
```

- [ ] **Step 6: Run tests**

Run: `go test ./consumer/ -run 'ObserveWorkerCount|ClusterRate' -race -v`
Expected: PASS.

- [ ] **Step 7: Lint + commit**

```bash
make lint
git add consumer/dynamic.go consumer/consumer_create_clusterrate_test.go
git commit -m "feat(consumer): retune built-in create limiter on observed worker-count"
```

---

## Task 7: composite updater — forward `ObserveWorkerCount` + late-add replay

**Files:**
- Modify: `composite_updater.go`
- Test: `composite_updater_test.go` (extend or create `composite_updater_fleet_test.go`)

- [ ] **Step 1: Write the failing test**

Create `composite_updater_fleet_test.go`:

```go
package parti

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type fleetChild struct {
	last int
}

func (f *fleetChild) UpdateWorkerConsumer(_ context.Context, _ string, _ []Partition) error {
	return nil
}
func (f *fleetChild) ObserveWorkerCount(n int) { f.last = n }

func TestComposite_ForwardsObserveWorkerCount(t *testing.T) {
	a, b := &fleetChild{}, &fleetChild{}
	c := NewCompositeConsumerUpdater(a, b)
	c.ObserveWorkerCount(7)
	require.Equal(t, 7, a.last)
	require.Equal(t, 7, b.last)
}

func TestComposite_ReplaysLastNToLateAddedChild(t *testing.T) {
	a := &fleetChild{}
	c := NewCompositeConsumerUpdater(a)
	c.ObserveWorkerCount(9)
	require.Equal(t, 9, a.last)

	late := &fleetChild{}
	c.Add(late)
	require.Equal(t, 9, late.last, "late-added child must receive the cached N")
}

func TestComposite_LateAddBeforeAnyObserve_NoReplay(t *testing.T) {
	c := NewCompositeConsumerUpdater()
	late := &fleetChild{last: -1}
	c.Add(late)
	require.Equal(t, -1, late.last, "no N observed yet -> no replay")
}

var _ FleetSizeObserver = (*CompositeConsumerUpdater)(nil)
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run 'Composite_(Forwards|Replays|LateAdd)' -v`
Expected: FAIL — `ObserveWorkerCount` undefined on `*CompositeConsumerUpdater`.

- [ ] **Step 3: Add cached-N state to the composite**

In `composite_updater.go`, in the `CompositeConsumerUpdater` struct, add (beside the existing mutex-guarded state, e.g. near the stream-missing fields):

```go
	// lastFleetN is the most recent worker-count observed via ObserveWorkerCount,
	// replayed to children added later (mirrors the stream-missing replay). 0 =
	// none observed yet.
	lastFleetN int
```

- [ ] **Step 4: Implement `ObserveWorkerCount` + replay on `Add`**

In `composite_updater.go`:

```go
// ObserveWorkerCount implements [FleetSizeObserver] by caching the worker-count
// and forwarding it to every child that implements FleetSizeObserver. The cache
// is replayed to children added later via Add.
func (c *CompositeConsumerUpdater) ObserveWorkerCount(n int) {
	c.mu.Lock()
	c.lastFleetN = n
	snapshot := make([]WorkerConsumerUpdater, len(c.updaters))
	copy(snapshot, c.updaters)
	c.mu.Unlock()

	for _, u := range snapshot {
		if fo, ok := u.(FleetSizeObserver); ok {
			fo.ObserveWorkerCount(n)
		}
	}
}

var _ FleetSizeObserver = (*CompositeConsumerUpdater)(nil)
```

Replace the existing `Add` method **in full** with the version below. The current
`Add` (composite_updater.go:100-114) forwards the stream-missing observer *under*
`c.mu` (via `defer c.mu.Unlock()`), which contradicts the struct's documented
"callbacks run unlocked" contract and the correct pattern in
`SetOnStreamMissingError`. This rewrite snapshots the newly-added children +
inherited state under the lock, then forwards **both** stream-missing and
fleet-N **outside** the lock — fixing that inconsistency and adding the replay:

```go
// Add appends additional updaters to the composite.
// This is useful for dynamically registering consumers after creation.
//
// Newly-added children inherit any state the composite has already received:
// an installed stream-missing observer (via SetOnStreamMissingError) and the
// last observed fleet worker-count (via ObserveWorkerCount), so a late
// registration does not silently miss either. Inherited-state forwarding runs
// outside c.mu so a slow child cannot serialize peers or deadlock a re-entrant
// Add.
func (c *CompositeConsumerUpdater) Add(updaters ...WorkerConsumerUpdater) {
	c.mu.Lock()
	var added []WorkerConsumerUpdater
	for _, u := range updaters {
		if u == nil {
			continue
		}
		c.updaters = append(c.updaters, u)
		added = append(added, u)
	}
	observer := c.currentObserver
	fleetN := c.lastFleetN
	c.mu.Unlock()

	for _, u := range added {
		if observer != nil {
			if obs, ok := u.(recovery.StreamMissingObserver); ok {
				obs.SetOnStreamMissingError(observer)
			}
		}
		if fleetN > 0 {
			if fo, ok := u.(FleetSizeObserver); ok {
				fo.ObserveWorkerCount(fleetN)
			}
		}
	}
}
```

> This preserves the existing stream-missing replay behaviour (the child still
> receives the observer) — it only moves the forward out of the lock, matching
> `SetOnStreamMissingError`. Run the existing composite tests to confirm no
> regression: `go test . -run Composite -race -v`.

- [ ] **Step 5: Run tests**

Run: `go test . -run 'Composite_(Forwards|Replays|LateAdd)' -race -v`
Expected: PASS.

- [ ] **Step 6: Lint + commit**

```bash
make lint
git add composite_updater.go composite_updater_fleet_test.go
git commit -m "feat(manager): forward and replay observed worker-count in composite updater"
```

---

## Task 8: integration tests (fleet retune, steady-state bound, concurrency stress)

**Files:**
- Create: `test/integration/manager/adaptive_ratelimit_test.go`

**What is proven where:** the rate *math* `min(ceiling, cluster/N)` for both
features is already unit-proven (Task 3 for claim-write via `lim.Limit()`; Task 6
for consumer via `d.inner.ConsumerCreateLimiter().Limit()`). The integration test
proves what units cannot: that a *real* fleet-size change this worker observes —
**even when its own partition slice is unchanged** (the pre-debounce hook) —
actually reaches `observeFleetSize`, that the consumer push retunes end-to-end,
and that the new commit-watch work is race-clean. It does **not** try to read the
unexported `m.claimWriteLimiter` from the external test package.

- [ ] **Step 1: Add the test-only N accessor**

In `manager.go`, add a same-package test seam (the integration package is
external `manager_test`, so this must be an exported-for-test method; mirror
existing `*ForTest` seams in the codebase):

```go
// ObservedWorkerCountForTest returns the last observed committed worker-count.
// Exported solely for integration tests of the adaptive rate limiter.
func (m *Manager) ObservedWorkerCountForTest() int {
	m.fleetMu.Lock()
	defer m.fleetMu.Unlock()
	return m.lastObservedN
}
```

> If the repo already has an unexported `*ForTest` convention reachable from
> `test/integration/manager` (e.g. via an export_test.go bridge in package
> `parti`), follow that instead of exporting on the public type. Check for an
> existing pattern before adding a new exported method.

- [ ] **Step 2: Write the integration tests**

Create `test/integration/manager/adaptive_ratelimit_test.go`. Follow the existing
patterns in that directory (`claim_write_ratelimit_startup_test.go`,
`epoch_monitor_concurrency_test.go`). Structure:

```go
//go:build integration

package manager_test

// TestAdaptive_WorkerObservesFleetGrowthWithUnchangedSlice:
//   - embedded NATS + manager-1 with a Dynamic consumer configured
//     WithConsumerCreateRate(1000, big) + WithConsumerCreateClusterRate(100),
//     and ClaimWritePerSec=1000 + ClaimWriteClusterRate=100 + two-phase handoff
//   - drive the cluster to N=2 (a second worker joins) WITHOUT changing
//     manager-1's partition slice (e.g. extra partitions assigned only to w2)
//   - require.Eventually: manager-1.ObservedWorkerCountForTest() == 2  (proves
//     the pre-debounce hook fires on a slice-unchanged fleet change)
//   - require.InEpsilon the Dynamic consumer's create limiter Limit() == 50
//     (min(1000, 100/2)) — proves the FleetSizeObserver push retunes end-to-end.
//     (Claim-write rate math is unit-proven in Task 3; here we only assert the
//     observed N reached the manager, since m.claimWriteLimiter is unexported.)
//
// TestAdaptive_CommitChurnRaceClean:
//   - 2-3 managers, aggressive commit churn (rapid join/leave or source changes),
//     OperationTimeout small, run ~5s, assert no -race trigger (monitor-goroutine
//     rule). Pure -race gate; no rate assertion.
```

> Use `require.Eventually` for the observation (commit propagation is async), and
> assert the consumer limiter's `Limit()` (reachable via the Dynamic) rather than
> timing-based pacing, which is flaky. Keep the concurrency-stress test a pure
> `-race` gate.

- [ ] **Step 3: Run the integration tests**

Run: `go test -tags=integration -race ./test/integration/manager/ -run Adaptive -v`
Expected: PASS, race-clean.

- [ ] **Step 4: Lint + commit**

```bash
make lint
git add test/integration/manager/adaptive_ratelimit_test.go manager.go
git commit -m "test(integration): cover fleet-aware retune and commit-churn race safety"
```

---

## Task 9: documentation + CHANGELOG

**Files:**
- Modify: `docs/CONSUMERS.md` (consumer-create rate section)
- Modify: `CHANGELOG.md` (`## [Unreleased]`)
- Modify: relevant handoff/config doc if present (e.g. `docs/HANDOFF.md` or `docs/CONFIG.md`)

- [ ] **Step 1: Document the consumer option**

In `docs/CONSUMERS.md`, in the consumer-create rate-limit section, add a subsection for `WithConsumerCreateClusterRate` with the `min(perWorkerCeiling, clusterRate/N)` formula, the "requires WithConsumerCreateRate / not with injected limiter" rule, and the steady-state-vs-transient note (the aggregate bound holds at steady state; the ceiling bounds transients; aggregate burst is `Σ burst`).

- [ ] **Step 2: Document the claim-write knob**

Document `HandoffConfig.ClaimWriteClusterRate` wherever `ClaimWritePerSec` is documented (same formula, requires-two-phase + requires-perSec rules, inert WARN behaviour).

- [ ] **Step 3: Update CHANGELOG**

Under `## [Unreleased]`, in the existing rate-limit entry, add: fleet-size-aware (adaptive) variants — `WithConsumerCreateClusterRate` and `HandoffConfig.ClaimWriteClusterRate` — bounding the steady-state cluster-wide rate to the configured target.

- [ ] **Step 4: Commit**

```bash
make lint
git add docs/ CHANGELOG.md
git commit -m "docs: document fleet-aware consumer-create and claim-write rate limits"
```

---

## Task 10: full pre-PR gate

- [ ] **Step 1: Run the full gate**

```bash
make pre-pr
```

Expected: lint clean; `make test` (`-race`) green; `make test-integration` (`-race`) green, including the three cross-feature contracts (whole-bucket-missing → degraded; peer-takeover; OnDegraded-once) and the new adaptive tests.

- [ ] **Step 2: Verify the static-rate path is byte-for-byte unchanged**

Run the pre-existing static rate-limit tests to confirm `clusterRate == 0` behaviour is untouched:

Run: `go test ./consumer/ ./internal/durable/ . -run 'ConsumerCreate|ClaimWrite' -race -v`
Expected: PASS (the unrelated static tests still green).

- [ ] **Step 3: Final confirmation**

Confirm: no plan jargon in commit messages (`git log --oneline feat/claim-write-ratelimit`), no attribution trailers, lint clean. Ready for `/post-impl-review` and squash-by-scope per the repo workflow.

---

## Self-review checklist (completed by plan author)

- **Spec coverage:** §2 model → Tasks 1,3,6; §3 API → Tasks 2,5; §4 N-observation + fence → Tasks 3,4; §5.1 SetRate → Task 1; §5.2 claim-write → Tasks 2,3,4; §5.3 consumer push → Tasks 5,6; §5.3 composite → Task 7; §6 edges (clamp, stale, no-op, clusterRate=0) → Task 3 tests; §7 concurrency → Tasks 1,3,8; §8 validation → Tasks 2,5; §10 testing → every task + Task 8; §9 metrics → explicitly deferred. No gaps.
- **Placeholders:** none — every code step shows full code; the two `>` notes point at existing helpers to reuse, not unwritten code.
- **Type consistency:** `observeFleetSize(*types.AssignmentCommit)` / `observeFleetSizeN(int64, uint64, int)` / `effectiveRate(float64, float64, int) float64` / `RateSetter.SetRate(float64)` / `FleetSizeObserver.ObserveWorkerCount(int)` / `Dynamic.ObserveWorkerCount(int)` used consistently across tasks. `commit.Version int64`, `commit.LeaderRevision uint64` match `types/assignment_commit.go`.
