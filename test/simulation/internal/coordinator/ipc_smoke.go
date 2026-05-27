package coordinator

// Phase 7a — IPC smoke oracle and process-worker observer shim.
//
// IPCSmokeOracle gates Phase 7b: within smokeWindow of a worker's first frame,
// the orchestrator MUST have received at least one of EACH of the four
// lifecycle-only kinds (assignment_report, start_latency, state, leader).
// received_message and degraded are workload/chaos-driven and explicitly NOT
// part of the smoke check (a smoke run without a producer would otherwise
// false-positive).
//
// ProcessWorkerObserver satisfies WorkerObserver so the existing
// LeaderUniquenessWatcher and StateReconcileWatcher work unchanged in
// process mode. Its state/leader/stable fields are written from the IPC reader
// and read by the watcher poll goroutines under atomic semantics.

import (
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// IPCSmokeOracle tracks per-worker first-seen times for each lifecycle frame
// kind. After smokeWindow elapses from the worker's first observed frame, any
// kind never seen counts as one ipc_smoke_violation.
//
// Each worker contributes at most one violation per missing kind. Multiple
// missing kinds on the same worker contribute multiple violations.
type IPCSmokeOracle struct {
	mu          sync.Mutex
	smokeWindow time.Duration
	now         func() time.Time

	// state[workerID][kind] = first-seen wallclock time, or zero if unseen.
	state      map[string]map[string]time.Time
	firstFrame map[string]time.Time // workerID → first frame ever
	evaluated  map[string]bool      // workerID → smoke window already evaluated

	violations atomic.Int64
}

// smokeKinds lists the four lifecycle kinds the oracle requires. Workload
// kinds (received_message, degraded) are intentionally absent.
var smokeKinds = []string{
	IPCKindAssignmentReport,
	IPCKindStartLatency,
	IPCKindState,
	IPCKindLeader,
}

// NewIPCSmokeOracle constructs an oracle with the given smoke window. The
// optional now func is for deterministic unit tests; nil = time.Now.
func NewIPCSmokeOracle(smokeWindow time.Duration, now func() time.Time) *IPCSmokeOracle {
	if now == nil {
		now = time.Now
	}
	return &IPCSmokeOracle{
		smokeWindow: smokeWindow,
		now:         now,
		state:       make(map[string]map[string]time.Time),
		firstFrame:  make(map[string]time.Time),
		evaluated:   make(map[string]bool),
	}
}

// Ingest records that a frame of the given kind arrived for the given worker
// at the given wallclock instant. Idempotent for repeat (worker, kind) pairs:
// only the first arrival is recorded.
func (o *IPCSmokeOracle) Ingest(workerID, kind string, at time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.firstFrame[workerID]; !ok {
		o.firstFrame[workerID] = at
	}
	kinds, ok := o.state[workerID]
	if !ok {
		kinds = make(map[string]time.Time)
		o.state[workerID] = kinds
	}
	if _, seen := kinds[kind]; !seen {
		kinds[kind] = at
	}
}

// Check evaluates the smoke window for every known worker. Workers whose
// smoke window has already closed (now-firstFrame >= smokeWindow) get their
// missing kinds counted into the violation counter; subsequent Check calls
// won't double-count thanks to evaluated[workerID]. Call this periodically
// from a background goroutine and once at shutdown.
func (o *IPCSmokeOracle) Check() {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.now()
	for wid, first := range o.firstFrame {
		if o.evaluated[wid] {
			continue
		}
		if now.Sub(first) < o.smokeWindow {
			continue
		}
		o.evaluated[wid] = true
		kinds := o.state[wid]
		for _, k := range smokeKinds {
			if _, ok := kinds[k]; !ok {
				log.Printf("[IPCSmokeOracle] VIOLATION worker=%s missing_kind=%s window=%v", wid, k, o.smokeWindow)
				o.violations.Add(1)
			}
		}
	}
}

// Violations returns the cumulative ipc_smoke_violation count.
func (o *IPCSmokeOracle) Violations() int64 {
	return o.violations.Load()
}

// WorkersSeen returns the number of distinct workers for which at least one
// IPC frame has been ingested. Used by the orchestrator to detect the
// "zero frames from any worker" failure mode that the per-worker smoke
// check cannot, by construction, catch.
func (o *IPCSmokeOracle) WorkersSeen() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.firstFrame)
}

