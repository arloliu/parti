// Readiness signalling for run-matrix.sh (Item 1: capture-window
// gating). Independent of the profiling plumbing in pprof.go — see
// StartReadyListener's godoc for why the two listeners are separate.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// ReadyTracker reports whether the harness cluster has reached its
// initial steady state — every worker StateStable per WaitStableAll
// (main.go Step 6), i.e. workers provisioned and assignments settled.
// It starts not-ready and is flipped exactly once, right before the
// warmup sleep begins.
//
// run-matrix.sh polls this (via StartReadyListener's /ready endpoint)
// before starting the external capture scripts (cgroup/iostat/jsz/
// node_exporter), so a capture window never starts while the cluster
// is still provisioning — a fixed wall-clock capture start landed
// inside provisioning at N>=2000, contaminating "idle steady-state"
// captures with provisioning churn (see RUNBOOK.md and run-matrix.sh's
// wait_for_ready).
type ReadyTracker struct {
	ready atomic.Bool
}

// NewReadyTracker returns a tracker in the not-ready state.
func NewReadyTracker() *ReadyTracker { return &ReadyTracker{} }

// SetReady flips the tracker to ready. Idempotent; safe to call at
// most once in normal operation (main.go calls it right after
// WaitStableAll succeeds).
func (rt *ReadyTracker) SetReady() { rt.ready.Store(true) }

// IsReady reports whether SetReady has been called.
func (rt *ReadyTracker) IsReady() bool { return rt.ready.Load() }

// readyHandler serves 200 "ready\n" once rt is ready, 503 "not
// ready\n" otherwise. Polled by run-matrix.sh's wait_for_ready.
func readyHandler(rt *ReadyTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if !rt.IsReady() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready\n"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	}
}

// StartReadyListener starts a minimal HTTP server bound to addr
// exposing only /ready (see readyHandler), fed by rt.
//
// This is intentionally a SEPARATE listener from StartPprofListener
// (pprof.go) rather than another handler bundled onto the same mux:
// readiness gating is needed on every run-matrix.sh run, while the
// pprof listener stays opt-in (--pprof-addr empty by default) for
// dedicated profiling sessions. Tying /ready to --pprof-addr would
// force an operator to always enable the pprof listener just to get
// readiness gating, or require run-matrix.sh to always pass
// --pprof-addr — conflating two independent concerns for no benefit,
// since a bare HTTP listener with a single trivial handler carries no
// measurable steady-state overhead of its own.
//
// addr should be a localhost or rig-network-only address (e.g.
// "127.0.0.1:6061" — chosen to avoid colliding with the NATS cluster's
// own host-mapped ports 4222/6060/8222+ in docker-compose.yaml). This
// listener has no authentication.
//
// The returned *http.Server is already serving in a background
// goroutine; the caller is responsible for closing it (e.g. via
// shutdownReadyListener) when the harness run ends.
func StartReadyListener(ctx context.Context, addr string, rt *ReadyTracker) (*http.Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ready", readyHandler(rt))

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
		// listener, so it is intentionally dropped (matches
		// StartPprofListener's identical background-goroutine pattern).
		_ = srv.Serve(ln)
	}()

	return srv, nil
}

// shutdownReadyListener mirrors shutdownPprofListener's bounded grace
// window (see that function's godoc for the rationale — SIGINT already
// cancels the caller's context, so this uses its own timeout instead).
func shutdownReadyListener(srv *http.Server) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
