package coordinator

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// Sentinel errors for gap, duplicate, redelivery, and ownership-violation
// detection. Note that ErrMessageRedelivery is informational — not a
// failure — but is surfaced via the error channel for consistent
// errors.Is-based dispatch in the coordinator.
var (
	ErrMessageGap                   = errors.New("message gap detected")
	ErrMessageDuplicate             = errors.New("duplicate message detected")
	ErrMessageRedelivery            = errors.New("at-least-once redelivery")
	ErrMessageOwnershipViolation    = errors.New("ownership violation: same sequence processed by different workers")
	ErrMessageOwnershipInconclusive = errors.New("ownership inconclusive: cross-worker duplicate with no current owner reported")
	ErrMessageOwnershipUnobserved   = errors.New("ownership unobserved: cross-worker duplicate before any owner snapshot was ingested")
)

// MessageGapError wraps message gap details.
type MessageGapError struct {
	PartitionID int   `json:"partition_id"`
	ExpectedSeq int64 `json:"expected_seq"`
	ReceivedSeq int64 `json:"received_seq"`
	LastSent    int64 `json:"last_sent"`
}

func (e *MessageGapError) Error() string {
	return fmt.Sprintf("message gap detected: partition=%d expected=%d received=%d last_sent=%d",
		e.PartitionID, e.ExpectedSeq, e.ReceivedSeq, e.LastSent)
}

func (e *MessageGapError) Unwrap() error {
	return ErrMessageGap
}

// MessageDuplicateError wraps message duplicate details.
type MessageDuplicateError struct {
	PartitionID int
	Sequence    int64
}

func (e *MessageDuplicateError) Error() string {
	return fmt.Sprintf("duplicate message detected: partition=%d seq=%d",
		e.PartitionID, e.Sequence)
}

func (e *MessageDuplicateError) Unwrap() error {
	return ErrMessageDuplicate
}

// MessageRedeliveryEvent is returned as the error value when the same
// worker reprocesses a sequence (legitimate JetStream at-least-once
// redelivery — not a failure). The coordinator dispatches it via
// errors.Is(err, ErrMessageRedelivery) to record an informational metric
// and explicitly does NOT propagate it to the stop-on-failure path,
// DupTracer, or the failure report. The "Event" suffix signals "not a
// failure"; the type implements error solely for unified dispatch.
//
//nolint:errname // see docstring above; Event suffix is intentional
type MessageRedeliveryEvent struct {
	PartitionID int    `json:"partition_id"`
	Sequence    int64  `json:"sequence"`
	WorkerID    string `json:"worker_id"`
}

func (e *MessageRedeliveryEvent) Error() string {
	return fmt.Sprintf("redelivery: partition=%d seq=%d worker=%s",
		e.PartitionID, e.Sequence, e.WorkerID)
}

func (e *MessageRedeliveryEvent) Unwrap() error {
	return ErrMessageRedelivery
}

// MessageOwnershipViolationError signals that the same (partition, seq)
// was processed by two different workers — a parti exclusivity contract
// violation (Processing Gate or handoff regression). The coordinator
// records it into FailureReport.OwnershipViolations and treats it as a
// hard stability-invariant failure.
//
// Reason is populated when an owner-lookup callback is installed, to
// record which row of the classification table fired. Empty for legacy
// (nil-lookup) violations.
//
// ConcurrentOwners is true when the snapshot reported >1 current owner
// for the partition at the moment of the duplicate receipt — the most
// severe form of violation (assignment-layer split-brain).
type MessageOwnershipViolationError struct {
	PartitionID      int      `json:"partition_id"`
	Sequence         int64    `json:"sequence"`
	OriginalWorker   string   `json:"original_worker"`
	CurrentWorker    string   `json:"current_worker"`
	CurrentOwners    []string `json:"current_owners,omitempty"`
	Reason           string   `json:"reason,omitempty"`
	ConcurrentOwners bool     `json:"concurrent_owners,omitempty"`
}

func (e *MessageOwnershipViolationError) Error() string {
	return fmt.Sprintf("ownership violation: partition=%d seq=%d original=%s current=%s reason=%s concurrent=%t",
		e.PartitionID, e.Sequence, e.OriginalWorker, e.CurrentWorker, e.Reason, e.ConcurrentOwners)
}

func (e *MessageOwnershipViolationError) Unwrap() error {
	return ErrMessageOwnershipViolation
}

// MessageOwnershipInconclusiveError signals a cross-worker duplicate
// where the owner snapshot has been initialized but reports no current
// owner for the partition (mid-handoff window after baseline). Not a
// hard failure on its own; counts against Outcome A — a non-zero count
// means the discriminator lacked sufficient signal to classify safely.
//
//nolint:errname // matches MessageOwnershipViolationError naming for consistency
type MessageOwnershipInconclusiveError struct {
	PartitionID    int    `json:"partition_id"`
	Sequence       int64  `json:"sequence"`
	OriginalWorker string `json:"original_worker"`
	CurrentWorker  string `json:"current_worker"`
}

func (e *MessageOwnershipInconclusiveError) Error() string {
	return fmt.Sprintf("ownership inconclusive: partition=%d seq=%d original=%s current=%s",
		e.PartitionID, e.Sequence, e.OriginalWorker, e.CurrentWorker)
}

func (e *MessageOwnershipInconclusiveError) Unwrap() error {
	return ErrMessageOwnershipInconclusive
}

