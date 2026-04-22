package parti

import (
	"time"

	"github.com/arloliu/parti/v2/internal/natsutil"
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
		if m.connDownSince.Load() == 0 {
			// First detection of disconnection
			m.connDownSince.Store(now.UnixNano())
			m.connUpSince.Store(0)
			m.logger.Warn("NATS connection lost", "time", now)
		} else {
			// Check if we should enter degraded mode
			downSince := time.Unix(0, m.connDownSince.Load())
			if time.Since(downSince) >= m.cfg.DegradedBehavior.EnterThreshold {
				m.enterDegraded("NATS connection down")
			}
		}

		return
	}

	// Connection is up
	if m.connUpSince.Load() == 0 {
		// First detection of reconnection
		m.connUpSince.Store(now.UnixNano())
		m.connDownSince.Store(0)
		m.logger.Info("NATS connection restored", "time", now)
	} else {
		// Check if we should exit degraded mode
		upSince := time.Unix(0, m.connUpSince.Load())
		if time.Since(upSince) >= m.cfg.DegradedBehavior.ExitThreshold {
			m.attemptRecoveryFromDegraded()
		}
	}
}

// recordKVError records a KV operation error and may trigger degraded mode.
//
// Errors that drive the circuit are either connectivity failures (the NATS
// connection itself is down) or degrading JetStream errors (bucket/stream/
// consumer missing while the connection remains up — e.g. operator wipe,
// non-replicated JetStream data loss). Sustained NotFound without connection
// loss would otherwise produce a silent-drift failure mode where publishes
// fail silently and no state transition occurs.
func (m *Manager) recordKVError(err error) {
	if err == nil {
		return
	}
	if !natsutil.IsConnectivityError(err) && !natsutil.IsDegradingJetStreamError(err) {
		return
	}
	// Short-circuit once already Degraded. Every subsystem (heartbeat 500ms,
	// election ticks, stableID renew, assignment watcher, attemptRecoveryFromDegraded
	// at 1s) retries against the same failure indefinitely; without this we would
	// re-enter the locked window-append + threshold-warn on every call and grow
	// kvErrorWindow unboundedly until the pod restarts. degradedSince is cleared
	// atomically by exitDegraded, so recovery is unaffected.
	if m.degradedSince.Load() != 0 {
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
//
// Uses a CAS on degradedSince (int64 UnixNano; 0 = unset) as the entry gate,
// ensuring exactly one goroutine performs the transition even under concurrent calls.
// After a successful exitDegraded, degradedSince is reset to 0, so future
// enterDegraded calls succeed without the re-entry bug that typed-nil atomic.Value
// storage caused previously.
//
// Lock contract: must not acquire m.mu. Callers (notably recordKVError) may
// already hold m.mu; taking it here would self-deadlock.
func (m *Manager) enterDegraded(reason string) {
	// Reject degraded entry from terminal Shutdown state.
	if m.State() == StateShutdown {
		return
	}

	// Atomically claim the degraded-entry slot.
	// If degradedSince is already non-zero, we (or another goroutine) already entered.
	now := time.Now()
	if !m.degradedSince.CompareAndSwap(0, now.UnixNano()) {
		return
	}

	// Attempt validated state transition. Roll back degradedSince on failure
	// (can only happen from Init/ClaimingID states that forbid degraded entry).
	if !m.transitionState(StateDegraded) {
		m.degradedSince.Store(0)
		return
	}

	m.logger.Warn("entering degraded mode",
		"reason", reason,
		"time", now,
	)

	// OnDegraded hook (OnStateChanged already fired by transitionState)
	if m.hooks.OnDegraded != nil {
		m.invokeHook("degraded", func() error {
			return m.hooks.OnDegraded(m.ctx, reason)
		})
	}

	// Record degraded mode metric (state transition already recorded by transitionState)
	m.metrics.SetDegradedMode(1.0)

	// Start alert monitoring
	m.wg.Go(m.monitorDegradedAlerts)
}

// exitDegraded transitions the manager out of degraded mode.
func (m *Manager) exitDegraded() {
	since := m.degradedSince.Load()
	if since == 0 {
		return
	}

	duration := time.Since(time.Unix(0, since))

	// Transition out of degraded mode via validated CAS.
	// If already Shutdown, transitionState returns false and we skip cleanup.
	if !m.transitionState(StateStable) {
		return
	}

	// Clear degradedSince after a successful state transition so that future
	// enterDegraded calls can re-arm correctly (fixes the re-entry bug).
	m.degradedSince.Store(0)

	m.logger.Info("exiting degraded mode",
		"duration", duration,
		"next_state", StateStable,
	)

	// Record metrics (state transition already recorded by transitionState)
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
	if m.degradedSince.Load() == 0 {
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
	m.recoveryGraceStart.Store(time.Now().UnixNano())
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
	if startNano := m.recoveryGraceStart.Load(); startNano != 0 {
		duration = time.Since(time.Unix(0, startNano))
	}

	m.recoveryGraceStart.Store(0)
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
			since := m.degradedSince.Load()
			if since == 0 {
				return // Exited degraded mode
			}

			degradedAt := time.Unix(0, since)
			duration := time.Since(degradedAt)
			level := m.calculateAlertLevel(degradedAt)

			// Update cache age metric
			m.metrics.SetCacheAge(duration.Seconds())

			// Only emit if level increased
			if level > lastAlertLevel {
				m.emitDegradedAlert(level, degradedAt)
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
