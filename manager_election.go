package parti

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/arloliu/parti/v2/internal/election"
	"github.com/arloliu/parti/v2/internal/heartbeat"
	"github.com/arloliu/parti/v2/internal/stableid"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// claimLostStopTimeout bounds the self-stop triggered by a lost stable-ID
// claim, so a sluggish KV cannot make the stopping worker hang indefinitely.
const claimLostStopTimeout = 30 * time.Second

// revokeWorkerConsumer removes all partitions from the worker consumer, if one
// is configured, so it stops pulling messages for an ID this worker no longer
// owns. UpdateWorkerConsumer with a nil partition set reconciles the consumer
// to zero subjects, tearing down every per-partition pull loop. Best-effort:
// errors are logged, not returned.
func (m *Manager) revokeWorkerConsumer(ctx context.Context, workerID string) {
	if m.consumerUpdater == nil {
		return
	}
	if err := m.consumerUpdater.UpdateWorkerConsumer(ctx, workerID, nil); err != nil {
		m.logger.Error("failed to revoke worker consumer partitions after claim loss",
			"worker_id", workerID, "error", err)
	}
}

// claimLostShutdown stops the Manager after its stable-ID claim is lost. A lost
// claim is unrecoverable in place, so the worker must cease all partition
// processing. It is a package-level indirection so onClaimerError's routing is
// unit-testable without a fully wired Manager; production never reassigns it.
//
// Manager.Stop transitions to Shutdown and tears the worker down, but it does
// NOT revoke the worker consumer — the WorkerConsumerUpdater is owned by the
// embedding application, and on a normal (app-initiated) Stop the app drains it
// itself. A claim-loss stop is not app-initiated, so claimLostShutdown revokes
// the worker consumer explicitly, applying an empty partition set so it stops
// pulling messages for the lost ID.
//
// Order matters: Stop runs FIRST, then the revoke. Stop cancels m.ctx and
// m.wg.Wait()s the assignment-apply loop dead, so after it returns nothing can
// re-apply a non-empty assignment and the worker consumer's update mutex is
// uncontended. Revoking BEFORE Stop could deadlock — see the inline comment.
//
// The whole sequence runs in its own goroutine: onClaimerError executes on the
// claimer's renewal goroutine, and Manager.Stop -> idClaimer.Release waits on
// that goroutine's doneCh — calling Stop synchronously here would deadlock.
var claimLostShutdown = func(m *Manager) {
	go func() {
		workerID := m.WorkerID()

		// Stop the Manager first. Stop cancels m.ctx — which unblocks any
		// in-flight assignment update — and waits for the assignment-apply loop
		// to exit (m.wg.Wait). Revoking the worker consumer BEFORE Stop could
		// deadlock: revokeWorkerConsumer blocks on the worker consumer's update
		// mutex, which an in-flight assignment update may hold while parked in
		// an m.ctx-scoped JetStream call — and m.ctx is only cancelled once
		// Stop runs.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), claimLostStopTimeout)
		defer stopCancel()
		if err := m.Stop(stopCtx); err != nil && !errors.Is(err, types.ErrNotStarted) {
			m.logger.Error("error stopping worker after stable-ID claim loss", "error", err)
		}

		// With the apply loop provably gone and m.ctx cancelled, revoke the
		// worker consumer (empty partition set) so it stops pulling messages
		// for the lost ID. This is the authoritative final state. A fresh
		// context is used since stopCtx may be near-expired after a slow Stop.
		revokeCtx, revokeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer revokeCancel()
		m.revokeWorkerConsumer(revokeCtx, workerID)
	}()
}

// onClaimerError handles background errors from the stable-ID claimer.
//
// ErrClaimLost means another worker now owns this ID. recordKVError would
// silently drop it (it is neither a connectivity nor a degrading-JetStream
// error), and Degraded mode does not halt processing — so the worker is
// stopped outright via claimLostShutdown, and the cause is surfaced through the
// OnError hook (fired by logError) so the embedding application can start a
// fresh worker. Every other error flows through the normal degraded-mode KV
// circuit.
func (m *Manager) onClaimerError(err error) {
	if errors.Is(err, stableid.ErrClaimLost) {
		m.logError("stable worker ID claim lost, stopping worker", "worker_id", m.WorkerID(), "error", err)
		claimLostShutdown(m)
		return
	}
	m.recordKVError(err)
}

