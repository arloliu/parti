package load

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	natssrv "github.com/arloliu/parti/test/perf-measurement/internal/testnats"
)

func TestProducerRun_PublishesAtRate(t *testing.T) {
	url, shutdown := natssrv.Start(t) // embedded single-node JS server
	defer shutdown()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name: "perf-test", Subjects: []string{"perf.rig.>"}, Storage: jetstream.MemoryStorage,
	}); err != nil {
		t.Fatal(err)
	}

	p := NewProducer(ProducerConfig{
		JS: js, N: 4, AggregateX: 200,
		SkewP99Limit: 5 * time.Millisecond, LateLimit: 10 * time.Millisecond, LateFraction: 0.01,
	})
	runCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	p.Run(runCtx)

	h := p.Health()
	// ~200 msg/s for ~0.5s ⇒ ~100 sends; allow generous slack for CI.
	if h.Sent < 50 {
		t.Fatalf("expected >=50 sends, got %d", h.Sent)
	}
	if h.AsyncErrors != 0 {
		t.Fatalf("unexpected async errors: %d", h.AsyncErrors)
	}
	if h.ProducerBound {
		t.Fatalf("producer should not be bound at 200 msg/s: %+v", h)
	}
}

// TestProducerRun_ConcurrentWindowAndHealth exercises the concurrent design:
// Run drives the schedule in a goroutine while the test concurrently calls
// SetWindow (mirroring the harness at captureStart) and Health. Under -race
// this validates SetWindow's atomic stores against Run's window reads and the
// counter writes. Keep total wall-time well under ~2s.
func TestProducerRun_ConcurrentWindowAndHealth(t *testing.T) {
	url, shutdown := natssrv.Start(t) // embedded single-node JS server
	defer shutdown()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name: "perf-test", Subjects: []string{"perf.rig.>"}, Storage: jetstream.MemoryStorage,
	}); err != nil {
		t.Fatal(err)
	}

	p := NewProducer(ProducerConfig{
		JS: js, N: 4, AggregateX: 200,
		SkewP99Limit: 5 * time.Millisecond, LateLimit: 10 * time.Millisecond, LateFraction: 0.01,
	})

	runCtx, cancel := context.WithCancel(ctx)
	go p.Run(runCtx)

	// Let the schedule warm up, then set a window that overlaps the remaining
	// run so some sends land in-window. Concurrently poll Health to race
	// SetWindow's stores against Run's reads/counter writes.
	time.Sleep(100 * time.Millisecond)
	start := MonoNanos()
	end := start + (200 * time.Millisecond).Nanoseconds()
	p.SetWindow(start, end)

	deadline := time.After(300 * time.Millisecond)
poll:
	for {
		select {
		case <-deadline:
			break poll
		default:
			_ = p.Health()
			time.Sleep(5 * time.Millisecond)
		}
	}

	cancel()
	p.Wait()
	h := p.Health()

	if h.InWindowSent <= 0 {
		t.Fatalf("expected in-window sends > 0, got %d (health=%+v)", h.InWindowSent, h)
	}
	if h.AsyncErrors != 0 {
		t.Fatalf("unexpected async errors: %d", h.AsyncErrors)
	}
	if h.ProducerBound {
		t.Fatalf("producer should not be bound at 200 msg/s: %+v", h)
	}
}
