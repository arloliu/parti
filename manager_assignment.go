package parti

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment"
	"github.com/arloliu/parti/v2/kvutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// RefreshPartitions triggers partition discovery refresh.
//
// This method forces the partition source to be re-queried and, if the worker is
// the leader, triggers an immediate rebalance with the updated partition list.
// Non-leader workers will receive the updated assignments automatically.
//
// Use this when:
//   - Partitions are added/removed dynamically (e.g., Kafka topics, Redis shards)
//   - You want to redistribute work after manual partition changes
//   - Your partition source has changed but workers haven't detected it yet
//
// Parameters:
//   - ctx: Context for operation timeout
//
// Returns:
//   - error: Refresh error, or ErrNotStarted if manager isn't running
//
// Example:
//
//	// After adding new partitions to your partition source
//	if err := manager.RefreshPartitions(ctx); err != nil {
//	    log.Printf("Failed to refresh partitions: %v", err)
//	}
func (m *Manager) RefreshPartitions(ctx context.Context) error {
	// Check if manager is started
	currentState := m.State()
	if currentState == StateInit || currentState == StateShutdown {
		return types.ErrNotStarted
	}

	// Only leaders can trigger rebalancing
	// Followers will receive updated assignments automatically
	if !m.IsLeader() {
		m.logger.Info("skipping partition refresh: not leader")
		return nil
	}

	// Check if calculator is available
	m.mu.RLock()
	calc := m.calculator
	m.mu.RUnlock()

	if _, ok := calc.(*assignment.NopCalculator); ok {
		return errors.New("calculator not initialized")
	}

	m.logger.Info("refreshing partitions and triggering rebalance")

	// Trigger rebalance which will call source.ListPartitions() to get fresh partition list
	if err := calc.TriggerRebalance(ctx); err != nil {
		return fmt.Errorf("failed to trigger rebalance: %w", err)
	}

	return nil
}

// startCalculator starts the assignment calculator (leader only).
func (m *Manager) startCalculator(assignmentKV, heartbeatKV jetstream.KeyValue) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.calculator.(*assignment.Calculator); ok {
		return nil // Already started
	}

	calc, err := assignment.NewCalculator(&assignment.Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		Source:               m.source,
		Strategy:             m.strategy,
		AssignmentPrefix:     "assignment",
		HeartbeatPrefix:      "heartbeat",
		HeartbeatTTL:         m.cfg.HeartbeatTTL,
		EmergencyGracePeriod: m.cfg.EmergencyGracePeriod,
		Cooldown:             m.cfg.RebalanceCooldown,
		RestartRatio:         m.cfg.RestartDetectionRatio,
		ColdStartWindow:      m.cfg.ColdStartWindow,
		PlannedScaleWindow:   m.cfg.PlannedScaleWindow,
		Metrics:              m.metrics,
		Logger:               m.logger,
		StateProvider:        m, // Pass manager as state provider for degraded mode checks
	})
	if err != nil {
		return fmt.Errorf("failed to create calculator: %w", err)
	}

	m.calculator = calc

	// Start monitoring calculator state BEFORE starting the calculator
	// This ensures we don't miss any state transitions that happen during startup
	m.wg.Go(m.monitorCalculatorState)

	// Give the monitor goroutine a moment to set up its subscription
	// This prevents race conditions where calculator state changes before monitor is ready
	time.Sleep(10 * time.Millisecond)

	// Start calculator in background
	if err := calc.Start(m.ctx); err != nil {
		m.calculator = nil
		return fmt.Errorf("failed to start calculator: %w", err)
	}

	m.logger.Info("assignment calculator started", "worker_id", m.WorkerID())

	return nil
}

// monitorCalculatorState monitors the calculator's internal state and syncs it to Manager state.
//
// This goroutine listens to the Calculator's state change channel and updates
// the Manager's state machine accordingly. Replaces the previous polling-based
// approach (200ms ticker) with event-driven synchronization for zero-lag updates.
//
// This method runs only on the leader and translates calculator states to Manager states:
//   - types.CalcStateScaling → StateScaling
//   - types.CalcStateRebalancing → StateRebalancing
//   - types.CalcStateEmergency → StateEmergency
//   - types.CalcStateIdle (after rebalancing) → StateStable
func (m *Manager) monitorCalculatorState() {
	m.logger.Info("starting calculator state monitor")

	// Subscribe to calculator state changes with mutex protection
	m.mu.RLock()
	calc := m.calculator
	m.mu.RUnlock()

	stateCh, unsubscribe := calc.SubscribeToStateChanges()
	defer unsubscribe()

	for {
		select {
		case <-m.ctx.Done():
			m.logger.Info("calculator state monitor stopped")
			return

		case calcState, ok := <-stateCh:
			if !ok {
				m.logger.Info("calculator state channel closed, stopping monitor")
				return
			}
			// Synchronize Manager state based on Calculator state
			if err := m.syncStateFromCalculator(calcState); err != nil {
				m.logError("failed to sync state from calculator",
					"calc_state", calcState,
					"error", err,
				)
			}
		}
	}
}

