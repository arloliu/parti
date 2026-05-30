package durable

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// captureLogger is a minimal types.Logger that records messages so a test can
// assert a specific line was (or was not) emitted.
type captureLogger struct {
	mu   sync.Mutex
	msgs []string
}

var _ types.Logger = (*captureLogger)(nil)

func (c *captureLogger) record(msg string) {
	c.mu.Lock()
	c.msgs = append(c.msgs, msg)
	c.mu.Unlock()
}

func (c *captureLogger) Debug(msg string, _ ...any) { c.record(msg) }
func (c *captureLogger) Info(msg string, _ ...any)  { c.record(msg) }
func (c *captureLogger) Warn(msg string, _ ...any)  { c.record(msg) }
func (c *captureLogger) Error(msg string, _ ...any) { c.record(msg) }
func (c *captureLogger) Fatal(msg string, _ ...any) { c.record(msg) }

func (c *captureLogger) has(substr string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.msgs {
		if strings.Contains(m, substr) {
			return true
		}
	}

	return false
}

// TestReconcile_LogsUnreadableKeys pins F-D2c: a reconcile pass that lists keys
// but fails to Get some of them must surface that read failure (previously the
// per-key Get error was silently swallowed). One aggregated line per pass.
//
// Non-vacuous: the healthy control below runs the same reconcile without the
// read fault and asserts the line is NOT emitted.
func TestReconcile_LogsUnreadableKeys(t *testing.T) {
	kv := newHealthyKV(t)
	kv.getErrByKey = map[string]error{quorumTestFullKey: context.DeadlineExceeded}
	cl := &captureLogger{}
	r := NewClaimBasedResolver(kv, "claims/", cl, WithReconcileInterval(0))
	seedResolverCache(r)

	r.reconcileOnce(context.Background())

	require.True(t, cl.has("unreadable"),
		"reconcile must surface a listed-but-unreadable read failure (F-D2c)")
}

// TestReconcile_NoUnreadableLogWhenHealthy is the F-D2c control: a healthy
// reconcile (all Gets succeed) must not emit the unreadable-keys line.
func TestReconcile_NoUnreadableLogWhenHealthy(t *testing.T) {
	kv := newHealthyKV(t)
	cl := &captureLogger{}
	r := NewClaimBasedResolver(kv, "claims/", cl, WithReconcileInterval(0))
	seedResolverCache(r)

	r.reconcileOnce(context.Background())

	require.False(t, cl.has("unreadable"),
		"a healthy reconcile must not emit an unreadable-keys read-failure log")
}
