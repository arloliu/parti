# Degraded Mode Implementation Plan

## Executive Summary

Implement a resilient degraded mode that allows the system to continue operating with cached data during NATS connectivity failures, preventing unnecessary service disruptions while providing clear operational visibility.

**Core Principle**: "Stale but stable" > "Fresh but broken"

## Problem Statement

### Current Behavior (Without Degraded Mode)

1. **Manager (Workers)**:
   - NATS connection loss is silent - no awareness of disconnection
   - Workers continue processing but health checks report "healthy"
   - No alerts fired to operators
   - Frozen assignments are accidental, not intentional

2. **Calculator (Leader)**:
   - KV read failures abort rebalancing immediately
   - No cache fallback for worker list
   - Empty worker list from errors could trigger false emergency
   - Watcher exits permanently on creation failure

3. **Risk**: Brief NATS outage causes cascading failures and unnecessary rebalancing

### Goals

1. **Resilience**: Continue serving with cached data during NATS outages
2. **Visibility**: Clear operational signals via state, metrics, and hooks
3. **Stability**: Never invalidate cache or trigger false emergencies
4. **Alerting**: Graduated escalation from info → warn → error → critical

## Design Decisions

### 1. Naming: "Degraded"

**Selected**: `StateDegraded`

**Rationale**:
- Industry standard (Kubernetes, AWS, distributed systems)
- Accurately describes "working but impaired"
- Clear operational meaning for SREs
- Positive framing (still functioning)

### 2. State Model: Hybrid Approach

**Manager**: Add new `StateDegraded` state
- **Why**: User-visible lifecycle change requiring operational awareness
- **Duration**: Long-lived (minutes to hours)
- **Observable**: Via health checks, metrics, hooks

**Calculator**: Error handling with cache fallback only
- **Why**: Internal rebalancing workflow, not user-facing
- **Duration**: Transient (seconds)
- **Internal**: No new workflow state needed

### 3. Cache Philosophy: Valid Forever

**Key Decision**: Cache is NEVER invalidated due to age

**Rationale**:
- Better to serve stale data than disrupt service
- Operators can manually intervene if needed
- Alert escalation provides visibility without breaking assignments

**Cache Staleness Threshold**: For alerting only, NOT invalidation
- Purpose: Escalate alert severity based on age
- Default: 5 minutes → error level, 30 minutes → critical
- Never triggers cache expiry or state transition to Emergency

### 4. State Transitions

```
Normal flow:
  Init → ClaimingID → Election → WaitingAssignment → Stable

Degradation:
  Stable → Degraded (NATS connection lost)
  Degraded → Stable (connection restored + fresh data obtained)

Mid-rebalance degradation:
  Rebalancing → Degraded (connection lost during rebalance)
  Degraded → Stable (recovery with fresh data)

Shutdown (always available):
  Degraded → Shutdown (graceful shutdown works in degraded mode)

NO automatic escalation:
  Degraded → Emergency (REMOVED - never escalate from staleness)
```

**Emergency state** should ONLY trigger from:
- Actual worker crashes detected via fresh KV data
- Explicit operator intervention
- NEVER from connectivity issues or stale cache

## Implementation Plan

### Phase 1: Core State & Types (4 hours)

#### 1.1 Add State Constant

**File**: `types/state.go`

```go
const (
	StateInit State = iota
	StateClaimingID
	StateElection
	StateWaitingAssignment
	StateStable
	StateScaling
	StateRebalancing
	StateEmergency
	StateDegraded  // Operating with cached data, NATS unavailable
	StateShutdown
)

func (s State) String() string {
	switch s {
	// ... existing cases ...
	case StateDegraded:
		return "Degraded"
	// ... rest of cases ...
	}
}
```

#### 1.2 Add Error Types

**File**: `types/errors.go`

```go
// Connectivity and degraded mode errors
var (
	// ErrConnectivity indicates a NATS/KV connectivity issue.
	// This is used to distinguish network failures from application errors.
	ErrConnectivity = errors.New("connectivity issue")

	// ErrDegraded indicates the system is operating in degraded mode.
	// Operations may use cached data due to unavailable NATS connection.
	ErrDegraded = errors.New("degraded operation: using cached data")
)
```

**File**: `internal/natsutil/errors.go` (new)

```go
package natsutil

import (
	"errors"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/arloliu/parti/types"
)

// IsConnectivityError checks if an error is caused by connectivity issues.
//
// This includes NATS timeouts, connection refused, disconnections, etc.
// Used to determine when to enter degraded mode and use cached data.
//
// Kept in internal/natsutil to avoid importing NATS dependencies in types/ package.
//
// Parameters:
//   - err: Error to check
//
// Returns:
//   - bool: true if error indicates connectivity issue
func IsConnectivityError(err error) bool {
	if err == nil {
		return false
	}

	// Check for known connectivity error types
	return errors.Is(err, types.ErrConnectivity) ||
		errors.Is(err, nats.ErrTimeout) ||
		errors.Is(err, nats.ErrNoServers) ||
		errors.Is(err, nats.ErrDisconnected) ||
		errors.Is(err, nats.ErrConnectionClosed) ||
		errors.Is(err, jetstream.ErrNoStreamResponse) ||
		strings.Contains(err.Error(), "connection refused") ||
		strings.Contains(err.Error(), "i/o timeout")
}
```

#### 1.3 Add Configuration Types

**File**: `types/config.go` or `config.go`

```go
// DegradedAlertConfig defines alert escalation thresholds for degraded mode.
//
// These thresholds control when to escalate alerts during NATS connectivity issues.
// They do NOT affect system behavior - assignments remain frozen indefinitely.
//
// The alert levels escalate based on how long the system has been in degraded mode:
//   - InfoThreshold: Initial notification of degraded state
//   - WarnThreshold: Extended connectivity issue
//   - ErrorThreshold: Significant issue requiring attention
//   - CriticalThreshold: Prolonged outage requiring intervention
//
// Example:
//
//	config := &parti.Config{
//	    DegradedAlerts: &parti.DegradedAlertConfig{
//	        InfoThreshold:     30 * time.Second,
//	        WarnThreshold:     2 * time.Minute,
//	        ErrorThreshold:    5 * time.Minute,
//	        CriticalThreshold: 30 * time.Minute,
//	        AlertInterval:     30 * time.Second,
//	    },
//	}
type DegradedAlertConfig struct {
	// InfoThreshold triggers info-level alerts after this duration in degraded mode.
	// Use logger.Info, set severity field to info
	// Default: 30 seconds
	InfoThreshold time.Duration

	// WarnThreshold escalates to warning level after this duration.
	// Use logger.Warn, set severity field to warning
	// Default: 2 minutes
	WarnThreshold time.Duration

	// ErrorThreshold escalates to error level after this duration.
	// Use logger.Error, set severity field to error
	// Default: 5 minutes
	ErrorThreshold time.Duration

	// CriticalThreshold triggers critical alerts (paging) after this duration.
	// Use logger.Error, set severity field to critical
	// Default: 30 minutes
	// Set to 0 to disable critical alerts.
	CriticalThreshold time.Duration

	// AlertInterval controls how often to emit alerts while degraded.
	// Default: 30 seconds
	// Prevents log spam while maintaining visibility.
	AlertInterval time.Duration
}

// DefaultDegradedAlertConfig returns sensible defaults for alert escalation.
//
// Default escalation timeline:
//   - 0-30s: No alerts (allows transient issues to resolve)
//   - 30s-2m: Info level (temporary connectivity issue)
//   - 2m-5m: Warning level (extended connectivity issue)
//   - 5m-30m: Error level (significant issue)
//   - 30m+: Critical level (requires manual intervention)
//
// Returns:
//   - *DegradedAlertConfig: Configuration with production-ready defaults
func DefaultDegradedAlertConfig() *DegradedAlertConfig {
	return &DegradedAlertConfig{
		InfoThreshold:     30 * time.Second,   // Give transient issues time to resolve
		WarnThreshold:     2 * time.Minute,    // More buffer before warning
		ErrorThreshold:    5 * time.Minute,    // Significant issue requiring attention
		CriticalThreshold: 30 * time.Minute,   // More time before paging
		AlertInterval:     30 * time.Second,
	}
}

// DegradedBehaviorConfig controls degraded mode detection and recovery behavior.
//
// These settings affect when the system enters/exits degraded mode and how
// it handles recovery scenarios. Defaults work for most environments, but
// fine-tuning may be needed for specific network conditions or SLA requirements.
//
// Example:
//
//	config := &parti.Config{
//	    DegradedBehavior: &parti.DegradedBehaviorConfig{
//	        EnterThreshold:      15 * time.Second,  // More tolerant to transient issues
//	        ExitThreshold:       3 * time.Second,   // Faster recovery
//	        KVErrorThreshold:    10,                // Less sensitive to KV errors
//	        RecoveryGracePeriod: 20 * time.Second,  // Longer split-brain protection
//	    },
//	}
type DegradedBehaviorConfig struct {
	// EnterThreshold is how long the connection must be down before entering degraded mode.
	// Prevents transient network blips from triggering degraded state.
	// Default: 10 seconds
	EnterThreshold time.Duration

	// ExitThreshold is how long the connection must be up before exiting degraded mode.
	// Prevents rapid state flapping during unstable connectivity.
	// Default: 5 seconds
	ExitThreshold time.Duration

	// KVErrorThreshold is the number of KV errors in KVErrorWindow to trigger degraded.
	// Allows degraded mode even when IsConnected() reports true but KV operations fail.
	// Default: 5 errors
	KVErrorThreshold int

	// KVErrorWindow is the time window for counting KV errors.
	// Default: 30 seconds
	KVErrorWindow time.Duration

	// RecoveryGracePeriod is how long after exiting degraded before emergency detection resumes.
	// Prevents split-brain scenarios where leader recovers before other workers.
	// During this period, the leader won't misinterpret delayed worker recovery as crashes.
	// Default: 15 seconds
	RecoveryGracePeriod time.Duration
}

// DefaultDegradedBehaviorConfig returns sensible defaults for degraded mode behavior.
//
// These defaults balance responsiveness with stability:
//   - 10s enter threshold: Tolerates brief network issues without state change
//   - 5s exit threshold: Quick recovery without flapping
//   - 5 errors in 30s: Detects sustained KV problems
//   - 15s grace period: Prevents split-brain false emergencies
//
// Returns:
//   - *DegradedBehaviorConfig: Configuration with production-ready defaults
func DefaultDegradedBehaviorConfig() *DegradedBehaviorConfig {
	return &DegradedBehaviorConfig{
		EnterThreshold:      10 * time.Second,
		ExitThreshold:       5 * time.Second,
		KVErrorThreshold:    5,
		KVErrorWindow:       30 * time.Second,
		RecoveryGracePeriod: 15 * time.Second,
	}
}

// DegradedBehaviorPreset returns pre-configured behavior settings for common scenarios.
//
// Presets simplify configuration for typical use cases while allowing
// fine-grained control when needed.
//
// Available presets:
//   - "conservative": Tolerant to transient issues, slower to enter/exit degraded
//   - "balanced": Default behavior, good for most environments (same as DefaultDegradedBehaviorConfig)
//   - "aggressive": Quick detection and recovery, suitable for stable networks with low tolerance
//
// Example:
//
//	config := &parti.Config{
//	    DegradedBehavior: parti.DegradedBehaviorPreset("conservative"),
//	}
//
// Parameters:
//   - preset: One of "conservative", "balanced", "aggressive"
//
// Returns:
//   - *DegradedBehaviorConfig: Pre-configured behavior settings
func DegradedBehaviorPreset(preset string) *DegradedBehaviorConfig {
	switch preset {
	case "conservative":
		return &DegradedBehaviorConfig{
			EnterThreshold:      30 * time.Second,  // Very tolerant
			ExitThreshold:       10 * time.Second,  // Cautious recovery
			KVErrorThreshold:    10,                // High tolerance
			KVErrorWindow:       60 * time.Second,  // Longer window
			RecoveryGracePeriod: 30 * time.Second,  // Extended grace
		}
	case "aggressive":
		return &DegradedBehaviorConfig{
			EnterThreshold:      5 * time.Second,   // Quick detection
			ExitThreshold:       3 * time.Second,   // Fast recovery
			KVErrorThreshold:    3,                 // Low tolerance
			KVErrorWindow:       15 * time.Second,  // Shorter window
			RecoveryGracePeriod: 10 * time.Second,  // Shorter grace
		}
	case "balanced":
		fallthrough
	default:
		return DefaultDegradedBehaviorConfig()
	}
}

// Config represents the configuration for the Manager.
type Config struct {
	// ... existing fields ...

	// DegradedAlerts configures alert escalation for degraded mode.
	// If nil, DefaultDegradedAlertConfig() is used.
	DegradedAlerts *DegradedAlertConfig

	// DegradedBehavior configures degraded mode detection and recovery.
	// If nil, DefaultDegradedBehaviorConfig() is used.
	// Fine-tune for specific network conditions or SLA requirements.
	DegradedBehavior *DegradedBehaviorConfig

	// ConnectionCheckInterval defines how often to check NATS connection health.
	// Default: 5 seconds
	ConnectionCheckInterval time.Duration

	// FailReadinessWhenDegraded controls readiness probe behavior during degraded mode.
	// If true, readiness probe fails when in degraded state (workers removed from load balancer).
	// If false (default), readiness probe succeeds (workers continue accepting work).
	// Default: false (remain ready during degraded)
	FailReadinessWhenDegraded bool
}
```