// MessageOwnershipUnobservedError signals a cross-worker duplicate
// during the cold-start window before any owner snapshot was ingested.
// Tolerated pre-first-ChaosEvent; counted against Outcome A
// post-first-ChaosEvent. Two distinct counters track these regimes.
//
//nolint:errname // matches MessageOwnershipViolationError naming for consistency
type MessageOwnershipUnobservedError struct {
	PartitionID    int    `json:"partition_id"`
	Sequence       int64  `json:"sequence"`
	OriginalWorker string `json:"original_worker"`
	CurrentWorker  string `json:"current_worker"`
}

func (e *MessageOwnershipUnobservedError) Error() string {
	return fmt.Sprintf("ownership unobserved: partition=%d seq=%d original=%s current=%s",
		e.PartitionID, e.Sequence, e.OriginalWorker, e.CurrentWorker)
}

func (e *MessageOwnershipUnobservedError) Unwrap() error {
	return ErrMessageOwnershipUnobserved
}

// OwnerLookupFunc returns the current owners (worker IDs) of a
// partition per the leader-reported assignment snapshot, plus a flag
// indicating whether the snapshot has been initialized. Returning
// (nil, false) means no AssignmentReport has been ingested yet
// (cold start). Returning (nil, true) or ([], true) means an empty
// owner set after the snapshot has been initialized (mid-handoff).
type OwnerLookupFunc func(partitionID int) (owners []string, snapshotInitialized bool)

// DefaultWorkerCacheMaxPerPartition bounds the per-partition seq→worker
// map. Beyond this window, ownership-violation detection falls back to
// the legacy duplicate counter (a "detection horizon", not full-history
// proof). Configurable via Coordinator.WorkerCacheMaxPerPartition.
const DefaultWorkerCacheMaxPerPartition = 4096

// MessageTracker tracks sent and received message sequences.
type MessageTracker struct {
	mu                   sync.RWMutex
	lastSentPerPartition map[int]int64
	// lastReceivedPerPartition tracks the highest contiguous sequence received per partition (no holes)
	lastReceivedPerPartition map[int]int64
	// highWatermarkPerPartition tracks the highest sequence number seen per partition (including out-of-order)
	highWatermarkPerPartition map[int]int64
	// missingPerPartition stores missing sequence numbers -> firstSeen timestamp between
	// lastReceived+1 and the high watermark for each partition. When a missing seq arrives,
	// it is removed and lastReceived may advance. Aged entries may be escalated to gaps.
	missingPerPartition map[int]map[int64]time.Time
	lastSentPerProducer map[string]int64
	gapCount            int // Count of gaps detected
	duplicateCount      int // Count of duplicates detected
	// eventReceivedCount counts every received event (including out-of-order and duplicates).
	// Useful to estimate processing throughput regardless of contiguous window advancement.
	eventReceivedCount int64
	// holesHealedCount counts how many previously missing sequences were later received.
	holesHealedCount int64
	// suppressedHolesCount counts holes that would have aged into gaps but were
	// intentionally deferred during a cooldown (quiesced drain window). These holes
	// remain eligible for escalation at shutdown if still missing, but tracking the
	// suppression provides visibility into recovery debt avoided during cooldown.
	suppressedHolesCount int64
	// suppressedMarked tracks which specific head-of-line aged holes have already
	// been counted as suppressed to avoid double-counting across cooldown ticks.
	// Keyed by partition -> sequence.
	suppressedMarked map[int]map[int64]struct{}
	// physicalReceivedCount counts the number of unique sequences physically observed
	// (excluding virtual advancement via gap escalation). This increases only when a
	// message with a new sequence arrives.
	physicalReceivedCount int64
	// gapsHealedCount counts sequences that were escalated as gaps (aged out) and
	// later physically arrived. This distinguishes late arrivals from ordinary duplicates.
	gapsHealedCount int64
	// gapEscalated tracks sequences escalated to gaps (partition -> set of seqs) so we
	// can classify their eventual arrival as a gap heal rather than a duplicate.
	// Maps sequence -> escalation time for pruning.
	gapEscalated map[int]map[int64]time.Time
	// lastWorkerPerSeq stores the workerID that first physically processed
	// each (partition, seq). Used to distinguish legitimate JetStream
	// redelivery (same worker) from a true ownership violation (different
	// worker). Per-partition map is bounded at workerCacheMax — when the
	// cap is reached, the lowest-seq entry is evicted.
	lastWorkerPerSeq map[int]map[int64]string
	// workerCacheMax caps each partition's lastWorkerPerSeq map. Zero
	// means "use DefaultWorkerCacheMaxPerPartition".
	workerCacheMax int
	// redeliveryCount counts partSeq<=lastReceived observations where the
	// reporting worker matches the original; informational.
	redeliveryCount int64
	// ownershipViolationCount counts partSeq<=lastReceived observations
	// where the reporting worker differs from the original; a hard failure.
	ownershipViolationCount int64
	// concurrentOwnersViolationCount counts violations where the owner
	// snapshot reported >1 current owner (split-brain) at the moment of
	// the duplicate receipt. Subset of ownershipViolationCount.
	concurrentOwnersViolationCount int64
	// ownershipInconclusiveCount counts cross-worker duplicates where the
	// owner snapshot was initialized but reported no current owner
	// (mid-handoff). Blocks Outcome A — see docs/plans/sim-oracle-phase5.
	ownershipInconclusiveCount int64
	// ownershipUnobservedPreChaosCount counts cross-worker duplicates
	// during cold start, before the first ChaosEvent has fired. Tolerated.
	ownershipUnobservedPreChaosCount int64
	// ownershipUnobservedPostChaosCount counts cross-worker duplicates
	// observed before any owner snapshot was ingested, but after the
	// first ChaosEvent. Always a real signal — blocks Outcome A.
	ownershipUnobservedPostChaosCount int64
	// ownerLookup is the optional owner-snapshot discriminator
	// . When nil, classifier falls back to legacy
	// origWorker-mismatch-only logic.
	ownerLookup OwnerLookupFunc
	// chaosStarted flips to true after MarkChaosStarted is called once.
	// Read by the classifier to route unobserved-owner duplicates to the
	// pre- or post-chaos bucket. atomic.Bool — no t.mu needed for reads.
	chaosStarted atomic.Bool
	// logOutOfOrder toggles verbose logging of out-of-order observations.
	// Disabled by default to avoid noisy logs when holes heal quickly.
	logOutOfOrder bool
}

