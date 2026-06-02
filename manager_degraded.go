package parti

import (
	"context"
	"errors"
	"time"

	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/arloliu/parti/v2/kvutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
)

// degradedReasonKVUnavailable is the distinct enterDegraded reason for the
// connected-but-KV-unavailable condition (a bucket reachable on the connection
// but unable to serve ops because its RAFT quorum is lost). It is kept separate
// from "KV error threshold exceeded" so the operator-facing Degraded surface
// distinguishes a quorum-loss op stall from a whole-bucket wipe, and so the
// docstring contract that whole-bucket loss is the ONLY path to the threshold
// reason is preserved.
const degradedReasonKVUnavailable = "kv-unavailable"

// degradedReasonEnumerationStall is the distinct enterDegraded reason for a
// sustained leader-side worker-enumeration (heartbeat Keys scan) stall that the
// connectivity / degrading classifiers miss (NP-10). Kept separate so the
// recovery exit can require an enumeration success before exiting, and so the
// operator surface distinguishes it from a kv-unavailable op stall.
const degradedReasonEnumerationStall = "heartbeat-enumeration-stall"

// kvErrorEvent is one entry in the degraded-circuit error window.
//
// transient marks an F-D1 connected-but-KV-unavailable timeout (an
// [ErrKVUnavailable]-wrapped deadline / no-responders from a periodic KV-op
// site). A successful periodic KV op while not degraded clears ONLY the
// transient entries (see [Manager.recordKVHealthyOp]), giving the F-D1 circuit
// consecutive-error semantics. Whole-bucket-loss entries (connectivity /
// degrading-JetStream, transient=false) are never cleared by a healthy op, so
// they still accumulate to the threshold — preserving the AGENTS.md contract
// that whole-bucket loss reliably drives every worker Degraded even when an
// unaffected bucket (e.g. heartbeat) keeps succeeding.
type kvErrorEvent struct {
	at        time.Time
	transient bool
}

// ErrKVUnavailable marks a KV operation that failed because its backing
// bucket is reachable on the live NATS connection but cannot serve the op
// (deadline / no-responders) — the quorum-loss condition the connection-status
// monitor and the connectivity/degrading-JetStream classifiers all miss.
//
// It is applied ONLY at the manager's own periodic KV-op call sites via
// [markKVUnavailable], never added to any global predicate. That keeps an
// unwrapped deadline / no-responders from anywhere else (notably the
// peer-takeover claim path through onClaimerError) out of the degraded circuit,
// preserving the cross-feature classification contracts.
var ErrKVUnavailable = errors.New("kv unavailable")