// stopCalculator stops the assignment calculator, returning true if it was running.
func (m *Manager) stopCalculator() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	calc, ok := m.calculator.(*assignment.Calculator)
	if !ok {
		return false
	}

	// Before stopping, check if we need to transition state
	// If we're in a leader-only state (Scaling/Rebalancing/Emergency),
	// transition back to a follower state
	currentState := m.State()
	switch currentState {
	case StateScaling, StateRebalancing, StateEmergency:
		// Lost leadership while in leader-only state
		// Transition to Stable if we have an assignment, otherwise WaitingAssignment
		currentAssignment := m.CurrentAssignment()
		if len(currentAssignment.Partitions) > 0 {
			m.transitionState(currentState, StateStable)
			m.logger.Info("transitioned to Stable after losing leadership",
				"worker_id", m.WorkerID(),
				"from_state", currentState.String(),
			)
		} else {
			m.transitionState(currentState, StateWaitingAssignment)
			m.logger.Info("transitioned to WaitingAssignment after losing leadership",
				"worker_id", m.WorkerID(),
				"from_state", currentState.String(),
			)
		}

	default:
		// No state transition needed for non-leader states
	}

	// Stop calculator with fresh context for cleanup
	// IMPORTANT: Cannot use m.ctx here because it's already cancelled during Stop()
	// Creating a timeout from cancelled context would result in immediate cancellation
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()

	if err := calc.Stop(stopCtx); err != nil {
		m.logError("failed to stop calculator", "error", err)
	}

	m.calculator = assignment.NewNopCalculator()

	return true
}

// calculateAndPublish calculates and publishes assignments.
func (m *Manager) calculateAndPublish(ctx context.Context) error {
	m.mu.RLock()
	calc := m.calculator
	m.mu.RUnlock()

	if _, ok := calc.(*assignment.NopCalculator); ok {
		return errors.New("calculator not started")
	}

	// Calculator runs in background and publishes automatically.
	// Wait briefly for the initial calculation, respecting the context.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(500 * time.Millisecond):
	}

	return nil
}

// fetchAssignment fetches the assignment for this worker from KV.
func (m *Manager) fetchAssignment(ctx context.Context, kv jetstream.KeyValue) (*Assignment, error) {
	workerID := m.WorkerID()
	key := fmt.Sprintf("assignment.%s", workerID) // Match calculator's key format

	asgn, _, err := kvutil.GetJSON[Assignment](ctx, kv, key)
	if err != nil {
		return nil, fmt.Errorf("failed to get assignment: %w", err)
	}

	return asgn, nil
}

// monitorAssignmentChanges monitors for assignment changes.
func (m *Manager) monitorAssignmentChanges(ctx context.Context, kv jetstream.KeyValue) {
	workerID := m.WorkerID()
	key := fmt.Sprintf("assignment.%s", workerID) // Match calculator's key format

	// Watch for updates to this worker's assignment key
	// The watcher will deliver initial value, then a nil entry marker, then future updates
	watcher, err := kv.Watch(ctx, key)
	if err != nil {
		m.logError("failed to watch assignments", "error", err)

		return
	}

	defer func() {
		if err := watcher.Stop(); err != nil {
			m.logError("failed to stop watcher", "error", err)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			m.logger.Debug("assignment monitor stopping (context cancelled)", "worker_id", workerID)
			return
		case entry, ok := <-watcher.Updates():
			if !ok {
				m.logger.Debug("assignment watcher closed", "worker_id", workerID)
				return
			}
			if entry == nil {
				// Nil entry indicates end of initial values replay
				// This is normal - continue watching for future updates
				continue
			}

			m.handleAssignmentEntry(workerID, entry)
		}
	}
}

func (m *Manager) handleAssignmentEntry(workerID string, entry jetstream.KeyValueEntry) {
	if entry.Operation() == jetstream.KeyValueDelete {
		m.logger.Debug("ignoring assignment deletion during leader transition")
		return
	}

	newAssignment, ok := m.decodeAssignmentEntry(entry)
	if !ok {
		return
	}

	oldAssignment := m.CurrentAssignment()
	if oldAssignment.Version >= newAssignment.Version {
		return
	}

	m.applyAssignmentUpdate(workerID, oldAssignment, newAssignment)
}

func (m *Manager) decodeAssignmentEntry(entry jetstream.KeyValueEntry) (Assignment, bool) {
	var newAssignment Assignment
	if err := json.Unmarshal(entry.Value(), &newAssignment); err != nil {
		m.logError("failed to unmarshal assignment", "error", err)
		return Assignment{}, false
	}

	return newAssignment, true
}