// NewMessageTracker creates a new message tracker.
//
// Returns:
//   - *MessageTracker: Initialized tracker
func NewMessageTracker() *MessageTracker {
	return NewMessageTrackerWithCap(DefaultWorkerCacheMaxPerPartition)
}

// NewMessageTrackerWithCap is NewMessageTracker with an explicit
// per-partition worker-cache cap. Pass 0 to use the default.
func NewMessageTrackerWithCap(workerCacheMax int) *MessageTracker {
	if workerCacheMax <= 0 {
		workerCacheMax = DefaultWorkerCacheMaxPerPartition
	}
	return &MessageTracker{
		lastSentPerPartition:      make(map[int]int64),
		lastReceivedPerPartition:  make(map[int]int64),
		highWatermarkPerPartition: make(map[int]int64),
		missingPerPartition:       make(map[int]map[int64]time.Time),
		lastSentPerProducer:       make(map[string]int64),
		gapCount:                  0,
		duplicateCount:            0,
		logOutOfOrder:             false,
		suppressedMarked:          make(map[int]map[int64]struct{}),
		gapEscalated:              make(map[int]map[int64]time.Time),
		lastWorkerPerSeq:          make(map[int]map[int64]string),
		workerCacheMax:            workerCacheMax,
	}
}

// SetLogOutOfOrder enables or disables verbose out-of-order logging.
// Parameters:
//   - enabled: true to log out-of-order observations, false to suppress logs
func (t *MessageTracker) SetLogOutOfOrder(enabled bool) {
	t.mu.Lock()
	t.logOutOfOrder = enabled
	t.mu.Unlock()
}

// SetOwnerLookup installs the owner-snapshot discriminator .
// When non-nil, cross-worker duplicates are classified per the refined
// table in docs/plans/sim-oracle-phase5/00-plan.md §3; when nil, the
// classifier falls back to legacy origWorker-mismatch-only logic.
//
// Pass nil to disable (default).
func (t *MessageTracker) SetOwnerLookup(fn OwnerLookupFunc) {
	t.mu.Lock()
	t.ownerLookup = fn
	t.mu.Unlock()
}

// MarkChaosStarted signals that the first ChaosEvent has fired. Must be
// called by the chaos dispatch loop BEFORE invoking the chaos handler
// for the first event. Subsequent calls are no-ops. After this returns,
// any subsequent classification of an unobserved-owner duplicate counts
// toward the post-chaos bucket; before it returns, toward the pre-chaos
// bucket. Safe to call concurrently with classification.
func (t *MessageTracker) MarkChaosStarted() {
	t.chaosStarted.Store(true)
}

// RecordSent records that a message was sent.
//
// Parameters:
//   - partitionID: Partition ID
//   - producerID: Producer ID
//   - partitionSeq: Partition sequence number
//   - producerSeq: Producer sequence number
func (t *MessageTracker) RecordSent(partitionID int, producerID string, partitionSeq, producerSeq int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.lastSentPerPartition[partitionID] = partitionSeq
	t.lastSentPerProducer[producerID] = producerSeq
}

// RecordReceived is the workerID-less variant of RecordReceivedFromWorker,
// equivalent to passing workerID="". Same-worker redelivery vs ownership
// violation classification is disabled — any partSeq<=lastReceived
// observation that isn't a gap heal counts as a plain duplicate. Kept
// for tests and the checkpoint-restore replay path that have no worker
// attribution.
func (t *MessageTracker) RecordReceived(partitionID int, partitionSeq int64) ([]time.Duration, error) {
	return t.RecordReceivedFromWorker(partitionID, partitionSeq, "")
}

