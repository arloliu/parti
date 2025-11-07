package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// Checkpoint represents a snapshot of the simulation state.
type Checkpoint struct {
	Timestamp         time.Time               `json:"timestamp"`
	TotalSent         int64                   `json:"total_sent"`
	TotalReceived     int64                   `json:"total_received"`
	GapCount          int                     `json:"gap_count"`
	DuplicateCount    int                     `json:"duplicate_count"`
	PartitionStates   map[int]*PartitionState `json:"partition_states"`
	ActiveWorkers     int                     `json:"active_workers"`
	ActiveProducers   int                     `json:"active_producers"`
	SimulationRuntime time.Duration           `json:"simulation_runtime"`
}

// PartitionState represents the state of a single partition.
type PartitionState struct {
	LastSentSequence     int64  `json:"last_sent_sequence"`
	LastReceivedSequence int64  `json:"last_received_sequence"`
	ProducerID           string `json:"producer_id"`
	MessageCount         int64  `json:"message_count"`
}

// CheckpointManager manages state checkpointing and recovery.
type CheckpointManager struct {
	checkpointDir   string
	interval        time.Duration
	tracker         *MessageTracker
	startTime       time.Time
	activeWorkers   int
	activeProducers int
}

// NewCheckpointManager creates a new checkpoint manager.
//
// Parameters:
//   - checkpointDir: Directory to store checkpoints
//   - interval: Interval between checkpoints
//   - tracker: Message tracker to checkpoint
//
// Returns:
//   - *CheckpointManager: Initialized checkpoint manager
func NewCheckpointManager(checkpointDir string, interval time.Duration, tracker *MessageTracker) *CheckpointManager {
	return &CheckpointManager{
		checkpointDir: checkpointDir,
		interval:      interval,
		tracker:       tracker,
		startTime:     time.Now(),
	}
}

// Start begins periodic checkpointing.
//
// Parameters:
//   - ctx: Context for cancellation
func (cm *CheckpointManager) Start(ctx context.Context) {
	if cm.interval == 0 {
		log.Println("[Checkpoint] Checkpointing disabled")
		return
	}

	// Create checkpoint directory if it doesn't exist
	if err := os.MkdirAll(cm.checkpointDir, 0o755); err != nil {
		log.Printf("[Checkpoint] Failed to create checkpoint directory: %v", err)
		return
	}

	log.Printf("[Checkpoint] Starting checkpoint manager (interval: %v, dir: %s)",
		cm.interval, cm.checkpointDir)

	ticker := time.NewTicker(cm.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[Checkpoint] Stopping checkpoint manager")
			// Save final checkpoint
			if err := cm.SaveCheckpoint(); err != nil {
				log.Printf("[Checkpoint] Failed to save final checkpoint: %v", err)
			} else {
				log.Println("[Checkpoint] Final checkpoint saved")
			}

			return

		case <-ticker.C:
			if err := cm.SaveCheckpoint(); err != nil {
				log.Printf("[Checkpoint] Failed to save checkpoint: %v", err)
			}
		}
	}
}

// SaveCheckpoint saves the current state to disk.
//
// Returns:
//   - error: Error if checkpoint fails
func (cm *CheckpointManager) SaveCheckpoint() error {
	checkpoint := cm.createCheckpoint()

	filename := fmt.Sprintf("checkpoint_%s.json",
		time.Now().Format("20060102_150405"))
	filePath := filepath.Join(cm.checkpointDir, filename)

	data, err := json.MarshalIndent(checkpoint, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal checkpoint: %w", err)
	}

	if err := os.WriteFile(filePath, data, 0o600); err != nil {
		return fmt.Errorf("failed to write checkpoint: %w", err)
	}

	log.Printf("[Checkpoint] Saved checkpoint to %s (sent: %d, received: %d, gaps: %d)",
		filename, checkpoint.TotalSent, checkpoint.TotalReceived, checkpoint.GapCount)

	return nil
}

// LoadCheckpoint loads a checkpoint from disk.
//
// Parameters:
//   - filename: Checkpoint filename (without path)
//
// Returns:
//   - *Checkpoint: Loaded checkpoint
//   - error: Error if load fails
func (cm *CheckpointManager) LoadCheckpoint(filename string) (*Checkpoint, error) {
	filePath := filepath.Join(cm.checkpointDir, filename)

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read checkpoint: %w", err)
	}

	var checkpoint Checkpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return nil, fmt.Errorf("failed to unmarshal checkpoint: %w", err)
	}

	log.Printf("[Checkpoint] Loaded checkpoint from %s (sent: %d, received: %d, gaps: %d)",
		filename, checkpoint.TotalSent, checkpoint.TotalReceived, checkpoint.GapCount)

	return &checkpoint, nil
}

