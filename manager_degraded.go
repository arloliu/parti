package parti

import (
	"time"

	"github.com/arloliu/parti/internal/natsutil"
	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go"
)

// monitorNATSConnection starts a goroutine that monitors NATS connectivity.
// Uses connMonitorOnce to ensure only one monitor runs per Manager instance.
func (m *Manager) monitorNATSConnection() {
	m.connMonitorOnce.Do(func() {
		m.wg.Go(func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-m.ctx.Done():
					return
				case <-m.connMonitorStop:
					return
				case <-ticker.C:
					m.checkConnectionHealth()
				}
			}
		})
	})
}

// checkConnectionHealth checks NATS connection status and updates degraded state.
func (m *Manager) checkConnectionHealth() {
	conn := m.js.Conn()
	isConnected := conn != nil && conn.Status() == nats.CONNECTED

	now := time.Now()

	if !isConnected {
		// Connection is down
		if val := m.connDownSince.Load(); val == nil {
			// First detection of disconnection
			m.connDownSince.Store(&now)
			m.connUpSince.Store((*time.Time)(nil))
			m.logger.Warn("NATS connection lost", "time", now)
		} else {
			// Check if we should enter degraded mode
			downSince, _ := val.(*time.Time)
			if downSince != nil && time.Since(*downSince) >= m.cfg.DegradedBehavior.EnterThreshold {
				m.enterDegraded("NATS connection down")
			}
		}

		return
	}

	// Connection is up
	if val := m.connUpSince.Load(); val == nil {
		// First detection of reconnection
		m.connUpSince.Store(&now)
		m.connDownSince.Store((*time.Time)(nil))
		m.logger.Info("NATS connection restored", "time", now)
	} else {
		// Check if we should exit degraded mode
		upSince, _ := val.(*time.Time)
		if upSince != nil && time.Since(*upSince) >= m.cfg.DegradedBehavior.ExitThreshold {
			m.attemptRecoveryFromDegraded()
		}
	}
}

// recordKVError records a KV operation error and may trigger degraded mode.
func (m *Manager) recordKVError(err error) {
	if err == nil || !natsutil.IsConnectivityError(err) {
		return
	}

	now := time.Now()

	m.mu.Lock()
	defer m.mu.Unlock()

	// Add error timestamp
	m.kvErrorWindow = append(m.kvErrorWindow, now)

	// Remove errors outside the window
	windowStart := now.Add(-m.cfg.DegradedBehavior.KVErrorWindow)
	validIdx := 0
	for i, t := range m.kvErrorWindow {
		if t.After(windowStart) {
			validIdx = i
			break
		}
	}
	m.kvErrorWindow = m.kvErrorWindow[validIdx:]

	// Update error count (safe conversion with bounds check)
	// Extremely unlikely, but handle overflow case
	windowLen := min(len(m.kvErrorWindow), 0x7FFFFFFF)

	count := int32(windowLen) // #nosec G115 - bounded above
	m.kvErrorCount.Store(count)

	// Check threshold
	if int(count) >= m.cfg.DegradedBehavior.KVErrorThreshold {
		m.logger.Warn("KV error threshold exceeded",
			"count", count,
			"threshold", m.cfg.DegradedBehavior.KVErrorThreshold,
			"window", m.cfg.DegradedBehavior.KVErrorWindow,
		)
		m.enterDegraded("KV error threshold exceeded")
	}
}

// recordKVSuccess records a successful KV operation and resets error count.
func (m *Manager) recordKVSuccess() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.kvErrorWindow = m.kvErrorWindow[:0]
	m.kvErrorCount.Store(0)
}

// enterDegraded transitions the manager to degraded mode.
func (m *Manager) enterDegraded(reason string) {
	// Check if already in degraded mode
	if val := m.degradedSince.Load(); val != nil {
		return
	}

	now := time.Now()
	m.degradedSince.Store(&now)

	// Update state
	oldState := State(m.state.Swap(int32(types.StateDegraded)))

	m.logger.Warn("entering degraded mode",
		"reason", reason,
		"previous_state", oldState,
		"time", now,
	)

	// Trigger state change hook
	if m.hooks.OnStateChanged != nil {
		go func() {
			if err := m.hooks.OnStateChanged(m.ctx, oldState, types.StateDegraded); err != nil {
				m.logError("state change hook error", "error", err)
			}
		}()
	}

	// Record metrics
	m.metrics.RecordStateTransition(oldState, types.StateDegraded, 0)
	m.metrics.SetDegradedMode(1.0)

	// Start alert monitoring
	m.wg.Go(m.monitorDegradedAlerts)
}

