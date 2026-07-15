// Profiling plumbing for the harness process: a net/http/pprof debug
// listener plus a /leader endpoint so a capture script can attribute a
// captured profile to the in-process worker that held parti leadership
// at capture time.
//
// Topology note: every harness invocation is a single OS process
// hosting all --workers parti.Manager instances for one run (see
// scripts/run-matrix.sh et al. — exactly one $HARNESS_PID at a time,
// never run concurrently with a sibling harness process). All workers
// in that process belong to the same parti cluster/election, so at
// most one of them holds leadership at any instant. That collapses the
// general "one listener per manager group, addressed by base port +
// group index" scheme to the simplest case: one listener for the one
// group the process hosts, at a single configured address.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"sync/atomic"
	"time"
)

// LeaderTracker records which in-process worker (by index) currently
// holds parti leadership. It is fed by each worker's OnLeadershipChanged
// hook (see StartWorker in harness.go) and read by the /leader HTTP
// handler registered by StartPprofListener.
//
// Because every worker in one harness process is a member of the same
// parti cluster, at most one worker holds leadership at a time. Record
// only clears the tracked leader when the reporting worker is still the
// one currently attributed as leader (compare-and-swap on the worker
// index), so a stale "lost leadership" callback from a former leader
// can never clobber a newer leader's claim raced in from another
// worker's goroutine.
type LeaderTracker struct {
	idx atomic.Int64 // worker index of the current leader; -1 = none
}

// noLeader is the sentinel LeaderTracker.idx value meaning "no in-process
// worker currently holds leadership" (e.g. mid-election).
const noLeader = -1

// NewLeaderTracker returns a LeaderTracker with no recorded leader.
func NewLeaderTracker() *LeaderTracker {
	lt := &LeaderTracker{}
	lt.idx.Store(noLeader)

	return lt
}

// Record updates the tracker from worker workerIdx's OnLeadershipChanged
// callback. isLeader=true always claims leadership for workerIdx;
// isLeader=false only clears the tracker if workerIdx is still the
// recorded leader (see type doc for why).
func (lt *LeaderTracker) Record(workerIdx int, isLeader bool) {
	if isLeader {
		lt.idx.Store(int64(workerIdx))
		return
	}
	lt.idx.CompareAndSwap(int64(workerIdx), noLeader)
}

// Current returns the worker index currently believed to hold
// leadership, and false if no in-process worker currently holds it.
func (lt *LeaderTracker) Current() (idx int, ok bool) {
	v := lt.idx.Load()
	if v == noLeader {
		return 0, false
	}

	return int(v), true
}

// leaderResponse is the JSON body served by /leader.
type leaderResponse struct {
	// Leader is true iff an in-process worker currently holds parti
	// leadership.
	Leader bool `json:"leader"`
	// WorkerIdx is the leading worker's index (matches the harness
	// "worker_idx" column in rpc_counts.csv). Only meaningful when
	// Leader is true — worker indices are 0-based, so this field is
	// NOT tagged omitempty (worker 0 is a legitimate leader and must
	// not be indistinguishable from an absent field).
	WorkerIdx int `json:"workerIdx"`
}

// leaderHandler serves the current leadership snapshot from lt: 200 with
// {"leader":true,"workerIdx":N} when this process holds leadership
// through worker N, 404 with {"leader":false} otherwise (e.g. between
// election rounds).
func leaderHandler(lt *LeaderTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		idx, ok := lt.Current()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(leaderResponse{Leader: false})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(leaderResponse{Leader: true, WorkerIdx: idx})
	}
}

// StartPprofListener starts an HTTP server bound to addr exposing the
// standard net/http/pprof debug endpoints (/debug/pprof/...) plus
// /leader (see leaderHandler), fed by lt. Handlers are registered on a
// dedicated ServeMux rather than http.DefaultServeMux to avoid the
// process-wide side effects of importing net/http/pprof for its init()
// registration.
//
// addr should be a localhost or rig-network-only address (e.g.
// "127.0.0.1:6060") — this listener has no authentication and exposes
// full CPU/heap/goroutine profiling and stack traces of the process.
//
// The returned *http.Server is already serving in a background
// goroutine; the caller is responsible for closing it (e.g. via
// defer pprofSrv.Close()) when the harness run ends.
func StartPprofListener(ctx context.Context, addr string, lt *LeaderTracker) (*http.Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("/leader", leaderHandler(lt))

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %q: %w", addr, err)
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		// ErrServerClosed is the expected return from Close(); nothing
		// useful to do with any other error from a background debug
		// listener, so it is intentionally dropped (matches the
		// harness's other best-effort background goroutines, e.g. the
		// load producer).
		_ = srv.Serve(ln)
	}()

	return srv, nil
}

// shutdownPprofListener gives an in-flight pprof request (e.g. a
// multi-second CPU profile) a bounded grace period to finish instead of
// Close()'s hard cut, then gives up. It intentionally uses its own
// timeout rather than the caller's context: on SIGINT that context is
// already cancelled, and we still want the grace window to apply.
// Best-effort: any Shutdown error is dropped, matching the harness's
// other best-effort teardown paths (e.g. setupConn.Close()).
func shutdownPprofListener(srv *http.Server) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
