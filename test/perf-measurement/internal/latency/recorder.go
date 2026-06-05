// Package latency records end-to-end delivery latency into per-worker HDR
// histograms and merges them for percentile export (design §6). Latency is
// recv_mono − intended_mono; only messages whose intended time falls in the
// capture window are recorded.
package latency

import (
	"context"
	"sync"
	"sync/atomic"

	hdr "github.com/HdrHistogram/hdrhistogram-go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/arloliu/parti/test/perf-measurement/internal/load"
)

const (
	minLatencyNs = 1_000          // 1µs floor
	maxLatencyNs = 60_000_000_000 // 60s ceiling
	sigFigs      = 3
)

// Recorder implements consumer.MessageHandler. One Recorder per worker, but
// parti's Dynamic consumer dispatches the handler from ONE GOROUTINE PER
// PARTITION (internal/durable/worker_consumer.go: `go pc.Run(...)`), so a
// worker's Recorder is called concurrently across its partitions. `mu` guards
// the histogram + count (hdr.Histogram is not thread-safe). Contention is
// negligible at these light rates. Window bounds are atomics so SetWindow can
// arm the window at captureStart concurrently with in-flight Handle calls.
type Recorder struct {
	windowStartMono atomic.Int64
	windowEndMono   atomic.Int64
	mu              sync.Mutex
	hist            *hdr.Histogram
	count           int64
}

// NewRecorder builds a Recorder. The window is initially unset (0,0) so the
// recorder records NOTHING until SetWindow is called — this is correct: the
// harness constructs recorders at worker-spawn time (before warmup) and only
// arms the window at captureStart, so warmup traffic is never recorded.
func NewRecorder() *Recorder {
	return &Recorder{hist: hdr.New(minLatencyNs, maxLatencyNs, sigFigs)}
}

// SetWindow arms the capture window [startMono, endMono] (CLOCK_MONOTONIC ns).
// Called once at captureStart.
func (r *Recorder) SetWindow(startMono, endMono int64) {
	r.windowStartMono.Store(startMono)
	r.windowEndMono.Store(endMono)
}

// Handle records recv−intended for in-window messages, then returns nil so
// the default auto-ack path acks exactly once (plan header: ManualAck=false).
// It NEVER calls msg.Ack() — that would double-ack and pollute IOPS (§6).
// Safe for concurrent use (per-partition goroutines share one Recorder).
func (r *Recorder) Handle(_ context.Context, msg jetstream.Msg) error {
	recv := load.MonoNanos()
	m, err := load.Decode(msg.Data())
	if err != nil {
		return nil // malformed payload: ack and skip, do not block delivery
	}
	ws, we := r.windowStartMono.Load(), r.windowEndMono.Load()
	if we <= ws || m.IntendedMonoNs < ws || m.IntendedMonoNs > we {
		return nil // window unset or message outside it
	}
	lat := recv - m.IntendedMonoNs
	if lat < minLatencyNs {
		lat = minLatencyNs
	}
	if lat > maxLatencyNs {
		lat = maxLatencyNs
	}
	r.mu.Lock()
	_ = r.hist.RecordValue(lat)
	r.count++
	r.mu.Unlock()

	return nil
}

// Count returns the number of in-window messages recorded (lock-guarded).
func (r *Recorder) Count() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// snapshotInto merges this recorder's histogram into dst under the lock, and
// returns the count. Used by MergeRecorders so end-of-run merging is also
// race-free (defensive — by merge time the handlers are drained).
func (r *Recorder) snapshotInto(dst *hdr.Histogram) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	dst.Merge(r.hist)
	return r.count
}

// Histogram exposes the underlying histogram WITHOUT locking. Use only from
// tests or after all delivery has drained (never concurrently with Handle).
func (r *Recorder) Histogram() *hdr.Histogram { return r.hist }