// RestoreFromCheckpoint restores the tracker state from a checkpoint.
//
// Parameters:
//   - checkpoint: Checkpoint to restore from
//
// Returns:
//   - error: Error if restore fails
func (cm *CheckpointManager) RestoreFromCheckpoint(checkpoint *Checkpoint) error {
	log.Printf("[Checkpoint] Restoring state from checkpoint (timestamp: %v, runtime: %v)",
		checkpoint.Timestamp, checkpoint.SimulationRuntime)

	// Restore partition states
	for partitionID, state := range checkpoint.PartitionStates {
		// Restore sent sequence
		for seq := int64(1); seq <= state.LastSentSequence; seq++ {
			cm.tracker.RecordSent(partitionID, state.ProducerID, seq, seq)
		}

		// Restore received sequence
		for seq := int64(1); seq <= state.LastReceivedSequence; seq++ {
			if err := cm.tracker.RecordReceived(partitionID, seq); err != nil {
				// Log but continue - errors may be expected during restore
				log.Printf("[Checkpoint] Warning during restore: %v", err)
			}
		}
	}

	// Adjust start time to account for previous runtime
	cm.startTime = time.Now().Add(-checkpoint.SimulationRuntime)

	log.Printf("[Checkpoint] State restored: %d partitions, sent=%d, received=%d",
		len(checkpoint.PartitionStates), checkpoint.TotalSent, checkpoint.TotalReceived)

	return nil
}

// FindLatestCheckpoint finds the most recent checkpoint file.
//
// Returns:
//   - string: Filename of the latest checkpoint (empty if none found)
//   - error: Error if directory read fails
func (cm *CheckpointManager) FindLatestCheckpoint() (string, error) {
	entries, err := os.ReadDir(cm.checkpointDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}

		return "", fmt.Errorf("failed to read checkpoint directory: %w", err)
	}

	var latestFile string
	var latestTime time.Time

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		if filepath.Ext(name) != ".json" {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		if info.ModTime().After(latestTime) {
			latestTime = info.ModTime()
			latestFile = name
		}
	}

	return latestFile, nil
}

// createCheckpoint creates a checkpoint from current state.
func (cm *CheckpointManager) createCheckpoint() *Checkpoint {
	stats := cm.tracker.GetStats()

	// Copy partition states
	partitionStates := make(map[int]*PartitionState)
	cm.tracker.mu.RLock()
	for partitionID, lastSent := range cm.tracker.lastSentPerPartition {
		lastReceived := cm.tracker.lastReceivedPerPartition[partitionID]
		partitionStates[partitionID] = &PartitionState{
			LastSentSequence:     lastSent,
			LastReceivedSequence: lastReceived,
			ProducerID:           "", // We don't track individual producer per partition
			MessageCount:         lastSent,
		}
	}
	cm.tracker.mu.RUnlock()

	return &Checkpoint{
		Timestamp:         time.Now(),
		TotalSent:         stats.TotalSent,
		TotalReceived:     stats.TotalReceived,
		GapCount:          stats.GapCount,
		DuplicateCount:    stats.DuplicateCount,
		PartitionStates:   partitionStates,
		ActiveWorkers:     cm.activeWorkers,
		ActiveProducers:   cm.activeProducers,
		SimulationRuntime: time.Since(cm.startTime),
	}
}

// GetCheckpointDir returns the checkpoint directory path.
//
// Returns:
//   - string: Checkpoint directory path
func (cm *CheckpointManager) GetCheckpointDir() string {
	return cm.checkpointDir
}

// SetActiveWorkers sets the current number of active workers.
//
// Parameters:
//   - count: Number of active workers
func (cm *CheckpointManager) SetActiveWorkers(count int) {
	cm.activeWorkers = count
}

// SetActiveProducers sets the current number of active producers.
//
// Parameters:
//   - count: Number of active producers
func (cm *CheckpointManager) SetActiveProducers(count int) {
	cm.activeProducers = count
}
