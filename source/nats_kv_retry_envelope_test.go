package source

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// withWatcherBackoff temporarily shortens the package-level retry
// constants so the integration tests run in seconds, not the production
// 60s cycle. Restores the originals via t.Cleanup. The package vars are
// declared specifically for this test seam — production code MUST NOT
// mutate them.
//
// NOT safe for t.Parallel across multiple tests that touch these vars
// (a parallel test could observe mid-restore values). The tests that
// use this helper run serially.
func withWatcherBackoff(t *testing.T, base, maxBackoff time.Duration, maxAttempts int) {
	t.Helper()
	origBase, origMax, origMaxAttempts := watcherBaseBackoff, watcherMaxBackoff, watcherMaxAttempts
	watcherBaseBackoff = base
	watcherMaxBackoff = maxBackoff
	watcherMaxAttempts = maxAttempts
	t.Cleanup(func() {
		watcherBaseBackoff = origBase
		watcherMaxBackoff = origMax
		watcherMaxAttempts = origMaxAttempts
	})
}

// TestRestartWatcher_BoundedByEnvelope verifies the F2 envelope wires
// into restartWatcher correctly: under sustained failure, the goroutine
// exits after watcherMaxAttempts attempts and the onWatcherRetryExhausted
// callback fires exactly once. Without the bound the previous loop
// retried forever.
func TestRestartWatcher_BoundedByEnvelope(t *testing.T) {
	// NOT t.Parallel — the test mutates package-level retry vars via the
	// withWatcherBackoff seam.
	withWatcherBackoff(t, 5*time.Millisecond, 20*time.Millisecond, 4)

	var watchFnCalls atomic.Int64
	bucketGone := errors.New("simulated: bucket missing")

	var warnMu sync.Mutex
	var warnLines []string
	spy := &captureWarnLogger{
		mu:    &warnMu,
		warns: &warnLines,
	}

	s := &NatsKV{
		logger: spy,
	}
	s.watchFn = func(context.Context) (jetstream.KeyWatcher, error) {
		watchFnCalls.Add(1)
		return nil, bucketGone
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.restartWatcher(ctx)
	}()

	// With the test seam (5ms/20ms × 4 attempts) the envelope exhausts
	// in well under a second. Generous deadline so transient CI scheduling
	// jitter does not cause flakes.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("restartWatcher did not return promptly; the envelope bound is not in effect")
	}

	require.LessOrEqual(t, watchFnCalls.Load(), int64(watcherMaxAttempts),
		"watchFn must be called at most watcherMaxAttempts times under sustained failure")
	require.Equal(t, int64(watcherMaxAttempts), watchFnCalls.Load(),
		"watchFn should be called exactly watcherMaxAttempts times when every attempt fails")

	warnMu.Lock()
	defer warnMu.Unlock()
	var sawExhausted bool
	for _, w := range warnLines {
		if w == "source watcher restart attempt budget exhausted; relying on reconciler for recovery" {
			sawExhausted = true
			break
		}
	}
	require.True(t, sawExhausted,
		"onWatcherRetryExhausted must emit the named WARN line at exhaustion; got %v", warnLines)
}

// TestRestartWatcher_ContextCancelStopsCleanly verifies a cancelled
// context terminates the envelope without firing the permanent-failure
// callback (cancellation ≠ exhaustion).
func TestRestartWatcher_ContextCancelStopsCleanly(t *testing.T) {
	// Use a generous backoff so the test exercises the "cancel during
	// sleep" path (default 2s base is plenty); does not touch the
	// shared package vars so concurrent tests are unaffected.

	bucketGone := errors.New("simulated")

	var warnMu sync.Mutex
	var warnLines []string
	spy := &captureWarnLogger{
		mu:    &warnMu,
		warns: &warnLines,
	}

	s := &NatsKV{logger: spy}
	s.watchFn = func(context.Context) (jetstream.KeyWatcher, error) {
		return nil, bucketGone
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.restartWatcher(ctx)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("restartWatcher did not exit promptly after ctx cancel")
	}

	warnMu.Lock()
	defer warnMu.Unlock()
	for _, w := range warnLines {
		if w == "source watcher restart attempt budget exhausted; relying on reconciler for recovery" {
			t.Fatalf("context cancel must NOT fire the exhaustion WARN; got %q", w)
		}
	}
}

// captureWarnLogger records WARN messages so the bounded-envelope test
// can assert on the exhaustion line without coupling to the production
// logger.
type captureWarnLogger struct {
	mu    *sync.Mutex
	warns *[]string
}

func (l *captureWarnLogger) Debug(string, ...any) {}
func (l *captureWarnLogger) Info(string, ...any)  {}
func (l *captureWarnLogger) Warn(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	*l.warns = append(*l.warns, msg)
}
func (l *captureWarnLogger) Error(string, ...any) {}
func (l *captureWarnLogger) Fatal(string, ...any) {}