#### 1.4 Add Hook Types

**File**: `types/hooks.go`

```go
// AlertLevel represents the severity of a degraded mode alert.
type AlertLevel int

const (
	// AlertLevelInfo indicates temporary connectivity issue.
	AlertLevelInfo AlertLevel = iota

	// AlertLevelWarn indicates extended connectivity issue.
	AlertLevelWarn

	// AlertLevelError indicates significant connectivity issue.
	AlertLevelError

	// AlertLevelCritical indicates prolonged outage requiring intervention.
	AlertLevelCritical
)

// String returns the string representation of the alert level.
//
// Returns:
//   - string: "info", "warning", "error", or "critical"
func (l AlertLevel) String() string {
	switch l {
	case AlertLevelInfo:
		return "info"
	case AlertLevelWarn:
		return "warning"
	case AlertLevelError:
		return "error"
	case AlertLevelCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// DegradedInfo provides context about degraded mode for hooks.
//
// This information is passed to OnDegradedAlert hook to enable
// custom monitoring, alerting, and metrics integration.
type DegradedInfo struct {
	// Duration is how long the system has been in degraded mode.
	Duration time.Duration

	// CacheAge is how old the cached assignment/worker data is.
	CacheAge time.Duration

	// Level is the current alert level based on configured thresholds.
	Level AlertLevel

	// WorkerCount is the number of workers in the cached data.
	WorkerCount int

	// AssignmentCount is the number of partitions in the cached assignment.
	AssignmentCount int
}

// Hooks defines callbacks for Manager lifecycle events.
type Hooks struct {
	// ... existing hooks ...

	// OnDegradedAlert is called periodically during degraded mode.
	//
	// This hook allows custom alerting and monitoring integration.
	// It receives information about the degraded state including duration,
	// cache age, and alert level based on configured thresholds.
	//
	// The hook is called at intervals defined by DegradedAlertConfig.AlertInterval.
	// Hook errors are logged but don't affect degraded mode operation.
	//
	// Example:
	//
	//	OnDegradedAlert: func(ctx context.Context, info parti.DegradedInfo) error {
	//	    if info.Level >= parti.AlertLevelError {
	//	        return alertManager.Send(parti.Alert{
	//	            Severity: info.Level.String(),
	//	            Message:  fmt.Sprintf("NATS unavailable for %v", info.Duration),
	//	            CacheAge: info.CacheAge,
	//	        })
	//	    }
	//	    return nil
	//	}
	OnDegradedAlert func(ctx context.Context, info DegradedInfo) error
}
```

**Tests**: `types/errors_test.go`, `types/state_test.go`

### Phase 2: Manager Implementation (5 hours)

#### 2.1 Add Manager Fields

**File**: `manager.go`

```go
type Manager struct {
	// ... existing fields ...

	// Degraded mode tracking
	degradedSince     time.Time              // When we entered degraded mode
	lastAssignmentAt  time.Time              // Timestamp of last good assignment
	lastAssignment    []types.Partition      // Last known good assignment (defensive copy)

	// Connection monitoring with hysteresis
	connMonitorOnce     sync.Once            // Ensure single monitor start
	connMonitorStop     chan struct{}        // Stop signal for connection monitor
	connDownSince       time.Time            // When connection loss detected (for hysteresis)
	connUpSince         time.Time            // When connection restored (for hysteresis)
	kvErrorCount        atomic.Int32         // Recent KV operation failures
	kvErrorWindow       time.Time            // Window start for error counting

	// Recovery grace period (split-brain prevention)
	recoveryGraceStart  time.Time            // When we exited degraded mode
	inRecoveryGrace     atomic.Bool          // True during post-recovery grace period

	// Watcher retry tracking
	watcherBackoff      time.Duration        // Current backoff for watcher retry
}
```

#### 2.2 Connection Monitor with Hysteresis

**File**: `manager.go`