// RecordReceivedFromWorker records that a message was received and
// validates the sequence. When workerID is non-empty, late-arrival
// duplicates are classified as either redelivery (same worker) or
// ownership violation (different worker). When workerID is empty,
// classification falls back to the legacy duplicate counter.
//
// Parameters:
//   - partitionID: Partition ID
//   - partitionSeq: Partition sequence number
//   - workerID: Reporting worker ID; empty disables ownership classification.
//
// Returns:
//   - []time.Duration: Lifetimes for healed holes consumed while advancing the contiguous window
//   - error: One of:
//     *MessageGapError on confirmed gap (via AgeOut path; not from this call)
//     *MessageDuplicateError on pruned-fallback / empty-workerID duplicate
//     *MessageRedeliveryEvent on same-worker reprocess (informational)
//     *MessageOwnershipViolationError on different-worker reprocess (failure)
//     nil on normal arrival, contiguous advance, or gap heal
//
//nolint:cyclop // Branching mirrors the receive-classification matrix.
func (t *MessageTracker) RecordReceivedFromWorker(partitionID int, partitionSeq int64, workerID string) ([]time.Duration, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Count every receive event for throughput visibility (even if out-of-order/duplicate)
	t.eventReceivedCount++
	healedDurations := make([]time.Duration, 0)

	lastReceived, exists := t.lastReceivedPerPartition[partitionID]
	if !exists {
		// Anchor lastReceived at 0 and fall through to the regular gap /
		// duplicate detection path. The producer's first sequence is 1
		// (see producer.go), so partitionSeq==1 hits the contiguous-advance
		// branch and partitionSeq>1 enters the out-of-order branch which
		// registers [1, partitionSeq-1] as missing. Pre-fix this branch
		// seeded lastReceived=partitionSeq, which silently swallowed
		// pre-seq losses (false negative) and misclassified a later seq=1
		// arrival as a duplicate (false positive).
		lastReceived = 0
		t.lastReceivedPerPartition[partitionID] = 0
		if _, ok := t.missingPerPartition[partitionID]; !ok {
			t.missingPerPartition[partitionID] = make(map[int64]time.Time)
		}
	}

	// Ensure maps are initialized
	if _, ok := t.missingPerPartition[partitionID]; !ok {
		t.missingPerPartition[partitionID] = make(map[int64]time.Time)
	}

	// Early classification of duplicate-of-known-seq. The invariant is:
	// if partitionSeq is already in lastWorkerPerSeq, it was physically
	// observed (the only places that record into lastWorkerPerSeq are
	// the three physical-receipt paths below: out-of-order, contiguous
	// advance, gap-heal). A new receipt of the same seq is therefore a
	// duplicate, regardless of whether it would otherwise route through
	// the out-of-order branch (partitionSeq > expectedSeq, when the
	// missing window hasn't closed yet) or the post-advance branch
	// (partitionSeq <= lastReceived). Catching both paths here ensures
	// a same-seq cross-worker observation can never bypass classification
	// by being "still out-of-order".
	if origWorker, known := t.lookupOrigWorkerLocked(partitionID, partitionSeq); known {
		return healedDurations, t.classifyKnownDuplicateLocked(partitionID, partitionSeq, workerID, origWorker)
	}

	expectedSeq := lastReceived + 1

	if partitionSeq > expectedSeq {
		// Out-of-order jump ahead: we have a hole between lastReceived and partitionSeq.
		if t.logOutOfOrder {
			log.Printf("[Tracker] OUT_OF_ORDER partition=%d seq=%d last_received=%d high_watermark=%d", partitionID, partitionSeq, lastReceived, t.highWatermarkPerPartition[partitionID])
		}

		// Safety check for massive gaps to prevent OOM
		gapSize := partitionSeq - expectedSeq
		if gapSize <= 10000 {
			// Track missing range [expectedSeq, partitionSeq-1] without counting as an error yet.
			// Sequences <= prior high watermark that aren't already in `miss` were physically
			// observed earlier out-of-order (window-advance invariant); skip them or they
			// become phantom holes that block the window-advance loop and can be escalated
			// to false gaps by AgeOut.
			miss := t.missingPerPartition[partitionID]
			now := time.Now()
			hwm := t.highWatermarkPerPartition[partitionID]
			for s := expectedSeq; s < partitionSeq; s++ {
				if _, present := miss[s]; present {
					continue
				}
				if s <= hwm {
					continue
				}
				miss[s] = now
			}
			// Update high watermark
			if partitionSeq > t.highWatermarkPerPartition[partitionID] {
				t.highWatermarkPerPartition[partitionID] = partitionSeq
			}
			// Physically received this out-of-order sequence
			t.physicalReceivedCount++
			// Record worker for this out-of-order seq; future duplicates
			// of this same seq from a different worker → ownership violation.
			t.recordWorkerForSeqLocked(partitionID, partitionSeq, workerID)
			// Do not count as a gap immediately; treat as out-of-order and let holes fill
			return healedDurations, nil
		}

		log.Printf("[Tracker] MASSIVE GAP partition=%d size=%d. Skipping tracking for intermediate holes to prevent OOM.", partitionID, gapSize)
		// Treat skipped range as immediate gaps
		t.gapCount += int(gapSize)
		// Fast-forward lastReceived to skip these holes
		t.lastReceivedPerPartition[partitionID] = partitionSeq - 1
		// Fall through to normal processing as if we are now in sync
	}

	// Duplicate (origin not in lastWorkerPerSeq — pruned, empty-workerID
	// replay, or unrecorded), or gap-healed (late arrival of previously
	// escalated gap).
	if partitionSeq <= lastReceived {
		if escalatedSet, ok := t.gapEscalated[partitionID]; ok {
			if _, wasGap := escalatedSet[partitionSeq]; wasGap {
				// Late arrival heals an escalated gap
				delete(escalatedSet, partitionSeq)
				t.gapsHealedCount++
				// Count physical receipt (this sequence not previously physically observed)
				t.physicalReceivedCount++
				// Record the worker that healed; future duplicates can be
				// classified as redelivery vs violation against this id
				// (via the early-classification check at the top).
				t.recordWorkerForSeqLocked(partitionID, partitionSeq, workerID)
				log.Printf("[Tracker] GAP_HEALED partition=%d seq=%d last_received=%d high_watermark=%d", partitionID, partitionSeq, lastReceived, t.highWatermarkPerPartition[partitionID])

				return healedDurations, nil
			}
		}

		// Reaching here means partitionSeq was not in lastWorkerPerSeq AND
		// not in gapEscalated. This is the "unclassifiable duplicate"
		// fallback (origin pruned, or replayed from checkpoint with empty
		// workerID and so never recorded). Increment the legacy counter.
		log.Printf("[Tracker] DUPLICATE partition=%d seq=%d last_received=%d high_watermark=%d", partitionID, partitionSeq, lastReceived, t.highWatermarkPerPartition[partitionID])
		t.duplicateCount++

		return healedDurations, &MessageDuplicateError{PartitionID: partitionID, Sequence: partitionSeq}
	}

	// partitionSeq == expectedSeq: advance contiguous window by exactly one
	// Remove from missing if present (healed hole) and record lifetime
	if ts, ok := t.missingPerPartition[partitionID][partitionSeq]; ok {
		delete(t.missingPerPartition[partitionID], partitionSeq)
		// Also remove from suppressedMarked if present to prevent leak
		if _, ok := t.suppressedMarked[partitionID]; ok {
			delete(t.suppressedMarked[partitionID], partitionSeq)
		}

		lifetime := time.Since(ts)
		healedDurations = append(healedDurations, lifetime)
		// Hole healed counts toward holesHealed and physical received
		t.holesHealedCount++
	}
	last := partitionSeq
	t.lastReceivedPerPartition[partitionID] = last
	// Update high watermark if needed (defensive)
	if last > t.highWatermarkPerPartition[partitionID] {
		t.highWatermarkPerPartition[partitionID] = last
	}
	// Count physical receipt of this sequence (unique arrival)
	t.physicalReceivedCount++
	// Record worker for this contiguous-advance seq.
	t.recordWorkerForSeqLocked(partitionID, partitionSeq, workerID)

	// Advance window over any subsequent messages that were already received out-of-order
	for {
		next := t.lastReceivedPerPartition[partitionID] + 1
		if next > t.highWatermarkPerPartition[partitionID] {
			break
		}
		if _, missing := t.missingPerPartition[partitionID][next]; missing {
			break
		}
		// next is not missing and <= highWatermark, so it must have been received
		t.lastReceivedPerPartition[partitionID] = next
	}

	return healedDurations, nil
}

