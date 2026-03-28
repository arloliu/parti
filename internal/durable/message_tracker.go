package durable

import (
	"context"
	"sync"
	"time"
)

// NAKReason enumerates reasons for issuing a negative acknowledgement that triggers redelivery.
// Initial set aligns with stability plan; may expand as implementation evolves.
type NAKReason string

const (
	NAKReasonAssignment NAKReason = "assignment" // Partition not yet assigned / ownership gating
	NAKReasonHandoff    NAKReason = "handoff"    // Controlled handoff / leadership or cooldown transition
	NAKReasonBackoff    NAKReason = "backoff"    // Transient downstream/backpressure condition
	NAKReasonHealth     NAKReason = "health"     // Consumer unhealthy / degraded state
	NAKReasonOrdering   NAKReason = "ordering"   // Ordering deferral (waiting for earlier sequence)
	NAKReasonInit       NAKReason = "init"       // Initialization / cold start gating
)

// AuditResult summarizes a drain audit outcome for a partition.
// LostMessages > 0 indicates invariant breach; UnresolvedHoles are sequences still missing at audit timeout.
type AuditResult struct {
	Partition       string
	LostMessages    int
	UnresolvedHoles int
	Duration        time.Duration
	TimedOut        bool
	LateMessages    int
}

// MessageTracker defines lifecycle hooks used to enforce and measure consumer stability invariants.
// Methods are invoked by the consumer loop and processing gate.
// Implementations must be concurrency-safe.
type MessageTracker interface {
	// OnDelivery records arrival of a message sequence for a partition.
	// publishTime is the server publish timestamp or derived header time.
	OnDelivery(partition string, sequence uint64, publishTime time.Time)

	// OnNAK records a negative acknowledgement and its reason.
	OnNAK(partition string, sequence uint64, reason NAKReason)

	// OnAck records successful final processing of a message sequence.
	OnAck(partition string, sequence uint64)

	// DrainAudit performs a bounded audit to classify unresolved holes as lost.
	DrainAudit(ctx context.Context, partition string) (AuditResult, error)
}

// TrackerPolicy allows configuring latency-related policy on a tracker implementation.
// Implementations that do not support policy configuration may ignore it.
type TrackerPolicy interface {
	// ConfigurePolicy sets latency classification policy.
	ConfigurePolicy(latencyThreshold time.Duration, strict bool, allowLateTail int)
}

// noopMessageTracker is a placeholder implementation used until full tracking logic is added.
// It preserves call sites without affecting behavior.
type noopMessageTracker struct{}

// NewNoopMessageTracker creates a no-op tracker implementation.
func NewNoopMessageTracker() MessageTracker { return &noopMessageTracker{} }

func (n *noopMessageTracker) OnDelivery(_ string, _ uint64, _ time.Time) {}
func (n *noopMessageTracker) OnNAK(_ string, _ uint64, _ NAKReason)      {}
func (n *noopMessageTracker) OnAck(_ string, _ uint64)                   {}
func (n *noopMessageTracker) DrainAudit(_ context.Context, partition string) (AuditResult, error) {
	return AuditResult{Partition: partition}, nil
}

// simpleTracker is a concurrency-safe per-partition message tracker implementation.
// It maintains basic invariants without exporting metrics yet.
type simpleTracker struct {
	mu   sync.Mutex
	part map[string]*pstate
	// policy
	latencyThreshold time.Duration
	strict           bool
	allowLateTail    int
}

type pstate struct {
	mu sync.Mutex
	// sliding frontier
	highestSeen      uint64
	highestProcessed uint64

	processed  map[uint64]bool
	holes      map[uint64]struct{}
	publishTS  map[uint64]time.Time
	firstDelTS map[uint64]time.Time
	inflight   int
	lateCount  int

	// maxWindow controls how many sequence numbers we keep detailed state for
	// beyond the highestProcessed frontier. This bounds memory growth when
	// holes are never filled in noisy or long-running streams.
	maxWindow uint64
}

// NewSimpleMessageTracker constructs the concrete tracker.
func NewSimpleMessageTracker() *simpleTracker {
	return &simpleTracker{part: make(map[string]*pstate)}
}

// ConfigurePolicy sets latency policy used for late classification.
func (t *simpleTracker) ConfigurePolicy(latencyThreshold time.Duration, strict bool, allowLateTail int) {
	t.mu.Lock()
	t.latencyThreshold = latencyThreshold
	t.strict = strict
	t.allowLateTail = allowLateTail
	t.mu.Unlock()
}

