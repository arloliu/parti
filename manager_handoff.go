package parti

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/ratelimit"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// HandoffClaimState represents the logical state of a handoff claim.
//
// Values:
//   - "stable":  No handoff in progress
//   - "prepare": Target worker declared intent to take over
//   - "commit":  Ownership switch in-progress, awaiting final stabilization
//   - "abort":   Handoff aborted (future use)
//   - "unknown": Parsing fallback
type HandoffClaimState string

const (
	HandoffClaimStable  HandoffClaimState = "stable"
	HandoffClaimPrepare HandoffClaimState = "prepare"
	HandoffClaimCommit  HandoffClaimState = "commit"
	HandoffClaimAbort   HandoffClaimState = "abort"
	HandoffClaimUnknown HandoffClaimState = "unknown"
)

// HandoffClaim is a read-only view of a partition handoff claim stored in the
// two-phase handoff KV bucket. It is intentionally decoupled from internal
// implementation details and suitable for diagnostics and tests.
type HandoffClaim struct {
	PartitionID   string            `json:"partition_id"`
	Owner         string            `json:"owner"`
	PendingOwner  string            `json:"pending_owner,omitempty"`
	State         HandoffClaimState `json:"state"`
	Epoch         int64             `json:"epoch"`
	LastUpdated   time.Time         `json:"last_updated"`
	TTLSeconds    int64             `json:"ttl_seconds"`
	ConflictCount int64             `json:"conflict_count,omitempty"`
}

// orphanClaimGrace is how long a stable handoff claim must be continuously
// observed absent from the leader's vouched partition set (source ∪ latest
// committed assignment — see livePartitionSet) before the coordinator's
// sweep reaps it. Claims for partitions permanently removed from the source
// otherwise accumulate forever (the handoff bucket deliberately carries no
// MaxAge — see reconcileHandoffBucketMaxAge). The grace is deliberately
// generous: orphan bloat is a slow leak, so the only hard requirement is
// that transient source churn (remove-then-readd within minutes) never
// qualifies. The revision-CAS delete in the sweep independently guarantees
// any concurrent claim transition wins over the reaper.
const orphanClaimGrace = 10 * time.Minute

// livePartitionSet supplies the two-phase coordinator's orphan-reap pass
// with the authoritative current partition set, keyed by SubjectKey (the
// claim key). It vouches (ok=true) only when this worker is the leader: a
// follower — whose source could be config-skewed during a rolling upgrade —
// never vouches, and the reap pass is skipped.
//
// The vouched set is the UNION of the leader's source view and the latest
// committed assignment's partition set. The source alone is not authority
// enough: after a partition is removed from the source there is a window —
// unbounded if the follow-up rebalance publish stalls — in which the live
// commit still references the partition and its owner is still consuming
// it through the processing gate. Reaping the claim in that window would
// make the gate NAK the legitimate owner (unknown_ownership) until some
// later apply recreates the claim. A partition referenced by EITHER view
// is therefore never an orphan.
func (m *Manager) livePartitionSet(ctx context.Context) (map[string]struct{}, bool) {
	if !m.isLeader.Load() {
		return nil, false
	}
	parts, err := m.source.List(ctx)
	if err != nil {
		return nil, false
	}
	commit, _, err := m.readCommitEntry(ctx)
	if err != nil {
		return nil, false
	}
	var commitSet map[string]struct{}
	if commit != nil {
		// Cached by (version, _commit revision); the payload fan-out only
		// re-runs when the commit actually changes.
		commitSet, err = m.currentCommitPartitionSet(ctx, commit.Version)
		if err != nil {
			return nil, false
		}
	}
	set := make(map[string]struct{}, len(parts)+len(commitSet))
	for _, p := range parts {
		set[p.SubjectKey()] = struct{}{}
	}
	maps.Copy(set, commitSet)

	return set, true
}

