package assignment

import (
	"context"
	"slices"
	"time"

	"github.com/arloliu/parti/v2/types"
)

// requiredAuditCaps is the full safety chain a worker must report before the
// leader-side audit may rebalance away from it (§4.2). Workers reporting any
// subset of this mask are gated out of audit_repair escalation regardless of
// how behind they appear.
const requiredAuditCaps = types.CapAckV1 | types.CapTwoPhaseHandoff | types.CapProcessingGate

// auditLoop drives auditApplied on c.AuditInterval ticks until the calculator
// stops. Closes c.auditDoneCh on return so tests can deterministically observe
// loop termination.
func (c *Calculator) auditLoop(ctx context.Context) {
	defer close(c.auditDoneCh)

	ticker := time.NewTicker(c.AuditInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.auditApplied(ctx)
		}
	}
}

// auditApplied performs one pass of the leader-side apply audit (§4.2).
//
// The audit reads the publisher's cached LastCommit() (no per-tick KV Get) and
// the live heartbeat map, classifies every worker in commit.Workers as
// fullyApplied / behind / unverifiable, records gauges, and — only past the
// ExtendedApplyGracePeriod with the full safety chain on both sides and
// EnableTwoPhaseHandoff=true — schedules an audit_repair rebalance.
//
// Source-revision drift between currentSourceRevision and commit.SourceRevision
// is intentionally NOT audited here. That mismatch is rebalance-eligible via
// monitorPartitions on the leader side, not via the worker-receipt audit.
//
// All comparisons are against the in-memory commit snapshot; no fresh source
// list is fetched.
func (c *Calculator) auditApplied(ctx context.Context) {
	commit := c.publisher.LastCommit()
	if commit == nil {
		// Pre-first-commit bootstrap path. Nothing to audit yet.
		return
	}

	hbs, err := c.monitor.GetHeartbeats(ctx)
	if err != nil {
		c.Logger.Debug("audit: GetHeartbeats failed", "error", err)
		return
	}

	fullyApplied, behind, unverifiable := c.classifyAuditWorkers(commit, hbs)
	c.Metrics.RecordAuditCounts(len(fullyApplied), len(behind), len(unverifiable))

	// Grace window 1: do not emit retry-pressure metrics for workers that have
	// simply not had time to apply yet.
	if time.Since(commit.PublishedAt) < c.ApplyGracePeriod {
		return
	}
	for w := range behind {
		c.Metrics.RecordWorkerBehind(w, commit.Version)
	}

	// Grace window 2: even past retry-pressure, do not escalate until
	// ExtendedApplyGracePeriod. This avoids ping-ponging assignments while
	// slow workers catch up.
	if time.Since(commit.PublishedAt) < c.ExtendedApplyGracePeriod {
		return
	}

	c.maybeEscalateAudit(ctx, behind, fullyApplied, hbs)
}

