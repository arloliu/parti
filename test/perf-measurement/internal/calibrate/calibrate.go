// Package calibrate provides shared helpers for the M4 calibration driver
// (cmd/calibrate). It is intentionally thin — Rule 2. Each sub-command is
// the primary unit; this package holds only the three primitives that every
// sub-command would otherwise duplicate:
//
//   - [RunRate] — tick-based workload loop at a fixed ops/s rate.
//   - [ParseStorage] — CLI string to [jetstream.StorageType].
//   - [NewInstrumentedJS] — connect + wrap in a single call.
//
// Block-IOPS measurement for M4 happens via the external capture scripts
// (cgroup/iostat in scripts/). The calibrate binary does NOT measure IOPS
// itself. It emits wrapper-counted RPC totals so the post-processing step
// can pair them with capture data to derive per-op IOPS factors.
package calibrate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/arloliu/parti/test/perf-measurement/internal/instrumentedjs"
)

// ParseStorage maps the CLI string ("file" / "memory") to a
// [jetstream.StorageType]. Returns an error on unknown values so typos
// surface at flag-parse time rather than silently defaulting.
func ParseStorage(s string) (jetstream.StorageType, error) {
	switch s {
	case "file":
		return jetstream.FileStorage, nil
	case "memory":
		return jetstream.MemoryStorage, nil
	default:
		return jetstream.FileStorage, fmt.Errorf("invalid storage type %q (want file|memory)", s)
	}
}

// NewInstrumentedJS connects to NATS at natsURL and returns a wrapped
// [instrumentedjs.InstrumentedJS] and the underlying [nats.Conn]. The caller
// is responsible for closing the connection when done.
func NewInstrumentedJS(natsURL string) (*instrumentedjs.InstrumentedJS, *nats.Conn, error) {
	nc, err := nats.Connect(natsURL)
	if err != nil {
		return nil, nil, fmt.Errorf("nats connect: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("jetstream.New: %w", err)
	}

	return instrumentedjs.New(js), nc, nil
}

// CreateIdleStream (re-)creates a JetStream stream with the given name,
// replication factor, and storage type, and a single wildcard subject
// `<name>.>`. Any existing stream with the same name is deleted first
// so consecutive grid points start from fresh state.
func CreateIdleStream(ctx context.Context, ijs *instrumentedjs.InstrumentedJS, name string, replicas int, storage jetstream.StorageType) (jetstream.Stream, error) {
	_ = ijs.DeleteStream(ctx, name)
	s, err := ijs.CreateStream(ctx, jetstream.StreamConfig{
		Name:     name,
		Subjects: []string{name + ".>"},
		Storage:  storage,
		Replicas: replicas,
	})
	if err != nil {
		return nil, fmt.Errorf("create stream %q: %w", name, err)
	}

	return s, nil
}

// PullConsumer wraps a single durable pull consumer + its drain goroutine.
// Stop() signals the goroutine to exit and waits for it; after Stop()
// returns it is safe to delete the consumer/stream without log spam.
type PullConsumer struct {
	Name     string
	Consumer jetstream.Consumer
	cancel   context.CancelFunc
	done     chan struct{}
}

// Stop signals the pull goroutine to exit and waits for it. Idempotent.
func (p *PullConsumer) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
	if p.done != nil {
		<-p.done
	}
}

// minPullHeartbeat and maxPullHeartbeat mirror nats.go's PullHeartbeat
// validity range [500ms, 30s] (jetstream_options.go configureConsume /
// configureMessages, nats.go v1.52.0 — the same bounds the parent module
// tracks as internal/natsutil.MinPullHeartbeat/MaxPullHeartbeat). This rig
// is a separate Go module and deliberately does not import the parent
// module's internal packages, so the constants are mirrored locally rather
// than imported.
const (
	minPullHeartbeat = 500 * time.Millisecond
	maxPullHeartbeat = 30 * time.Second
)

// boundedPullHeartbeat derives a PullHeartbeat from a pull expiry using the
// same expiry/2-clamped-to-[500ms,30s] formula as the production
// derivation, internal/natsutil.DerivePullHeartbeat — that function is the
// source of truth; this is a local mirror of its formula only (this rig has
// no PullHeartbeatCap knob, so there is no further-capping arm to mirror).
// Without the 30s clamp, a FetchTimeout above 60s derives a heartbeat above
// nats.go's ceiling, which nats.go rejects at iterator creation.
func boundedPullHeartbeat(expiry time.Duration) time.Duration {
	heartbeat := max(expiry/2, minPullHeartbeat)

	return min(heartbeat, maxPullHeartbeat)
}

