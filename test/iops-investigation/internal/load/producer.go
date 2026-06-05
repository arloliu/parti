package load

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// PartitionSubject returns the data-stream subject for partition index i,
// matching parti's Dynamic subject template iops.rig.{{.PartitionID}} with
// PartitionID = "p-<i>" (see plan header "Verified API facts").
func PartitionSubject(i int) string { return fmt.Sprintf("iops.rig.p-%d", i) }

// intervalForRate converts an aggregate rate (msg/s) to the inter-send
// interval. Rate <= 0 returns 0 (caller treats as idle).
func intervalForRate(ratePerSec float64) time.Duration {
	if ratePerSec <= 0 {
		return 0
	}
	return time.Duration(float64(time.Second) / ratePerSec)
}

// ProducerConfig parameterises the open-loop producer (design §5).
type ProducerConfig struct {
	JS           jetstream.JetStream // dedicated producer connection (not a worker wrapper)
	N            int                 // partition count (round-robin target subjects)
	AggregateX   float64             // total msg/s across all partitions = k·M
	SkewP99Limit time.Duration       // producer-bound threshold (design §5: 5ms)
	LateLimit    time.Duration       // per-send "late" threshold (design §5: 10ms)
	LateFraction float64             // producer-bound if > this fraction late (design §5: 0.01)
}

// ProducerHealth summarises send-side timing for the producer-bound guard.
type ProducerHealth struct {
	Sent          int64
	InWindowSent  int64 // sends whose intended time falls in the capture window
	AsyncErrors   int64
	LateSends     int64 // sends with skew > LateLimit
	SkewP99Ns     int64 // P99 of (actual−intended) over the run
	ProducerBound bool  // any guard tripped (design §5)
}

// Producer fires sends on a fixed CLOCK_MONOTONIC schedule independent of
// consumer progress, embedding the intended (scheduled) send instant.
//
// Window note: the producer runs during BOTH warmup and capture (so
// steady-state traffic exists while IOPS is captured), but only sends whose
// intended time falls in [winStart,winEnd] are counted as InWindowSent. The
// delivery-deficit guard (§9) compares delivered against InWindowSent — NOT
// total Sent — because the latency recorder only records in-window messages.
type Producer struct {
	cfg      ProducerConfig
	sent     atomic.Int64
	inWindow atomic.Int64
	asyncEr  atomic.Int64
	late     atomic.Int64
	winStart atomic.Int64 // mono-ns; 0,0 ⇒ window unset (no in-window counting yet)
	winEnd   atomic.Int64
	skews    []int64 // actual−intended ns, guarded by mu
	mu       sync.Mutex
	done     chan struct{} // closed when Run returns (after futures drained)
}

// NewProducer constructs a Producer.
func NewProducer(cfg ProducerConfig) *Producer {
	return &Producer{cfg: cfg, done: make(chan struct{})}
}

// Wait blocks until Run has returned and all async publish futures have been
// drained. Call after cancelling Run's context and BEFORE Health(), so
// AsyncErrors is fully counted (a PubAckFuture only guarantees an ack once
// its Ok()/Err() channel fires, not at PublishAsync return).
func (p *Producer) Wait() { <-p.done }

// SetWindow sets the in-window accounting bounds (CLOCK_MONOTONIC ns). Called
// at captureStart by the harness, concurrently with Run, so the bounds are
// atomics. Until set, no send is counted in-window.
func (p *Producer) SetWindow(startMono, endMono int64) {
	p.winStart.Store(startMono)
	p.winEnd.Store(endMono)
}

// Run drives the open-loop schedule until ctx is cancelled. Sends fire at
// intended times base+i*interval (CLOCK_MONOTONIC); if the goroutine is
// behind schedule it fires immediately (catch-up) so lateness is captured
// in the skew, not hidden (design §5). Publishing is async so a slow ack
// never stalls the schedule.
func (p *Producer) Run(ctx context.Context) {
	defer close(p.done) // Wait() unblocks only after the drain defer below completes
	interval := intervalForRate(p.cfg.AggregateX)
	if interval <= 0 || p.cfg.N <= 0 {
		<-ctx.Done()
		return
	}
	base := MonoNanos()
	intervalNs := interval.Nanoseconds()

	// Drain async acks in the background to count errors without blocking
	// the schedule. Buffered so the scheduler rarely blocks at these rates.
	futs := make(chan jetstream.PubAckFuture, 4096)
	var drainWG sync.WaitGroup
	drainWG.Add(1)
	go func() {
		defer drainWG.Done()
		for f := range futs {
			select {
			case <-f.Ok():
			case <-f.Err():
				p.asyncEr.Add(1)
			}
		}
	}()
	// On any exit, stop accepting futures and wait for the drain goroutine so
	// AsyncErrors reflects every in-flight publish before Wait() returns. This
	// defer runs BEFORE `defer close(p.done)` (LIFO).
	defer func() {
		close(futs)
		drainWG.Wait()
	}()

	for i := int64(0); ; i++ {
		intended := base + i*intervalNs
		now := MonoNanos()
		if d := intended - now; d > 0 {
			t := time.NewTimer(time.Duration(d))
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		} else if ctx.Err() != nil {
			return
		}

		actual := MonoNanos()
		partIdx := int(i % int64(p.cfg.N))
		payload := Message{IntendedMonoNs: intended, Seq: i, PartitionIndex: int64(partIdx)}.Encode()

		fut, err := p.cfg.JS.PublishAsync(PartitionSubject(partIdx), payload)
		if err != nil {
			p.asyncEr.Add(1)
		} else {
			select {
			case futs <- fut:
			default:
				// Drain backlog full: count as async error rather than block.
				p.asyncEr.Add(1)
			}
		}
		p.sent.Add(1)
		if ws, we := p.winStart.Load(), p.winEnd.Load(); we > ws && intended >= ws && intended <= we {
			p.inWindow.Add(1)
		}
		skew := actual - intended
		if time.Duration(skew) > p.cfg.LateLimit {
			p.late.Add(1)
		}
		p.mu.Lock()
		p.skews = append(p.skews, skew)
		p.mu.Unlock()
	}
}

// Health computes the producer-bound verdict (design §5).
func (p *Producer) Health() ProducerHealth {
	p.mu.Lock()
	skews := append([]int64(nil), p.skews...)
	p.mu.Unlock()
	sent := p.sent.Load()
	h := ProducerHealth{
		Sent:         sent,
		InWindowSent: p.inWindow.Load(),
		AsyncErrors:  p.asyncEr.Load(),
		LateSends:    p.late.Load(),
		SkewP99Ns:    percentileInt64(skews, 0.99),
	}
	lateFrac := 0.0
	if sent > 0 {
		lateFrac = float64(h.LateSends) / float64(sent)
	}
	h.ProducerBound = h.AsyncErrors > 0 ||
		time.Duration(h.SkewP99Ns) > p.cfg.SkewP99Limit ||
		lateFrac > p.cfg.LateFraction

	return h
}

// percentileInt64 returns the p-quantile (0..1) of xs via nearest-rank.
// It sorts xs IN PLACE; callers pass a private copy (Health already snapshots
// p.skews under the mutex). Returns 0 for empty input.
func percentileInt64(xs []int64, p float64) int64 {
	if len(xs) == 0 {
		return 0
	}
	slices.Sort(xs)
	idx := int(p * float64(len(xs)-1))
	return xs[idx]
}