func (m *Manager) applyAssignmentUpdate(workerID string, oldAssignment, newAssignment Assignment) {
	m.assignment.Store(newAssignment)

	m.logger.Info("assignment updated",
		"worker_id", workerID,
		"old_version", oldAssignment.Version,
		"new_version", newAssignment.Version,
		"old_partitions", len(oldAssignment.Partitions),
		"new_partitions", len(newAssignment.Partitions),
	)

	m.applyHandoffAndHooks(workerID, oldAssignment, newAssignment)
	m.recordAssignmentMetrics(oldAssignment, newAssignment)
}

func (m *Manager) applyHandoffAndHooks(workerID string, oldAssignment, newAssignment Assignment) {
	// 1) Apply consumer update SYNCHRONOUSLY before invoking user hooks.
	// This ensures the NATS consumer filter subjects are updated before
	// application code in OnAssignmentChanged runs, preventing a race
	// condition where the app expects to receive messages for new partitions
	// but the subscription isn't active yet.
	if err := m.handoffCoordinator.Apply(m.ctx, workerID, oldAssignment, newAssignment); err != nil {
		m.logError("handoff apply failed", "error", err)
		// Continue to invoke user hooks even on failure - the assignment
		// is already stored, and hooks may need to react to it.
	}

	// 2) Invoke OnAssignmentChanged hook (async to avoid blocking monitor)
	if m.hooks.OnAssignmentChanged != nil {
		m.invokeHook("assignment change", func() error {
			return m.hooks.OnAssignmentChanged(m.ctx, oldAssignment.Partitions, newAssignment.Partitions)
		})
	}

	// 3) Invoke convenience hooks (Assigned/Revoked)
	if m.hooks.OnPartitionsAssigned != nil || m.hooks.OnPartitionsRevoked != nil {
		m.invokeHook("partition hooks", func() error {
			added, removed := diffPartitions(oldAssignment.Partitions, newAssignment.Partitions)

			if len(added) > 0 && m.hooks.OnPartitionsAssigned != nil {
				if err := m.hooks.OnPartitionsAssigned(m.ctx, added); err != nil {
					m.logError("partitions assigned hook error", "error", err)
				}
			}

			if len(removed) > 0 && m.hooks.OnPartitionsRevoked != nil {
				if err := m.hooks.OnPartitionsRevoked(m.ctx, removed); err != nil {
					m.logError("partitions revoked hook error", "error", err)
				}
			}

			return nil
		})
	}
}

func (m *Manager) recordAssignmentMetrics(oldAssignment, newAssignment Assignment) {
	added := len(newAssignment.Partitions) - len(oldAssignment.Partitions)
	if added < 0 {
		added = 0
	}
	removed := len(oldAssignment.Partitions) - len(newAssignment.Partitions)
	if removed < 0 {
		removed = 0
	}
	m.metrics.RecordAssignmentChange(added, removed, newAssignment.Version)
}

// refreshAssignmentFromNATS attempts to fetch the current assignment from NATS KV.
func (m *Manager) refreshAssignmentFromNATS() error {
	workerID := m.WorkerID()
	if workerID == "" {
		return errors.New("worker ID not set")
	}

	key := fmt.Sprintf("assignment.%s", workerID)
	entry, err := m.assignmentKV.Get(m.ctx, key)
	if err != nil {
		return fmt.Errorf("failed to get assignment from KV: %w", err)
	}

	var curAssignment Assignment
	if err := json.Unmarshal(entry.Value(), &curAssignment); err != nil {
		return fmt.Errorf("failed to unmarshal assignment: %w", err)
	}

	now := time.Now()
	m.assignment.Store(curAssignment)
	m.lastAssignmentAt.Store(&now)
	m.lastAssignment.Store(m.clonePartitions(curAssignment.Partitions))

	m.logger.Info("assignment refreshed from NATS",
		"version", curAssignment.Version,
		"partitions", len(curAssignment.Partitions),
	)

	return nil
}

// clonePartitions creates a deep copy of partition slice.
func (m *Manager) clonePartitions(partitions []Partition) []Partition {
	if partitions == nil {
		return nil
	}

	cloned := make([]Partition, len(partitions))
	for i, p := range partitions {
		cloned[i] = Partition{
			Keys:   append([]string(nil), p.Keys...),
			Weight: p.Weight,
		}
	}

	return cloned
}

// diffPartitions calculates added and removed partitions between two sets.
func diffPartitions(oldPartitions, newPartitions []Partition) (added, removed []Partition) {
	oldMap := make(map[string]Partition, len(oldPartitions))
	for _, p := range oldPartitions {
		oldMap[p.ID()] = p
	}

	newMap := make(map[string]Partition, len(newPartitions))
	for _, p := range newPartitions {
		newMap[p.ID()] = p
	}

	for _, p := range newPartitions {
		if _, exists := oldMap[p.ID()]; !exists {
			added = append(added, p)
		}
	}

	for _, p := range oldPartitions {
		if _, exists := newMap[p.ID()]; !exists {
			removed = append(removed, p)
		}
	}

	return added, removed
}
