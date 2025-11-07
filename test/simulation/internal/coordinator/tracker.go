package coordinator

import (
	"errors"
	"fmt"
	"sync"
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
}

func (e *MessageGapError) Error() string {
	return fmt.Sprintf("message gap detected: partition=%d expected=%d received=%d",
		e.PartitionID, e.ExpectedSeq, e.ReceivedSeq)
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
	mu                       sync.RWMutex
	lastSentPerPartition     map[int]int64
	lastReceivedPerPartition map[int]int64
	lastSentPerProducer      map[string]int64
	gapCount                 int // Count of gaps detected
	duplicateCount           int // Count of duplicates detected
}

// NewMessageTracker creates a new message tracker.
//
// Returns:
//   - *MessageTracker: Initialized tracker
func NewMessageTracker() *MessageTracker {
	return &MessageTracker{
		lastSentPerPartition:     make(map[int]int64),
		lastReceivedPerPartition: make(map[int]int64),
		lastSentPerProducer:      make(map[string]int64),
		gapCount:                 0,
		duplicateCount:           0,
	}
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
func (t *MessageTracker) RecordReceived(partitionID int, partitionSeq int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	lastReceived, exists := t.lastReceivedPerPartition[partitionID]
	if !exists {
		t.lastReceivedPerPartition[partitionID] = partitionSeq
		return nil
	}

	expectedSeq := lastReceived + 1

	// Check for gap
	if partitionSeq > expectedSeq {
		t.gapCount++

		return &MessageGapError{
			PartitionID: partitionID,
			ExpectedSeq: expectedSeq,
			ReceivedSeq: partitionSeq,
		}
	}

	// Check for duplicate
	if partitionSeq <= lastReceived {
		t.duplicateCount++

		return &MessageDuplicateError{
			PartitionID: partitionID,
			Sequence:    partitionSeq,
		}
	}

	t.lastReceivedPerPartition[partitionID] = partitionSeq

	return nil
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
	TotalPartitions int
	TotalSent       int64
	TotalReceived   int64
	InFlight        int64 // Messages sent but not yet received
	GapCount        int
	DuplicateCount  int
}
