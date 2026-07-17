// handoffLogger is rig-only measurement plumbing for the post-hardening
// validation session (Session V brief). The harness normally builds every
// parti.Manager without a logger — parti defaults to a Nop logger (see
// manager.go's logger resolution) — so the coordinator's Debug-level
// "handoff_discontinuous_apply" event (internal/assignment/handoff/
// twophase.go, fields: worker_id, previous_version, next_version,
// partitions) is invisible by default. --handoff-log wires one shared
// handoffLogger into every worker's Manager options so that single event
// becomes observable without perturbing worker log volume otherwise.
package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/arloliu/parti/v2/types"
)

// handoffDiscontinuousApplyMsg is the exact Debug msg string emitted by
// the two-phase handoff coordinator when Apply fails open to a full
// prepare walk (see internal/assignment/handoff/twophase.go).
const handoffDiscontinuousApplyMsg = "handoff_discontinuous_apply"

// handoffLogger implements types.Logger. It forwards ONLY Debug calls
// whose msg equals handoffDiscontinuousApplyMsg to an append-only file
// (one line per event: RFC3339 timestamp, then the key=value pairs).
// Every other Debug call, and every Info/Warn/Error/Fatal call, is a
// no-op — worker log volume stays at zero so sharing one instance across
// every worker (including churn-re-added ones) never perturbs the
// measurement. Safe for concurrent use.
type handoffLogger struct {
	mu sync.Mutex
	f  *os.File
}

var _ types.Logger = (*handoffLogger)(nil)

// newHandoffLogger opens (creating if needed) an append-only file at
// path for handoffLogger to write into.
func newHandoffLogger(path string) (*handoffLogger, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open handoff log %q: %w", path, err)
	}

	return &handoffLogger{f: f}, nil
}

// Close closes the underlying file.
func (h *handoffLogger) Close() error {
	return h.f.Close()
}

// Debug forwards only handoff_discontinuous_apply events to the capture
// file; every other message is dropped.
func (h *handoffLogger) Debug(msg string, keysAndValues ...any) {
	if msg != handoffDiscontinuousApplyMsg {
		return
	}

	var b strings.Builder
	b.WriteString(time.Now().UTC().Format(time.RFC3339Nano))
	b.WriteByte(' ')
	b.WriteString(msg)
	for i := 0; i+1 < len(keysAndValues); i += 2 {
		fmt.Fprintf(&b, " %v=%v", keysAndValues[i], keysAndValues[i+1])
	}
	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, _ = h.f.WriteString(b.String())
}

// Info, Warn, Error, and Fatal are no-ops: capture is scoped to exactly
// one Debug event (see package doc above), and none of them is ever
// called by parti's runtime code paths for Fatal, so dropping it here is
// safe (nothing in the library relies on Fatal calling os.Exit).
func (h *handoffLogger) Info(string, ...any)  {}
func (h *handoffLogger) Warn(string, ...any)  {}
func (h *handoffLogger) Error(string, ...any) {}
func (h *handoffLogger) Fatal(string, ...any) {}