// lookupOrigWorkerLocked returns the worker that first physically processed
// (partitionID, partitionSeq), and whether the entry is known. Caller must
// hold t.mu. The empty-string and not-found cases are both reported as
// "known=false" intentionally — both route to the legacy duplicate fallback
// in classifyKnownDuplicateLocked.
func (t *MessageTracker) lookupOrigWorkerLocked(partitionID int, partitionSeq int64) (string, bool) {
	pmap, ok := t.lastWorkerPerSeq[partitionID]
	if !ok {
		return "", false
	}
	w, found := pmap[partitionSeq]
	if !found || w == "" {
		return "", false
	}

	return w, true
}

// classifyKnownDuplicateLocked is the unified classification path for a
// duplicate where the original worker is already recorded. Used by the
// early-detection branch at the top of RecordReceivedFromWorker so that
// out-of-order duplicates and post-advance duplicates are classified
// identically. Caller must hold t.mu.
//
// When ownerLookup is set , the table from
// docs/plans/sim-oracle-phase5/00-plan.md §3 is evaluated top-down to
// distinguish handoff redelivery, stale receipt, stranger receiver,
// split-brain, and the two empty-owner regimes. When ownerLookup is
// nil, the legacy origWorker-mismatch-only behavior is preserved.
//
//nolint:cyclop,gocyclo // mirrors the classification table; collapsing arms loses readability of the §3 mapping.
func (t *MessageTracker) classifyKnownDuplicateLocked(partitionID int, partitionSeq int64, workerID, origWorker string) error {
	if workerID == "" {
		// Caller doesn't know its own worker (e.g., checkpoint restore).
		// Cannot classify; fall back to legacy duplicate counter.
		log.Printf("[Tracker] DUPLICATE partition=%d seq=%d (current workerID empty; cannot classify)", partitionID, partitionSeq)
		t.duplicateCount++

		return &MessageDuplicateError{PartitionID: partitionID, Sequence: partitionSeq}
	}
	// Row 1: same worker → legitimate redelivery (caught before owner
	// lookup since this doesn't depend on the snapshot).
	if origWorker == workerID {
		t.redeliveryCount++

		return &MessageRedeliveryEvent{
			PartitionID: partitionID,
			Sequence:    partitionSeq,
			WorkerID:    workerID,
		}
	}
	// Row 2: legacy fallback when no discriminator is installed.
	if t.ownerLookup == nil {
		log.Printf("[Tracker] OWNERSHIP_VIOLATION partition=%d seq=%d original=%s current=%s (legacy)", partitionID, partitionSeq, origWorker, workerID)
		t.ownershipViolationCount++

		return &MessageOwnershipViolationError{
			PartitionID:    partitionID,
			Sequence:       partitionSeq,
			OriginalWorker: origWorker,
			CurrentWorker:  workerID,
			Reason:         "legacy_no_lookup",
		}
	}
	owners, initialized := t.ownerLookup(partitionID)
	ownersCopy := make([]string, len(owners))
	copy(ownersCopy, owners)
	// Row 3: concurrent ownership — most severe.
	if len(owners) > 1 {
		log.Printf("[Tracker] CONCURRENT_OWNERS partition=%d seq=%d original=%s current=%s owners=%v", partitionID, partitionSeq, origWorker, workerID, owners)
		t.ownershipViolationCount++
		t.concurrentOwnersViolationCount++

		return &MessageOwnershipViolationError{
			PartitionID:      partitionID,
			Sequence:         partitionSeq,
			OriginalWorker:   origWorker,
			CurrentWorker:    workerID,
			CurrentOwners:    ownersCopy,
			Reason:           "concurrent_owners",
			ConcurrentOwners: true,
		}
	}
	// Row 7/8: empty owner set — distinguish initialized (mid-handoff)
	// vs uninitialized (cold start).
	if len(owners) == 0 {
		if initialized {
			log.Printf("[Tracker] OWNERSHIP_INCONCLUSIVE partition=%d seq=%d original=%s current=%s", partitionID, partitionSeq, origWorker, workerID)
			t.ownershipInconclusiveCount++

			return &MessageOwnershipInconclusiveError{
				PartitionID:    partitionID,
				Sequence:       partitionSeq,
				OriginalWorker: origWorker,
				CurrentWorker:  workerID,
			}
		}
		if t.chaosStarted.Load() {
			t.ownershipUnobservedPostChaosCount++
		} else {
			t.ownershipUnobservedPreChaosCount++
		}
		log.Printf("[Tracker] OWNERSHIP_UNOBSERVED partition=%d seq=%d original=%s current=%s chaosStarted=%v", partitionID, partitionSeq, origWorker, workerID, t.chaosStarted.Load())

		return &MessageOwnershipUnobservedError{
			PartitionID:    partitionID,
			Sequence:       partitionSeq,
			OriginalWorker: origWorker,
			CurrentWorker:  workerID,
		}
	}
	// Rows 4-6: len(owners) == 1.
	only := owners[0]
	switch only {
	case workerID:
		// Row 4: handoff redelivery — new owner reports the partition.
		t.redeliveryCount++

		return &MessageRedeliveryEvent{
			PartitionID: partitionID,
			Sequence:    partitionSeq,
			WorkerID:    workerID,
		}
	case origWorker:
		// Row 5: stale receipt — original worker still assigned, but
		// receiver is not. Real violation.
		log.Printf("[Tracker] OWNERSHIP_VIOLATION_STALE partition=%d seq=%d original=%s current=%s owner=%s", partitionID, partitionSeq, origWorker, workerID, only)
		t.ownershipViolationCount++

		return &MessageOwnershipViolationError{
			PartitionID:    partitionID,
			Sequence:       partitionSeq,
			OriginalWorker: origWorker,
			CurrentWorker:  workerID,
			CurrentOwners:  ownersCopy,
			Reason:         "stale_receipt",
		}
	default:
		// Row 6: stranger receiver — neither worker is currently
		// assigned. Real violation.
		log.Printf("[Tracker] OWNERSHIP_VIOLATION_STRANGER partition=%d seq=%d original=%s current=%s owner=%s", partitionID, partitionSeq, origWorker, workerID, only)
		t.ownershipViolationCount++

		return &MessageOwnershipViolationError{
			PartitionID:    partitionID,
			Sequence:       partitionSeq,
			OriginalWorker: origWorker,
			CurrentWorker:  workerID,
			CurrentOwners:  ownersCopy,
			Reason:         "stranger_receiver",
		}
	}
}