// classifyAuditWorkers partitions commit.Workers into three disjoint sets:
//   - fullyApplied: receipts match the commit on version, leader, source rev,
//     and payload digest.
//   - behind: receipts disagree on at least one of those fields, OR the
//     commit is malformed for this worker (Workers lists them but Payloads
//     does not).
//   - unverifiable: heartbeat missing, malformed, schema=0, or CapAckV1 clear.
//
// Workers with a heartbeat but absent from commit.Workers are implicitly
// revoked at this commit; they are not audited.
func (c *Calculator) classifyAuditWorkers(commit *types.AssignmentCommit, hbs map[string]types.Heartbeat) (
	fullyApplied, behind, unverifiable map[string]struct{},
) {
	fullyApplied = make(map[string]struct{}, len(commit.Workers))
	behind = make(map[string]struct{}, len(commit.Workers))
	unverifiable = make(map[string]struct{}, len(commit.Workers))

	for _, w := range commit.Workers {
		hb, ok := hbs[w]
		if !ok {
			unverifiable[w] = struct{}{}
			continue
		}
		if hb.SchemaVersion == 0 || hb.Capabilities&types.CapAckV1 == 0 {
			unverifiable[w] = struct{}{}
			continue
		}

		// Strict source-revision check: when the commit declares its source
		// revision known, a worker reporting AppliedSourceRevKnown=false is
		// classified as behind (per the brief — not as unverifiable, since
		// the worker is otherwise ack-capable).
		srcRevMatch := !commit.SourceRevisionKnown ||
			(hb.AppliedSourceRevKnown && hb.AppliedSourceRevision == commit.SourceRevision)

		ref, hasRef := commit.Payloads[w]
		if !hasRef {
			// Worker is in commit.Workers but the commit map is missing its
			// payload ref. This is a malformed commit for this worker — count
			// as behind so retry pressure surfaces, but do NOT escalate (we
			// would only re-publish the same malformed commit).
			behind[w] = struct{}{}
			continue
		}

		mismatch := hb.LeaderRevision != commit.LeaderRevision ||
			hb.AppliedVersion != commit.Version ||
			!srcRevMatch ||
			hb.AppliedDigest != ref.SetDigest
		if mismatch {
			behind[w] = struct{}{}
			continue
		}
		fullyApplied[w] = struct{}{}
	}

	return fullyApplied, behind, unverifiable
}

// maybeEscalateAudit applies the capability and config gates and, when
// satisfied, schedules an "audit_repair" rebalance via the state machine.
//
// Three skip paths are tracked distinctly:
//   - cap_missing_behind (per behind worker without full caps)
//   - cap_missing_targets (no fully-applied worker carries the full chain)
//   - direct_mode (EnableTwoPhaseHandoff=false)
//
// Skip metrics never emit when there is nothing to escalate (i.e. the behind
// set is already empty after capability filtering AND no caller-visible
// target gap exists).
func (c *Calculator) maybeEscalateAudit(
	ctx context.Context,
	behind, fullyApplied map[string]struct{},
	hbs map[string]types.Heartbeat,
) {
	// Filter behind workers whose caps allow safe reassignment.
	behindReassignable := make([]string, 0, len(behind))
	for w := range behind {
		if hbs[w].Capabilities&requiredAuditCaps != requiredAuditCaps {
			c.Metrics.RecordAuditEscalationSkipped("cap_missing_behind", w)
			continue
		}
		behindReassignable = append(behindReassignable, w)
	}
	if len(behindReassignable) == 0 {
		// Nothing escalatable — cap_missing_behind metric(s) already covered
		// the per-worker case. Skipping the targets/direct-mode signals
		// avoids emitting noisy "no targets" / "direct mode" events when
		// there was nothing to escalate to begin with.
		return
	}

	// Filter target workers that carry the full safety chain.
	targets := make([]string, 0, len(fullyApplied))
	for w := range fullyApplied {
		if hbs[w].Capabilities&requiredAuditCaps == requiredAuditCaps {
			targets = append(targets, w)
		}
	}
	if len(targets) == 0 {
		c.Metrics.RecordAuditEscalationSkipped("cap_missing_targets", "")
		return
	}

	if !c.EnableTwoPhaseHandoff {
		c.Metrics.RecordAuditEscalationSkipped("direct_mode", "")
		return
	}

	// Sort for log determinism; the audit is rare so the allocation is fine.
	slices.Sort(behindReassignable)
	slices.Sort(targets)
	c.Logger.Info("audit escalating to audit_repair rebalance",
		"behind", behindReassignable,
		"targets", targets,
	)

	// Reuse EnterScaling with zero window — the behind set is already known,
	// no need for an additional stabilization window. We do NOT bypass the
	// cooldown: audit repair is non-emergency, and the state machine
	// already refuses to enter scaling from non-idle states.
	c.stateMach.EnterScaling(ctx, "audit_repair", 0)
}
