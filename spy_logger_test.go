package parti

import (
	"strings"
	"sync"

	"github.com/arloliu/parti/v2/types"
)

// warnSpy is a minimal types.Logger that records only WARN-level message
// strings into a mutex-guarded slice; all other levels are dropped. It is
// the single shared spy used by every package-parti test that needs to
// assert on emitted warnings (emission count and substring content) without
// pulling in the heavier integration-shaped capture loggers.
//
// Each test owns its own *warnSpy instance, so a single shared type does not
// couple tests to one another (it merely removes the byte-for-byte-duplicated
// definitions that previously lived per-file).
type warnSpy struct {
	mu    sync.Mutex
	warns []string
}

var _ types.Logger = (*warnSpy)(nil)

func (l *warnSpy) Debug(string, ...any) {}
func (l *warnSpy) Info(string, ...any)  {}
func (l *warnSpy) Warn(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg)
}
func (l *warnSpy) Error(string, ...any) {}
func (l *warnSpy) Fatal(string, ...any) {}

// snapshot returns a copy of the recorded WARN messages so the caller can
// assert on count and substring content without exposing the internal slice
// or holding the lock externally.
func (l *warnSpy) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.warns))
	copy(out, l.warns)

	return out
}

// contains reports whether any recorded WARN message contains substr.
func (l *warnSpy) contains(substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range l.warns {
		if strings.Contains(m, substr) {
			return true
		}
	}

	return false
}

// countContaining returns how many recorded WARN messages contain substr.
func (l *warnSpy) countContaining(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, m := range l.warns {
		if strings.Contains(m, substr) {
			n++
		}
	}

	return n
}