// claimWorkerID claims a stable worker ID.
func (m *Manager) claimWorkerID(ctx context.Context, kv jetstream.KeyValue) error {
	claimer := stableid.NewClaimer(
		kv,
		m.cfg.WorkerIDPrefix,
		m.cfg.WorkerIDMin,
		m.cfg.WorkerIDMax,
		m.cfg.WorkerIDTTL,
		m.logger,
	)
	// Route renewal failures: ErrClaimLost stops the worker, everything else
	// feeds the degraded-mode circuit — see onClaimerError.
	claimer.SetOnError(m.onClaimerError)
	m.idClaimer = claimer

	workerID, err := claimer.Claim(ctx)
	if err != nil {
		return fmt.Errorf("failed to claim ID: %w", err)
	}

	m.workerID.Store(workerID)
	m.logger.Info("claimed stable worker ID", "worker_id", workerID)

	if err := claimer.StartRenewal(); err != nil {
		return fmt.Errorf("failed to start renewal: %w", err)
	}

	return nil
}

// participateElection participates in leader election.
func (m *Manager) participateElection(ctx context.Context, kv jetstream.KeyValue) error {
	workerID := m.WorkerID()

	// Use injected election agent if provided, otherwise create NATS election
	if m.electionAgent != nil {
		m.election = m.electionAgent
	} else {
		m.election = election.NewNATSElectionWithLogger(kv, "leader", m.logger)
	}

	electionAgent := m.election

	// Request leadership (TTL enforced by KV bucket)
	leaseDuration := int64(m.cfg.ElectionTimeout.Seconds())
	// Bound the election request with an operation timeout based on the manager's lifetime context
	// Using m.ctx here avoids inheriting an already-expired startup context while still enforcing
	// a strict per-call timeout via OperationTimeout.
	reqCtx, reqCancel := context.WithTimeout(m.ctx, m.cfg.OperationTimeout)
	defer reqCancel()
	start := time.Now()
	isLeader, err := electionAgent.RequestLeadership(reqCtx, workerID, leaseDuration)
	elapsed := time.Since(start)
	if err != nil {
		// Surface timeout distinctly for diagnostics
		if errors.Is(err, context.DeadlineExceeded) {
			m.logger.Warn("election request timed out", "worker_id", workerID, "elapsed", elapsed)
		}
		// If the startup context has already expired, reflect that explicitly for caller semantics
		if ctx.Err() != nil {
			return fmt.Errorf("failed to request leadership within startup window: %w", ctx.Err())
		}

		return fmt.Errorf("failed to request leadership: %w", err)
	}

	m.isLeader.Store(isLeader)

	// Invoke leadership hook if status changed (initial state is false)
	if isLeader && m.hooks != nil && m.hooks.OnLeadershipChanged != nil {
		m.invokeHook("leadership changed", func() error {
			return m.hooks.OnLeadershipChanged(m.ctx, true)
		})
	}

	if isLeader {
		m.logger.Info("elected as leader", "worker_id", workerID)
	} else {
		m.logger.Info("participating as follower", "worker_id", workerID)
	}

	// Start leadership monitoring
	m.wg.Go(m.monitorLeadership)

	return nil
}

