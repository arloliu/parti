# Dynamic Partition-Consumer Performance Measurement — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend the `test/iops-investigation/` rig with an open-loop producer, end-to-end delivery-latency measurement, metacontroller-snapshot capture, CPU/disk isolation, and a cost model + estimator CLI, then run the matrix in `00-design.md` and publish `findings.md`.

**Architecture:** The idle IOPS rig (noopHandler, no publish) gains a `--load` mode. A new in-process open-loop producer publishes ~256 B messages at aggregate rate `X = k·M` carrying a host-wide `CLOCK_MONOTONIC` intended-send timestamp; a latency-recording handler replaces the noop and folds `recv−intended` into per-worker HDR histograms merged at the end. New `internal/load`, `internal/latency`, `internal/costmodel` packages and a `cmd/estimator` CLI are added under the rig's module. cgroup `cpuset` + a dedicated NVMe volume isolate NATS from the harness.

**Tech Stack:** Go 1.25 (module `github.com/arloliu/parti/test/iops-investigation`), NATS JetStream 2.12.6, `golang.org/x/sys/unix` (CLOCK_MONOTONIC), `github.com/HdrHistogram/hdrhistogram-go` (percentiles), Docker Compose, bash.

**Design reference:** `docs/plans/perf-measurement/00-design.md` (consensus-approved, codex round 4). Section numbers below (§N) refer to it.

**Verified API facts (do not re-derive):**
- Handler interface: `consumer.MessageHandler` — `Handle(ctx context.Context, msg jetstream.Msg) error` (`consumer/handler.go:11`). With `ManualAck=false` (default) a `nil` return auto-acks exactly once (`consumer/queue.go:476`, `internal/ipartition/key_dispatcher.go:242`). **The latency handler returns `nil` and never calls `msg.Ack()`.**
- Consumer options (all exist, accepted by both `NewDynamic`/`NewQueue` like the existing `WithFetchTimeout`): `WithBatchSize(int)`, `WithMaxWaiting(int)`, `WithMaxAckPending(int)`, `WithAckWait(time.Duration)`, `WithManualAck(bool)`, `WithFetchTimeout(time.Duration)`.
- `consumer.NewDynamic(js, stream, prefix, subjectTmpl, handler, opts...) (*consumer.Dynamic, error)`; driven by the manager via `WithWorkerConsumerUpdater` (no `Start`; has `Stop(ctx)`). `consumer.NewQueue(js, stream, name, filterSubj, handler, opts...) (*consumer.Queue, error)` has `Start(ctx)`/`Stop(ctx)`.
- Per-partition subject: template `iops.rig.{{.PartitionID}}` renders `PartitionID = partition.SubjectKey()` = `strings.Join(Keys,".")`. Partitions are seeded `{Keys:["p-<i>"]}` (`harness.go:378`), so **partition `i`'s subject is `iops.rig.p-<i>`**. The producer publishes to exactly these.
- HDR histogram is NOT yet a dependency. `golang.org/x/sys` already is (indirect) — promote to direct.
- Capture lifecycle (`cmd/harness/main.go`): connect setup → `WaitForJetStream` → `PreCreate` → `SeedPartitions` → `storageverify.Verify` → spawn workers (`StartWorker`) → `WaitStableAll` → sleep `--warmup` → `ResetAll` → capture loop streaming `rpc_counts.csv` → `WriteManifest`. The latency window aligns to the capture window (warmup=120s, capture-window=120s ⇒ t∈[120,240]s).

---

## Phase 0 — Dependencies & package scaffolding

### Task 0.1: Add HDR + promote x/sys to direct deps

**Files:**
- Modify: `test/iops-investigation/go.mod`
- Modify: `test/iops-investigation/go.sum` (via `go get`)

- [ ] **Step 1: Add the HDR histogram dependency**

Run (from repo root):
```bash
cd test/iops-investigation
go get github.com/HdrHistogram/hdrhistogram-go@v1.1.2
go get golang.org/x/sys/unix
```
Expected: `go.mod` now lists `github.com/HdrHistogram/hdrhistogram-go v1.1.2` and `golang.org/x/sys` as direct requires.

- [ ] **Step 2: Verify the module still builds**

Run: `cd test/iops-investigation && go build ./...`
Expected: exit 0, no output.

- [ ] **Step 3: Commit**

```bash
git add test/iops-investigation/go.mod test/iops-investigation/go.sum
git commit -m "build(perf): add hdrhistogram + promote x/sys to direct deps"
```

---

## Phase 1 — Monotonic clock + payload codec (`internal/load`)

### Task 1.1: Host-wide CLOCK_MONOTONIC reader

**Files:**
- Create: `test/iops-investigation/internal/load/clock.go`
- Test: `test/iops-investigation/internal/load/clock_test.go`

- [ ] **Step 1: Write the failing test**

```go
package load

import (
	"testing"
	"time"
)

func TestMonoNanos_Monotonic(t *testing.T) {
	a := MonoNanos()
	time.Sleep(2 * time.Millisecond)
	b := MonoNanos()
	if b <= a {
		t.Fatalf("expected monotonic increase, got a=%d b=%d", a, b)
	}
	if d := time.Duration(b - a); d < time.Millisecond || d > time.Second {
		t.Fatalf("implausible elapsed %v", d)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd test/iops-investigation && go test ./internal/load/ -run TestMonoNanos -v`
Expected: FAIL (undefined: MonoNanos).

- [ ] **Step 3: Implement**

```go
// Package load implements the open-loop producer and the host-wide
// monotonic clock used to stamp messages for end-to-end latency
// measurement (design §5). All timestamps are CLOCK_MONOTONIC nanoseconds,
// which on Linux are consistent across processes on one host and never
// step backward — see 00-design.md §5.
package load

import "golang.org/x/sys/unix"

// MonoNanos returns the current CLOCK_MONOTONIC reading in nanoseconds.
// Producer and consumers (in-process or the §8.2 out-of-process cell)
// read the same host-wide source, so recv-minus-intended is a valid
// latency even across process boundaries on the same machine.
func MonoNanos() int64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		// CLOCK_MONOTONIC is always available on Linux; a failure here is
		// catastrophic for the measurement. Fail loud rather than silently
		// poisoning every latency sample with a bogus zero.
		panic("load: CLOCK_MONOTONIC unavailable: " + err.Error())
	}
	return ts.Nano()
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd test/iops-investigation && go test ./internal/load/ -run TestMonoNanos -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add test/iops-investigation/internal/load/
git commit -m "feat(perf): host-wide CLOCK_MONOTONIC reader for latency stamping"
```

### Task 1.2: Fixed-size message codec

**Files:**
- Create: `test/iops-investigation/internal/load/message.go`
- Test: `test/iops-investigation/internal/load/message_test.go`

- [ ] **Step 1: Write the failing test**

```go
package load

import "testing"

func TestMessageRoundTrip(t *testing.T) {
	in := Message{IntendedMonoNs: 123456789, Seq: 42, PartitionIndex: 7}
	buf := in.Encode()
	if len(buf) != PayloadSize {
		t.Fatalf("payload size = %d, want %d", len(buf), PayloadSize)
	}
	out, err := Decode(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch: %+v != %+v", out, in)
	}
}

func TestDecodeRejectsShort(t *testing.T) {
	if _, err := Decode([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected error on short buffer")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd test/iops-investigation && go test ./internal/load/ -run TestMessage -v && go test ./internal/load/ -run TestDecode -v`
Expected: FAIL (undefined: Message/Encode/Decode/PayloadSize).

- [ ] **Step 3: Implement**