// recordWorkerForSeqLocked stores the workerID that physically processed
// (partitionID, partitionSeq). Caller must hold t.mu. Empty workerID is a
// no-op (preserves the "unclassifiable" fallback semantic). When the
// per-partition map exceeds t.workerCacheMax, the smallest-seq entry is
// evicted (O(N) walk; amortized cheap since the cache only grows on unique
// physical receipts).
func (t *MessageTracker) recordWorkerForSeqLocked(partitionID int, partitionSeq int64, workerID string) {
	if workerID == "" {
		return
	}
	pmap, ok := t.lastWorkerPerSeq[partitionID]
	if !ok {
		pmap = make(map[int64]string)
		t.lastWorkerPerSeq[partitionID] = pmap
	}
	pmap[partitionSeq] = workerID
	limit := t.workerCacheMax
	if limit <= 0 {
		limit = DefaultWorkerCacheMaxPerPartition
	}
	if len(pmap) > limit {
		// Evict the smallest-seq entry.
		var minSeq int64
		first := true
		for s := range pmap {
			if first || s < minSeq {
				minSeq = s
				first = false
			}
		}
		delete(pmap, minSeq)
	}
}

// AgeOut escalates aged missing sequences to gaps up to the cutoff time.
// It advances the contiguous lastReceived window by "consuming" aged holes.
// Returns a slice of MessageGapError describing each escalated gap.
func (t *MessageTracker) AgeOut(cutoff time.Time) []error {
	t.mu.Lock()
	defer t.mu.Unlock()

	var escalations []error
	for pid := range t.missingPerPartition {
		last := t.lastReceivedPerPartition[pid]
		miss := t.missingPerPartition[pid]
		// Advance over consecutive aged holes starting from last+1
		for {
			next := last + 1
			ts, ok := miss[next]
			if !ok {
				break // cannot advance; either not missing or future unknown
			}
			if ts.After(cutoff) {
				break // not aged yet
			}
			// Escalate this missing seq as a gap and consume it
			delete(miss, next)
			// Also remove from suppressedMarked if present to prevent leak
			if _, ok := t.suppressedMarked[pid]; ok {
				delete(t.suppressedMarked[pid], next)
			}

			t.gapCount++
			// Record escalation for later gap-healed classification
			if _, ok := t.gapEscalated[pid]; !ok {
				// Lazily allocate per-partition set
				t.gapEscalated[pid] = make(map[int64]time.Time)
			}
			// Mark this sequence as an escalated gap
			if _, already := t.gapEscalated[pid][next]; !already {
				// Should not be present; defensive check avoids overwrite race
				// (we hold lock so race is unlikely, but retain semantic clarity)
				t.gapEscalated[pid][next] = time.Now()
			}
			gap := &MessageGapError{
				PartitionID: pid,
				ExpectedSeq: next,
				// Report the highest seen as the ReceivedSeq context for debugging
				ReceivedSeq: t.highWatermarkPerPartition[pid],
				LastSent:    t.lastSentPerPartition[pid],
			}
			log.Printf("[Tracker] GAP partition=%d expected=%d received_up_to=%d last_sent=%d", gap.PartitionID, gap.ExpectedSeq, gap.ReceivedSeq, gap.LastSent)
			escalations = append(escalations, gap)
			last = next
		}
		t.lastReceivedPerPartition[pid] = last

		// Advance window over any subsequent messages that were already received out-of-order
		for {
			next := t.lastReceivedPerPartition[pid] + 1
			if next > t.highWatermarkPerPartition[pid] {
				break
			}
			if _, missing := miss[next]; missing {
				break
			}
			// next is not missing and <= highWatermark, so it must have been received
			t.lastReceivedPerPartition[pid] = next
		}
	}

	return escalations
}