// monitorLeadership monitors leader changes and renews leadership lease.
//
// Leaders periodically renew their lease to maintain leadership.
// Followers periodically attempt to claim leadership if it becomes vacant.
func (m *Manager) monitorLeadership() {
	ticker := time.NewTicker(m.cfg.ElectionTimeout / 3)
	defer ticker.Stop()

	leaseDuration := int64(m.cfg.ElectionTimeout.Seconds())

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			wasLeader := m.IsLeader()

			// If we're the leader, renew the lease
			if wasLeader {
				// Bound renew with operation timeout to avoid blocking the loop
				renewCtx, renewCancel := context.WithTimeout(m.ctx, m.cfg.OperationTimeout)
				err := m.election.RenewLeadership(renewCtx)
				renewCancel()
				if err != nil {
					m.logError("failed to renew leadership", "error", err)
					// Feed into degraded circuit: ErrBucketNotFound or
					// ErrStreamNotFound from the election bucket is the
					// canonical live-wipe signal on the leader side.
					m.recordKVError(err)
					// Leadership lost
					m.isLeader.Store(false)
					m.logger.Info("lost leadership", "worker_id", m.WorkerID())

					// Invoke leadership hook (tracked by WaitGroup)
					if m.hooks != nil && m.hooks.OnLeadershipChanged != nil {
						m.invokeHook("leadership lost", func() error {
							return m.hooks.OnLeadershipChanged(m.ctx, false)
						})
					}

					_ = m.stopCalculator()

					continue
				}
			} else {
				// Follower: Try to claim leadership if vacant
				// Bound acquisition with operation timeout to avoid blocking the loop
				reqCtx, reqCancel := context.WithTimeout(m.ctx, m.cfg.OperationTimeout)
				isLeader, err := m.election.RequestLeadership(reqCtx, m.WorkerID(), leaseDuration)
				reqCancel()
				if err != nil {
					m.logError("failed to request leadership", "error", err)
					// Feed into degraded circuit for the follower path:
					// if the election bucket is gone, every follower will
					// surface NotFound here each tick.
					m.recordKVError(err)

					continue
				}

				// Check if we became leader
				if isLeader {
					m.isLeader.Store(true)
					m.logger.Info("became leader", "worker_id", m.WorkerID())

					// Invoke leadership hook (tracked by WaitGroup)
					if m.hooks != nil && m.hooks.OnLeadershipChanged != nil {
						m.invokeHook("leadership gained", func() error {
							return m.hooks.OnLeadershipChanged(m.ctx, true)
						})
					}

					// Start calculator. If this fails we must NOT silently keep
					// leadership — the pod would hold the election lease without
					// a working calculator, so followers waiting on assignment
					// keys would hang until the lease TTL expires (potentially
					// forever if renewal keeps succeeding). Give up leadership
					// so another pod can attempt; if every pod fails the same
					// way (e.g. KV MaxValueSize too small for the assignment),
					// the error is logged cluster-wide and operators can
					// diagnose instead of seeing a silent hang.
					if err := m.startCalculator(m.assignmentKV, m.heartbeatKV); err != nil {
						m.releaseLeadershipAfterCalculatorFailure(err)
					}
				}
			}
		}
	}
}

// releaseLeadershipAfterCalculatorFailure gives up leadership when
// startCalculator fails on takeover. Holding the election lease without a
// working calculator would leave followers hanging on assignment keys, so we
// release so another pod can attempt; if every pod fails the same way (e.g.
// KV MaxValueSize too small for the assignment), the error is logged
// cluster-wide and operators can diagnose instead of seeing a silent hang.
func (m *Manager) releaseLeadershipAfterCalculatorFailure(cause error) {
	m.logError("failed to start calculator after takeover, releasing leadership", "error", cause)
	m.isLeader.Store(false)

	releaseCtx, releaseCancel := context.WithTimeout(m.ctx, m.cfg.OperationTimeout)
	relErr := m.election.ReleaseLeadership(releaseCtx)
	releaseCancel()
	if relErr != nil && !errors.Is(relErr, election.ErrNotLeader) {
		m.logError("failed to release leadership after calculator failure", "error", relErr)
	}

	if m.hooks != nil && m.hooks.OnLeadershipChanged != nil {
		m.invokeHook("leadership released after calculator failure", func() error {
			return m.hooks.OnLeadershipChanged(m.ctx, false)
		})
	}
}

// leaderReviser is satisfied by election implementations that expose their
// current KV revision (e.g. NATSElection). Checked via type assertion.
type leaderReviser interface {
	Revision() uint64
}