```go
// checkConnectionHealth checks NATS connection with hysteresis to prevent flapping.
//
// Uses thresholds from Config.DegradedBehavior (or defaults if not configured).
//
// Entry conditions (either triggers degraded):
//   1. IsConnected() == false for >= EnterThreshold
//   2. KV error count >= KVErrorThreshold within KVErrorWindow
//
// Exit conditions (all must be true):
//   1. IsConnected() == true for >= ExitThreshold
//   2. At least one successful KV operation (fresh assignment)
func (m *Manager) checkConnectionHealth() {
	// Get thresholds from config or use defaults
	behaviorCfg := m.config.DegradedBehavior
	if behaviorCfg == nil {
		behaviorCfg = types.DefaultDegradedBehaviorConfig()
	}

	currentState := m.State()
	isConnected := m.conn.IsConnected()
	now := time.Now()

	// Track connection state changes for hysteresis
	if !isConnected {
		if m.connDownSince.IsZero() {
			m.connDownSince = now
			m.logger.Debug("connection down detected, starting hysteresis timer")
		}
		m.connUpSince = time.Time{} // Reset up timer
	} else {
		if m.connUpSince.IsZero() && currentState == types.StateDegraded {
			m.connUpSince = now
			m.logger.Debug("connection up detected, starting recovery hysteresis timer")
		}
		m.connDownSince = time.Time{} // Reset down timer
	}

	// Check for degraded entry conditions
	// Allow entering degraded from ANY state (including initial states like Election)
	// to handle cold start scenarios where NATS is unavailable from the beginning
	if currentState != types.StateDegraded && currentState != types.StateShutdown {
		shouldEnterDegraded := false
		reason := ""

		// Condition 1: Sustained connection loss
		if !isConnected && !m.connDownSince.IsZero() {
			downDuration := now.Sub(m.connDownSince)
			if downDuration >= behaviorCfg.EnterThreshold {
				shouldEnterDegraded = true
				reason = fmt.Sprintf("NATS connection lost for %v", downDuration)
			}
		}

		// Condition 2: Excessive KV errors
		kvErrors := m.kvErrorCount.Load()
		if kvErrors >= int32(behaviorCfg.KVErrorThreshold) {
			errorWindowAge := now.Sub(m.kvErrorWindow)
			if errorWindowAge <= behaviorCfg.KVErrorWindow {
				shouldEnterDegraded = true
				reason = fmt.Sprintf("excessive KV errors: %d in %v", kvErrors, errorWindowAge)
			} else {
				// Window expired, reset counter
				m.kvErrorCount.Store(0)
				m.kvErrorWindow = now
			}
		}

		if shouldEnterDegraded {
			m.enterDegraded(reason)
		}
	}

	// Check for degraded exit conditions
	if currentState == types.StateDegraded {
		if isConnected && !m.connUpSince.IsZero() {
			upDuration := now.Sub(m.connUpSince)
			if upDuration >= behaviorCfg.ExitThreshold {
				// Connection sustained, attempt recovery with fresh KV read
				m.attemptRecoveryFromDegraded()
			}
		}
	}
}

// recordKVError increments the KV error counter for degraded mode detection.
//
// Call this whenever a KV operation fails due to connectivity.
// Resets the error window if it has expired.
func (m *Manager) recordKVError() {
	behaviorCfg := m.config.DegradedBehavior
	if behaviorCfg == nil {
		behaviorCfg = types.DefaultDegradedBehaviorConfig()
	}

	now := time.Now()

	// Reset window if expired
	if now.Sub(m.kvErrorWindow) > behaviorCfg.KVErrorWindow {
		m.kvErrorCount.Store(0)
		m.kvErrorWindow = now
	}

	count := m.kvErrorCount.Add(1)
	m.logger.Debug("KV error recorded",
		"count", count,
		"window_age", now.Sub(m.kvErrorWindow),
	)
}

// recordKVSuccess resets the KV error counter after successful operation.
func (m *Manager) recordKVSuccess() {
	if m.kvErrorCount.Load() > 0 {
		m.logger.Debug("KV operation successful, resetting error counter")
		m.kvErrorCount.Store(0)
	}
}

// enterDegraded transitions to degraded mode and preserves current state.
func (m *Manager) enterDegraded(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Record when we entered degraded mode
	m.degradedSince = time.Now()

	// Preserve current assignment with defensive copy
	if m.currentAssignment != nil {
		m.lastAssignment = m.clonePartitions(m.currentAssignment)
		m.lastAssignmentAt = time.Now()
	}

	m.logger.Warn("entering degraded mode",
		"reason", reason,
		"cached_partitions", len(m.lastAssignment),
		"cache_age", time.Since(m.lastAssignmentAt),
	)

	// Transition state
	oldState := m.State()
	m.transitionState(oldState, types.StateDegraded)

	// Update metrics
	if m.metrics != nil {
		m.metrics.RecordStateTransition(oldState, types.StateDegraded)
		m.metrics.SetDegradedMode(1)
	}
}

// attemptRecoveryFromDegraded tries to exit degraded mode when connection returns.
func (m *Manager) attemptRecoveryFromDegraded() {
	m.logger.Info("NATS connection restored, attempting recovery from degraded mode")

	// Try to refresh assignment from NATS (requires fresh KV read)
	if err := m.refreshAssignmentFromNATS(); err != nil {
		m.logger.Warn("failed to refresh assignment, staying in degraded mode",
			"error", err,
		)
		m.recordKVError()
		return
	}

	m.recordKVSuccess()

	// Successfully refreshed, exit degraded mode
	m.exitDegraded("connection restored and assignment refreshed")

	// Enter recovery grace period to prevent split-brain false emergencies
	// During this period, the leader will not trigger emergency rebalancing
	// to give other workers time to recover and publish their heartbeats
	m.enterRecoveryGracePeriod()
}

// enterRecoveryGracePeriod starts the post-recovery grace period.
//
// This prevents split-brain scenarios where the leader recovers before
// other workers and misinterprets their delayed recovery as crashes.
//
// During the grace period (configurable, default 15s):
//   - Leader publishes its own heartbeat
//   - Leader does NOT act on missing heartbeats from other workers
//   - Emergency detection is suppressed
//   - Normal rebalancing (scaling) can still proceed
//
// This gives all workers a chance to recover from degraded mode.
func (m *Manager) enterRecoveryGracePeriod() {
	behaviorCfg := m.config.DegradedBehavior
	if behaviorCfg == nil {
		behaviorCfg = types.DefaultDegradedBehaviorConfig()
	}

	gracePeriod := behaviorCfg.RecoveryGracePeriod

	m.recoveryGraceStart = time.Now()
	m.inRecoveryGrace.Store(true)

	m.logger.Info("entering recovery grace period",
		"duration", gracePeriod,
		"reason", "allow other workers to recover",
	)

	// Schedule grace period expiry with context-aware timer
	// This prevents goroutine leak and ensures clean shutdown
	go func() {
		timer := time.NewTimer(gracePeriod)
		defer timer.Stop()

		select {
		case <-timer.C:
			m.exitRecoveryGracePeriod()
		case <-m.ctx.Done():
			// Context cancelled, clean shutdown without exiting grace
			m.logger.Debug("recovery grace period cancelled due to shutdown")
		}
	}()
}

// exitRecoveryGracePeriod ends the post-recovery grace period.
func (m *Manager) exitRecoveryGracePeriod() {
	if !m.inRecoveryGrace.Load() {
		return
	}

	m.inRecoveryGrace.Store(false)

	m.logger.Info("exiting recovery grace period",
		"duration", time.Since(m.recoveryGraceStart),
	)
}

// isInRecoveryGrace checks if the manager is in the post-recovery grace period.
func (m *Manager) isInRecoveryGrace() bool {
	return m.inRecoveryGrace.Load()
}

// clonePartitions creates a defensive copy of partition slice.
//
// This prevents external mutations from affecting cached assignments.
func (m *Manager) clonePartitions(partitions []types.Partition) []types.Partition {
	if partitions == nil {
		return nil
	}
	cloned := make([]types.Partition, len(partitions))
	copy(cloned, partitions)
	return cloned
}

	// Try to refresh assignment from NATS
	if err := m.refreshAssignmentFromNATS(); err != nil {
		m.logger.Warn("failed to refresh assignment, staying in degraded mode",
			"error", err,
		)
		return
	}

	// Successfully refreshed, exit degraded mode
	m.exitDegraded("connection restored and assignment refreshed")
}

// refreshAssignmentFromNATS attempts to read fresh assignment from NATS KV.
func (m *Manager) refreshAssignmentFromNATS() error {
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	defer cancel()

	// Try to read assignment from KV
	entry, err := m.assignmentKV.Get(ctx, m.workerID)
	if err != nil {
		return fmt.Errorf("failed to get assignment: %w", err)
	}

	// Parse and update assignment
	var assignment []types.Partition
	if err := json.Unmarshal(entry.Value(), &assignment); err != nil {
		return fmt.Errorf("failed to unmarshal assignment: %w", err)
	}

	m.mu.Lock()
	m.currentAssignment = assignment
	m.lastAssignment = assignment
	m.lastAssignmentAt = time.Now()
	m.mu.Unlock()

	m.logger.Info("refreshed assignment from NATS",
		"partitions", len(assignment),
	)

	return nil
}

// exitDegraded transitions from degraded mode back to stable.
func (m *Manager) exitDegraded(reason string) {
	m.mu.Lock()
	duration := time.Since(m.degradedSince)
	m.mu.Unlock()

	m.logger.Info("exiting degraded mode",
		"reason", reason,
		"duration", duration,
	)

	// Transition state
	m.transitionState(types.StateDegraded, types.StateStable)

	// Update metrics
	if m.metrics != nil {
		m.metrics.RecordStateTransition(types.StateDegraded, types.StateStable)
		m.metrics.SetDegradedMode(0)
		m.metrics.RecordDegradedDuration(duration)
	}
}

// monitorDegradedAlerts emits graduated alerts while in degraded mode.
func (m *Manager) monitorDegradedAlerts() {
	alertConfig := m.config.DegradedAlerts
	if alertConfig == nil {
		alertConfig = types.DefaultDegradedAlertConfig()
	}

	ticker := time.NewTicker(alertConfig.AlertInterval)
	defer ticker.Stop()

	m.logger.Info("degraded alert monitor started", "interval", alertConfig.AlertInterval)

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			if m.State() == types.StateDegraded {
				m.emitDegradedAlert()
			}
		}
	}
}

// emitDegradedAlert calculates alert level and emits appropriate alerts.
func (m *Manager) emitDegradedAlert() {
	m.mu.RLock()
	duration := time.Since(m.degradedSince)
	cacheAge := time.Since(m.lastAssignmentAt)
	partitionCount := len(m.lastAssignment)
	m.mu.RUnlock()

	// Calculate alert level based on duration
	level := m.calculateAlertLevel(duration)

	// Log at appropriate level
	logFields := []any{
		"state", "degraded",
		"duration", duration,
		"cache_age", cacheAge,
		"level", level.String(),
		"partitions", partitionCount,
	}

	switch level {
	case types.AlertLevelInfo:
		m.logger.Info("operating in degraded mode", logFields...)
	case types.AlertLevelWarn:
		m.logger.Warn("extended degraded mode", logFields...)
	case types.AlertLevelError:
		m.logger.Error("prolonged degraded mode", logFields...)
	case types.AlertLevelCritical:
		m.logger.Error("CRITICAL: degraded mode requires intervention", logFields...)
	}

	// Update metrics
	if m.metrics != nil {
		m.metrics.SetDegradedDuration(duration)
		m.metrics.SetCacheAge(cacheAge)
		m.metrics.SetAlertLevel(int(level))
	}

	// Call hook if configured
	if m.hooks != nil && m.hooks.OnDegradedAlert != nil {
		info := types.DegradedInfo{
			Duration:        duration,
			CacheAge:        cacheAge,
			Level:           level,
			WorkerCount:     0, // Only leader tracks workers
			AssignmentCount: partitionCount,
		}

		if err := m.callHook(func(ctx context.Context) error {
			return m.hooks.OnDegradedAlert(ctx, info)
		}); err != nil {
			m.logger.Error("degraded alert hook failed", "error", err)
		}
	}
}

// calculateAlertLevel determines alert level based on degraded duration.
func (m *Manager) calculateAlertLevel(duration time.Duration) types.AlertLevel {
	cfg := m.config.DegradedAlerts
	if cfg == nil {
		cfg = types.DefaultDegradedAlertConfig()
	}

	// Check thresholds in descending severity order
	if cfg.CriticalThreshold > 0 && duration >= cfg.CriticalThreshold {
		return types.AlertLevelCritical
	}
	if cfg.ErrorThreshold > 0 && duration >= cfg.ErrorThreshold {
		return types.AlertLevelError
	}
	if cfg.WarnThreshold > 0 && duration >= cfg.WarnThreshold {
		return types.AlertLevelWarn
	}
	if cfg.InfoThreshold > 0 && duration >= cfg.InfoThreshold {
		return types.AlertLevelInfo
	}

	// Below info threshold, return info (we're in degraded mode)
	return types.AlertLevelInfo
}
```