// CountAgedHoles returns the number of currently missing sequence numbers whose
// firstSeen timestamp is at or before the provided cutoff. It does NOT mutate
// tracker state and is safe to call frequently to assess potential escalations.
//
// Parameters:
//   - cutoff: Timestamp; holes first seen before or equal to this time are considered aged
//
// Returns:
//   - int: Count of aged holes that would qualify for gap escalation
//
// MarkHeadOfLineAgedHolesSuppressed scans each partition's head-of-line (lastReceived+1)
// and consecutive missing sequences, and for those with firstSeen <= cutoff it marks
// them as suppressed (if not already marked) and returns the number newly marked.
// This avoids double-counting the same aged holes across repeated cooldown ticks.
// Does not mutate lastReceived or missing sets; only records suppression markers.
func (t *MessageTracker) MarkHeadOfLineAgedHolesSuppressed(cutoff time.Time) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	newly := 0
	for pid, miss := range t.missingPerPartition {
		cur := t.lastReceivedPerPartition[pid]
		// Ensure per-partition suppression map exists
		if _, ok := t.suppressedMarked[pid]; !ok {
			t.suppressedMarked[pid] = make(map[int64]struct{})
		}
		// Walk consecutive head-of-line missing sequences
		for {
			next := cur + 1
			ts, ok := miss[next]
			if !ok {
				break
			}
			if ts.After(cutoff) {
				break
			}
			// If not already marked suppressed, mark now
			if _, seen := t.suppressedMarked[pid][next]; !seen {
				t.suppressedMarked[pid][next] = struct{}{}
				newly++
			}
			// Advance cursor to continue scanning consecutive HOLES
			cur = next
		}
	}

	return newly
}

// IncrementSuppressedHoles adds n to the suppressed holes counter. No-op for n<=0.
// Parameters:
//   - n: Number of aged holes suppressed this interval
func (t *MessageTracker) IncrementSuppressedHoles(n int) {
	if n <= 0 {
		return
	}
	t.mu.Lock()
	t.suppressedHolesCount += int64(n)
	t.mu.Unlock()
}

// GetStats returns current tracking statistics.
//
// Returns:
//   - TrackerStats: Current statistics
func (t *MessageTracker) GetStats() TrackerStats {
	t.mu.RLock()

	defer t.mu.RUnlock()

	totalSent := int64(0)
	for _, seq := range t.lastSentPerPartition {
		totalSent += seq
	}

	totalReceived := int64(0)
	for _, seq := range t.lastReceivedPerPartition {
		// We count only the contiguous received window; holes pending aren't counted until filled
		totalReceived += seq
	}

	// Calculate in-flight messages (sent but not yet received)
	inFlight := int64(0)
	for partitionID, sentSeq := range t.lastSentPerPartition {
		receivedSeq := t.lastReceivedPerPartition[partitionID]
		if sentSeq > receivedSeq {
			inFlight += (sentSeq - receivedSeq)
		}
	}

	return TrackerStats{
		TotalPartitions:                   len(t.lastSentPerPartition),
		TotalSent:                         totalSent,
		TotalReceived:                     totalReceived,
		InFlight:                          inFlight,
		GapCount:                          t.gapCount,
		DuplicateCount:                    t.duplicateCount,
		RedeliveryCount:                   t.redeliveryCount,
		OwnershipViolationCount:           t.ownershipViolationCount,
		ConcurrentOwnersViolationCount:    t.concurrentOwnersViolationCount,
		OwnershipInconclusiveCount:        t.ownershipInconclusiveCount,
		OwnershipUnobservedPreChaosCount:  t.ownershipUnobservedPreChaosCount,
		OwnershipUnobservedPostChaosCount: t.ownershipUnobservedPostChaosCount,
	}
}

// SetWorkerCacheMax updates the per-partition seq→worker cache cap.
// Intended to be called once after NewMessageTracker, before the tracker
// has seen significant traffic. Pass 0 to reset to the default.
func (t *MessageTracker) SetWorkerCacheMax(n int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n <= 0 {
		n = DefaultWorkerCacheMaxPerPartition
	}
	t.workerCacheMax = n
}

// GetOwnershipViolationCount returns the number of detected ownership
// violations (same-seq processed by different workers). A non-zero count
// is a hard stability-invariant failure for the simulation.
func (t *MessageTracker) GetOwnershipViolationCount() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.ownershipViolationCount
}

// GetRedeliveryCount returns the number of detected same-worker
// redeliveries. Informational; expected to be > 0 under chaos.
func (t *MessageTracker) GetRedeliveryCount() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.redeliveryCount
}

// GetConcurrentOwnersViolationCount returns the subset of ownership
// violations where the owner snapshot reported >1 current owner —
// assignment-layer split-brain.
func (t *MessageTracker) GetConcurrentOwnersViolationCount() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.concurrentOwnersViolationCount
}

// GetOwnershipInconclusiveCount returns the number of cross-worker
// duplicates classified as inconclusive (initialized snapshot reported
// no current owner). Non-zero blocks Outcome A.
func (t *MessageTracker) GetOwnershipInconclusiveCount() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.ownershipInconclusiveCount
}