// runInitialHandoffResumeIfPending kicks the resume pass when the manager
// detected in-flight handoff claims at startup. Called from
// applyInitialAssignment after the unified apply pipeline succeeds.
//
// The pre-Phase-4 emitInitialAssignmentEvents + applyInitialHandoffAsync
// helpers were folded into applyAssignment (Apply → Store → Ack → Hooks);
// only the resume kick remains as separate startup-only logic.
func (m *Manager) runInitialHandoffResumeIfPending() {
	if !m.pendingHandoffResume.Load() {
		return
	}
	if !m.cfg.EnableTwoPhaseHandoff {
		return
	}
	m.wg.Go(func() {
		m.runHandoffResume(m.ctx)
	})
}

// handoffStartupHygiene performs a best-effort pass to clean up expired non-stable
// claims on manager startup. It resets such claims back to stable and clears
// pendingOwner while preserving the current epoch (no increment) to avoid
// introducing artificial transitions.
//
// This does not attempt to "complete" in-flight phases without context of
// intended assignments; the full resume logic is handled by subsequent Apply
// cycles. The goal here is only hygiene for obviously stale entries.
func (m *Manager) handoffStartupHygiene(ctx context.Context, store handoff.ClaimStore) bool {
	if store == nil {
		return false
	}

	start := time.Now()
	keys, err := store.ListKeys(ctx)
	if err != nil || len(keys) == 0 {
		if err != nil {
			m.logger.Debug("handoff_hygiene_list_error", "error", err)
		} else {
			m.logger.Debug("handoff_hygiene_list_empty", "elapsed", time.Since(start))
		}
		return false
	}

	now := time.Now().UTC()
	resumable := false
	resets := 0
	for _, pid := range keys {
		cur, rev, err := store.Get(ctx, pid)
		if err != nil || rev == 0 {
			continue
		}
		if cur.State == handoff.ClaimStateStable {
			continue
		}
		if !cur.IsExpired(now) {
			// Non-expired non-stable claim: mark resumable (prepare/commit) for later pass.
			resumable = true
			continue
		}

		next := cur.Copy()
		next.State = handoff.ClaimStateStable
		next.PendingOwner = ""
		next.LastUpdated = now

		// Pace the physical write if a claim-write limiter is configured. ctx
		// cancellation (this pass runs under OperationTimeout) stops the
		// best-effort sweep early — the remaining stale claims are reset on a
		// later startup or by the coordinator's periodic sweep. Conservatively
		// flag resumable: the truncated scan may not have reached a later
		// non-expired prepare/commit claim, so let the bounded, idempotent
		// resume pass run rather than risk skipping it.
		if err := ratelimit.Wait(ctx, m.claimWriteLimiter); err != nil {
			resumable = true
			break
		}

		// Best-effort CAS; ignore failures.
		_, _ = store.PutIfEpoch(ctx, pid, cur.Epoch, next)
		resets++

		// Emit stale claim metric for hygiene-driven reset (same semantic as sweeper) if recorder injected.
		if m.handoffMetrics != nil {
			m.handoffMetrics.IncClaimStoreStale()
		}
	}
	m.logger.Debug("handoff_hygiene_done", "keys", len(keys), "resets", resets, "resumable", resumable, "elapsed", time.Since(start))

	return resumable
}

// runHandoffResume completes safe, idempotent resume steps for this worker
// after the initial assignment has been applied. Currently it finalizes any
// commit->stable transitions for partitions owned by this worker, which is safe
// because owner is already set to this worker in the commit state.
func (m *Manager) runHandoffResume(ctx context.Context) {
	// Preconditions
	if !m.cfg.EnableTwoPhaseHandoff {
		return
	}
	// We need a ClaimStore. Re-open from KV to avoid storing global handle.
	bucket := m.cfg.KVBuckets.HandoffBucket
	if strings.TrimSpace(bucket) == "" {
		return
	}
	kv, err := m.js.KeyValue(ctx, bucket)
	if err != nil {
		return
	}
	store := handoff.NewNATSClaimStore(kv, "claims/")

	wid := m.WorkerID()
	finalized, completed := m.finalizeResumeClaims(ctx, store, wid)
	if finalized > 0 {
		m.logger.Info("handoff_resume_finalize", "worker_id", wid, "finalized", finalized)
	}

	// Clear the flag only when the pass ran to completion. A paced Wait
	// cancelled mid-pass leaves it set so a later pass can finish finalizing.
	if completed {
		m.pendingHandoffResume.Store(false)
	}
}