#### 2.3 Update Start/Stop

**File**: `manager.go`

```go
// In Start() method, add after other goroutines:
m.connMonitorStop = make(chan struct{})
m.wg.Go(func() { m.monitorNATSConnection() })
m.wg.Go(func() { m.monitorDegradedAlerts() })
m.wg.Go(func() { m.monitorAssignmentChangesWithRetry() })

// In Stop() method, add cleanup (with sync.Once to prevent double-close):
var closeOnce sync.Once
closeOnce.Do(func() {
	close(m.connMonitorStop)
})
```

#### 2.4 Watcher with Retry Logic

**File**: `manager.go`

```go
// monitorAssignmentChangesWithRetry wraps the assignment watcher with retry logic.
//
// This prevents permanent watcher failure from transient issues.
// Uses capped exponential backoff with jitter to avoid thundering herd.
//
// CRITICAL: Skips retry attempts when in degraded mode to avoid unnecessary
// KV operations when we already know NATS is unavailable.
func (m *Manager) monitorAssignmentChangesWithRetry() {
	const (
		initialBackoff = 250 * time.Millisecond
		maxBackoff     = 30 * time.Second
		backoffFactor  = 2.0
		degradedCheckInterval = 10 * time.Second  // How often to check if degraded cleared
	)

	backoff := initialBackoff

	for {
		select {
		case <-m.ctx.Done():
			m.logger.Info("assignment watcher stopped due to context cancellation")
			return
		case <-m.connMonitorStop:
			m.logger.Info("assignment watcher stopped by signal")
			return
		default:
		}

		// Skip watcher attempts when in degraded mode (we know NATS is down)
		if m.State() == types.StateDegraded {
			m.logger.Debug("skipping watcher retry while in degraded mode")
			select {
			case <-time.After(degradedCheckInterval):
				continue  // Re-check after interval
			case <-m.ctx.Done():
				return
			case <-m.connMonitorStop:
				return
			}
		}

		m.logger.Info("starting assignment watcher", "backoff", backoff)

		// Try to start watcher
		err := m.monitorAssignmentChanges()

		if err == nil {
			// Clean exit (context done), reset backoff
			backoff = initialBackoff
			return
		}

		// Watcher failed, check if we should retry
		if natsutil.IsConnectivityError(err) {
			m.logger.Warn("assignment watcher failed due to connectivity, will retry",
				"error", err,
				"backoff", backoff,
			)
			m.recordKVError()
		} else {
			m.logger.Error("assignment watcher failed, will retry",
				"error", err,
				"backoff", backoff,
			)
		}

		// Apply backoff with jitter
		jitter := time.Duration(rand.Float64() * float64(backoff) * 0.3)
		sleepDuration := backoff + jitter

		select {
		case <-time.After(sleepDuration):
			// Increase backoff for next iteration (capped)
			backoff = time.Duration(float64(backoff) * backoffFactor)
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		case <-m.ctx.Done():
			return
		case <-m.connMonitorStop:
			return
		}
	}
}

// monitorAssignmentChanges creates and monitors the KV watcher for assignments.
//
// Returns error if watcher creation fails or watcher stops with error.
// Returns nil if context is cancelled (clean exit).
func (m *Manager) monitorAssignmentChanges() error {
	// Create watcher
	watcher, err := m.assignmentKV.Watch(m.ctx, m.workerID)
	if err != nil {
		return fmt.Errorf("failed to create assignment watcher: %w", err)
	}
	defer watcher.Stop()

	m.logger.Info("assignment watcher created successfully")

	// Reset backoff on successful watcher creation
	m.watcherBackoff = 250 * time.Millisecond

	// Monitor for changes
	for {
		select {
		case <-m.ctx.Done():
			return nil // Clean exit

		case entry, ok := <-watcher.Updates():
			if !ok {
				// Channel closed, watcher stopped
				return errors.New("watcher channel closed")
			}

			if entry == nil {
				continue
			}

			// Process assignment update
			if err := m.handleAssignmentUpdate(entry); err != nil {
				m.logger.Error("failed to process assignment update", "error", err)
				m.recordKVError()
			} else {
				m.recordKVSuccess()
			}
		}
	}
}
```

#### 2.5 Update CurrentAssignment with Defensive Copies

**File**: `manager.go`

```go
// CurrentAssignment returns a copy of the current partition assignment.
//
// During degraded mode, this returns the last known good assignment.
// The returned slice is a defensive copy to prevent external mutations.
//
// Returns:
//   - []Partition: Copy of current or cached partition assignment (nil if no assignment yet)
func (m *Manager) CurrentAssignment() []types.Partition {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// In degraded mode, return cached assignment (already a defensive copy)
	if m.State() == types.StateDegraded {
		return m.clonePartitions(m.lastAssignment)
	}

	// Return defensive copy of current assignment
	return m.clonePartitions(m.currentAssignment)
}
```

#### 2.6 Health Check Implementation

**File**: `manager.go`

```go
// HealthStatus returns the current health status of the manager.
//
// During degraded mode, the worker is still healthy (still processing)
// but reports degraded condition for monitoring and alerting.
//
// Returns:
//   - HealthStatus: Current health status with details
func (m *Manager) HealthStatus() HealthStatus {
	state := m.State()

	switch state {
	case types.StateDegraded:
		m.mu.RLock()
		age := time.Since(m.lastAssignmentAt)
		duration := time.Since(m.degradedSince)
		partitionCount := len(m.lastAssignment)
		m.mu.RUnlock()

		return HealthStatus{
			Healthy: true,  // Still healthy, just degraded
			Status: "degraded",
			Reason: "NATS connectivity lost",
			Details: map[string]interface{}{
				"cache_age_seconds":    age.Seconds(),
				"degraded_duration":    duration.Seconds(),
				"serving_requests":     true,
				"accepting_work":       true,  // Still processing partitions
				"cached_partitions":    partitionCount,
			},
		}
	case types.StateEmergency:
		return HealthStatus{
			Healthy: false,
			Status: "emergency",
			Reason: "Emergency rebalancing in progress",
		}
	default:
		return HealthStatus{
			Healthy: true,
			Status: state.String(),
		}
	}
}

// LivenessProbe returns nil if the manager process is alive.
//
// This should always return nil unless the process is truly dead.
// Used for Kubernetes liveness probes.
//
// Returns:
//   - error: nil if alive, error if dead
func (m *Manager) LivenessProbe() error {
	// Always return success unless truly dead
	return nil
}

// ReadinessProbe returns nil if the manager is ready to accept work.
//
// By default, degraded mode still returns success (ready) because
// the worker continues processing with cached assignments.
//
// Set Config.FailReadinessWhenDegraded=true to fail readiness during degraded.
//
// Returns:
//   - error: nil if ready, error if not ready
func (m *Manager) ReadinessProbe() error {
	state := m.State()

	// Option 1 (default): Remain ready even in degraded mode
	// Option 2: Fail readiness if degraded (configurable)
	if m.config.FailReadinessWhenDegraded && state == types.StateDegraded {
		return fmt.Errorf("degraded for %v", time.Since(m.degradedSince))
	}

	// Not ready during init phases or shutdown
	if state == types.StateInit || state == types.StateShutdown {
		return fmt.Errorf("not ready: %s", state.String())
	}

	return nil
}
```

**Tests**: `manager_degraded_test.go` (new file)

### Phase 3: Calculator Cache Implementation (4 hours)

#### 3.1 Add Calculator Fields

**File**: `internal/assignment/calculator.go`

```go
type Calculator struct {
	// ... existing fields ...

	// Worker list cache for degraded mode with atomic freshness tracking
	cachedWorkers     cachedWorkerList
	cacheMu           sync.RWMutex

	// Manager state provider for degraded mode checks
	stateProvider     StateProvider
}

// cachedWorkerList bundles worker data with its timestamp for atomic operations.
//
// This ensures that the worker list and its freshness timestamp are always
// consistent when read together, preventing race conditions between updates
// and emergency detection checks.
type cachedWorkerList struct {
	workers   []string
	timestamp time.Time
}

// StateProvider interface allows calculator to check manager state
// without circular dependencies.
//
// Extended to support recovery grace period checks for split-brain prevention.
type StateProvider interface {
	State() types.State
	IsInRecoveryGrace() bool  // Check if in post-recovery grace period
}
```#### 3.2 Update Calculator Initialization

