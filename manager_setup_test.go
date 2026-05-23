package parti

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

func TestManager_prepareStart(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		m := &Manager{
			cfg: Config{StartupTimeout: 5 * time.Second},
		}
		ctx, cancel, err := m.prepareStart(context.Background())
		require.NoError(t, err)
		require.NotNil(t, ctx)
		require.NotNil(t, cancel)
		defer cancel()

		// Verify context has timeout
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		require.WithinDuration(t, time.Now().Add(5*time.Second), deadline, 100*time.Millisecond)
	})

	t.Run("already started", func(t *testing.T) {
		m := &Manager{
			ctx: context.Background(), // Simulate started
		}
		ctx, cancel, err := m.prepareStart(context.Background())
		require.ErrorIs(t, err, types.ErrAlreadyStarted)
		require.Nil(t, ctx)
		require.NotNil(t, cancel) // Should return no-op cancel
		cancel()
	})

	t.Run("no timeout", func(t *testing.T) {
		m := &Manager{
			cfg: Config{StartupTimeout: 0},
		}
		ctx, cancel, err := m.prepareStart(context.Background())
		require.NoError(t, err)
		defer cancel()

		// Verify context has NO deadline (if parent doesn't)
		_, ok := ctx.Deadline()
		require.False(t, ok)
	})
}

// warnCaptureLogger records WARN-level message strings so the
// warnOnFiniteMaxReconnects test can assert on emission count and
// content without taking on the full captureLogger from
// pull_gating_repro_test.go (which is integration-shaped).
type warnCaptureLogger struct {
	mu    sync.Mutex
	warns []string
}

func (l *warnCaptureLogger) Debug(string, ...any) {}
func (l *warnCaptureLogger) Info(string, ...any)  {}
func (l *warnCaptureLogger) Warn(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg)
}
func (l *warnCaptureLogger) Error(string, ...any) {}
func (l *warnCaptureLogger) Fatal(string, ...any) {}

// snapshot returns the recorded WARN messages so the caller can
// assert on count and substring content without exposing the
// internal slice or holding the lock externally.
func (l *warnCaptureLogger) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.warns))
	copy(out, l.warns)

	return out
}

// TestManager_warnOnFiniteMaxReconnects covers the read-only startup
// warning that fires when the caller-owned nats.Conn is configured
// with a finite MaxReconnects. -1 (unlimited) is the recommended
// posture and must be silent. Anything else (including 0 = disabled
// and any positive cap) must emit the warning exactly once.
//
// Defensive cases: a nil m.js or a nil m.js.Conn() must NOT panic and
// must NOT emit a warning (the helper is read-only and must not
// constrain test doubles that bypass the real JetStream surface).
func TestManager_warnOnFiniteMaxReconnects(t *testing.T) {
	// The helper accesses ONLY conn.Opts.MaxReconnects, which is a
	// value field on nats.Options. A zero-valued *nats.Conn with only
	// Opts populated is therefore safe to construct directly for this
	// unit test — no embedded NATS server required.
	// nats.Options field name is MaxReconnect (singular). The nats.MaxReconnects
	// setter (plural) is the Option-constructor; the underlying field is singular.
	mkConn := func(maxReconnect int) *nats.Conn {
		return &nats.Conn{Opts: nats.Options{MaxReconnect: maxReconnect}}
	}

	const warnSubstr = "finite MaxReconnect"

	// assertWarnedAbout fails the test unless exactly one WARN line
	// containing warnSubstr was emitted. Inlined assertion sidesteps
	// the unparam lint warning that fires on a single-call helper
	// whose substring argument never varies.
	assertWarnedOnce := func(t *testing.T, log *warnCaptureLogger) {
		t.Helper()
		var matches int
		for _, w := range log.snapshot() {
			if strings.Contains(w, warnSubstr) {
				matches++
			}
		}
		require.Equal(t, 1, matches, "expected exactly one warning matching %q; got warns=%v", warnSubstr, log.snapshot())
	}
	assertSilent := func(t *testing.T, log *warnCaptureLogger) {
		t.Helper()
		var matches int
		for _, w := range log.snapshot() {
			if strings.Contains(w, warnSubstr) {
				matches++
			}
		}
		require.Equal(t, 0, matches, "expected no warnings; got warns=%v", log.snapshot())
	}

	t.Run("unlimited -1 silent (recommended posture)", func(t *testing.T) {
		log := &warnCaptureLogger{}
		warnOnFiniteMaxReconnects(mkConn(-1), log)
		assertSilent(t, log)
	})

	t.Run("zero (disabled reconnect) warns", func(t *testing.T) {
		log := &warnCaptureLogger{}
		warnOnFiniteMaxReconnects(mkConn(0), log)
		assertWarnedOnce(t, log)
	})

	t.Run("finite positive warns", func(t *testing.T) {
		log := &warnCaptureLogger{}
		warnOnFiniteMaxReconnects(mkConn(5), log)
		assertWarnedOnce(t, log)
	})

	t.Run("nil conn silent (defensive)", func(t *testing.T) {
		log := &warnCaptureLogger{}
		require.NotPanics(t, func() {
			warnOnFiniteMaxReconnects(nil, log)
		})
		assertSilent(t, log)
	})
}
