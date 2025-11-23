package parti

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/arloliu/parti/internal/assignment/handoff"
	"github.com/arloliu/parti/types"
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

// emitInitialAssignmentEvents records metrics and invokes the OnAssignmentChanged hook
// to emit an initial change event from empty -> current.
func (m *Manager) emitInitialAssignmentEvents() {
	initial := m.CurrentAssignment()
	if len(initial.Partitions) == 0 {
		return
	}

	m.metrics.RecordAssignmentChange(len(initial.Partitions), 0, initial.Version)
	if m.hooks.OnAssignmentChanged != nil {
		m.wg.Go(func() {
			old := []types.Partition{}
			if err := m.hooks.OnAssignmentChanged(m.ctx, old, initial.Partitions); err != nil {
				m.logError("initial assignment hook error", "error", err)
			}
		})
	}
}

// applyInitialHandoffAsync applies the initial assignment via the handoff coordinator
// and optionally runs a resume pass.
func (m *Manager) applyInitialHandoffAsync() {
	initial := m.CurrentAssignment()
	if len(initial.Partitions) == 0 {
		return
	}
	wid := m.WorkerID()
	m.wg.Go(func() {
		if err := m.handoffCoordinator.Apply(m.ctx, wid, Assignment{}, initial); err != nil {
			m.logError("initial consumer update error", "error", err)
		}
		if m.pendingHandoffResume.Load() && m.cfg.EnableTwoPhaseHandoff {
			m.runHandoffResume(m.ctx)
		}
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

	keys, err := store.ListKeys(ctx)
	if err != nil || len(keys) == 0 {
		return
	}

	wid := m.WorkerID()
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
		// Finalize commit -> stable (epoch++ handled by NextStable)
		next := cur.NextStable(now)
		if _, err := store.PutIfEpoch(ctx, pid, cur.Epoch, next); err == nil {
			finalized++
		}
	}
	if finalized > 0 {
		m.logger.Info("handoff_resume_finalize", "worker_id", wid, "finalized", finalized)
	}

	// Clear flag to avoid repeated runs
	m.pendingHandoffResume.Store(false)
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
		return nil, errors.New("two-phase handoff is disabled")
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