// FrameCounts returns the per-kind count of frames seen so far, summed
// across all workers. Useful for diagnostic logging.
func (o *IPCSmokeOracle) FrameCounts() map[string]int {
	o.mu.Lock()
	defer o.mu.Unlock()
	counts := make(map[string]int)
	for _, kinds := range o.state {
		for k := range kinds {
			counts[k]++
		}
	}

	return counts
}

// Run starts a background goroutine that calls Check on the given interval
// until ctx is done.
func (o *IPCSmokeOracle) Run(ctx interface{ Done() <-chan struct{} }, interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				// Final pass before exit so workers whose window just closed are counted.
				o.Check()
				return
			case <-ticker.C:
				o.Check()
			}
		}
	}()
}

// ---------------------------------------------------------------------------
// ProcessWorkerObserver — WorkerObserver shim for process-mode workers.
// ---------------------------------------------------------------------------

// ProcessWorkerObserver caches the latest state/leader/stable-id values
// emitted by a process-mode worker via the IPC stream. It satisfies
// WorkerObserver so LeaderUniquenessWatcher and StateReconcileWatcher work
// unchanged when registered into the GoroutineRegistry under the spawned
// worker's sim ID.
//
// All fields are read concurrently by oracle pollers and written by the IPC
// reader, so the integer fields use atomics and the string is guarded by an
// atomic.Pointer.
type ProcessWorkerObserver struct {
	state    atomic.Int32
	leader   atomic.Bool
	stableID atomic.Pointer[string]
	// suspended, when true, indicates the worker process is currently
	// SIGSTOP'd (Phase 7b long-pause chaos). The IPC stream is frozen, so
	// the cached IsLeader()/WorkerStateInt() values are stale.
	// LeaderUniquenessWatcher polls IsLeader() across all live workers; if
	// a frozen process is still reported as leader, a NEW leader elected
	// by the live quorum would produce a false-positive double_leader
	// observation. Hide the frozen worker from oracle polls by returning
	// suspendedStateValue / IsLeader=false while suspended.
	suspended atomic.Bool
}

// suspendedStateValue is returned by WorkerStateInt when the observer is
// marked suspended. It is intentionally a value that none of the existing
// state-machine watchers treat as either stable or shutdown so frozen
// workers are simply ignored. -1 lies outside the production state enum
// (0..9 per types/state.go) so future state additions cannot collide.
const suspendedStateValue = -1

// NewProcessWorkerObserver constructs an observer with zero values (state=0
// = StateInit, leader=false, stableID="").
func NewProcessWorkerObserver() *ProcessWorkerObserver {
	return &ProcessWorkerObserver{}
}

// IsLeader returns the most recent leader flag from the worker's IPC stream.
// Implements WorkerObserver.
func (o *ProcessWorkerObserver) IsLeader() bool {
	if o == nil {
		return false
	}
	if o.suspended.Load() {
		return false
	}
	return o.leader.Load()
}

// WorkerStateInt returns the most recent state int from the worker's IPC
// stream. Implements WorkerObserver.
func (o *ProcessWorkerObserver) WorkerStateInt() int {
	if o == nil {
		return 0
	}
	if o.suspended.Load() {
		return suspendedStateValue
	}
	return int(o.state.Load())
}

// SetSuspended toggles the observer's suspended flag. While suspended,
// IsLeader returns false and WorkerStateInt returns suspendedStateValue so
// registry-polling oracles ignore the frozen worker. Phase 7b drives this
// from the SIGSTOP / SIGCONT chaos handlers.
func (o *ProcessWorkerObserver) SetSuspended(s bool) {
	if o == nil {
		return
	}
	o.suspended.Store(s)
}

// StableWorkerID returns the most recent stable worker ID claimed by the
// worker's manager (carried in every IPC frame's envelope), or "" if the
// worker has not yet claimed an ID. Implements WorkerObserver.
func (o *ProcessWorkerObserver) StableWorkerID() string {
	if o == nil {
		return ""
	}
	p := o.stableID.Load()
	if p == nil {
		return ""
	}

	return *p
}

// SetState records a new state observation.
func (o *ProcessWorkerObserver) SetState(state int) {
	o.state.Store(int32(state)) //nolint:gosec // bounded parti.State enum (0..9)
}

// SetLeader records a new leader observation.
func (o *ProcessWorkerObserver) SetLeader(isLeader bool) {
	o.leader.Store(isLeader)
}

// SetStableID records the worker's stable ID once it is known.
func (o *ProcessWorkerObserver) SetStableID(id string) {
	if id == "" {
		return
	}
	o.stableID.Store(&id)
}