// exitDegraded transitions the manager out of degraded mode.
func (m *Manager) exitDegraded() {
	// Check if in degraded mode
	val := m.degradedSince.Load()
	if val == nil {
		return
	}

	tVal, _ := val.(*time.Time)
	duration := time.Since(*tVal)
	m.degradedSince.Store((*time.Time)(nil))

	// Restore previous state (typically Stable or WaitingAssignment)
	oldState := State(m.state.Swap(int32(StateStable)))

	m.logger.Info("exiting degraded mode",
		"duration", duration,
		"next_state", StateStable,
	)

	// Trigger state change hook
	if m.hooks.OnStateChanged != nil {
		go func() {
			if err := m.hooks.OnStateChanged(m.ctx, oldState, StateStable); err != nil {
				m.logError("state change hook error", "error", err)
			}
		}()
	}

	// Record metrics
	m.metrics.RecordStateTransition(oldState, StateStable, duration.Seconds())
	m.metrics.RecordDegradedDuration(duration.Seconds())
	m.metrics.SetDegradedMode(0.0)
	m.metrics.SetCacheAge(0.0)
	m.metrics.SetAlertLevel(0)

	// Start recovery grace period if leader
	if m.isLeader.Load() {
		m.enterRecoveryGracePeriod()
	}
}

// attemptRecoveryFromDegraded checks if recovery conditions are met and exits degraded mode.
func (m *Manager) attemptRecoveryFromDegraded() {
	// Check if in degraded mode
	if val := m.degradedSince.Load(); val == nil {
		return
	}

	// Try to refresh assignment from NATS
	if err := m.refreshAssignmentFromNATS(); err != nil {
		m.logger.Warn("failed to refresh assignment during recovery", "error", err)
		m.recordKVError(err)
		return
	}

	// Success - exit degraded mode
	m.recordKVSuccess()
	m.exitDegraded()
}

// enterRecoveryGracePeriod starts the recovery grace period for the leader.
func (m *Manager) enterRecoveryGracePeriod() {
	now := time.Now()
	m.recoveryGraceStart.Store(&now)
	m.inRecoveryGrace.Store(true)

	m.logger.Info("entering recovery grace period",
		"duration", m.cfg.DegradedBehavior.RecoveryGracePeriod,
	)

	m.wg.Go(func() {
		timer := time.NewTimer(m.cfg.DegradedBehavior.RecoveryGracePeriod)
		defer timer.Stop()

		select {
		case <-m.ctx.Done():
			return
		case <-timer.C:
			m.exitRecoveryGracePeriod()
		}
	})
}

// exitRecoveryGracePeriod ends the recovery grace period.
func (m *Manager) exitRecoveryGracePeriod() {
	if !m.inRecoveryGrace.Load() {
		return
	}

	duration := time.Duration(0)
	if val := m.recoveryGraceStart.Load(); val != nil {
		if timePtr, ok := val.(*time.Time); ok {
			duration = time.Since(*timePtr)
		}
	}

	m.recoveryGraceStart.Store((*time.Time)(nil))
	m.inRecoveryGrace.Store(false)

	m.logger.Info("exiting recovery grace period", "duration", duration)
}

// IsInRecoveryGrace returns true if currently in recovery grace period.
//
// This is part of the StateProvider interface and allows components like
// Calculator to check recovery grace status without circular dependencies.
//
// Returns:
//   - bool: true if in recovery grace period
func (m *Manager) IsInRecoveryGrace() bool {
	return m.inRecoveryGrace.Load()
}

// monitorDegradedAlerts monitors degraded mode duration and emits alerts.
func (m *Manager) monitorDegradedAlerts() {
	ticker := time.NewTicker(m.cfg.DegradedAlert.AlertInterval)
	defer ticker.Stop()

	lastAlertLevel := AlertLevelInfo - 1 // Start below Info to trigger first alert

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			// Check if still in degraded mode
			val := m.degradedSince.Load()
			if val == nil {
				return // Exited degraded mode
			}

			degradedSince, _ := val.(*time.Time)
			duration := time.Since(*degradedSince)
			level := m.calculateAlertLevel(*degradedSince)

			// Update cache age metric
			m.metrics.SetCacheAge(duration.Seconds())

			// Only emit if level increased
			if level > lastAlertLevel {
				m.emitDegradedAlert(level, *degradedSince)
				lastAlertLevel = level
			}
		}
	}
}

// emitDegradedAlert emits a degraded mode alert at the specified level.
func (m *Manager) emitDegradedAlert(level AlertLevel, degradedSince time.Time) {
	duration := time.Since(degradedSince)

	var levelName string
	switch level {
	case AlertLevelInfo:
		levelName = "info"
	case AlertLevelWarn:
		levelName = "warn"
	case AlertLevelError:
		levelName = "error"
	case AlertLevelCritical:
		levelName = "critical"
	default:
		levelName = "unknown"
	}

	m.logger.Warn("degraded mode alert",
		"level", levelName,
		"duration", duration,
		"degraded_since", degradedSince,
	)

	// Record metrics
	m.metrics.SetAlertLevel(int(level))
	m.metrics.IncrementAlertEmitted(levelName)
}

// calculateAlertLevel determines the alert level based on degraded duration.
func (m *Manager) calculateAlertLevel(degradedSince time.Time) AlertLevel {
	duration := time.Since(degradedSince)

	if duration >= m.cfg.DegradedAlert.CriticalThreshold {
		return AlertLevelCritical
	}
	if duration >= m.cfg.DegradedAlert.ErrorThreshold {
		return AlertLevelError
	}
	if duration >= m.cfg.DegradedAlert.WarnThreshold {
		return AlertLevelWarn
	}
	if duration >= m.cfg.DegradedAlert.InfoThreshold {
		return AlertLevelInfo
	}

	return AlertLevelInfo - 1 // Below Info level
}
