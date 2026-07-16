package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestReadyTracker_NotReadyInitially locks the zero-value contract: a
// freshly constructed tracker reports not-ready until SetReady is called.
func TestReadyTracker_NotReadyInitially(t *testing.T) {
	rt := NewReadyTracker()
	require.False(t, rt.IsReady())
}

// TestReadyTracker_BecomesReady covers the one-shot flip.
func TestReadyTracker_BecomesReady(t *testing.T) {
	rt := NewReadyTracker()
	rt.SetReady()
	require.True(t, rt.IsReady())
}

// TestReadyHandler_ReportsState exercises the /ready HTTP handler's own
// branching: 503 before SetReady, 200 after.
func TestReadyHandler_ReportsState(t *testing.T) {
	rt := NewReadyTracker()
	h := readyHandler(rt)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)

	rt.SetReady()
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	require.Equal(t, http.StatusOK, rec.Code)
}

// TestStartReadyListener_ServesReadyOverHTTP is an end-to-end check of
// the actual listener (not just the handler in isolation): a real HTTP
// GET against the bound address must observe the 503->200 transition,
// matching exactly what run-matrix.sh's wait_for_ready polls for. Binds
// an explicit loopback port (rather than ":0") so the test can make
// real HTTP requests without needing to recover an ephemeral port from
// the net.Listener, which StartReadyListener does not expose.
func TestStartReadyListener_ServesReadyOverHTTP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rt := NewReadyTracker()
	const addr = "127.0.0.1:16061" // arbitrary unused port for the test
	srv, err := StartReadyListener(ctx, addr, rt)
	require.NoError(t, err)
	defer shutdownReadyListener(srv)

	url := "http://" + addr + "/ready"

	get := func() int {
		resp, err := http.Get(url) //nolint:gosec,noctx // test-only, fixed loopback URL
		require.NoError(t, err)
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	require.Eventually(t, func() bool {
		return get() == http.StatusServiceUnavailable
	}, 2*time.Second, 10*time.Millisecond, "listener must serve 503 before SetReady")

	rt.SetReady()

	require.Eventually(t, func() bool {
		return get() == http.StatusOK
	}, 2*time.Second, 10*time.Millisecond, "listener must serve 200 after SetReady")
}