// markKVUnavailable wraps err with [ErrKVUnavailable] iff err is an
// otherwise-unclassified deadline / no-responders timeout. Existing classifiers
// win first: if err already classifies as connectivity (e.g.
// jetstream.ErrNoStreamResponse, the whole-bucket-loss surface) or as degrading
// JetStream (bucket / stream / consumer missing), it is returned unchanged so
// the established routes keep ownership. nil and unrelated errors pass through
// unchanged. Applied at the manager's heartbeat / election / assignment-watcher
// / stableid-renew / commit KV-op sites.
func markKVUnavailable(err error) error {
	if err == nil {
		return nil
	}
	// Existing classifiers win first — this is what makes it impossible to
	// steal the whole-bucket (ErrNoStreamResponse) or bucket-missing route.
	if natsutil.IsConnectivityError(err) || natsutil.IsDegradingJetStreamError(err) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, nats.ErrNoResponders) {
		return errors.Join(ErrKVUnavailable, err)
	}

	return err
}

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
	// Stream-missing exhaustion is routed through the dynamic-consumer
	// observer (Manager.onStreamMissingError → enterDegraded(
	// "stream-missing-recovery-exhausted")), NOT through the generic
	// KV-error threshold. Short-circuit here so an incidental wrap of
	// jetstream.ErrStreamNotFound (which natsutil treats as a
	// degrading-JetStream error) does not double-count or trip the
	// threshold. Preserves the AGENTS.md cross-feature contract that
	// whole-bucket loss is the ONLY path through recordKVError →
	// enterDegraded("KV error threshold exceeded").
	if errors.Is(err, types.ErrStreamMissing) {
		return
	}
	// kvUnavailable is the F-D1 path: a marked connected-but-KV-unavailable
	// timeout from one of the manager's periodic KV-op sites. It is admitted
	// here (the connection-status monitor and the connectivity/degrading
	// classifiers all miss it) and degrades with a distinct reason below.
	kvUnavailable := errors.Is(err, ErrKVUnavailable)
	if !natsutil.IsConnectivityError(err) && !natsutil.IsDegradingJetStreamError(err) && !kvUnavailable {
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

	// Add error timestamp, tagged with its class so a healthy-op success can
	// later clear the transient (F-D1) entries while leaving whole-bucket-loss
	// entries to accumulate.
	m.kvErrorWindow = append(m.kvErrorWindow, kvErrorEvent{at: now, transient: kvUnavailable})

	// Remove errors outside the window
	windowStart := now.Add(-m.cfg.DegradedBehavior.KVErrorWindow)
	validIdx := 0
	for i, e := range m.kvErrorWindow {
		if e.at.After(windowStart) {
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
		// Whole-bucket loss (connectivity / degrading JetStream) keeps the
		// canonical threshold reason — the AGENTS.md contract that whole-bucket
		// loss is the ONLY path to "KV error threshold exceeded". A marked
		// connected-but-KV-unavailable timeout degrades with the distinct
		// reason. Errors that classify both ways (a bucket-missing error never
		// reaches markKVUnavailable's wrap branch) keep the threshold
		// reason, so the contract holds.
		reason := "KV error threshold exceeded"
		if kvUnavailable {
			reason = degradedReasonKVUnavailable
		}
		m.logger.Warn(reason,
			"count", count,
			"threshold", m.cfg.DegradedBehavior.KVErrorThreshold,
			"window", m.cfg.DegradedBehavior.KVErrorWindow,
		)
		m.enterDegraded(reason)
	}
}

// recordKVOpError marks err as a possible connected-but-KV-unavailable
// timeout (via [markKVUnavailable]) and feeds it to [Manager.recordKVError].
// It is the single entry point for the manager's periodic KV-op call sites
// (heartbeat / election / assignment-watcher / stableid-renew / commit) so a
// future site only has to call this — not remember to wrap. The
// peer-takeover claim path (onClaimerError's ErrClaimLost branch) intentionally
// does NOT route through here, keeping its claim-lost shutdown semantics.
func (m *Manager) recordKVOpError(err error) {
	m.recordKVError(markKVUnavailable(err))
}

// recordKVSuccess clears the ENTIRE degraded-circuit error window (both
// transient and whole-bucket entries). It is the full reset used by the recovery
// path (attemptRecoveryFromDegraded) after a healthy assignment read confirms KV
// is serving again, so a freshly-recovered worker does not instantly re-trip on
// stale window entries.
func (m *Manager) recordKVSuccess() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.kvErrorWindow = m.kvErrorWindow[:0]
	m.kvErrorCount.Store(0)
}