```go
package load

import (
	"encoding/binary"
	"fmt"
)

// PayloadSize is the fixed wire size of every message (~256 B "small
// message", design §5). A 24-byte header carries the fields; the rest is
// zero padding so payload size is constant across the matrix.
const PayloadSize = 256

const headerSize = 24 // 3 × int64

// Message is the producer payload. IntendedMonoNs is the SCHEDULED send
// instant (CLOCK_MONOTONIC ns), not the actual publish instant — latency
// is recv−intended so producer lateness is captured (coordinated-omission
// correction, design §5).
type Message struct {
	IntendedMonoNs int64
	Seq            int64
	PartitionIndex int64
}

// Encode serialises m into a PayloadSize byte slice (little-endian header
// + zero padding).
func (m Message) Encode() []byte {
	b := make([]byte, PayloadSize)
	binary.LittleEndian.PutUint64(b[0:8], uint64(m.IntendedMonoNs))
	binary.LittleEndian.PutUint64(b[8:16], uint64(m.Seq))
	binary.LittleEndian.PutUint64(b[16:24], uint64(m.PartitionIndex))
	return b
}

// Decode parses the header from a payload. Padding is ignored.
func Decode(b []byte) (Message, error) {
	if len(b) < headerSize {
		return Message{}, fmt.Errorf("payload too short: %d < %d", len(b), headerSize)
	}
	return Message{
		IntendedMonoNs: int64(binary.LittleEndian.Uint64(b[0:8])),
		Seq:            int64(binary.LittleEndian.Uint64(b[8:16])),
		PartitionIndex: int64(binary.LittleEndian.Uint64(b[16:24])),
	}, nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd test/iops-investigation && go test ./internal/load/ -v`
Expected: PASS (all load tests).

- [ ] **Step 5: Commit**

```bash
git add test/iops-investigation/internal/load/
git commit -m "feat(perf): fixed-256B message codec with intended-mono timestamp"
```

---

## Phase 2 — Open-loop producer (`internal/load`)

### Task 2.1: Producer options + subject builder

**Files:**
- Create: `test/iops-investigation/internal/load/producer.go`
- Test: `test/iops-investigation/internal/load/producer_test.go`

- [ ] **Step 1: Write the failing test (subject + interval math, pure)**

```go
package load

import (
	"testing"
	"time"
)

func TestPartitionSubject(t *testing.T) {
	if got := PartitionSubject(0); got != "iops.rig.p-0" {
		t.Fatalf("got %q", got)
	}
	if got := PartitionSubject(4999); got != "iops.rig.p-4999" {
		t.Fatalf("got %q", got)
	}
}

func TestIntervalForRate(t *testing.T) {
	// X = 100 msg/s ⇒ 10ms interval.
	if got := intervalForRate(100); got != 10*time.Millisecond {
		t.Fatalf("got %v", got)
	}
	// X <= 0 ⇒ zero interval sentinel (idle).
	if got := intervalForRate(0); got != 0 {
		t.Fatalf("got %v", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd test/iops-investigation && go test ./internal/load/ -run 'TestPartitionSubject|TestIntervalForRate' -v`
Expected: FAIL (undefined).

- [ ] **Step 3: Implement the pure helpers + the Producer skeleton**

```go
package load

import (
	"context"
	"fmt"
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
	InWindowSent  int64   // sends whose intended time falls in the capture window
	AsyncErrors   int64
	LateSends     int64   // sends with skew > LateLimit
	SkewP99Ns     int64   // P99 of (actual−intended) over the run
	ProducerBound bool    // any guard tripped (design §5)
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
	cfg       ProducerConfig
	sent      atomic.Int64
	inWindow  atomic.Int64
	asyncEr   atomic.Int64
	late      atomic.Int64
	winStart  atomic.Int64 // mono-ns; 0,0 ⇒ window unset (no in-window counting yet)
	winEnd    atomic.Int64
	skews     []int64 // actual−intended ns, guarded by mu
	mu        sync.Mutex
	done      chan struct{} // closed when Run returns (after futures drained)
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
```

- [ ] **Step 4: Run to verify the pure tests pass**

Run: `cd test/iops-investigation && go test ./internal/load/ -run 'TestPartitionSubject|TestIntervalForRate' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add test/iops-investigation/internal/load/
git commit -m "feat(perf): producer config, subject builder, interval math"
```

### Task 2.2: Producer run loop + health

**Files:**
- Modify: `test/iops-investigation/internal/load/producer.go`
- Test: `test/iops-investigation/internal/load/producer_run_test.go`

- [ ] **Step 1: Write the failing test (drive against a NATS test server)**

```go
package load

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	natssrv "github.com/arloliu/parti/test/iops-investigation/internal/testnats" // see Step 3 note
)

func TestProducerRun_PublishesAtRate(t *testing.T) {
	url, shutdown := natssrv.Start(t) // embedded single-node JS server
	defer shutdown()
	nc, err := nats.Connect(url)
	if err != nil { t.Fatal(err) }
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil { t.Fatal(err) }

	ctx := context.Background()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name: "perf-test", Subjects: []string{"iops.rig.>"}, Storage: jetstream.MemoryStorage,
	}); err != nil { t.Fatal(err) }

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
```

> Step 3 note: a tiny embedded-server helper is needed for the test. The rig already vendors `github.com/nats-io/nats-server/v2`. Create `internal/testnats/testnats.go` exposing `Start(t) (url string, shutdown func())` that runs `server.NewServer` with `JetStream:true, StoreDir:t.TempDir()` and waits `ReadyForConnections`. (Mirror the pattern in `cmd/harness/e2e_smoke_test.go`, which already starts an embedded server — copy its setup verbatim into the helper.)

- [ ] **Step 2: Create the embedded-server helper, then run to verify the test fails**

First inspect the existing embedded-server bring-up:
Run: `sed -n '1,80p' test/iops-investigation/cmd/harness/e2e_smoke_test.go`
Then create `internal/testnats/testnats.go` with the same `server.Options{JetStream:true, StoreDir:t.TempDir(), Port:-1}` + `natsserver.RunServer` + `srv.ReadyForConnections(10*time.Second)` pattern, returning `srv.ClientURL()` and `srv.Shutdown`.

Run: `cd test/iops-investigation && go test ./internal/load/ -run TestProducerRun -v`
Expected: FAIL (undefined: p.Run / p.Health).

- [ ] **Step 3: Implement Run + Health**

Append to `producer.go`:
```go
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

// percentileInt64 returns the p-quantile (0..1) of xs via nearest-rank on a
// sorted copy. Returns 0 for empty input.
func percentileInt64(xs []int64, p float64) int64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int64(nil), xs...)
	sortInt64(s)
	idx := int(p * float64(len(s)-1))
	return s[idx]
}

func sortInt64(s []int64) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
```

> Note: `sortInt64` is an insertion sort kept dependency-free; replace with `slices.Sort` if preferred. `percentileInt64` is reused by `internal/latency` only via copy — keep it unexported here.

- [ ] **Step 4: Run to verify it passes**

Run: `cd test/iops-investigation && go test ./internal/load/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add test/iops-investigation/internal/load/ test/iops-investigation/internal/testnats/
git commit -m "feat(perf): open-loop async producer with coordinated-omission skew guard"
```

---

## Phase 3 — Latency handler + HDR (`internal/latency`)

### Task 3.1: Per-worker recorder + window gating + handler

**Files:**
- Create: `test/iops-investigation/internal/latency/recorder.go`
- Test: `test/iops-investigation/internal/latency/recorder_test.go`

- [ ] **Step 1: Write the failing test**

```go
package latency

import (
	"context"
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
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				_ = r.Handle(context.Background(), fakeMsg{
					data: load.Message{IntendedMonoNs: 1, Seq: int64(i), PartitionIndex: 0}.Encode(),
				})
			}
		}()
	}
	wg.Wait()
	if got := r.Count(); got != goroutines*per {
		t.Fatalf("count = %d, want %d", got, goroutines*per)
	}
}
```

> Add `"sync"` to the test imports.

- [ ] **Step 2: Run to verify it fails**

Run: `cd test/iops-investigation && go test -race ./internal/latency/ -v`
Expected: FAIL (undefined: NewRecorder/Handle/Count). (Use `-race` for this package from now on — it is the one with cross-goroutine handler dispatch.)

- [ ] **Step 3: Implement**

```go
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

	"github.com/arloliu/parti/test/iops-investigation/internal/load"
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
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd test/iops-investigation && go test -race ./internal/latency/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add test/iops-investigation/internal/latency/
git commit -m "feat(perf): window-gated per-worker HDR latency recorder (auto-ack)"
```