**File**: `internal/assignment/calculator.go`

```go
// NewCalculator creates a new assignment calculator.
//
// Parameters:
//   - ... existing parameters ...
//   - stateProvider: Provides manager state for degraded mode checks
//
// Returns:
//   - *Calculator: A new calculator instance
func NewCalculator(..., stateProvider StateProvider) *Calculator {
	return &Calculator{
		// ... existing fields ...
		stateProvider: stateProvider,
	}
}
```

#### 3.3 Cache-Aware Worker Fetching with Freshness Tracking

**File**: `internal/assignment/calculator.go`

```go
// getActiveWorkers fetches active workers with degraded mode fallback.
//
// This method attempts to read from NATS KV. On connectivity errors,
// it falls back to cached data if available and logs appropriately.
//
// Atomically updates both worker list and timestamp together to ensure
// consistency for emergency detection freshness checks.
//
// Parameters:
//   - ctx: Context for the KV operation
//
// Returns:
//   - []string: List of active worker IDs
//   - error: Error if both KV and cache are unavailable
func (c *Calculator) getActiveWorkers(ctx context.Context) ([]string, error) {
	// Try to fetch from NATS
	workers, err := c.monitor.GetActiveWorkers(ctx)
	if err != nil {
		// Check if this is a connectivity error
		if natsutil.IsConnectivityError(err) {
			// Try to use cached data
			if cached, freshAge, ok := c.getCachedWorkers(); ok {
				c.logger.Warn("using cached worker list due to connectivity error",
					"workers", len(cached),
					"fresh_age", freshAge,
					"error", err,
				)

				// Update metrics
				if c.metrics != nil {
					c.metrics.RecordCacheUsage("workers", freshAge)
				}

				return cached, nil
			}

			// No cache available
			c.logger.Error("no cached worker list available during connectivity error",
				"error", err,
			)
			return nil, fmt.Errorf("%w: no cached workers: %v", types.ErrDegraded, err)
		}

		// Non-connectivity error, propagate
		return nil, fmt.Errorf("failed to get active workers: %w", err)
	}

	// Success - update cache atomically with timestamp
	c.updateCachedWorkers(workers)

	return workers, nil
}

// getCachedWorkers returns cached worker list with freshness timestamp.
//
// Returns defensive copy to prevent external mutations.
// Returns the timestamp atomically with the data to ensure consistency.
//
// Returns:
//   - []string: Copy of cached worker list
//   - time.Duration: Age of the cached data (time since last fresh read)
//   - bool: true if cache is available, false otherwise
func (c *Calculator) getCachedWorkers() ([]string, time.Duration, bool) {
	c.cacheMu.RLock()
	defer c.cacheMu.RUnlock()

	if c.cachedWorkers.workers == nil {
		return nil, 0, false
	}

	// Return a copy to prevent external modification
	cached := make([]string, len(c.cachedWorkers.workers))
	copy(cached, c.cachedWorkers.workers)

	// Calculate age based on timestamp
	age := time.Since(c.cachedWorkers.timestamp)

	return cached, age, true
}

// updateCachedWorkers updates the cached worker list atomically with timestamp.
//
// Creates defensive copy to prevent external mutations.
// Bundles worker list and timestamp together for atomic freshness tracking.
//
// Parameters:
//   - workers: Fresh worker list from KV
func (c *Calculator) updateCachedWorkers(workers []string) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	// Create defensive copy
	cached := make([]string, len(workers))
	copy(cached, workers)

	// Update atomically
	c.cachedWorkers = cachedWorkerList{
		workers:   cached,
		timestamp: time.Now(),
	}

	c.logger.Debug("updated worker cache",
		"workers", len(workers),
	)
}

	// Return a copy to prevent external modification
	cached := make([]string, len(c.cachedWorkers))
	copy(cached, c.cachedWorkers)

	return cached, true
}

// getCachedWorkersTime returns when the cache was last updated.
func (c *Calculator) getCachedWorkersTime() time.Time {
	c.cacheMu.RLock()
	defer c.cacheMu.RUnlock()
	return c.cachedWorkersTime
}

// updateCachedWorkers updates the cached worker list.
func (c *Calculator) updateCachedWorkers(workers []string) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	c.cachedWorkers = make([]string, len(workers))
	copy(c.cachedWorkers, workers)
	c.cachedWorkersTime = time.Now()

	c.logger.Debug("updated worker cache",
		"workers", len(workers),
	)
}
```

#### 3.4 Update Emergency Detection with Freshness Checks

**File**: `internal/assignment/calculator.go` or `emergency.go`

```go
// checkEmergencyConditions determines if emergency rebalancing is needed.
//
// Emergency is triggered by actual worker crashes detected from fresh KV data.
// During degraded mode or connectivity errors, emergency is suppressed to
// prevent false positives.
//
// This method implements multiple safety checks:
//   - Bypasses emergency during degraded mode
//   - Bypasses emergency during post-recovery grace period (split-brain prevention)
//   - Bypasses emergency when last fresh read was >30s ago
//   - Only triggers on actual worker crashes with fresh data
//
// Returns:
//   - bool: true if emergency rebalancing is required
func (c *Calculator) checkEmergencyConditions() bool {
	// CRITICAL: Never trigger emergency during degraded mode
	if c.isManagerDegraded() {
		c.logger.Info("emergency detection bypassed: manager in degraded mode")
		if c.metrics != nil {
			c.metrics.IncrementEmergencyBypassed("degraded_mode")
		}
		return false
	}

	// CRITICAL: Never trigger emergency during recovery grace period
	// This prevents split-brain scenarios where leader recovers before other workers
	if c.isInRecoveryGrace() {
		c.logger.Info("emergency detection bypassed: in recovery grace period")
		if c.metrics != nil {
			c.metrics.IncrementEmergencyBypassed("recovery_grace")
		}
		return false
	}

	// Additional safety: only evaluate emergency with fresh data
	// Get cached data with its atomic timestamp
	_, freshAge, hasCachedWorkers := c.getCachedWorkers()
	if !hasCachedWorkers || freshAge > 30*time.Second {
		c.logger.Warn("emergency detection bypassed: no fresh data",
			"has_cache", hasCachedWorkers,
			"fresh_age", freshAge)
		if c.metrics != nil {
			c.metrics.IncrementEmergencyBypassed("no_fresh_data")
		}
		return false
	}

	// Try to get active workers (may use cache if connectivity error)
	workers, err := c.getActiveWorkers(c.ctx)
	if err != nil {
		if errors.Is(err, types.ErrDegraded) {
			// Connectivity issue, not a real emergency
			c.logger.Debug("suppressing emergency due to connectivity error")
			if c.metrics != nil {
				c.metrics.IncrementEmergencyBypassed("connectivity_error")
			}
			return false
		}

		// Other errors - be conservative, don't trigger emergency
		c.logger.Error("failed to check emergency conditions",
			"error", err,
		)
		return false
	}

	// Only trigger emergency for REAL worker loss with fresh data
	if len(workers) == 0 && c.lastKnownWorkerCount > 0 {
		c.logger.Warn("all workers disappeared (EMERGENCY)",
			"last_count", c.lastKnownWorkerCount,
			"fresh_data_age", freshAge,
		)
		return true
	}

	// Check for crashes using emergency detector
	if c.emergencyDetector != nil {
		// Get cached worker list for comparison
		cachedWorkers, _, _ := c.getCachedWorkers()
		crashed := c.emergencyDetector.Detect(workers, cachedWorkers)
		if crashed {
			c.logger.Warn("worker crashes detected (EMERGENCY)",
				"current_workers", len(workers),
				"cached_workers", len(cachedWorkers),
				"fresh_data_age", freshAge,
			)
		}
		return crashed
	}

	return false
}

// isManagerDegraded checks if the manager is in degraded state.
//
// Uses the StateProvider interface to avoid circular dependencies.
//
// Returns:
//   - bool: true if manager is in degraded mode
func (c *Calculator) isManagerDegraded() bool {
	if c.stateProvider == nil {
		return false
	}
	return c.stateProvider.State() == types.StateDegraded
}

// isInRecoveryGrace checks if the manager is in post-recovery grace period.
//
// Uses the StateProvider interface to check recovery grace status.
// The StateProvider interface needs to expose this method.
//
// Returns:
//   - bool: true if in recovery grace period
func (c *Calculator) isInRecoveryGrace() bool {
	// Requires StateProvider to expose IsInRecoveryGrace() method
	// This will be added to the StateProvider interface
	if sp, ok := c.stateProvider.(interface{ IsInRecoveryGrace() bool }); ok {
		return sp.IsInRecoveryGrace()
	}
	return false
}
```

**Tests**: `internal/assignment/calculator_degraded_test.go` (new file)

### Phase 4: Metrics & Observability (2 hours)

#### 4.1 Add Metrics Interface

**File**: `types/metrics_collector.go`