// GetOwnershipUnobservedCounts returns the (pre, post) split counters
// for cross-worker duplicates observed before any owner snapshot was
// ingested. Pre-chaos is tolerated; post-chaos blocks Outcome A.
func (t *MessageTracker) GetOwnershipUnobservedCounts() (preChaos, postChaos int64) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.ownershipUnobservedPreChaosCount, t.ownershipUnobservedPostChaosCount
}

// TrackerStats represents tracker statistics.
type TrackerStats struct {
	TotalPartitions                   int   `json:"total_partitions"`
	TotalSent                         int64 `json:"total_sent"`
	TotalReceived                     int64 `json:"total_received"`
	InFlight                          int64 `json:"in_flight"` // Messages sent but not yet received
	GapCount                          int   `json:"gap_count"`
	DuplicateCount                    int   `json:"duplicate_count"`
	RedeliveryCount                   int64 `json:"redelivery_count"`
	OwnershipViolationCount           int64 `json:"ownership_violation_count"`
	ConcurrentOwnersViolationCount    int64 `json:"concurrent_owners_violation_count"`
	OwnershipInconclusiveCount        int64 `json:"ownership_inconclusive_count"`
	OwnershipUnobservedPreChaosCount  int64 `json:"ownership_unobserved_pre_chaos_count"`
	OwnershipUnobservedPostChaosCount int64 `json:"ownership_unobserved_post_chaos_count"`
}

// GetPendingHoles returns the total number of currently missing sequences across all partitions.
func (t *MessageTracker) GetPendingHoles() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var holes int64
	for _, miss := range t.missingPerPartition {
		holes += int64(len(miss))
	}

	return holes
}

// GetEventReceivedCount returns the total number of receive events observed.
func (t *MessageTracker) GetEventReceivedCount() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.eventReceivedCount
}

// GetDisorderDepth returns the maximum out-of-order depth across partitions.
// Depth is defined as highWatermark - lastReceived for a partition.
func (t *MessageTracker) GetDisorderDepth() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var maxDepth int64
	for pid, hw := range t.highWatermarkPerPartition {
		lr := t.lastReceivedPerPartition[pid]
		depth := hw - lr
		if depth > maxDepth {
			maxDepth = depth
		}
	}
	if maxDepth < 0 {
		return 0
	}

	return maxDepth
}

// GetHolesHealedCount returns the number of holes healed so far.
func (t *MessageTracker) GetHolesHealedCount() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.holesHealedCount
}

// GetPhysicalReceivedCount returns the count of unique sequences physically observed.
func (t *MessageTracker) GetPhysicalReceivedCount() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.physicalReceivedCount
}

// GetGapsHealedCount returns the number of previously escalated gaps later physically received.
func (t *MessageTracker) GetGapsHealedCount() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.gapsHealedCount
}

// GetSuppressedHolesCount returns the cumulative number of holes whose gap
// escalation was deferred during cooldown.
func (t *MessageTracker) GetSuppressedHolesCount() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.suppressedHolesCount
}

// GetPendingHoleAges returns the age of each currently pending hole (missing sequence)
// across all partitions. Age is defined as time.Since(firstSeenTimestamp).
//
// Returns:
//   - []time.Duration: Slice of ages for all pending holes at call time
func (t *MessageTracker) GetPendingHoleAges() []time.Duration {
	t.mu.RLock()
	defer t.mu.RUnlock()
	now := time.Now()
	// Pre-size slice with rough estimate (total holes) for efficiency
	total := 0
	for _, miss := range t.missingPerPartition {
		total += len(miss)
	}
	ages := make([]time.Duration, 0, total)
	for _, miss := range t.missingPerPartition {
		for _, firstSeen := range miss {
			ages = append(ages, now.Sub(firstSeen))
		}
	}

	return ages
}

// GetPartitionHoleSnapshot returns the number of pending holes and the oldest hole age
// for a specific partition at call time. If there are no holes, count=0 and oldestAge=0.
// Parameters:
//   - partitionID: Partition ID
//
// Returns:
//   - int: Count of pending holes for the partition
//   - time.Duration: Age of the oldest hole (0 if none)
func (t *MessageTracker) GetPartitionHoleSnapshot(partitionID int) (int, time.Duration) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	miss := t.missingPerPartition[partitionID]
	if len(miss) == 0 {
		return 0, 0
	}
	now := time.Now()
	var oldest time.Duration
	for _, firstSeen := range miss {
		age := now.Sub(firstSeen)
		if age > oldest {
			oldest = age
		}
	}

	return len(miss), oldest
}

// GetPartitionState returns the last sent and last received sequence numbers for a partition.
func (t *MessageTracker) GetPartitionState(partitionID int) (lastSent int64, lastReceived int64) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.lastSentPerPartition[partitionID], t.lastReceivedPerPartition[partitionID]
}

// PruneEscalatedGaps removes escalated gaps older than the specified age.
// This prevents unbounded memory growth during long simulations with data loss.
//
// Parameters:
//   - maxAge: Maximum age of escalated gaps to retain
//
// Returns:
//   - int: Number of pruned entries
func (t *MessageTracker) PruneEscalatedGaps(maxAge time.Duration) int {
	t.mu.Lock()
	defer t.mu.Unlock()

	pruned := 0
	cutoff := time.Now().Add(-maxAge)

	for pid, gaps := range t.gapEscalated {
		for seq, escalatedAt := range gaps {
			if escalatedAt.Before(cutoff) {
				delete(gaps, seq)
				pruned++
			}
		}
		// Clean up empty maps
		if len(gaps) == 0 {
			delete(t.gapEscalated, pid)
		}
	}

	return pruned
}
