package parti

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// opTimeoutSpy captures WARN messages for the F9-A companion warning.
type opTimeoutSpy struct {
	mu    sync.Mutex
	warns []string
}

func (s *opTimeoutSpy) Debug(string, ...any) {}
func (s *opTimeoutSpy) Info(string, ...any)  {}
func (s *opTimeoutSpy) Warn(msg string, _ ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.warns = append(s.warns, msg)
}
func (s *opTimeoutSpy) Error(string, ...any) {}
func (s *opTimeoutSpy) Fatal(string, ...any) {}

func (s *opTimeoutSpy) countContaining(substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, w := range s.warns {
		if strings.Contains(w, substr) {
			n++
		}
	}

	return n
}

var _ types.Logger = (*opTimeoutSpy)(nil)

// TestWarnOnOperationTimeoutVsElection covers the F9-A companion
// warning that fires when OperationTimeout > ElectionTimeout/3 — at
// that ratio a single slow renew can consume the lease's three-
// attempt budget and produce false leadership flips.
func TestWarnOnOperationTimeoutVsElection(t *testing.T) {
	t.Parallel()
	const warnSubstr = "OperationTimeout exceeds ElectionTimeout/3"

	cases := []struct {
		name        string
		opTimeout   time.Duration
		electionTO  time.Duration
		expectWarns int
	}{
		{"safe ratio (OT=1/6 ET)", 5 * time.Second, 30 * time.Second, 0},
		{"boundary OT=ET/3 (silent)", 10 * time.Second, 30 * time.Second, 0},
		{"OT just over ET/3 warns", 11 * time.Second, 30 * time.Second, 1},
		{"OT equals ET warns", 30 * time.Second, 30 * time.Second, 1},
		{"default pair (10s/10s) warns", 10 * time.Second, 10 * time.Second, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spy := &opTimeoutSpy{}
			warnOnOperationTimeoutVsElection(Config{
				OperationTimeout: tc.opTimeout,
				ElectionTimeout:  tc.electionTO,
			}, spy)
			require.Equal(t, tc.expectWarns, spy.countContaining(warnSubstr),
				"warn count mismatch for OperationTimeout=%v ElectionTimeout=%v",
				tc.opTimeout, tc.electionTO)
		})
	}
}