### Task 3.2: Merge + sample-gated percentile export

**Files:**
- Modify: `test/iops-investigation/internal/latency/recorder.go`
- Create: `test/iops-investigation/internal/latency/report.go`
- Test: `test/iops-investigation/internal/latency/report_test.go`

- [ ] **Step 1: Write the failing test**

```go
package latency

import "testing"

func TestReport_SampleGating(t *testing.T) {
	r := NewRecorder()
	r.SetWindow(0, 1<<62)
	// Record 2400 samples (the N=1000,k=1 cell): P99.9 must be gated to n/a
	// because n·(1−p) = 2400·0.001 = 2.4 < 10 (design §6).
	for i := 0; i < 2400; i++ {
		_ = r.Histogram().RecordValue(1_000_000) // 1ms
	}
	r.count = 2400
	rep := BuildReport([]*Recorder{r})
	if rep.Count != 2400 {
		t.Fatalf("count = %d", rep.Count)
	}
	if rep.P50Ns == 0 || rep.P99Ns == 0 {
		t.Fatalf("P50/P99 should be present: %+v", rep)
	}
	if rep.P999Present {
		t.Fatalf("P99.9 should be gated off at n=2400")
	}

	// 20000 samples ⇒ n·(1−p)=20 ≥ 10 ⇒ P99.9 present.
	r2 := NewRecorder()
	r2.SetWindow(0, 1<<62)
	for i := 0; i < 20000; i++ {
		_ = r2.Histogram().RecordValue(1_000_000)
	}
	r2.count = 20000
	rep2 := BuildReport([]*Recorder{r2})
	if !rep2.P999Present {
		t.Fatalf("P99.9 should be present at n=20000")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd test/iops-investigation && go test -race ./internal/latency/ -run TestReport -v`
Expected: FAIL (undefined: BuildReport).

- [ ] **Step 3: Implement**

Create `report.go`:
```go
package latency

import hdr "github.com/HdrHistogram/hdrhistogram-go"

// Report is the per-cell latency summary (design §6). Percentiles are in
// nanoseconds. A percentile is "present" only if the pooled sample count
// gives ≥ minTailSamples expected samples beyond it (n·(1−p) ≥ 10).
type Report struct {
	Count       int64
	P50Ns       int64
	P90Ns       int64
	P95Ns       int64
	P99Ns       int64
	P999Ns      int64
	P999Present bool
	MaxNs       int64
}

const minTailSamples = 10.0

// MergeRecorders folds per-worker recorders into one histogram (one rep),
// merging each under its lock (race-free even if called before full drain).
func MergeRecorders(recs []*Recorder) (*hdr.Histogram, int64) {
	merged := hdr.New(minLatencyNs, maxLatencyNs, sigFigs)
	var n int64
	for _, r := range recs {
		n += r.snapshotInto(merged)
	}
	return merged, n
}

// PercentilesFrom builds a gated Report from an already-merged histogram and
// its pooled sample count. Used both per-rep (BuildReport) and across reps
// (cmd/fitmodel merges rep snapshots first, then calls this) so the §6
// gating is always applied to the POOLED count, never to averaged
// percentiles.
func PercentilesFrom(h *hdr.Histogram, n int64) Report {
	rep := Report{
		Count:  n,
		P50Ns:  h.ValueAtQuantile(50),
		P90Ns:  h.ValueAtQuantile(90),
		P95Ns:  h.ValueAtQuantile(95),
		P99Ns:  h.ValueAtQuantile(99),
		P999Ns: h.ValueAtQuantile(99.9),
		MaxNs:  h.Max(),
	}
	// Gate P99.9: need n·(1−0.999) ≥ 10 ⇒ n ≥ 10000.
	rep.P999Present = float64(n)*(1.0-0.999) >= minTailSamples
	return rep
}

// BuildReport produces the single-rep report.
func BuildReport(recs []*Recorder) Report {
	merged, n := MergeRecorders(recs)
	return PercentilesFrom(merged, n)
}

// Snapshot is the JSON-serializable form of a merged histogram. hdrhistogram-go
// exposes Export() *hdr.Snapshot and hdr.Import(*hdr.Snapshot) *Histogram; we
// persist the Snapshot in latency.json so cmd/fitmodel can Import + Merge
// across the 3 reps and compute POOLED percentiles (averaging per-rep
// percentiles would be statistically invalid — design §6/§11).
func ExportSnapshot(recs []*Recorder) *hdr.Snapshot {
	merged, _ := MergeRecorders(recs)
	return merged.Export()
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd test/iops-investigation && go test -race ./internal/latency/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add test/iops-investigation/internal/latency/
git commit -m "feat(perf): merge recorders + sample-gated percentile report"
```

---

## Phase 4 — Harness `--load` mode wiring

### Task 4.1: Extend Options + flags (rate, fetch params, startup budget, load toggle)

**Files:**
- Modify: `test/iops-investigation/cmd/harness/harness.go` (`Options`, `ManifestOptions`, `buildManifestOptions`)
- Modify: `test/iops-investigation/cmd/harness/main.go` (`parseFlags`)
- Test: `test/iops-investigation/cmd/harness/main_test.go` (extend the existing parseFlags test)

- [ ] **Step 1: Add fields to `Options`** (after `FastConfig` in `harness.go`)

```go
	// --- perf-measurement (design §5–§9) ---
	Load            bool          // enable the open-loop producer + latency handler
	PerWorkerRate   float64       // k: msg/s per worker; aggregate X = k·Workers
	BatchSize       int           // consumer fetch batch size (pinned, §7)
	MaxWaiting      int           // consumer MaxWaiting (§7)
	MaxAckPending   int           // consumer MaxAckPending (§7)
	AckWait         time.Duration // consumer AckWait (§7)
	StartupBudget   time.Duration // WaitState/WaitStableAll budget (§9; 0 ⇒ max(60s, N·60ms))
```

- [ ] **Step 2: Add the matching `ManifestOptions` fields + map them in `buildManifestOptions`**

In `ManifestOptions`:
```go
	Load          bool    `yaml:"load"`
	PerWorkerRate float64 `yaml:"perWorkerRate"`
	AggregateX    float64 `yaml:"aggregateX"`
	BatchSize     int     `yaml:"batchSize"`
	MaxWaiting    int     `yaml:"maxWaiting"`
	MaxAckPending int     `yaml:"maxAckPending"`
	AckWait       string  `yaml:"ackWait"`
	StartupBudget string  `yaml:"startupBudget"`
```
In `buildManifestOptions` return literal:
```go
		Load:          o.Load,
		PerWorkerRate: o.PerWorkerRate,
		AggregateX:    o.PerWorkerRate * float64(o.Workers),
		BatchSize:     o.BatchSize,
		MaxWaiting:    o.MaxWaiting,
		MaxAckPending: o.MaxAckPending,
		AckWait:       o.AckWait.String(),
		StartupBudget: o.StartupBudget.String(),
```

- [ ] **Step 3: Add flags + assignment in `parseFlags` (`main.go`)**

In the `var (...)` block:
```go
		load          = fs.Bool("load", false, "enable open-loop producer + latency measurement (design §5)")
		perWorkerRate = fs.Float64("per-worker-rate", 2, "k: msg/s per worker; aggregate X = k·workers")
		batchSize     = fs.Int("batch-size", 1, "consumer fetch batch size (pinned, §7)")
		maxWaiting    = fs.Int("max-waiting", 512, "consumer MaxWaiting (§7)")
		maxAckPending = fs.Int("max-ack-pending", 1000, "consumer MaxAckPending (§7)")
		ackWait       = fs.Duration("ack-wait", 30*time.Second, "consumer AckWait (§7)")
		startupBudget = fs.Duration("startup-budget", 0, "WaitStable budget; 0 ⇒ max(60s, N·60ms) (§9)")
```
In the returned `Options{...}` literal:
```go
		Load:          *load,
		PerWorkerRate: *perWorkerRate,
		BatchSize:     *batchSize,
		MaxWaiting:    *maxWaiting,
		MaxAckPending: *maxAckPending,
		AckWait:       *ackWait,
		StartupBudget: *startupBudget,
```