// finalizeResumeClaims is the testable core of runHandoffResume: it finalizes
// commit->stable transitions for the claims this worker (wid) owns, pacing every
// physical PutIfEpoch by the claim-write limiter. It returns the number
// finalized and whether the pass ran to completion — completed is false when the
// store cannot be enumerated or a paced Wait is cancelled, signalling the caller
// to leave pendingHandoffResume set for a later retry.
func (m *Manager) finalizeResumeClaims(ctx context.Context, store handoff.ClaimStore, wid string) (int, bool) {
	keys, err := store.ListKeys(ctx)
	if err != nil || len(keys) == 0 {
		return 0, false
	}

	now := time.Now().UTC()
	finalized := 0
	for _, pid := range keys {
		cur, rev, err := store.Get(ctx, pid)
		if err != nil || rev == 0 {
			continue
		}
		if cur.Owner != wid {
			continue
		}
		if cur.State != handoff.ClaimStateCommit {
			continue
		}
		// Pace the physical write if a claim-write limiter is configured. ctx
		// cancellation (shutdown) stops the resume pass.
		if err := ratelimit.Wait(ctx, m.claimWriteLimiter); err != nil {
			return finalized, false
		}

		// Finalize commit -> stable (epoch++ handled by NextStable)
		next := cur.NextStable(now)
		if _, err := store.PutIfEpoch(ctx, pid, cur.Epoch, next); err == nil {
			finalized++
		}
	}

	return finalized, true
}

// InspectHandoffClaims returns all current handoff claims from the configured
// handoff KV bucket for this Manager instance.
//
// The method is best-effort and intended for integration tests and operational
// diagnostics. It requires two-phase handoff to be enabled and the handoff
// bucket to exist.
//
// Parameters:
//   - ctx: Context for cancellation
//
// Returns:
//   - []HandoffClaim: Decoded claim entries under the claims/ prefix
//   - error: Any failure opening the bucket or listing/decoding entries
func (m *Manager) InspectHandoffClaims(ctx context.Context) ([]HandoffClaim, error) {
	if !m.cfg.EnableTwoPhaseHandoff {
		return nil, types.ErrTwoPhaseHandoffDisabled
	}
	bucket := m.cfg.KVBuckets.HandoffBucket
	if strings.TrimSpace(bucket) == "" {
		return nil, errors.New("handoff bucket name is empty")
	}

	return InspectHandoffClaims(ctx, m.js, bucket)
}

// InspectHandoffClaims opens the provided JetStream KV bucket and returns all
// current handoff claims stored under the "claims/" prefix.
//
// This helper is public to allow tests to inspect claims without a Manager
// instance, given a JetStream context and bucket name.
//
// Parameters:
//   - ctx: Context for cancellation
//   - js: JetStream context used to open the bucket
//   - bucket: KV bucket name where handoff claims are stored
//
// Returns:
//   - []HandoffClaim: Decoded claims
//   - error: Error opening the bucket or reading/decoding entries
func InspectHandoffClaims(ctx context.Context, js jetstream.JetStream, bucket string) ([]HandoffClaim, error) {
	if js == nil {
		return nil, errors.New("nil JetStream context")
	}
	kv, err := js.KeyValue(ctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("open KV bucket %q: %w", bucket, err)
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		// Treat "no keys found" as empty result for convenience.
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return []HandoffClaim{}, nil
		}
		return nil, fmt.Errorf("list keys: %w", err)
	}
	out := make([]HandoffClaim, 0, len(keys))
	for _, k := range keys {
		if !strings.HasPrefix(k, "claims/") {
			continue
		}
		entry, err := kv.Get(ctx, k)
		if err != nil {
			// Best-effort: skip keys that vanish or error mid-scan
			continue
		}
		var c HandoffClaim
		if err := json.Unmarshal(entry.Value(), &c); err != nil {
			// Skip malformed entries to avoid failing callers that want best-effort views.
			continue
		}
		out = append(out, c)
	}

	return out, nil
}