func (t *simpleTracker) OnDelivery(partition string, sequence uint64, publishTime time.Time) {
	ps := t.ensure(partition)
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if sequence > ps.highestSeen {
		ps.highestSeen = sequence
	}

	// backfill holes for newly revealed gap
	if sequence > ps.highestProcessed+1 {
		for s := ps.highestProcessed + 1; s < sequence; s++ {
			if !ps.processed[s] {
				ps.holes[s] = struct{}{}
			}
		}
	}

	if _, ok := ps.publishTS[sequence]; !ok {
		ps.publishTS[sequence] = publishTime
	}
	if _, ok := ps.firstDelTS[sequence]; !ok {
		ps.firstDelTS[sequence] = time.Now()
	}
	// only increase inflight for sequences not yet fully processed
	if !ps.processed[sequence] {
		ps.inflight++
	}

	ps.pruneWindow()
}

func (t *simpleTracker) OnNAK(partition string, sequence uint64, _ NAKReason) {
	ps := t.ensure(partition)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.inflight > 0 {
		ps.inflight--
	}
}

func (t *simpleTracker) OnAck(partition string, sequence uint64) {
	ps := t.ensure(partition)
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if ps.inflight > 0 {
		ps.inflight--
	}
	ps.processed[sequence] = true
	delete(ps.holes, sequence)

	// advance contiguous frontier
	for s := ps.highestProcessed + 1; ps.processed[s]; s++ {
		delete(ps.processed, s) // GC older bit
		delete(ps.holes, s)
		ps.highestProcessed = s
	}

	// compute latency if publishTS present and classify late if over threshold
	if pub, ok := ps.publishTS[sequence]; ok {
		latency := time.Since(pub)
		t.mu.Lock()
		threshold := t.latencyThreshold
		t.mu.Unlock()
		if threshold > 0 && latency > threshold {
			ps.lateCount++
		}
		delete(ps.publishTS, sequence)
	}
	delete(ps.firstDelTS, sequence)

	ps.pruneWindow()
}

func (t *simpleTracker) DrainAudit(ctx context.Context, partition string) (AuditResult, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	start := time.Now()
	res := AuditResult{Partition: partition}

	ps := t.ensure(partition)
	for {
		ps.mu.Lock()
		inflight := ps.inflight
		holes := len(ps.holes)
		late := ps.lateCount
		ps.mu.Unlock()

		if inflight == 0 && holes == 0 {
			res.Duration = time.Since(start)
			res.LateMessages = late
			return res, nil
		}
		if time.Now().After(deadline) {
			ps.mu.Lock()
			res.UnresolvedHoles = len(ps.holes)
			res.LostMessages = res.UnresolvedHoles
			ps.mu.Unlock()
			res.Duration = time.Since(start)
			res.TimedOut = true
			res.LateMessages = late

			return res, nil
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func (t *simpleTracker) ensure(partition string) *pstate {
	t.mu.Lock()
	defer t.mu.Unlock()
	ps, ok := t.part[partition]
	if ok {
		return ps
	}

	ps = &pstate{
		mu:         sync.Mutex{},
		processed:  make(map[uint64]bool),
		holes:      make(map[uint64]struct{}),
		publishTS:  make(map[uint64]time.Time),
		firstDelTS: make(map[uint64]time.Time),
		// track a bounded window beyond highestProcessed; values here are
		// deliberately conservative to avoid impacting normal ordering while
		// still capping memory in long, disordered streams.
		maxWindow: 1000,
	}
	t.part[partition] = ps

	return ps
}

// pruneWindow drops per-sequence state that is too far behind the processed
// frontier. This prevents unbounded growth of maps when gaps are never filled.
//
// It assumes ps.mu is held by the caller.
func (ps *pstate) pruneWindow() {
	if ps.maxWindow == 0 {
		return
	}
	// All sequences <= highestProcessed are already compacted in OnAck. Here we
	// bound how far past highestProcessed we keep detailed state.
	cutoff := ps.highestProcessed + ps.maxWindow
	for seq := range ps.holes {
		if seq < cutoff {
			continue
		}
		delete(ps.holes, seq)
	}
	for seq := range ps.publishTS {
		if seq < cutoff {
			continue
		}
		delete(ps.publishTS, seq)
	}
	for seq := range ps.firstDelTS {
		if seq < cutoff {
			continue
		}
		delete(ps.firstDelTS, seq)
	}
}
