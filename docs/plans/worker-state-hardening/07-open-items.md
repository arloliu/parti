# Open Verification Items — Worker-State Hardening

The worker-state audit (`tmp/worker_state_analysis/00-report.md`) surfaced three
scenarios that neither the audit nor either reviewer could close conclusively.
Each needs a separate investigation round before it can be promoted into a fix
PR. Listed here so the next reviewer does not re-derive them from the audit.

## 1. Capability-bit propagation

**Question.** Does any code path advertise `CapAckV1` (`types/heartbeat.go:40-47`)
while the ack-publish wiring is broken or absent? The capability bit is a
contract with consumers; a worker that publishes ack-supported in its heartbeat
but cannot actually deliver acks would silently degrade downstream behavior.

**Suggested investigation.** Trace every site that sets `Capability` fields on a
heartbeat payload. For each, verify the corresponding ack publisher is wired
and not nil at the moment the capability is advertised. If any path stores the
bit before the publisher is initialized, file as a real bug; otherwise mark
verified.

## 2. Rolling-upgrade `LeaderRevision` consistency

**Question.** Does `m.lastSeenLeaderRevision` initialize correctly across leader
handoff under all bootstrap paths (cold start, takeover from a v2.3.0 leader,
takeover from a v2.x leader after a graceful Stop, recovery after a degraded
window)? A stale `lastSeenLeaderRevision` lets the authority selector pick a
prior leader's commit over a fresher one (see `manager_select_authority.go:33-66`).

**Suggested investigation.** Map the four bootstrap paths against the
`lastSeenLeaderRevision` write sites. Confirm every path either reads the
revision from a published commit on takeover or seeds it to zero so the next
commit advances monotonically. Add a manager-level rolling-upgrade test if
any path lacks a seed point.

## 3. Source-revision rollback handling

**Question.** A custom `WatchablePartitionSource` implementation could emit a
regressed `SourceRevision` (revision N+1 followed by revision N) — either by
bug, by a source-side restore-from-backup, or by a fleet-wide rollback. The
calculator currently treats every emitted source revision as authoritative.
Does it need a defensive guard?

**Suggested investigation.** Audit the partition-source contract documentation
(`docs/STRATEGIES.md`, the `WatchablePartitionSource` interface). If the
contract says "monotonic SourceRevision", the calculator should assert it
defensively (log + skip on regression). If the contract permits regressions,
the calculator should document the operational effect — a regression replays
the rebalance pipeline for what looks like a topology change.

---

These items are intentionally not part of the PR-1 through PR-7 sequence
because each one needs verification before its scope can be sized. They are
tracked here so the next worker-state-hardening review picks them up rather
than re-discovering them from the audit transcript.