- [ ] **Step 4: Extend the parseFlags test** in `main_test.go` — add assertions that `--load --per-worker-rate 4 --batch-size 8` populate `o.Load==true`, `o.PerWorkerRate==4`, `o.BatchSize==8`. (Find the existing `TestParseFlags`/equivalent and add a sub-case; if none exists, add one mirroring the existing default-value checks.)

- [ ] **Step 5: Run + commit**

Run: `cd test/iops-investigation && go test ./cmd/harness/ -run ParseFlags -v && go build ./...`
Expected: PASS, build clean.
```bash
git add test/iops-investigation/cmd/harness/
git commit -m "feat(perf): harness flags for load mode, fetch params, startup budget"
```

### Task 4.2: Wire the latency handler + pinned fetch params into StartWorker

**Files:**
- Modify: `test/iops-investigation/cmd/harness/harness.go` (`WorkerHandle`, `StartWorker`)

- [ ] **Step 1: Add a recorder field to `WorkerHandle`**

```go
	recorder *latency.Recorder // non-nil in --load mode
```
Add **only** import `"github.com/arloliu/parti/test/iops-investigation/internal/latency"` here. (The `internal/load` import is added in Task 4.3, where `load.ProducerHealth`/`load.MonoNanos` are first used — adding it now would be an unused import and fail `go build` at Step 4.)

- [ ] **Step 2: Build the handler in StartWorker (window armed later)**

`StartWorker` keeps its original signature (no window params — the window is
unknown at spawn time; it is armed at captureStart via `recorder.SetWindow`,
see Task 4.3). Right after `wh := &WorkerHandle{...}`:
```go
	var handler consumer.MessageHandler = noopHandler{}
	if o.Load {
		rec := latency.NewRecorder() // records nothing until SetWindow at captureStart
		wh.recorder = rec
		handler = rec
	}
```
Replace the two `noopHandler{}` arguments in `consumer.NewDynamic(...)` and `consumer.NewQueue(...)` with `handler`, and append the pinned fetch options:
```go
		dyn, derr := consumer.NewDynamic(
			ijs, o.DataStreamName, dynamicPrefix, dynamicSubjectTmpl, handler,
			consumer.WithFetchTimeout(o.FetchTimeout),
			consumer.WithBatchSize(o.BatchSize),
			consumer.WithMaxWaiting(o.MaxWaiting),
			consumer.WithMaxAckPending(o.MaxAckPending),
			consumer.WithAckWait(o.AckWait),
		)
```
(same option set on the `consumer.NewQueue(...)` call.)

- [ ] **Step 3: Scale the per-worker startup budget**

Add a shared helper to `harness.go`:
```go
// defaultStartupBudget scales the Stable-wait budget with partition count;
// the rig's old fixed 30s is too small for N=5000/RF=5 (design §9).
func defaultStartupBudget(n int) time.Duration {
	return max(60*time.Second, time.Duration(n)*60*time.Millisecond)
}
```
Replace the hard-coded `30*time.Second` in StartWorker's `WaitState` call:
```go
	budget := o.StartupBudget
	if budget <= 0 {
		budget = defaultStartupBudget(o.N)
	}
	if err := <-mgr.WaitState(parti.StateStable, budget); err != nil {
```
(`Run` resolves `o.StartupBudget` once before spawning workers — Task 4.3 Step 0 — so `WaitStableAll`, the per-worker wait, and the manifest all use the same value; this fallback only guards direct callers/tests.)

- [ ] **Step 4: Build** (StartWorker signature unchanged, so the `main.go` call site is untouched here; window arming is added in Task 4.3)

Run: `cd test/iops-investigation && go build ./...`
Expected: clean (Task 4.3 adds the producer + window arming).

### Task 4.3: Start producer, align window, emit `latency.json`, gate on producer-bound/delivery deficit

**Files:**
- Modify: `test/iops-investigation/cmd/harness/main.go` (`Run`)
- Modify: `test/iops-investigation/cmd/harness/harness.go` (add `WriteLatencyReport`, extend `Manifest`)

**Lifecycle ordering (trace this — the bugs live here):** `Run` order is
spawn workers → `WaitStableAll` → start producer → sleep `--warmup` →
`ResetAll`+`captureStart` → **arm window here** → capture loop until
`captureDeadline` → **drain** → build latency report. The recorder/producer
windows are armed at `captureStart` (NOT projected before spawn — stabilization
takes minutes at N=5000), and delivery is compared against *in-window* sends
(NOT total sends — the producer runs warmup+capture ≈ 2× the window).

- [ ] **Step 0: Resolve the startup budget once (before spawning workers)**

At the top of `Run`, after `cfg := BuildPartiConfig(o)`, resolve the budget so
the per-worker wait, the cluster gate, and the manifest all agree (§9):
```go
	if o.StartupBudget <= 0 {
		o.StartupBudget = defaultStartupBudget(o.N)
	}
```
Then replace the existing cluster gate
```go
	gateTimeout := max(3*o.Warmup, 30*time.Second)
	if err := WaitStableAll(workers, gateTimeout); err != nil {
```
with
```go
	if err := WaitStableAll(workers, o.StartupBudget); err != nil {
```
(`buildManifestOptions` now records the resolved `o.StartupBudget`.)

- [ ] **Step 1: Start the producer on a dedicated connection (load mode), right after `WaitStableAll`**

