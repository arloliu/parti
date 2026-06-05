package latency

import (
	"context"
	"sync"
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/arloliu/parti/test/iops-investigation/internal/load"
)

// fakeMsg is a minimal jetstream.Msg carrying only Data(); the handler only
// reads Data and returns nil (auto-ack path, so Ack is never called here).
type fakeMsg struct {
	jetstream.Msg
	data []byte
}

func (f fakeMsg) Data() []byte { return f.data }

func TestRecorder_WindowGating(t *testing.T) {
	// Window [1000, 2000] mono-ns.
	r := NewRecorder()
	r.SetWindow(1000, 2000)
	mk := func(intended int64) fakeMsg {
		return fakeMsg{data: load.Message{IntendedMonoNs: intended, Seq: 1, PartitionIndex: 0}.Encode()}
	}
	// In-window message: recorded.
	_ = r.Handle(context.Background(), mk(1500))
	// Out-of-window (before): dropped.
	_ = r.Handle(context.Background(), mk(500))
	// Out-of-window (after): dropped.
	_ = r.Handle(context.Background(), mk(2500))

	if got := r.Count(); got != 1 {
		t.Fatalf("recorded count = %d, want 1", got)
	}
}

// TestRecorder_ConcurrentHandle pins the per-partition concurrency contract:
// parti dispatches the handler from one goroutine per partition, so one
// Recorder is called concurrently. Must be race-clean under `-race`.
func TestRecorder_ConcurrentHandle(t *testing.T) {
	r := NewRecorder()
	r.SetWindow(0, 1<<62)
	const goroutines, per = 8, 1000
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Go(func() {
			for i := 0; i < per; i++ {
				_ = r.Handle(context.Background(), fakeMsg{
					data: load.Message{IntendedMonoNs: 1, Seq: int64(i), PartitionIndex: 0}.Encode(),
				})
			}
		})
	}
	wg.Wait()
	if got := r.Count(); got != goroutines*per {
		t.Fatalf("count = %d, want %d", got, goroutines*per)
	}
}
