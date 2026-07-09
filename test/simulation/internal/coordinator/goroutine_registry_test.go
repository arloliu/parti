package coordinator

import (
	"context"
	"testing"
)

// markerObj is a distinct heap object used to stand in for two successive
// *worker.Worker incarnations registered under the same id.
type markerObj struct{ n int }

// TestGoroutineRegistry_MarkInactiveObj_GuardsAgainstStaleClobber reproduces
// the crash+restart registry clobber that produced the label_tight_takeover_churn
// flakiness: a crashed worker's deferred cleanup deactivating the fresh
// successor that Restart already re-registered and re-activated. It asserts the
// incarnation-guarded MarkInactiveObj leaves the live successor active, while an
// unconditional MarkInactive would (and here does) clobber it.
func TestGoroutineRegistry_MarkInactiveObj_GuardsAgainstStaleClobber(t *testing.T) {
	reg := NewGoroutineRegistry()
	v1 := &markerObj{1}
	v2 := &markerObj{2}
	noop := func(context.Context) {}

	// Initial incarnation.
	reg.Register("worker-0", WorkerGoroutine, func() {}, noop, v1)

	// Chaos crash: mark inactive, then Restart re-registers a fresh incarnation
	// and re-activates it (mirrors handleLeaderGoroutineFailure + Restart).
	reg.MarkInactive("worker-0")
	reg.Register("worker-0", WorkerGoroutine, func() {}, noop, v2)

	// The OLD (v1) goroutine's deferred cleanup now fires. With the guarded
	// call it must be a no-op because the registered object is the successor v2.
	reg.MarkInactiveObj("worker-0", v1)

	live := reg.GetByType(WorkerGoroutine)
	if len(live) != 1 {
		t.Fatalf("stale v1 cleanup clobbered the live successor: got %d active workers, want 1", len(live))
	}
	if live[0].Obj != any(v2) {
		t.Fatalf("active worker Obj = %v, want the successor v2", live[0].Obj)
	}

	// The current incarnation can still deactivate itself on its own exit.
	reg.MarkInactiveObj("worker-0", v2)
	if got := reg.GetByType(WorkerGoroutine); len(got) != 0 {
		t.Fatalf("current incarnation failed to deactivate itself: %d active, want 0", len(got))
	}
}

// TestGoroutineRegistry_MarkInactive_UnconditionalClobbers documents the exact
// bug: the unconditional MarkInactive(id) that the worker goroutines used to
// call would deactivate the live successor after a restart. This is why the
// worker-goroutine cleanup path was switched to the guarded MarkInactiveObj;
// the chaos crash handler keeps using unconditional MarkInactive on purpose.
func TestGoroutineRegistry_MarkInactive_UnconditionalClobbers(t *testing.T) {
	reg := NewGoroutineRegistry()
	v1 := &markerObj{1}
	v2 := &markerObj{2}
	noop := func(context.Context) {}

	reg.Register("worker-0", WorkerGoroutine, func() {}, noop, v1)
	reg.MarkInactive("worker-0")
	reg.Register("worker-0", WorkerGoroutine, func() {}, noop, v2)

	// Unconditional deactivation clobbers the live successor v2.
	reg.MarkInactive("worker-0")
	if got := reg.GetByType(WorkerGoroutine); len(got) != 0 {
		t.Fatalf("expected the unconditional clobber to hide the live successor, got %d active", len(got))
	}
}