Add import `"github.com/arloliu/parti/test/iops-investigation/internal/load"` and `"github.com/arloliu/parti/test/iops-investigation/internal/latency"` (and `hdr "github.com/HdrHistogram/hdrhistogram-go"` for `WriteLatencyReport`'s `*hdr.Snapshot`). After the `WaitStableAll` block:
```go
	var producer *load.Producer
	var prodCancel context.CancelFunc
	if o.Load {
		prodConn, perr := ConnectNATS(o.NATSURLs)
		if perr != nil {
			cleanup()
			return fmt.Errorf("producer connect: %w", perr)
		}
		defer prodConn.Close()
		prodJS, perr := jetstream.New(prodConn, jetstream.WithPublishAsyncMaxPending(4096))
		if perr != nil {
			cleanup()
			return fmt.Errorf("producer jetstream.New: %w", perr)
		}
		producer = load.NewProducer(load.ProducerConfig{
			JS: prodJS, N: o.N, AggregateX: o.PerWorkerRate * float64(o.Workers),
			SkewP99Limit: 5 * time.Millisecond, LateLimit: 10 * time.Millisecond, LateFraction: 0.01,
		})
		var pctx context.Context
		pctx, prodCancel = context.WithCancel(ctx)
		go producer.Run(pctx)
		defer prodCancel()
	}
```
The producer now runs through warmup (so consumers are warm) but counts no
in-window sends yet (window unset).

- [ ] **Step 2: Arm the window at `captureStart`**

Immediately after `ResetAll(workers)` / `captureStart := time.Now()`:
```go
	if o.Load {
		captureStartMono := load.MonoNanos()
		captureEndMono := captureStartMono + o.CaptureWindow.Nanoseconds()
		for _, w := range workers {
			if w.recorder != nil {
				w.recorder.SetWindow(captureStartMono, captureEndMono)
			}
		}
		producer.SetWindow(captureStartMono, captureEndMono)
	}
```
Now recorder and producer agree on the exact same in-window interval, both in
CLOCK_MONOTONIC, both armed at the real capture start.

**Early-exit paths (interrupt / mid-capture degraded):** the `ctx.Done()` and
mid-loop `DecideRunStatus`-degraded branches in the capture loop return via
`finishRun` WITHOUT the load finalization below. That is intentional and
acceptable: (a) `defer prodCancel()` already stops the producer goroutine on
every return (no leak), and (b) such a cell is invalid and is excluded by the
runner (Phase 8), so its `latency.json` would never be consumed. The manifest
status (`interrupted`/`degraded`) already records why. Do NOT duplicate the
finalization into those branches — only the clean post-capture path (which
covers degraded-detected-at-window-end) writes `latency.json`.

- [ ] **Step 3: After the capture loop, drain then build the report + apply guards**

After `break captureLoop` and the existing `status, derr := DecideRunStatus(workers)` line, in load mode (before `commitCSV`/`WriteManifest`):
```go
	if o.Load {
		// Drain: let in-flight in-window messages arrive before reading
		// counts (fetch-floor latency ≈ FetchTimeout). Producer keeps running
		// during the drain but those sends are out-of-window and ignored.
		_ = sleepCtx(ctx, 3*o.FetchTimeout)
		prodCancel()
		producer.Wait() // block until Run drains all async futures (accurate AsyncErrors)
		h := producer.Health()
		recs := make([]*latency.Recorder, 0, len(workers))
		var delivered int64
		for _, w := range workers {
			if w.recorder != nil {
				recs = append(recs, w.recorder)
				delivered += w.recorder.Count()
			}
		}
		rep := latency.BuildReport(recs)
		snap := latency.ExportSnapshot(recs) // serialized histogram for cross-rep pooling
		// Delivery-deficit guard (§9): delivered / IN-WINDOW produced < 0.95.
		inWindow := h.InWindowSent
		deficit := inWindow > 0 && float64(delivered)/float64(inWindow) < 0.95
		if err := WriteLatencyReport(o.OutputDir, rep, h, delivered, snap); err != nil {
			return fmt.Errorf("write latency report: %w", err)
		}
		// Only override status when the run was otherwise clean (derr == nil);
		// never downgrade an already-degraded run's label.
		if h.ProducerBound {
			if derr == nil {
				status = "producer-bound"
				derr = fmt.Errorf("producer-bound: %+v", h)
			}
		} else if deficit {
			if derr == nil {
				status = "delivery-deficit"
				derr = fmt.Errorf("delivery deficit: delivered=%d in-window-produced=%d", delivered, inWindow)
			}
		}
	}
```

- [ ] **Step 4: Implement `WriteLatencyReport` in `harness.go`**

```go
// LatencyReport is the JSON artifact written per load cell. Snapshot holds the
// serialized merged histogram so cmd/fitmodel can Import + Merge the 3 reps
// and compute POOLED percentiles (§6/§11) — per-rep summary percentiles
// cannot be averaged.
type LatencyReport struct {
	Count         int64        `json:"count"`
	InWindowSent  int64        `json:"inWindowSent"`
	Delivered     int64        `json:"delivered"`
	DeliveryRatio float64      `json:"deliveryRatio"`
	P50Ns         int64        `json:"p50Ns"`
	P90Ns         int64        `json:"p90Ns"`
	P95Ns         int64        `json:"p95Ns"`
	P99Ns         int64        `json:"p99Ns"`
	P999Ns        int64        `json:"p999Ns"`
	P999Present   bool         `json:"p999Present"`
	MaxNs         int64        `json:"maxNs"`
	ProducerBound bool         `json:"producerBound"`
	SkewP99Ns     int64        `json:"skewP99Ns"`
	AsyncErrors   int64        `json:"asyncErrors"`
	LateSends     int64        `json:"lateSends"`
	Snapshot      *hdr.Snapshot `json:"snapshot"`
}

// WriteLatencyReport writes <outputDir>/latency.json atomically.
func WriteLatencyReport(dir string, rep latency.Report, h load.ProducerHealth, delivered int64, snap *hdr.Snapshot) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ratio := 0.0
	if h.InWindowSent > 0 {
		ratio = float64(delivered) / float64(h.InWindowSent)
	}
	lr := LatencyReport{
		Count: rep.Count, InWindowSent: h.InWindowSent, Delivered: delivered, DeliveryRatio: ratio,
		P50Ns: rep.P50Ns, P90Ns: rep.P90Ns, P95Ns: rep.P95Ns, P99Ns: rep.P99Ns,
		P999Ns: rep.P999Ns, P999Present: rep.P999Present, MaxNs: rep.MaxNs,
		ProducerBound: h.ProducerBound, SkewP99Ns: h.SkewP99Ns, AsyncErrors: h.AsyncErrors, LateSends: h.LateSends,
		Snapshot: snap,
	}
	buf, err := json.MarshalIndent(lr, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, "latency.json"), buf, 0o644)
}
```
Add imports to `harness.go`: `"encoding/json"`, `hdr "github.com/HdrHistogram/hdrhistogram-go"`, `"github.com/arloliu/parti/test/iops-investigation/internal/latency"`, `"github.com/arloliu/parti/test/iops-investigation/internal/load"`.

- [ ] **Step 5: Build, smoke-test, commit**

Run: `cd test/iops-investigation && go build ./... && go test ./cmd/harness/ -run Smoke -v`
Expected: build clean; smoke test passes (existing e2e smoke still green with `--load` defaulting off).
```bash
git add test/iops-investigation/cmd/harness/
git commit -m "feat(perf): start producer, gate window, emit latency.json with guards"
```

### Task 4.4: Load-mode e2e smoke test

**Files:**
- Create: `test/iops-investigation/cmd/harness/load_smoke_test.go`

- [ ] **Step 1: Write the test** — start an embedded JS server (reuse `internal/testnats`), run `Run` with `Load:true, FastConfig:true, Workers:2, N:8, PerWorkerRate:50, Warmup:1s, CaptureWindow:2s`, then assert `results/.../latency.json` exists, `count > 0`, `deliveryRatio > 0.5`, `producerBound == false`.

```go
func TestRun_LoadMode_EmitsLatency(t *testing.T) {
	url, shutdown := testnats.Start(t)
	defer shutdown()
	dir := t.TempDir()
	o := Options{
		NATSURLs: url, Workers: 2, N: 8, Replicas: 1,
		ConsumerMode: ConsumerModeDynamic, KVStorage: jetstream.MemoryStorage, DataStorage: jetstream.MemoryStorage,
		DataStreamName: "iops-rig-data", PartitionSourceKey: DefaultPartitionSourceKey,
		Warmup: time.Second, CaptureWindow: 2 * time.Second, RPCDumpInterval: 500 * time.Millisecond,
		FetchTimeout: 1 * time.Second, // MIN legal pull expiry (NATS rejects <1s); 3× drain = 3s
		OutputDir: dir, FastConfig: true,
		Load: true, PerWorkerRate: 50, BatchSize: 8, MaxWaiting: 64, MaxAckPending: 256, AckWait: 30 * time.Second,
	}
	if err := Run(context.Background(), o, io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	buf, err := os.ReadFile(filepath.Join(dir, "latency.json"))
	if err != nil {
		t.Fatalf("read latency.json: %v", err)
	}
	var lr LatencyReport
	if err := json.Unmarshal(buf, &lr); err != nil {
		t.Fatal(err)
	}
	if lr.Count == 0 || lr.DeliveryRatio < 0.5 || lr.ProducerBound {
		t.Fatalf("unexpected latency report: %+v", lr)
	}
}
```

- [ ] **Step 2: Run** — `cd test/iops-investigation && go test ./cmd/harness/ -run LoadMode -v` → PASS.
- [ ] **Step 3: Commit** — `git commit -am "test(perf): load-mode e2e smoke (latency.json delivery)"`.

---

## Phase 5 — Metacontroller snapshot capture (`internal/aggregate/jsz.go`)

### Task 5.1: Verify the /jsz meta-snapshot schema (gated, verify-first) — ✅ DONE

**DONE 2026-06-04** (captured live on a 3-node `nats:2.12.6` cluster). Fixture
committed at `test/iops-investigation/testdata/jsz_meta_sample.ndjson` (3 lines
in `capture-jsz.sh` ndjson-envelope form: two `jsz` polls + one `varz` line to
confirm it is ignored).

**Verified schema** — `body.meta_cluster.snapshot` (top-level of the `/jsz`
body, sibling of `account_details`):
```json
{"pending_entries":72,"pending_size":68319,
 "last_time":"2026-06-04T02:03:29.446274011Z","last_duration":2011874}
```
- `last_duration` — int **nanoseconds**, `omitempty` (ABSENT on a fresh cluster
  until the first snapshot fires — the parser must treat missing as 0/none).
- `pending_size` — int bytes, meta-raft WAL tail (what `meta_compact_size`
  gates on). `pending_entries` — int.
- `last_time` — RFC3339 string; distinct values over the capture ⇒ snapshot
  frequency/count.
- NO `count` and NO marshal/compressed size in `/jsz` (log-only, and only WRN'd
  when duration > 2s on 2.12.6). The fixture's `last_duration` = `2011874` ns
  (≈ 2.0 ms for 400 consumers).

### Task 5.2: Parse meta-snapshot stats into samples

**Files:**
- Modify: `test/iops-investigation/internal/aggregate/jsz.go`
- Test: `test/iops-investigation/internal/aggregate/jsz_meta_test.go`

- [ ] **Step 1: Write the failing test against the captured fixture**

```go
func TestParseMetaSnapshot(t *testing.T) {
	samples, err := ParseMetaSnapshot("../../testdata/jsz_meta_sample.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	// Fixture has 2 jsz lines (the varz line is ignored) ⇒ 2 samples.
	if len(samples) != 2 {
		t.Fatalf("expected 2 meta samples (2 jsz lines, varz ignored), got %d", len(samples))
	}
	// CONCRETE assertions pinned to the captured fixture (no loose >=0).
	s := samples[0]
	if s.LastDurationNs != 2011874 {
		t.Fatalf("LastDurationNs = %d, want 2011874", s.LastDurationNs)
	}
	if s.PendingSize != 68319 {
		t.Fatalf("PendingSize = %d, want 68319", s.PendingSize)
	}
	if s.PendingEntries != 72 {
		t.Fatalf("PendingEntries = %d, want 72", s.PendingEntries)
	}
	if s.LastTime != "2026-06-04T02:03:29.446274011Z" {
		t.Fatalf("LastTime = %q", s.LastTime)
	}
}
```

- [ ] **Step 2: Implement** — add to `jsz.go`:
  - `MetaSnapshotSample` struct: `TUnixNs int64`, `Node string`, `LastDurationNs int64`, `PendingSize int64`, `PendingEntries int64`, `LastTime string`.
  - Extend the `jszBody` unmarshal target (or add a dedicated one) with:
    ```go
    MetaCluster struct {
        Snapshot struct {
            PendingEntries int64  `json:"pending_entries"`
            PendingSize    int64  `json:"pending_size"`
            LastTime       string `json:"last_time"`
            LastDurationNs int64  `json:"last_duration"` // ns, omitempty (0 until first snapshot)
        } `json:"snapshot"`
    } `json:"meta_cluster"`
    ```
  - `ParseMetaSnapshot(path)`: reuse the `jszLine` ndjson scanner (same as `ParseJSZ`), keep only `endpoint=="jsz"` lines, emit one `MetaSnapshotSample` per jsz line from `body.meta_cluster.snapshot` (carry `TUnixNs`/`Node` from the envelope). `last_duration` absent ⇒ 0 (no snapshot yet) — that's valid, not an error.
  - Optionally a `MetaSnapshotCount(samples)` helper = number of DISTINCT non-zero `LastTime` values (snapshot frequency over the capture). The §8.1 "snapshot count ≥ 5" gate uses this.

> The exact struct tags come from the captured fixture, not from memory. Write the struct to match `testdata/jsz_meta_sample.json` field-for-field.

- [ ] **Step 3: Run** — `cd test/iops-investigation && go test ./internal/aggregate/ -run Meta -v` → PASS.
- [ ] **Step 4: Commit** — `git commit -am "feat(perf): parse /jsz meta-snapshot duration/size/count"`.

### Task 5.3: Capture meta stats during the run (extend capture-jsz.sh consumers)

**Files:**
- Modify: `test/iops-investigation/scripts/capture-jsz.sh` (ensure it polls from cluster startup, not just the window)
- Modify: `test/iops-investigation/cmd/aggregate/main.go` (emit a `meta_snapshot_*` column if the aggregate binary joins jsz)

- [ ] **Step 1:** Confirm `capture-jsz.sh` already records the field (it stores the raw `/jsz` body, so Task 5.2's parser can read it post-hoc). Add a `--from-startup` note in the script header and ensure `run-matrix.sh` starts the jsz poller *before* worker spawn for the meta cells (Phase 8). No code change if the body is already captured; verify with: `grep -n 'body' scripts/capture-jsz.sh`.
- [ ] **Step 2:** Extend `cmd/aggregate` to surface `meta_snapshot_last_duration_ms`, `meta_snapshot_count`, `meta_snapshot_bytes` per second-bucket from `ParseMetaSnapshot`, gated on count ≥ 5 (§8.1). Add a unit test feeding the fixture.
- [ ] **Step 3:** Run `cd test/iops-investigation && go test ./cmd/aggregate/ -v` → PASS. Commit.

---

## Phase 6 — cgroup / disk isolation

### Task 6.1: docker-compose cpuset + dedicated NVMe volume

**Files:**
- Modify: `test/iops-investigation/docker/docker-compose.yaml`

- [ ] **Step 1:** Add `cpuset: "0-7,16-23"` to each of the 5 NATS services (design §10). Add `cpu` reservation is NOT needed — cpuset pins the cores.
- [ ] **Step 2:** Point each node's JetStream `store_dir` volume at a bind mount on the Crucial T710. Inspect the current volume wiring first: `grep -n 'volumes\|store_dir\|/data' docker/docker-compose.yaml docker/nats-server.conf`. Then change the named volumes to bind mounts under a T710 mount path (e.g. `/mnt/t710/iops-nats-<n>`), documented in `README.md`. If the T710 is not mounted, add a `make mount-t710` note.
- [ ] **Step 3:** Validate compose parses: `cd test/iops-investigation && docker compose -f docker/docker-compose.yaml config >/dev/null && echo OK`.
- [ ] **Step 4:** Commit.

### Task 6.2: Isolation verification helper

**Files:**
- Create: `test/iops-investigation/scripts/verify-isolation.sh`

- [ ] **Step 1:** Write a bash script that, given the harness PID and the NATS container names, asserts (design §10):
  - NATS: `cat /sys/fs/cgroup/$(docker inspect -f '{{.Id}}' <ctr>)/cpuset.cpus.effective` (or `docker exec <ctr> cat /sys/fs/cgroup/cpuset.cpus.effective`) equals `0-7,16-23`.
  - Harness: `taskset -pc <harness_pid>` equals `8-15,24-31`.
  - Exit non-zero (abort the run) on any mismatch; echo the resolved values for the manifest.
- [ ] **Step 2:** Self-test the script's parsing with a `--dry-run` that echoes the expected vs actual format. Commit.

### Task 6.3: Pin harness affinity in run-matrix

**Files:**
- Modify: `test/iops-investigation/scripts/run-matrix.sh` (Phase 8 also touches this)

- [ ] **Step 1:** The harness must be running when isolation is verified (you cannot check a live PID's affinity after the process exits — design §10 requires a pre-run abort). So launch it in the **background** and verify against its PID before the window opens:
```bash
taskset -c 8-15,24-31 "$HARNESS_BIN" --load ... &
HARNESS_PID=$!
# Give it a moment to spawn workers, then verify NATS cgroup + harness affinity.
sleep 2
if ! scripts/verify-isolation.sh "$HARNESS_PID" "$CONTAINERS"; then
    kill "$HARNESS_PID" 2>/dev/null; wait "$HARNESS_PID" 2>/dev/null
    echo "ABORT cell <id>: isolation mismatch" >&2
    continue   # skip this cell, logged (no silent cap)
fi
wait "$HARNESS_PID"   # then let the run complete normally
```
(Implemented in the Phase 8 runner; Task 6.2's `verify-isolation.sh` takes the harness PID + container list and exits non-zero on mismatch.)

---

## Phase 7 — Cost model + estimator CLI

### Task 7.1: Multivariate OLS fit `cost ≈ a + b·N + c·X`

**Files:**
- Create: `test/iops-investigation/internal/costmodel/fit.go`
- Test: `test/iops-investigation/internal/costmodel/fit_test.go`

- [ ] **Step 1: Write the failing test**

```go
package costmodel

import (
	"math"
	"testing"
)

func TestFitAffine_RecoversCoefficients(t *testing.T) {
	// Synthetic: cost = 5 + 0.04*N + 0.5*X (no noise) ⇒ exact recovery.
	var pts []Point
	for _, n := range []float64{1000, 2000, 3000, 5000} {
		for _, x := range []float64{20, 40, 80} {
			pts = append(pts, Point{N: n, X: x, Cost: 5 + 0.04*n + 0.5*x})
		}
	}
	f, err := FitAffine(pts)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(f.A-5) > 1e-6 || math.Abs(f.B-0.04) > 1e-9 || math.Abs(f.C-0.5) > 1e-9 {
		t.Fatalf("bad coeffs: %+v", f)
	}
	if f.R2 < 0.999999 {
		t.Fatalf("R2=%v", f.R2)
	}
	if got := f.Predict(4000, 160); math.Abs(got-(5+0.04*4000+0.5*160)) > 1e-6 {
		t.Fatalf("predict=%v", got)
	}
}

func TestFitAffine_RejectsUnderdetermined(t *testing.T) {
	if _, err := FitAffine([]Point{{N: 1, X: 1, Cost: 1}, {N: 2, X: 2, Cost: 2}}); err == nil {
		t.Fatal("expected error: fewer than 3 distinct points")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd test/iops-investigation && go test ./internal/costmodel/ -v`
Expected: FAIL (undefined).

- [ ] **Step 3: Implement OLS via 3×3 normal equations**

```go
// Package costmodel fits a load-aware affine model cost ≈ a + b·N + c·X per
// metric per storage type and predicts cost at arbitrary (N,X) (design §11).
package costmodel

import (
	"errors"
	"math"
)

// Point is one measured cell. N is the structural axis = PARTITION COUNT
// (== consumer count; NOT the worker/member count). X is aggregate load (k·M).
// cmd/fitmodel builds Points with N=partition count and cmd/estimator predicts
// at partition count — both MUST agree or the structural term is off by 50×.
type Point struct{ N, X, Cost float64 }

// Fit holds the fitted coefficients and goodness of fit.
type Fit struct {
	A, B, C float64 // cost = A + B·N + C·X
	R2      float64
	N       int // number of points
}

// Predict evaluates the fitted model.
func (f Fit) Predict(n, x float64) float64 { return f.A + f.B*n + f.C*x }

// FitAffine solves the least-squares normal equations for a + b·N + c·X.
// Requires ≥ 3 points spanning ≥ 3 distinct (N,X) rows or it returns an error
// (under-determined ⇒ meaningless extrapolation, design §11).
func FitAffine(pts []Point) (Fit, error) {
	if len(pts) < 3 {
		return Fit{}, errors.New("costmodel: need at least 3 points")
	}
	// Design matrix columns: [1, N, X]. Build normal matrix M (3×3) and rhs.
	var m [3][3]float64
	var rhs [3]float64
	for _, p := range pts {
		xs := [3]float64{1, p.N, p.X}
		for i := 0; i < 3; i++ {
			for j := 0; j < 3; j++ {
				m[i][j] += xs[i] * xs[j]
			}
			rhs[i] += xs[i] * p.Cost
		}
	}
	coef, ok := solve3(m, rhs)
	if !ok {
		return Fit{}, errors.New("costmodel: singular system (points not spanning N and X)")
	}
	f := Fit{A: coef[0], B: coef[1], C: coef[2], N: len(pts)}
	// R²
	var mean, ssTot, ssRes float64
	for _, p := range pts {
		mean += p.Cost
	}
	mean /= float64(len(pts))
	for _, p := range pts {
		pred := f.Predict(p.N, p.X)
		ssRes += (p.Cost - pred) * (p.Cost - pred)
		ssTot += (p.Cost - mean) * (p.Cost - mean)
	}
	if ssTot == 0 {
		f.R2 = 1
	} else {
		f.R2 = 1 - ssRes/ssTot
	}
	return f, nil
}

// solve3 solves a 3×3 linear system by Gaussian elimination with partial
// pivoting. Returns ok=false if the matrix is singular.
func solve3(m [3][3]float64, b [3]float64) ([3]float64, bool) {
	a := [3][4]float64{
		{m[0][0], m[0][1], m[0][2], b[0]},
		{m[1][0], m[1][1], m[1][2], b[1]},
		{m[2][0], m[2][1], m[2][2], b[2]},
	}
	for col := 0; col < 3; col++ {
		piv := col
		for r := col + 1; r < 3; r++ {
			if math.Abs(a[r][col]) > math.Abs(a[piv][col]) {
				piv = r
			}
		}
		if math.Abs(a[piv][col]) < 1e-12 {
			return [3]float64{}, false
		}
		a[col], a[piv] = a[piv], a[col]
		for r := 0; r < 3; r++ {
			if r == col {
				continue
			}
			f := a[r][col] / a[col][col]
			for c := col; c < 4; c++ {
				a[r][c] -= f * a[col][c]
			}
		}
	}
	return [3]float64{a[0][3] / a[0][0], a[1][3] / a[1][1], a[2][3] / a[2][2]}, true
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd test/iops-investigation && go test ./internal/costmodel/ -v`
Expected: PASS.

- [ ] **Step 5: Commit** — `git commit -am "feat(perf): load-aware affine cost model (a+bN+cX) via OLS"`.

### Task 7.2: Model JSON I/O (one Fit per metric per storage)

**Files:**
- Modify: `test/iops-investigation/internal/costmodel/fit.go`
- Test: `test/iops-investigation/internal/costmodel/model_test.go`

- [ ] **Step 1: Test** — define `Model map[string]map[string]Fit` (metric → storage → Fit); round-trip JSON via `WriteModel`/`LoadModel`. Assert a loaded model predicts identically.
- [ ] **Step 2: Implement** `WriteModel(path, Model)` / `LoadModel(path) (Model, error)` using `encoding/json` + `writeFileAtomic`-style temp+rename (or `os.WriteFile`).
- [ ] **Step 3:** Run → PASS. Commit.

### Task 7.3: Estimator CLI

**Files:**
- Create: `test/iops-investigation/cmd/estimator/main.go`
- Test: `test/iops-investigation/cmd/estimator/main_test.go`

- [ ] **Step 1: Test** — table test on a pure `predict(model, n, k, rf int, storage string)` function:
  - derives `M=n/50`, `X=k·M`;
  - `rf != 5` ⇒ returns an error ("model fit at RF=5 only", design §11);
  - `n` outside `[1000,5000]` ⇒ result carries an `extrapolation=true` flag;
  - returns predicted values per metric from the loaded `Model`.
- [ ] **Step 2: Implement** `main.go` with flags `--model <path> --n --k --rf --storage`, calling the pure `predict`, printing a table + the caveat banner when extrapolating, and exiting non-zero on `rf != 5`.
- [ ] **Step 3:** Run `cd test/iops-investigation && go test ./cmd/estimator/ -v && go build ./...` → PASS/clean. Commit.

---

## Phase 8 — Run-matrix orchestration

### Task 8.1: Load-matrix runner

**Files:**
- Create: `test/iops-investigation/scripts/run-load-matrix.sh`

- [ ] **Step 1:** Write a runner (modeled on `run-matrix.sh`, reuse its capture/teardown helpers) that iterates the design §8 matrix:
  - dynamic: `N ∈ {1000,2000,3000,5000} × k ∈ {1,2,4} × storage ∈ {file,memory}` (24 cells);
  - queue floor: `N × storage at k=2` (8) + corners `k ∈ {1,4} × N ∈ {1000,5000} storage=file` (4);
  - each cell: `make reset` → start jsz poller (from startup, for meta) + the per-NATS-cgroup captures (`capture-cgroup-io.sh` → `cgroup_io.raw` for block-write IOPS, and `capture-cgroup-cpumem.sh` → `cgroup_cpumem.raw` for CPU `usage_usec` + `memory.current`, same 1 Hz mechanism) → launch `taskset -c 8-15,24-31 "$HARNESS_BIN" --load --replicas 5 --n N --workers $((N/50)) --per-worker-rate k --consumer-mode {dynamic|queue} --{kv,data}-storage S --warmup 120s --capture-window 120s ...` **in the background**, capture its PID, `sleep 2`, run `verify-isolation.sh "$PID" "$CONTAINERS"` and **abort+continue the cell on mismatch** (kill the PID), else `wait "$PID"` → on completion copy `latency.json`+`rpc_counts.csv`+jsz raw into `results/<cell>/rep<r>/` (see Task 6.3 Step 1 for the exact background/verify/wait snippet);
  - **3 reps**, **resumable**: skip a `results/<cell>/rep<r>/` only if it is a VALID complete cell — i.e. `manifest.yaml` exists AND its `status` is `ok` AND `latency.json` is present. A rep whose manifest has `status` in {`degraded`,`interrupted`,`producer-bound`,`delivery-deficit`} or lacks `latency.json` is an INVALID cell: **delete that rep dir and re-run it** (a bare manifest-presence check would silently accept an invalid cell as done — §13). Implement the gate as e.g. `grep -q '^status: ok$' manifest.yaml && test -f latency.json`.
  - `log` every skipped (valid), re-run (invalid), and aborted (isolation) cell with reason (no silent caps, §9).
- [ ] **Step 2:** Add `--dry-run` that prints the full cell list + derived flags without running. Self-test: `bash scripts/run-load-matrix.sh --dry-run | wc -l` equals the expected cell×rep count (36×3=108 + meta sub-matrix).
- [ ] **Step 3:** Commit.

### Task 8.2: meta_compact sweep + (stretch) out-of-process cell

**Files:**
- Create: `test/iops-investigation/docker/nats-server-meta16.conf`, `nats-server-meta64.conf`
- Modify: `scripts/run-load-matrix.sh`
- Create (stretch): `test/iops-investigation/cmd/worker/main.go`

- [ ] **Step 1:** Create two server configs adding `jetstream { meta_compact_size: 16MB }` and `64MB` to the base `nats-server.conf` (design §8.1). **NOTE: the value must be UNQUOTED** — `nats-server -c` rejects the quoted `"16MB"` (`strconv.ParseInt parsing "16M"`); unquoted `16MB` parses to 16777216 (verified against nats:2.12.6 with `nats-server -c <conf> -t`). Add a `--meta-sweep` mode to the runner that runs `N=5000,k=2,file,dynamic` against {default, 16MB, 64MB}, capturing jsz from startup and gating on snapshot count ≥ 5.
- [ ] **Step 2 (stretch, §8.2):** Create `cmd/worker` — a single-worker process variant of `StartWorker` that reads window bounds + config from flags/env and writes its own `latency.json`; add a `--out-of-process` runner mode at `N=1000,k=2` spawning `M=20` worker processes. If descoped, the runner logs "§8.2 out-of-process: not run" and latency stays in-process-only.
- [ ] **Step 3:** Commit.

---

## Phase 9 — Run + findings

### Task 9.1: Execute the matrix

- [ ] **Step 1:** Build binaries: `cd test/iops-investigation && go build ./cmd/harness ./cmd/aggregate ./cmd/estimator`.
- [ ] **Step 2:** `IOPS_RIG_NATS_REPLICAS=5 make up` (or via the runner). Run `scripts/run-load-matrix.sh --seed 42 --results-dir results/load1/` (multi-hour; ramps N upward; stops honestly per §9).
- [ ] **Step 3:** Run `--meta-sweep`. Tear down with `make down` (or `make reset` between campaigns).

### Task 9.2: Fit the model + write findings

**Files:**
- Create: `test/iops-investigation/cmd/fitmodel/main.go` (reads results/, builds `Model`, writes `model.json`)
- Create: `docs/plans/perf-measurement/findings.md`

- [ ] **Step 1:** Write `cmd/fitmodel` that walks `results/load1/`, reads each cell's `latency.json` + the aggregated NATS-side IOPS/CPU/RSS (from the existing aggregate output), assembles `[]Point` per (metric, storage) **with `Point.N` = the cell's PARTITION COUNT** (the structural axis — matches `cmd/estimator` and `costmodel.Point`; do NOT use worker count), `Point.X` = the cell's aggregate load, calls `costmodel.FitAffine`, and writes `model.json`. **NATS-side cost metrics:** `<mode>_write_iops` (from `cgroup_io.raw`), plus `<mode>_cpu_cores` and `<mode>_rss_bytes` (from `cgroup_cpumem.raw` via `ParseCgroupCPUMem`→`CPUMemDeltas`: CPU as fraction-of-one-core where 1.0 = one full core, RSS as instantaneous bytes), each summed across the 5 NATS containers per second and meaned over the same post-warmup window. The cpumem metrics are optional — a cell without `cgroup_cpumem.raw` logs a note and skips them (backward compatible with IOPS-only runs). **Cross-rep latency pooling (§6/§11):** for each cell, `hdr.Import` the 3 reps' `Snapshot` fields, `Merge` them into one histogram, sum the 3 `inWindowSent`/`delivered`, then call `latency.PercentilesFrom(merged, pooledN)` so percentiles and the P99.9 gate use the POOLED count — never average per-rep percentiles. Unit-test the results-walk + the snapshot-merge on a tiny fixture tree (two rep dirs with hand-written `latency.json` snapshots whose merged P50 is known).
- [ ] **Step 2:** Write `findings.md` mirroring `docs/plans/iops-investigation/findings.md`: a cell-mean table (IOPS/CPU/RSS + latency P50/P90/P95/P99/P99.9(gated)/max per N,k,storage), the affine coefficients `a,b,c` + R² per metric/storage, the metacontroller `meta_compact_size` result at N=5000, the documented saturation ceiling (which N rungs completed), the in-process latency caveat (+ §8.2 result if run), and the operator-facing verdict on whether one-consumer-per-partition scales to 5000.
- [ ] **Step 3:** Validate the estimator against a held-out check: `./cmd/estimator/estimator --model model.json --n 4000 --k 2 --rf 5 --storage file` and sanity-check vs the measured N=3000/N=5000 neighbors. Commit findings + model.

### Task 9.3: Pre-PR gate + finish

- [ ] **Step 1:** From the rig module: `cd test/iops-investigation && go vet ./... && go test ./...` → all green.
- [ ] **Step 2:** Lint per repo convention (`make lint` at repo root; the rig module is separate but keep it clean).
- [ ] **Step 3:** Use `superpowers:finishing-a-development-branch` to decide merge/PR. The rig is a measurement artifact (separate module, not shipped in parti), so this does NOT trigger the parti pre-PR gate in AGENTS.md (no `manager/`,`source/` etc. touched) — but note the measurement informs the dynamic-consumer-collapse design.

---

## Self-review notes (spec coverage)

- §5 producer → Phase 2; §6 latency/ack/gating → Phase 3; §7 fetch params → Task 4.1/4.2; §8 matrix → Phase 8; §8.1 meta → Phase 5 + Task 8.2; §8.2 out-of-process → Task 8.2 (stretch); §9 guards → Tasks 4.3 (producer-bound, delivery-deficit) + 4.2 (startup budget) + 6.2 (cpuset abort); §10 isolation → Phase 6; §11 model+CLI → Phase 7; §12 deliverables → Phase 9; §13 risks → resumable runner (8.1), verify-first jsz (5.1).
- The CPU-saturation guard (§9, NATS cpuset ≥95% for ≥10% window) is captured post-hoc from the NATS-cgroup `cpu.stat` the rig already records; flag it during `cmd/fitmodel` analysis (note in findings) rather than aborting mid-run, since it is a NATS-side signal the harness process does not see live. **This is the one §9 guard enforced at analysis time, not run time — called out so it is not silently dropped.**
