package recovery

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestCheckpoint_SeedAndValue(t *testing.T) {
	cp := newCheckpoint(logging.NewNop())

	require.Equal(t, uint64(0), cp.Value())

	cp.Seed(42)
	require.Equal(t, uint64(42), cp.Value())

	// Seeding with a smaller value is a no-op.
	cp.Seed(10)
	require.Equal(t, uint64(42), cp.Value())

	// Seeding with a larger value updates.
	cp.Seed(100)
	require.Equal(t, uint64(100), cp.Value())
}

func TestCheckpoint_Advance(t *testing.T) {
	cp := newCheckpoint(logging.NewNop())

	cp.Advance(&mockMsg{seq: 50})
	require.Equal(t, uint64(50), cp.Value())

	// Smaller sequence is ignored.
	cp.Advance(&mockMsg{seq: 30})
	require.Equal(t, uint64(50), cp.Value())

	// Larger sequence advances.
	cp.Advance(&mockMsg{seq: 75})
	require.Equal(t, uint64(75), cp.Value())
}

func TestCheckpoint_AdvanceMetadataError(t *testing.T) {
	cp := newCheckpoint(logging.NewNop())
	cp.Seed(10)

	// advance with a msg that has no metadata (error) — should be a no-op.
	cp.Advance(&mockMsg{seq: 0})
	require.Equal(t, uint64(10), cp.Value())
}

// TestCheckpoint_Advance_Concurrent verifies that concurrent Advance calls
// never corrupt the atomic max: the final value must equal the highest seq seen.
func TestCheckpoint_Advance_Concurrent(t *testing.T) {
	cp := newCheckpoint(logging.NewNop())

	const goroutines = 50
	const msgsEach = 200

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := range goroutines {
		go func(base int) {
			defer wg.Done()
			for i := range msgsEach {
				seq := uint64(base*msgsEach + i + 1)
				cp.Advance(&mockMsg{seq: seq})
			}
		}(g)
	}

	wg.Wait()

	want := uint64(goroutines * msgsEach)
	require.Equal(t, want, cp.Value())
}

// --- mock ---

type mockMsg struct {
	seq          uint64
	ackErr       error
	doubleAckErr error
}

func (m *mockMsg) Data() []byte                     { return nil }
func (m *mockMsg) Ack() error                       { return m.ackErr }
func (m *mockMsg) DoubleAck(context.Context) error  { return m.doubleAckErr }
func (m *mockMsg) Nak() error                       { return nil }
func (m *mockMsg) NakWithDelay(time.Duration) error { return nil }
func (m *mockMsg) Term() error                      { return nil }
func (m *mockMsg) TermWithReason(string) error      { return nil }
func (m *mockMsg) InProgress() error                { return nil }
func (m *mockMsg) Subject() string                  { return "" }
func (m *mockMsg) Reply() string                    { return "" }
func (m *mockMsg) Headers() nats.Header             { return nil }
func (m *mockMsg) Metadata() (*jetstream.MsgMetadata, error) {
	if m.seq == 0 {
		return nil, errors.New("no metadata")
	}
	return &jetstream.MsgMetadata{Sequence: jetstream.SequencePair{Stream: m.seq}}, nil
}