// leaderChecker is satisfied by election implementations that can perform a
// LIVE verification of the leader-key revision (e.g. NATSElection). The
// publisher uses this for its pre-alias and post-alias leadership rechecks
// (publish steps 5 and 7) — a cached revision is not sufficient because a
// former leader's local state may not yet reflect a takeover.
type leaderChecker interface {
	CheckLeadership(ctx context.Context, claimed uint64) error
}

// electionRevision returns the current leader KV revision, or 0 if the election
// implementation does not expose one. Used to populate Assignment.LeaderRevision.
func (m *Manager) electionRevision() uint64 {
	if r, ok := m.election.(leaderReviser); ok {
		return r.Revision()
	}
	return 0
}

// checkElectionLeadership performs a LIVE verification of the leader-key
// revision against the claimed value. Used by the assignment publisher's
// pre-alias and post-alias leadership fences.
//
// Behavior:
//   - When the election agent implements leaderChecker (production: NATSElection),
//     this calls into it; the agent performs a kv.Get on the leader key and
//     compares revisions, returning a typed sentinel on mismatch.
//   - When the agent does NOT implement leaderChecker (Nop / custom mocks),
//     the check is a no-op (returns nil).
//
// Parameters:
//   - ctx: Context for the underlying KV read.
//   - claimed: The leader-key revision the publisher asserts it holds.
//
// Returns:
//   - nil if the live revision matches (or the agent has no live check).
//   - a wrapped types.ErrLeadershipRevisionMismatch on a real mismatch.
//   - any other error from the underlying KV read.
func (m *Manager) checkElectionLeadership(ctx context.Context, claimed uint64) error {
	if c, ok := m.election.(leaderChecker); ok {
		return c.CheckLeadership(ctx, claimed)
	}
	return nil
}

// startHeartbeat starts publishing heartbeats.
//
// Wires the capability function so every heartbeat reflects the manager's
// live runtime capability bitmask. Sets CapAckV1 after a successful start
// because ack-publishing capability is intrinsic to the v1 publisher.
func (m *Manager) startHeartbeat(kv jetstream.KeyValue) error {
	workerID := m.WorkerID()
	publisher := heartbeat.New(kv, "heartbeat", workerID, m.cfg.HeartbeatInterval, m.metrics, m.logger)
	// Feed publish failures into the degraded-mode circuit so sustained KV
	// errors (connection loss or bucket wipe) drive the manager into Degraded.
	publisher.SetOnError(m.recordKVError)
	// Wire the capability function so the publisher reads the live bitmask on
	// every heartbeat composition, reflecting runtime wire-up state.
	publisher.SetCapabilitiesFn(m.Capabilities)
	m.heartbeat = publisher

	// Start heartbeat in background
	if err := publisher.Start(m.ctx); err != nil {
		return fmt.Errorf("failed to start publisher: %w", err)
	}

	// CapAckV1 is intrinsic to the v1 publisher: if we reach here, the worker
	// is ack-capable. Set the bit after a successful start so it is reflected
	// in all subsequent heartbeats.
	m.SetCapability(types.CapAckV1, true)

	return nil
}

// waitForAssignment waits for initial assignment.
func (m *Manager) waitForAssignment(ctx context.Context, assignmentKV, _ jetstream.KeyValue) error {
	// If leader, calculate and publish initial assignment
	if m.IsLeader() {
		if err := m.calculateAndPublish(ctx); err != nil {
			return fmt.Errorf("failed to calculate initial assignment: %w", err)
		}
	}

	// Wait for assignment to appear in KV
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			curAssignment, err := m.fetchAssignment(ctx, assignmentKV)
			if err != nil {
				return fmt.Errorf("failed to fetch assignment: %w", err)
			}

			if curAssignment != nil {
				m.assignment.Store(*curAssignment)
				m.logger.Info("received initial assignment",
					"worker_id", m.WorkerID(),
					"partitions", len(curAssignment.Partitions),
					"version", curAssignment.Version,
				)

				return nil
			}
		}
	}
}