// CreatePullConsumers creates n durable pull consumers on the given stream,
// each named `<prefix>-<NNNN>` (1-based, zero-padded), and starts a no-op
// drain goroutine per consumer using `consumer.Messages()` with the given
// fetch expiry. The drain goroutines discard messages — this is the M4.1
// idle scenario where no messages flow.
//
// Heartbeat is derived via boundedPullHeartbeat (expiry/2, clamped to
// [500ms, 30s]), mirroring the clamped production derivation in
// internal/natsutil.DerivePullHeartbeat so pull-iterator state-tracking
// behaves like production.
func CreatePullConsumers(ctx context.Context, stream jetstream.Stream, prefix string, n int, fetchTimeout time.Duration) ([]*PullConsumer, error) {
	out := make([]*PullConsumer, 0, n)
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("%s-%04d", prefix, i)
		cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
			Name:          name,
			Durable:       name,
			AckPolicy:     jetstream.AckExplicitPolicy,
			DeliverPolicy: jetstream.DeliverAllPolicy,
		})
		if err != nil {
			// Best-effort cleanup of already-created consumers.
			for _, pc := range out {
				pc.Stop()
			}
			return nil, fmt.Errorf("create consumer %q: %w", name, err)
		}

		runCtx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		pc := &PullConsumer{Name: name, Consumer: cons, cancel: cancel, done: done}

		heartbeat := boundedPullHeartbeat(fetchTimeout)
		it, err := cons.Messages(
			jetstream.PullMaxMessages(1),
			jetstream.PullExpiry(fetchTimeout),
			jetstream.PullHeartbeat(heartbeat),
		)
		if err != nil {
			cancel()
			close(done)
			for _, prev := range out {
				prev.Stop()
			}
			return nil, fmt.Errorf("messages iterator %q: %w", name, err)
		}

		go func() {
			defer close(done)
			// Stop the iterator when our context fires so Next() returns.
			stopOnce := make(chan struct{})
			go func() {
				select {
				case <-runCtx.Done():
					it.Stop()
				case <-stopOnce:
				}
			}()
			defer close(stopOnce)
			for {
				msg, err := it.Next()
				if err != nil {
					// Iterator stopped (Stop() called) or consumer deleted.
					return
				}
				if msg != nil {
					_ = msg.Ack()
				}
			}
		}()

		out = append(out, pc)
	}

	return out, nil
}

// LeaderOf returns the current RAFT leader name for the stream, or
// "single" if the stream has no cluster info (single-server case).
func LeaderOf(ctx context.Context, stream jetstream.Stream) (string, error) {
	info, err := stream.Info(ctx)
	if err != nil {
		return "", fmt.Errorf("stream info: %w", err)
	}
	if info.Cluster == nil || info.Cluster.Leader == "" {
		return "single", nil
	}

	return info.Cluster.Leader, nil
}

// LeaderLookup is the minimal contract PollLeader needs. It exists so
// tests can stub leadership transitions without an embedded NATS cluster.
type LeaderLookup func(context.Context) (string, error)

// PollLeader samples the leader at the given interval until ctx is
// cancelled, and returns every observed sample in chronological order.
// Lookup failures are recorded as the sentinel string "<error>" so the
// caller can tell "leader was indeterminate" apart from any real node
// name; a real leader-move is then any sample != baseline OR == "<error>".
//
// The first sample is taken immediately after one interval tick (the
// caller already captured `baseline` synchronously before starting the
// capture window). Returning the slice — rather than a bool — lets the
// caller emit a verbatim leader_samples column for forensics on retried
// points.
func PollLeader(ctx context.Context, interval time.Duration, lookup LeaderLookup) []string {
	if interval <= 0 {
		return nil
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	var samples []string
	for {
		select {
		case <-ctx.Done():
			return samples
		case <-t.C:
			// Race: ctx.Done() and t.C may both fire on the closing
			// tick; re-check the cancellation before issuing a lookup
			// so we don't record a spurious "<error>" for the cancel
			// itself.
			if ctx.Err() != nil {
				return samples
			}
			// Bound the per-sample lookup so a hung stream.Info call
			// can't extend the capture beyond the harness budget.
			lookupCtx, cancel := context.WithTimeout(ctx, interval)
			leader, err := lookup(lookupCtx)
			cancel()
			if err != nil {
				// If the parent ctx died while we were waiting on the
				// lookup, the error is the cancellation itself — not a
				// real stream.Info failure. Suppress so we don't false-
				// positive a leader move at shutdown.
				if ctx.Err() != nil {
					return samples
				}
				samples = append(samples, "<error>")
				continue
			}
			samples = append(samples, leader)
		}
	}
}

// LeaderMoved returns true if any sample in samples differs from
// baseline, or if any sample is the "<error>" sentinel (an indeterminate
// reading during the window is treated as a possible move per M4.1's
// "discard if leadership moves during capture" rule — fail loud).
func LeaderMoved(baseline string, samples []string) bool {
	for _, s := range samples {
		if s == "<error>" || s != baseline {
			return true
		}
	}
	return false
}

// RunRate calls fn at approximately rate ops/s until ctx is cancelled or
// the function returns an error. The first call is issued immediately; each
// subsequent call is delayed so the average rate is maintained. If fn takes
// longer than 1/rate seconds, the next tick fires immediately without
// backpressure accumulation.
func RunRate(ctx context.Context, rate int, fn func() error) error {
	if rate <= 0 {
		return errors.New("rate must be > 0")
	}
	interval := time.Second / time.Duration(rate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := fn(); err != nil {
				return err
			}
		}
	}
}
