package coordinator

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// Sentinel errors for gap and duplicate detection.
var (
	ErrMessageGap       = errors.New("message gap detected")
	ErrMessageDuplicate = errors.New("duplicate message detected")
)

// MessageGapError wraps message gap details.
type MessageGapError struct {
	PartitionID int
	ExpectedSeq int64
	ReceivedSeq int64
	LastSent    int64
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
	// logOutOfOrder toggles verbose logging of out-of-order observations.
	// Disabled by default to avoid noisy logs when holes heal quickly.
	logOutOfOrder bool
}

// NewMessageTracker creates a new message tracker.
//
// Returns:
//   - *MessageTracker: Initialized tracker
func NewMessageTracker() *MessageTracker {
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

// RecordReceived records that a message was received and validates sequence.
//
// Parameters:
//   - partitionID: Partition ID
//   - partitionSeq: Partition sequence number
//
// Returns:
//   - error: Error if gap or duplicate detected
//
// RecordReceived records that a message was received and validates sequence.
// It returns a slice of durations representing healed holes lifetimes (time from firstSeen to arrival).
//
// Parameters:
//   - partitionID: Partition ID
//   - partitionSeq: Partition sequence number
//
// Returns:
//   - []time.Duration: Lifetimes for healed holes consumed while advancing the contiguous window
//   - error: Error if gap or duplicate detected
func (t *MessageTracker) RecordReceived(partitionID int, partitionSeq int64) ([]time.Duration, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Count every receive event for throughput visibility (even if out-of-order/duplicate)
	t.eventReceivedCount++
	healedDurations := make([]time.Duration, 0)

	lastReceived, exists := t.lastReceivedPerPartition[partitionID]
	if !exists {
		// Initialize structures for this partition
		t.lastReceivedPerPartition[partitionID] = partitionSeq
		if _, ok := t.missingPerPartition[partitionID]; !ok {
			t.missingPerPartition[partitionID] = make(map[int64]time.Time)
		}
		if partitionSeq > t.highWatermarkPerPartition[partitionID] {
			t.highWatermarkPerPartition[partitionID] = partitionSeq
		}
		// First physical arrival for this partition
		t.physicalReceivedCount++

		return healedDurations, nil
	}

	// Ensure maps are initialized
	if _, ok := t.missingPerPartition[partitionID]; !ok {
		t.missingPerPartition[partitionID] = make(map[int64]time.Time)
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
			// Track missing range [expectedSeq, partitionSeq-1] without counting as an error yet
			miss := t.missingPerPartition[partitionID]
			now := time.Now()
			for s := expectedSeq; s < partitionSeq; s++ {
				if _, exists := miss[s]; !exists {
					miss[s] = now
				}
			}
			// Update high watermark
			if partitionSeq > t.highWatermarkPerPartition[partitionID] {
				t.highWatermarkPerPartition[partitionID] = partitionSeq
			}
			// Physically received this out-of-order sequence
			t.physicalReceivedCount++
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

	// Duplicate, or gap-healed (late arrival of previously escalated gap)
	if partitionSeq <= lastReceived {
		if escalatedSet, ok := t.gapEscalated[partitionID]; ok {
			if _, wasGap := escalatedSet[partitionSeq]; wasGap {
				// Late arrival heals an escalated gap
				delete(escalatedSet, partitionSeq)
				t.gapsHealedCount++
				// Count physical receipt (this sequence not previously physically observed)
				t.physicalReceivedCount++
				log.Printf("[Tracker] GAP_HEALED partition=%d seq=%d last_received=%d high_watermark=%d", partitionID, partitionSeq, lastReceived, t.highWatermarkPerPartition[partitionID])
				return healedDurations, nil
			}
		}
		// True duplicate (already in contiguous window and not a gap heal)
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
		TotalPartitions: len(t.lastSentPerPartition),
		TotalSent:       totalSent,
		TotalReceived:   totalReceived,
		InFlight:        inFlight,
		GapCount:        t.gapCount,
		DuplicateCount:  t.duplicateCount,
	}
}

// TrackerStats represents tracker statistics.
type TrackerStats struct {
	TotalPartitions int   `json:"total_partitions"`
	TotalSent       int64 `json:"total_sent"`
	TotalReceived   int64 `json:"total_received"`
	InFlight        int64 `json:"in_flight"` // Messages sent but not yet received
	GapCount        int   `json:"gap_count"`
	DuplicateCount  int   `json:"duplicate_count"`
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