// recordKVHealthyOp clears the transient (F-D1 [ErrKVUnavailable]) entries from
// the degraded-circuit window on a successful periodic KV op while the worker is
// not degraded. This gives the F-D1 circuit consecutive-error semantics: a run
// of connected-but-KV-unavailable timeouts only trips Degraded if no success
// intervenes, instead of summing intermittent blips across KVErrorWindow.
//
// Whole-bucket-loss entries (connectivity / degrading-JetStream) are retained,
// so a sustained whole-bucket loss still accumulates to the threshold even while
// an unaffected high-frequency op (the heartbeat publisher) keeps succeeding —
// preserving the AGENTS.md whole-bucket-loss contract.
//
// It is a no-op while degraded: recordKVError already short-circuits there so the
// window is not growing, and the early return keeps the hot heartbeat-success
// path (every HeartbeatInterval) off m.mu once a real outage is in progress.
func (m *Manager) recordKVHealthyOp() {
	// Stamp the heartbeat-success time unconditionally: the reason-scoped
	// recovery gate needs to observe a heartbeat Put succeeding AFTER a
	// kv-unavailable degrade. This must run even while degraded (the
	// window-clear below intentionally does not).
	m.lastHeartbeatSuccessAt.Store(time.Now().UnixNano())

	if m.degradedSince.Load() != 0 {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.kvErrorWindow) == 0 {
		return
	}

	kept := m.kvErrorWindow[:0]
	for _, e := range m.kvErrorWindow {
		if !e.transient {
			kept = append(kept, e)
		}
	}
	m.kvErrorWindow = kept

	windowLen := min(len(kept), 0x7FFFFFFF)
	m.kvErrorCount.Store(int32(windowLen)) // #nosec G115 - bounded above
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

	// Only the CAS winner reaches here, so this is the sole writer of the active
	// reason — no loser-clobber. The recovery gate treats the brief
	// CAS-won-but-reason-not-yet-stored window as "" and stays degraded that tick.
	m.lastDegradedReason.Store(reason)

	// Attempt validated state transition. Roll back BOTH reason and degradedSince
	// on failure (clear reason BEFORE degradedSince so the happens-before holds).
	// Failure can only happen from Init/ClaimingID states that forbid degraded
	// entry.
	if !m.transitionState(StateDegraded) {
		m.lastDegradedReason.Store("")
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

	// Clear the reason BEFORE clearing degradedSince: a subsequent enterDegraded
	// can only win its CAS after degradedSince becomes 0, so this clear
	// happens-before that winner's reason store — no clobber, and the gap between
	// an exit and the next store reads as "" (gate stays that tick). Clearing
	// degradedSince after a successful state transition also lets future
	// enterDegraded calls re-arm correctly (fixes the re-entry bug).
	m.lastDegradedReason.Store("")
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

// onEnumerationStall is wired to the calculator's OnEnumerationError. The
// calculator fires it only after EnumerationFailureThreshold consecutive
// non-connectivity enumeration failures, so the stall is already sustained:
// degrade directly, bypassing the transient kvErrorWindow (a still-succeeding
// heartbeat Put would clear a transient entry before it accumulated — the F-D1
// constraint). enterDegraded is idempotent (CAS), so repeated calls while
// degraded are no-ops.
func (m *Manager) onEnumerationStall(err error) {
	m.logger.Warn("sustained heartbeat-enumeration stall; entering degraded", "error", err)
	m.enterDegraded(degradedReasonEnumerationStall)
}

// recordEnumerationSuccess is wired to the calculator's OnEnumerationSuccess. It
// stamps the recovery signal the reason-scoped exit gate keys on. Stamped on
// every successful enumeration (even while degraded), mirroring recordKVHealthyOp.
func (m *Manager) recordEnumerationSuccess() {
	m.lastEnumerationSuccessAt.Store(time.Now().UnixNano())
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

	// The refresh succeeded (assignment reads are healthy), so the KV-error
	// window the read just exercised is stale. Record success on BOTH branches
	// below — it is not branch-dependent (harmless on the stay-degraded path
	// since recordKVError short-circuits while degraded).
	m.recordKVSuccess()

	// Self-heal guard: stay degraded + re-arm an apply whenever the current
	// snapshot assignment has NOT been successfully applied+acked
	// (currentAssignmentApplied compares the full applied-ack identity). This
	// covers a worker that never committed (startup bootstrap, F-D3), one that
	// committed an earlier version but whose version-advance apply failed (the
	// latched case), an empty/revoke-all apply that failed, and a same-version
	// higher-LR re-issue that only entered the snapshot via the refresh above.
	// exitDegraded here would otherwise report StateStable with the current
	// assignment's claims/ack uncommitted. cur is read AFTER the refresh so the
	// re-arm targets the post-refresh assignment. The guard keys on commitment
	// STATE, not the degraded reason — exiting with an unapplied assignment is
	// wrong regardless of why we degraded. See
	// docs/plans/auto-healing-quorum-loss-fix/03-latched-worker-version-commitment-plan.md.
	cur := m.CurrentAssignment()
	if !m.currentAssignmentApplied(cur) {
		m.scheduleApplyRetry(cur)
		return
	}

	// The degrade reason is written by the winning enterDegraded just after its
	// CAS (Step 4). An empty read means this entry's reason is not yet observable
	// (or we raced an exit) — stay degraded this tick rather than risk skipping a
	// reason-scoped gate below. One-tick delay only.
	reason, _ := m.lastDegradedReason.Load().(string)
	if reason == "" {
		return
	}

	// Family B — reason-scoped recover-on-wrong-signal gate. A kv-unavailable
	// degrade is a connected-but-KV-unavailable op stall on the heartbeat /
	// election / stableid buckets; the commitment guard above reads only the
	// (unaffected) assignment bucket, so it cannot tell the failing op recovered.
	// Require a heartbeat Put success stamped AFTER we degraded. Reason-scoped:
	// a non-kv-unavailable degrade (e.g. "startup-timeout", NP-5) recovers via
	// the commitment guard alone — an UNCONDITIONAL gate regresses NP-5, which
	// has no heartbeat publisher so lastHeartbeatSuccessAt stays 0. Cheap atomic
	// reads, evaluated before the Family A network re-probe added in a later task.
	if reason == degradedReasonKVUnavailable {
		hbAt := m.lastHeartbeatSuccessAt.Load()
		since := m.degradedSince.Load()
		if hbAt == 0 || hbAt <= since {
			m.logger.Debug("recovery: heartbeat KV not recovered since kv-unavailable degrade; staying Degraded",
				"last_heartbeat_success_unixnano", hbAt, "degraded_since_unixnano", since)
			return
		}
	}

	// Family A — refuse a recovery exit while a Parti-owned bucket wipe-and-
	// recreate is still outstanding. A live re-probe; a missing/timing-out bucket
	// is skipped (Family B's / the connection monitor's concern), so this conjunct
	// fires only on a CONFIRMED Created mismatch.
	if m.epochMismatchOutstanding(m.ctx) {
		m.logger.Warn("recovery: bucket epoch mismatch outstanding; staying Degraded for restart/rotation")
		return
	}

	// Heartbeat-bucket reachability backstop — refuse to exit while the heartbeat
	// bucket's stream is missing/unreachable (e.g. a single-node restart of an
	// un-migrated MemoryStorage heartbeat bucket). NOT reason-scoped: it backstops
	// the "KV error threshold exceeded" reason that the Family B kv-unavailable gate
	// does not cover and that Family A (recreate-only) does not catch for a missing
	// stream. Holds the worker terminally Degraded for rotation.
	if m.heartbeatBucketUnavailable(m.ctx) {
		m.logger.Warn("recovery: heartbeat bucket unavailable; staying Degraded for restart/rotation")
		return
	}

	// NP-10 — reason-scoped enumeration-recovery gate. A heartbeat-enumeration
	// stall degrade must not exit on the (unaffected) assignment read while the
	// Keys scan is still timing out — the leader would resume serving stale
	// membership. Require an enumeration success stamped AFTER we degraded.
	// Leadership-loss escape: enumeration is leader-only (startCalculator), so a
	// non-leader can never stamp a success — treat "not currently leader" as
	// enumeration-N/A and do NOT block, else a leader that lost leadership while in
	// this degrade would be stuck forever (a gate on a capability that cannot
	// fire). Reason-scoped, so other degrade reasons are unaffected.
	if reason == degradedReasonEnumerationStall && m.isLeader.Load() {
		ensAt := m.lastEnumerationSuccessAt.Load()
		since := m.degradedSince.Load()
		if ensAt == 0 || ensAt <= since {
			m.logger.Debug("recovery: worker enumeration not recovered since stall degrade; staying Degraded",
				"last_enumeration_success_unixnano", ensAt, "degraded_since_unixnano", since)
			return
		}
	}

	// Success - exit degraded mode
	m.exitDegraded()
}

// epochMismatchOutstanding reports whether any Parti-owned bucket's LIVE
// stream-Created no longer matches the value captured at Start — i.e. a
// wipe-and-recreate is still in effect. It is the Family A recovery-exit guard:
// the commitment guard can be satisfied by a version-monotonic republish into a
// recreated-empty bucket, so the exit must additionally refuse while a recreate
// is outstanding (terminal Degraded => restart/rotation, docs/OPERATIONS.md).
//
// This is a LIVE re-probe (not a latch) so there is no pre-arm window: the
// epoch-fence monitor ticks every OperationTimeout (10s) while recovery ticks
// every 1s, so a latch could arm after a faster recovery had already exited.
//
// Probe errors are NOT actionable and are skipped (mirroring checkBucketEpochs'
// continue-on-error): only a successful read with a DIFFERENT Created is a
// recreate. A missing/timing-out bucket is the connection monitor's / Family B's
// concern, not Family A's — this keeps the two exit conjuncts independent.
//
// Cost / timing: it runs inline on the 1s connection-monitor goroutine, but ONLY
// in the connected branch (attemptRecoveryFromDegraded is reached only after the
// connection is up for ExitThreshold), so the common case is a few sub-ms
// round-trips against reachable buckets (measured ~3ms for 4 buckets). Each
// bucket's probe is hard-bounded by OperationTimeout, so the worst case is
// len(bucketEpochs)*OperationTimeout of inline blocking — it degrades gracefully
// rather than wedging if a bucket goes unreachable mid-tick.
//
// Concurrency: m.bucketEpochs is written only at Start (captureBucketEpoch) and
// is read-only afterward, so ranging it from this (connection-monitor) goroutine
// is race-free — the SAME lock-free-read contract checkBucketEpochs relies on. It
// opens a FRESH probe handle per bucket rather than reusing ep.kv, which is owned
// by the monitorBucketEpochs goroutine and is not safe to share across goroutines
// (nats.go KeyValue handles cache *stream state).
func (m *Manager) epochMismatchOutstanding(ctx context.Context) bool {
	if m.js == nil {
		return false
	}
	for bucket, ep := range m.bucketEpochs {
		// Bound BOTH the handle open AND the status read with a single per-bucket
		// deadline: js.KeyValue does a stream-info round-trip, so passing the
		// unbounded ctx here would let it block on nats.go's internal default
		// (~5s) instead of OperationTimeout when a bucket is unreachable — turning
		// this into an unbounded inline stall on the connection-monitor goroutine.
		probeCtx, cancel := context.WithTimeout(ctx, m.cfg.OperationTimeout)
		probeKV, err := m.js.KeyValue(probeCtx, bucket)
		if err != nil {
			cancel()
			m.logger.Debug("epoch re-probe: open handle failed; not actionable", "bucket", bucket, "error", err)
			continue
		}
		live, err := kvutil.BucketStreamCreated(probeCtx, probeKV)
		cancel()
		if err != nil {
			m.logger.Debug("epoch re-probe: probe failed; not actionable", "bucket", bucket, "error", err)
			continue
		}
		if !live.Equal(ep.created) {
			return true
		}
	}

	return false
}

// heartbeatBucketUnavailable reports whether the heartbeat bucket's stream is
// currently missing or unreachable. It is the recovery-exit backstop for the
// heartbeat-bucket-loss flap that the reason-scoped Family B gate and the
// recreate-only Family A re-probe do not cover: a single-node restart of an
// un-migrated MemoryStorage heartbeat bucket loses the stream, the heartbeat
// publisher Put keeps failing, and the worker degrades with the whole-bucket-loss
// reason "KV error threshold exceeded" (manager_degraded.go:208-225) — NOT
// kv-unavailable (so Family B is silent) and not a Created mismatch (the stream is
// gone, not recreated, so epochMismatchOutstanding skips it). Without this guard
// the recovery exit heals on the unaffected assignment read and the fleet flaps.
//
// Unlike Family B this guard is NOT reason-scoped: it fires for ANY degrade reason,
// but only when the heartbeat bucket is genuinely unreachable, so it cannot regress
// a degrade whose heartbeat bucket is healthy (NP-5 startup-timeout, NP-3a disarmed
// — bucket reachable => returns false => recovery proceeds on the prior guards).
//
// Cost / timing: it runs inline on the 1s connection-monitor goroutine in the
// connected branch only, so the common case is one sub-ms KeyValue+Status round
// trip against a reachable bucket. The single per-call deadline (OperationTimeout)
// hard-bounds BOTH the handle open and the status read, so an unreachable bucket
// degrades to a bounded inline stall rather than wedging the goroutine.
//
// Concurrency: it opens a FRESH probe handle (not a cached one) for the same
// goroutine-safety reason as epochMismatchOutstanding — nats.go KeyValue handles
// cache *stream state and are not safe to share across goroutines. m.js == nil
// (unit-test managers) short-circuits to false.
func (m *Manager) heartbeatBucketUnavailable(ctx context.Context) bool {
	if m.js == nil {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, m.cfg.OperationTimeout)
	defer cancel()
	kv, err := m.js.KeyValue(probeCtx, m.cfg.KVBuckets.HeartbeatBucket)
	if err != nil {
		return true // bucket missing / unreachable
	}
	if _, err := kv.Status(probeCtx); err != nil {
		return true
	}

	return false
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