```go
// Add to ManagerMetrics interface:
type ManagerMetrics interface {
	// ... existing methods ...

	// RecordDegradedDuration records the duration spent in degraded mode.
	RecordDegradedDuration(duration time.Duration)

	// SetDegradedMode sets the current degraded mode status (0 or 1).
	SetDegradedMode(degraded float64)

	// SetCacheAge sets the age of cached data in seconds.
	SetCacheAge(age time.Duration)

	// SetAlertLevel sets the current alert level (0-3).
	SetAlertLevel(level int)

	// SetCachedPartitionCount sets the number of partitions in cache.
	SetCachedPartitionCount(count int)

	// SetLastSuccessfulRefresh records timestamp of last successful assignment refresh.
	SetLastSuccessfulRefresh(timestamp time.Time)

	// IncrementAlertEmitted tracks alert emission by level for spam detection.
	IncrementAlertEmitted(level string)
}

// Add to CalculatorMetrics interface:
type CalculatorMetrics interface {
	// ... existing methods ...

	// RecordCacheUsage records when cached data is used instead of fresh KV data.
	RecordCacheUsage(cacheType string, age time.Duration)

	// IncrementEmergencyBypassed tracks emergency detection bypasses by reason.
	// Reasons: "degraded_mode", "stale_cache", "connectivity_error"
	IncrementEmergencyBypassed(reason string)
}
```

#### 4.2 NOP Implementations

Update NOP implementations to include new methods (no-op).

**Tests**: Metrics tests in respective test files

### Phase 5: Configuration Validation (1 hour)

#### 5.1 Config Validation

**File**: `config.go`

```go
// Validate checks the configuration for errors.
func (c *Config) Validate() error {
	// ... existing validation ...

	// Validate degraded alert config
	if err := c.validateDegradedAlerts(); err != nil {
		return fmt.Errorf("invalid degraded alerts config: %w", err)
	}

	// Validate degraded behavior config
	if err := c.validateDegradedBehavior(); err != nil {
		return fmt.Errorf("invalid degraded behavior config: %w", err)
	}

	return nil
}

// validateDegradedAlerts ensures alert thresholds are sensible.
func (c *Config) validateDegradedAlerts() error {
	// Use defaults if not configured
	if c.DegradedAlerts == nil {
		c.DegradedAlerts = types.DefaultDegradedAlertConfig()
		return nil
	}

	da := c.DegradedAlerts

	// Ensure thresholds are in ascending order
	if da.WarnThreshold > 0 && da.WarnThreshold < da.InfoThreshold {
		return errors.New("warn threshold must be >= info threshold")
	}
	if da.ErrorThreshold > 0 && da.ErrorThreshold < da.WarnThreshold {
		return errors.New("error threshold must be >= warn threshold")
	}
	if da.CriticalThreshold > 0 && da.CriticalThreshold < da.ErrorThreshold {
		return errors.New("critical threshold must be >= error threshold")
	}

	// Set reasonable defaults for zero values
	if da.InfoThreshold == 0 {
		da.InfoThreshold = 30 * time.Second
	}
	if da.AlertInterval == 0 {
		da.AlertInterval = 30 * time.Second
	}

	return nil
}

// validateDegradedBehavior ensures behavior config values are sensible.
func (c *Config) validateDegradedBehavior() error {
	// Use defaults if not configured
	if c.DegradedBehavior == nil {
		c.DegradedBehavior = types.DefaultDegradedBehaviorConfig()
		return nil
	}

	db := c.DegradedBehavior

	// Validate thresholds are positive
	if db.EnterThreshold < 0 {
		return errors.New("enter threshold must be >= 0")
	}
	if db.ExitThreshold < 0 {
		return errors.New("exit threshold must be >= 0")
	}
	if db.RecoveryGracePeriod < 0 {
		return errors.New("recovery grace period must be >= 0")
	}
	if db.KVErrorThreshold < 0 {
		return errors.New("KV error threshold must be >= 0")
	}
	if db.KVErrorWindow < 0 {
		return errors.New("KV error window must be >= 0")
	}

	// Warn if exit threshold is larger than enter threshold (unusual)
	if db.ExitThreshold > db.EnterThreshold {
		// Not an error, but log warning (implementation will log this)
	}

	// Set reasonable defaults for zero values
	if db.EnterThreshold == 0 {
		db.EnterThreshold = 10 * time.Second
	}
	if db.ExitThreshold == 0 {
		db.ExitThreshold = 5 * time.Second
	}
	if db.KVErrorThreshold == 0 {
		db.KVErrorThreshold = 5
	}
	if db.KVErrorWindow == 0 {
		db.KVErrorWindow = 30 * time.Second
	}
	if db.RecoveryGracePeriod == 0 {
		db.RecoveryGracePeriod = 15 * time.Second
	}

	return nil
}
```

**Tests**: `config_test.go`
```

**Tests**: `config_test.go`

### Phase 6: Integration Testing (4 hours)

#### 6.1 Degraded Mode State Tests

**File**: `manager_degraded_test.go` (new)

```go
func TestManager_EnterDegradedMode(t *testing.T) {
	// Test entering degraded when NATS disconnects
}

func TestManager_ExitDegradedMode(t *testing.T) {
	// Test exiting degraded when NATS reconnects
}

func TestManager_DegradedPreservesAssignment(t *testing.T) {
	// Test that assignments are preserved during degraded mode
}

func TestManager_NeverEscalateToEmergency(t *testing.T) {
	// Test that staleness never triggers Emergency state
}
```

#### 6.2 Calculator Cache Tests

**File**: `internal/assignment/calculator_degraded_test.go` (new)

```go
func TestCalculator_CacheFallback(t *testing.T) {
	// Test cache fallback during KV errors
}

func TestCalculator_EmergencySuppressionDuringDegraded(t *testing.T) {
	// Test that emergency is not triggered from connectivity errors
}

func TestCalculator_CacheUpdateOnSuccess(t *testing.T) {
	// Test cache is updated when KV succeeds
}

func TestCalculator_FreshnessTracking(t *testing.T) {
	// Test that lastFreshAt is updated only on successful KV reads
	// Test that emergency detection uses lastFreshAt correctly
}

func TestCalculator_DefensiveCopies(t *testing.T) {
	// Test that cache operations return copies
	// Test that external mutations don't affect cached data
}
```

#### 6.3 Integration Tests with Edge Cases

**File**: `test/integration/degraded_mode_test.go` (new)

```go
func TestIntegration_DegradedMode_NATSOutage(t *testing.T) {
	// Simulate complete NATS outage
	// Verify workers stay in degraded mode
	// Verify assignments frozen
	// Verify no emergency triggered
}

func TestIntegration_DegradedMode_Recovery(t *testing.T) {
	// Enter degraded mode
	// Restore NATS
	// Verify automatic recovery
	// Verify assignments refreshed
	// Verify recovery grace period activated
}

func TestIntegration_DegradedMode_SplitBrainRecovery(t *testing.T) {
	// Leader and workers enter degraded mode
	// Leader recovers first (before workers)
	// Verify leader enters recovery grace period
	// Verify leader does NOT trigger emergency for missing worker heartbeats
	// Workers recover within grace period
	// Verify system transitions to normal operation without false emergency
}

func TestIntegration_DegradedMode_RecoveryGracePeriod(t *testing.T) {
	// Test that emergency detection is suppressed during grace period
	// Test that grace period expires after configured duration
	// Test that emergency detection resumes after grace period
}

func TestIntegration_DegradedMode_AlertEscalation(t *testing.T) {
	// Test alert level escalation over time
}

func TestIntegration_DegradedMode_RapidNATSFlapping(t *testing.T) {
	// Simulate NATS connection flapping every 2 seconds for 2 minutes
	// Verify system doesn't thrash between states
	// Verify hysteresis prevents excessive state transitions
	// Verify at most one degraded entry and one exit
}

func TestIntegration_DegradedMode_PartialConnectivity(t *testing.T) {
	// Simulate KV read success but write failures
	// Verify appropriate degraded behavior
	// Test that leader enters degraded when it can't publish
}

func TestIntegration_DegradedMode_ColdStartWithNATSDown(t *testing.T) {
	// Start manager with NATS already down
	// Verify enters degraded after thresholds
	// Verify no emergency logic triggered
	// Verify empty assignment doesn't cause panic
}

func TestIntegration_DegradedMode_LeaderLossMidRebalance(t *testing.T) {
	// Start rebalancing
	// Lose NATS connection mid-rebalance
	// Verify transition to Degraded
	// Verify frozen assignment is pre-rebalance stable (not in-flight)
}

func TestIntegration_DegradedMode_KVErrorThreshold(t *testing.T) {
	// Simulate sustained KV errors (5+ in 30s)
	// Verify enters degraded even with IsConnected=true
	// Verify KV error counter reset on success
}

func TestIntegration_DegradedMode_WatcherRetry(t *testing.T) {
	// Fail watcher creation multiple times
	// Verify exponential backoff with jitter
	// Verify watcher eventually succeeds on recovery
	// Verify backoff resets after success
}

func TestCalculator_CacheCoherency(t *testing.T) {
	// Verify cache updates are atomic
	// Verify no partial reads during updates
	// Verify copy-on-read prevents mutations
}
```

### Phase 7: Documentation Updates (2 hours)

#### 7.1 API Reference

**File**: `docs/API_REFERENCE.md`

- Add `StateDegraded` to state documentation
- Document new errors: `ErrConnectivity`, `ErrDegraded`
- Document `DegradedAlertConfig` configuration
- Document `OnDegradedAlert` hook
- Document new metrics methods

#### 7.2 User Guide

**File**: `docs/USER_GUIDE.md`

- Add degraded mode to worker lifecycle section
- Add configuration examples for alert thresholds
- Add troubleshooting section for degraded mode
- Add examples of custom alerting integration

#### 7.3 Operations Guide

**File**: `docs/OPERATIONS.md`

- Add degraded mode to monitoring section
- Add alerting integration examples
- Add operational runbook for degraded mode
- Add health check interpretation

#### 7.4 Examples

**File**: `examples/degraded-mode/` (new)

Create example showing:
- Custom alert threshold configuration
- Integration with alerting systems
- Metrics collection during degraded mode
- Health check monitoring

## Implementation Checklist

### Phase 1: Core State & Types ✅
- [ ] Add `StateDegraded` constant to `types/state.go`
- [ ] Add `ErrConnectivity` and `ErrDegraded` to `types/errors.go`
- [ ] Add `IsConnectivityError()` helper to `internal/natsutil/errors.go`
- [ ] Add `DegradedAlertConfig` type
- [ ] Add `DegradedBehaviorConfig` type (NEW)
- [ ] Add `AlertLevel` and `DegradedInfo` types
- [ ] Add `OnDegradedAlert` to `Hooks`
- [ ] Write unit tests for new types

### Phase 2: Manager Implementation ✅
- [ ] Add degraded tracking fields to Manager struct
- [ ] Add recovery grace period fields (NEW)
- [ ] Implement `monitorNATSConnection()`
- [ ] Implement `checkConnectionHealth()` with configurable thresholds (UPDATED)
- [ ] Implement `enterDegraded()`
- [ ] Implement `exitDegraded()`
- [ ] Implement `attemptRecoveryFromDegraded()` with grace period (UPDATED)
- [ ] Implement `enterRecoveryGracePeriod()` (NEW)
- [ ] Implement `exitRecoveryGracePeriod()` (NEW)
- [ ] Implement `isInRecoveryGrace()` (NEW)
- [ ] Implement `refreshAssignmentFromNATS()`
- [ ] Implement `monitorDegradedAlerts()`
- [ ] Implement `emitDegradedAlert()`
- [ ] Implement `calculateAlertLevel()`
- [ ] Update `Start()` to launch monitoring goroutines
- [ ] Update `Stop()` to cleanup monitoring
- [ ] Update `CurrentAssignment()` to return cached data
- [ ] Update `StateProvider` interface with `IsInRecoveryGrace()` (NEW)
- [ ] Write unit tests for degraded mode
- [ ] Write unit tests for recovery grace period (NEW)

### Phase 3: Calculator Cache Implementation ✅
- [ ] Add cache fields to Calculator struct with atomic struct (UPDATED)
- [ ] Define `cachedWorkerList` struct for atomic operations (NEW)
- [ ] Implement `getActiveWorkers()` with cache fallback (UPDATED)
- [ ] Implement `getCachedWorkers()` returning atomic timestamp (UPDATED)
- [ ] Implement `updateCachedWorkers()` with atomic update (UPDATED)
- [ ] Update emergency detection to suppress during degraded (UPDATED)
- [ ] Update emergency detection to suppress during recovery grace (NEW)
- [ ] Update emergency detection to use atomic freshness (UPDATED)
- [ ] Add `isInRecoveryGrace()` helper (NEW)
- [ ] Add manager state checking to calculator
- [ ] Write unit tests for cache behavior
- [ ] Write unit tests for atomic freshness tracking (NEW)

### Phase 4: Metrics & Observability ✅
- [ ] Add metrics methods to interface
- [ ] Update NOP metrics implementation
- [ ] Add metrics recording to manager
- [ ] Add metrics recording to calculator
- [ ] Write metrics tests

### Phase 5: Configuration Validation ✅
- [ ] Implement `validateDegradedAlerts()`
- [ ] Implement `validateDegradedBehavior()` (NEW)
- [ ] Update `Validate()` to check both alert and behavior configs
- [ ] Write config validation tests

### Phase 6: Integration Testing ✅
- [ ] Write manager degraded mode tests
- [ ] Write manager recovery grace period tests (NEW)
- [ ] Write calculator cache tests
- [ ] Write calculator atomic freshness tests (NEW)
- [ ] Write integration test for NATS outage scenario
- [ ] Write integration test for recovery scenario
- [ ] Write integration test for split-brain recovery (NEW)
- [ ] Write integration test for recovery grace period (NEW)
- [ ] Write integration test for alert escalation
- [ ] Write integration test for cold start with NATS down (NEW)
- [ ] Write integration test for configurable thresholds (NEW)

### Phase 7: Documentation Updates ✅
- [ ] Update API_REFERENCE.md
- [ ] Update USER_GUIDE.md
- [ ] Update OPERATIONS.md
- [ ] Create degraded-mode example
- [ ] Update README if needed

## Testing Strategy

### Unit Tests

1. **State Transitions**
   - Stable → Degraded
   - Degraded → Stable
   - Rebalancing → Degraded
   - Verify Emergency is never entered from staleness

2. **Cache Behavior**
   - Cache updated on successful KV reads
   - Cache used on connectivity errors
   - Cache never invalidated by age

3. **Alert Escalation**
   - Alert level calculation based on thresholds
   - Hook invocation with correct info
   - Metrics updated correctly

### Integration Tests

1. **NATS Outage Simulation**
   - Stop NATS server
   - Verify workers enter degraded
   - Verify assignments frozen
   - Verify no emergency triggered
   - Verify alert escalation

2. **Recovery Scenario**
   - Enter degraded mode
   - Restore NATS
   - Verify automatic recovery
   - Verify assignments refreshed

3. **Prolonged Outage**
   - Stay degraded for >30 minutes
   - Verify critical alerts
   - Verify system remains stable
   - Verify manual recovery works

## Operational Playbook

### When System Reports Degraded

The degraded state indicates NATS connectivity issues but the system continues processing with cached data.

#### Important: Leader Behavior During Degraded Mode

**Leaders Cannot Publish During Degraded:**
- When leader enters degraded mode, it **cannot publish heartbeats** (no NATS write capability)
- Leader does NOT step down - maintains leadership status internally
- Other workers cannot see leader's heartbeat disappear (they're also degraded, can't read KV)
- **Result**: System enters frozen state with all workers operating independently on cached assignments
- **Recovery**: When NATS connectivity returns, leader resumes publishing heartbeats normally

**Non-Leader Behavior During Degraded:**
- Non-leaders continue processing their cached partitions
- Cannot detect leader loss (no KV read capability)
- Will not attempt to become leader during degraded mode
- Resume normal heartbeat monitoring after recovery

**Key Insight**: Degraded mode creates a temporary "autonomous operation" mode where:
- All coordination is suspended (no heartbeats, no assignments, no leadership changes)
- Workers operate on last-known-good state (cached assignments)
- System stability depends on cache validity (which is infinite by design)
- Recovery is automatic when NATS connectivity returns

#### Timeline and Actions

**0-2 minutes: Monitor**
- Likely transient issue, observe
- Check NATS cluster health: `nats server ls`
- Verify network connectivity between workers and NATS
- Review recent logs for connectivity patterns

**2-5 minutes: Investigate**
- Review NATS server logs for errors
- Check for network partitions or firewall changes
- Verify NATS JetStream status: `nats stream ls`
- Check KeyValue bucket health: `nats kv ls`
- Review system metrics (CPU, memory, network)

**5-30 minutes: Active Intervention**
- Consider NATS cluster restart if safe
- Check for resource exhaustion on NATS servers
- Review recent deployments or configuration changes
- Verify DNS resolution for NATS endpoints
- Check TLS certificate expiry if using secure connection

**30+ minutes: Escalate**
- Page on-call engineer if not already alerted
- Prepare for potential manual worker restart
- Document timeline for post-mortem
- Consider emergency procedures

### Recovery Verification

After NATS connectivity is restored:

1. **Verify State Transitions**
   - All workers should exit degraded mode automatically
   - Check state via health endpoint: `GET /health`
   - Monitor state transition logs
   - Verify leader resumes publishing heartbeats immediately

2. **Verify Leadership**
   - Original leader should maintain leadership (no election during degraded)
   - Non-leaders should resume monitoring leader heartbeats
   - No leadership changes should occur unless workers restarted during outage
   - Check leadership stability via health endpoint

3. **Verify Assignment Integrity**
   - Confirm assignment redistribution did NOT occur during degraded
   - Workers should have same partitions as before outage
   - Check assignment consistency across workers
   - Verify no emergency rebalancing triggered (emergency bypassed during degraded)

4. **Review Metrics**
   - Verify cache usage metrics show fallback occurred
   - Check degraded duration metrics
   - Review emergency bypass events (should be > 0 if stale checks occurred)
   - Verify no emergency rebalancing triggered

5. **Post-Recovery Health**
   - Monitor for delayed failures
   - Watch for increased rebalancing activity
   - Verify no emergency rebalancing triggered
   - Check that normal rebalancing resumes if scaling occurred during outage

### Health Check Interpretation

**Liveness Probe**: Always returns success during degraded mode
- Purpose: Worker process is alive and functioning
- Action: Never restart based on liveness during degraded

**Readiness Probe**: Configurable behavior during degraded
- Default: Returns success (worker still processing)
- Optional: Fail readiness if `FailReadinessWhenDegraded=true`
- Purpose: Control load balancer traffic routing

**Health Status API**: Returns detailed degraded information
```json
{
  "healthy": true,
  "status": "degraded",
  "reason": "NATS connectivity lost",
  "details": {
    "cache_age_seconds": 180,
    "degraded_duration": 240,
    "serving_requests": true,
    "accepting_work": true,
    "cached_partitions": 128
  }
}
```

### Alert Severity Guidelines

| Alert Level | Duration | Action Required | Response Time |
|-------------|----------|-----------------|---------------|
| INFO | 30s-2m | Observe | None - auto-resolve expected |
| WARN | 2m-5m | Monitor | Review within 10 minutes |
| ERROR | 5m-30m | Investigate | Start investigation immediately |
| CRITICAL | 30m+ | Intervene | Page on-call, manual action needed |

### Common Scenarios

**Scenario 1: Brief Network Blip**
- Duration: < 30 seconds
- Expected: No alerts (below info threshold)
- Action: None required

**Scenario 2: NATS Pod Restart (Kubernetes)**
- Duration: 30-60 seconds
- Expected: INFO alert, automatic recovery
- Action: Monitor recovery, review pod logs

**Scenario 3: Network Partition**
- Duration: Several minutes
- Expected: WARN → ERROR alerts
- Action: Check network connectivity, involve network team

**Scenario 4: NATS Cluster Failure**
- Duration: 30+ minutes
- Expected: CRITICAL alert
- Action: Emergency NATS recovery or worker restart

### Performance Characteristics

**Overhead During Normal Operation**:
- Connection monitoring: ~100μs every 5s (negligible)
- No performance impact on request processing

**Overhead During Degraded Mode**:
- Alert emission: ~1ms every 30s (negligible)
- Cache operations: O(n) copy on read, n = worker count
- No impact on partition processing

## Rollout Plan

### Stage 1: Internal Review (Week 1)
- Complete implementation
- Pass all tests
- Internal code review
- Performance benchmarks

### Stage 2: Documentation (Week 2)
- Complete all documentation
- Review documentation for clarity
- Create operational runbooks
- Create example integrations

### Stage 3: Beta Testing (Week 3-4)
- Deploy to dev environment
- Simulate NATS outages
- Verify alerting works
- Gather feedback

### Stage 4: Production Rollout (Week 5)
- Deploy to staging
- Verify in staging for 1 week
- Deploy to production with monitoring
- Monitor degraded mode metrics

## Success Criteria

1. **Functionality**
   - ✅ System continues operating during NATS outages
   - ✅ Assignments remain stable and frozen
   - ✅ No false emergency rebalancing
   - ✅ Automatic recovery when NATS returns

2. **Observability**
   - ✅ Clear state visibility (Degraded state)
   - ✅ Graduated alerting (info → critical)
   - ✅ Metrics for duration and cache age
   - ✅ Hook integration working

3. **Configuration**
   - ✅ Sensible defaults work out-of-box
   - ✅ Configurable alert thresholds
   - ✅ Config validation prevents misconfigurations

4. **Testing**
   - ✅ Unit test coverage >80%
   - ✅ Integration tests pass
   - ✅ Performance benchmarks show no regression

## Risk Mitigation

### Risk: Masking Real Worker Crashes
**Mitigation**: Emergency detection only uses fresh KV data; connectivity errors explicitly checked; freshness tracked atomically

### Risk: Split-Brain False Emergencies
**Mitigation**: Recovery grace period prevents leader from triggering emergency when it recovers before other workers; configurable duration (default 15s)

### Risk: Staying Degraded Too Long
**Mitigation**: Critical alerts at 30m; clear metrics; operators can intervene manually

### Risk: Cache Inconsistency
**Mitigation**: Cache updated atomically with timestamp; copy-on-read prevents mutation; atomic struct guarantees consistency

### Risk: Alert Fatigue
**Mitigation**: Configurable alert interval; graduated escalation; hooks for custom filtering

### Risk: State Flapping
**Mitigation**: Configurable hysteresis thresholds (enter/exit); dual degraded triggers (connection loss + KV errors); defaults prevent flapping; preset configurations available

### Risk: Watcher Resource Waste During Degraded
**Mitigation**: Watcher retry loop skips attempts when in degraded mode; periodic check (10s) to resume after recovery; prevents unnecessary KV operations

### Risk: Timer Goroutine Leaks
**Mitigation**: Recovery grace period uses context-aware timers; proper cleanup on shutdown; no `time.AfterFunc` without cancellation

### Risk: Cold Start Failures
**Mitigation**: Degraded mode can be entered from any state (including Election); visible monitoring even during startup failures

### Risk: Breaking Existing Behavior
**Mitigation**: Additive changes only; defaults maintain current behavior; extensive testing

## Future Enhancements (Out of Scope)

1. **Smart Cache Invalidation**: Partial invalidation for specific workers
2. **Degraded Metrics Dashboard**: Pre-built Grafana dashboards
3. **Automatic NATS Reconnection**: Exponential backoff reconnection logic
4. **Degraded State Persistence**: Remember degraded state across restarts
5. **Multi-Level Caching**: Memory + disk cache for extended outages

## Design Review History

This plan underwent multiple review cycles to ensure production readiness:

### Initial Design Review (Split-Brain & Edge Cases)
**Focus**: Handling complex recovery scenarios and edge cases

**Key Findings**:
1. Split-brain recovery risk when leader recovers before workers
2. Need for configurable thresholds for different environments
3. Cold start scenarios not properly handled
4. Race condition between cached data and freshness timestamp

**Applied Solutions**:
1. Added recovery grace period (15s default) to prevent false emergencies
2. Created `DegradedBehaviorConfig` with fine-grained thresholds
3. Allowed degraded entry from any state (not just Stable/Rebalancing)
4. Implemented atomic `cachedWorkerList` struct for consistency

### Complexity & Loop Prevention Review
**Focus**: Identifying over-engineering and potential infinite loops

**Key Findings**:
1. ⚠️ **CRITICAL**: Watcher retry could loop indefinitely during degraded mode
2. ⚠️ **CRITICAL**: `time.AfterFunc` could leak goroutines on shutdown
3. **ADDRESSED**: Configuration complexity with 5 separate thresholds
4. **CONSIDERED**: Alert level granularity (4 levels)
5. **CONSIDERED**: StateProvider vs callback approach
6. **CONSIDERED**: Duplicate caching between Manager and Calculator

**Applied Solutions**:
1. ✅ Added degraded state check in watcher retry to skip attempts when NATS down
2. ✅ Replaced `time.AfterFunc` with context-aware goroutine for grace period
3. ✅ Added `DegradedBehaviorPreset()` helper (conservative/balanced/aggressive)

**Rejected Simplifications** (with rationale):
1. ❌ Reduce alert levels to 2: Four-level alerting is SRE industry standard, maps to escalation policies
2. ❌ Replace StateProvider with callbacks: Interface provides better abstraction and testability
3. ❌ Consolidate caches: Manager and Calculator caches serve different purposes (assignment vs worker list)

**Impact**:
- Prevented two potential infinite loop scenarios
- Added resource efficiency during degraded mode
- Improved configuration usability with presets
- Maintained operational best practices for alerting

## Conclusion

This implementation provides production-grade resilience to NATS connectivity issues while maintaining clear operational visibility and preventing false emergencies. The "cache valid forever" philosophy ensures stability over freshness, with graduated alerting providing operators the information needed for manual intervention when necessary.

**Key Principles**:
- "Stale but stable" > "Fresh but broken"
- Alert escalation without behavioral changes
- Safety through multiple emergency detection bypasses
- Clear operational visibility via state, metrics, and hooks

**Implementation Highlights**:
- **Split-brain prevention**: Recovery grace period prevents false emergencies when leader recovers first
- **Atomic freshness tracking**: Bundled worker list + timestamp prevents race conditions
- **Configurable behavior**: Fine-tune hysteresis and thresholds with presets or custom values
- **Resource efficiency**: Watcher retry skips attempts during degraded mode to avoid waste
- **Context-aware timers**: Grace period uses proper cleanup to prevent goroutine leaks
- **Cold start resilience**: Can enter degraded from any state, even during initial startup
- **StateProvider pattern**: Eliminates circular dependencies between manager and calculator
- **Health checks**: Distinguish liveness from readiness for smart load balancing
- **Enhanced emergency detection**: Multiple safety layers with fresh data validation
- **Comprehensive metrics**: Full observability for debugging and alerting
- **Operational playbook**: Detailed incident response guide with timelines

**Total Estimated Effort**: ~32 hours (updated from 27 with refinements)
- Phase 1: Core State & Types - 5 hours (+1 for DegradedBehaviorConfig and presets)
- Phase 2: Manager Implementation - 8 hours (+2 for recovery grace period and watcher improvements)
- Phase 3: Calculator Cache - 6 hours (+1 for atomic freshness tracking)
- Phase 4: Metrics & Observability - 3 hours (unchanged)
- Phase 5: Configuration Validation - 2 hours (+1 for behavior config validation)
- Phase 6: Integration Testing - 6 hours (+1 for split-brain and cold start tests)
- Phase 7: Documentation Updates - 2 hours (-1, streamlined with better structure)

**Refinements Applied**:

**From Initial Deep Review**:
1. ✅ **Split-brain recovery grace period**: Prevents false emergencies when leader recovers before workers
2. ✅ **Configurable hysteresis thresholds**: `DegradedBehaviorConfig` for environment-specific tuning
3. ✅ **Cold start degraded entry**: Can transition to degraded from any state, not just Stable/Rebalancing
4. ✅ **Atomic freshness tracking**: `cachedWorkerList` struct ensures data/timestamp consistency

**From Final Review (Complexity & Loop Prevention)**:
1. ✅ **Watcher retry infinite loop prevention**: Added degraded state check to skip unnecessary retry attempts when NATS known down
2. ✅ **Recovery grace timer safety**: Replaced `time.AfterFunc` with context-aware goroutine to prevent leaks on shutdown
3. ✅ **Configuration presets**: Added `DegradedBehaviorPreset()` helper for common scenarios (conservative/balanced/aggressive)
4. ❌ **Alert level reduction**: REJECTED - Four-level graduated alerting (info/warn/error/critical) is standard for production SRE practices
5. ❌ **StateProvider simplification**: REJECTED - Interface provides better abstraction and testability than raw callbacks
6. ❌ **Cache consolidation**: REJECTED - Manager and Calculator caches serve different purposes and are both necessary

**Target Completion**: 4 weeks (including testing, review, and operational validation)

**Production Readiness**: This feature is CRITICAL for production deployments and should be completed before any production rollout to prevent cascading failures during transient NATS outages. The refinements significantly improve resilience to complex failure scenarios including split-brain recovery, cold starts, and environment-specific requirements.
