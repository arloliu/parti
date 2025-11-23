package subscription

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti/internal/assignment/handoff"
	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go/jetstream"
)

// claimEntry is a compact cache entry for ownership lookups.
type claimEntry struct {
	owner    string
	state    types.HandoffState
	epoch    int64
	revision uint64
	deleted  bool
}

// pending represents a coalesced change for a partition ID during watch processing.
// op is either "delete" or "upsert"; data contains raw claim bytes for upserts.
type pending struct {
	op       string
	data     []byte
	revision uint64
}

// ClaimBasedResolver provides fast ownership lookups via a watch-backed KV cache.
// It implements types.OwnershipResolver.
type ClaimBasedResolver struct {
	kv         jetstream.KeyValue
	claimsPref string
	cache      atomic.Pointer[map[string]claimEntry]
	logger     types.Logger
	watcher    jetstream.KeyWatcher
	metrics    ResolverMetrics
	// batching controls
	batchWindow   time.Duration
	batchMaxItems int
	// mu serializes cache updates between watcher and force-refresh
	mu sync.Mutex
}

// Compile-time assertion that ClaimBasedResolver implements OwnershipResolver.
var _ types.OwnershipResolver = (*ClaimBasedResolver)(nil)

// NewClaimBasedResolver constructs a resolver on a KV bucket. prefix should match claims prefix used in ClaimStore.
// Example: prefix="claims/" will watch pattern "claims/*" and strip prefix from keys for partition IDs.
func NewClaimBasedResolver(kv jetstream.KeyValue, prefix string, logger types.Logger) *ClaimBasedResolver {
	p := prefix
	if p != "" && !strings.HasSuffix(p, "/") {
		p += "/"
	}

	r := &ClaimBasedResolver{
		kv:            kv,
		claimsPref:    p,
		logger:        logger,
		batchWindow:   5 * time.Millisecond,
		batchMaxItems: 1024,
	}

	// Initialize empty cache
	m := make(map[string]claimEntry)
	r.cache.Store(&m)

	return r
}

// SetMetrics configures optional metrics recording for the resolver.
// Safe to call before or after Start.
func (r *ClaimBasedResolver) SetMetrics(m ResolverMetrics) {
	r.metrics = m
}

// SetBatching overrides the default batching behavior. Zero or negative values are ignored.
// Safe to call before or after Start.
func (r *ClaimBasedResolver) SetBatching(window time.Duration, maxItems int) {
	if window > 0 {
		r.batchWindow = window
	}
	if maxItems > 0 {
		r.batchMaxItems = maxItems
	}
}

// Start warms the cache and begins watching for KV updates.
func (r *ClaimBasedResolver) Start(ctx context.Context) error {
	if err := r.warm(ctx); err != nil {
		return err
	}

	return r.startWatcher(ctx)
}

// Stop stops the KV watcher.
func (r *ClaimBasedResolver) Stop() {
	if r.watcher != nil {
		_ = r.watcher.Stop()
		r.watcher = nil
	}
}

// GetOwner returns ownership data for a partition from the cache.
//
// This is a lock-free O(1) operation that reads from the atomic cache pointer.
//
// Parameters:
//   - partitionID: Partition identifier to look up
//
// Returns:
//   - string: Owner worker ID
//   - HandoffState: Current handoff state (Stable, Prepare, Commit)
//   - int64: Claim epoch number
//   - bool: true if partition was found in cache, false otherwise
//
//nolint:revive // Interface requires 4 return values for complete ownership info
func (r *ClaimBasedResolver) GetOwner(partitionID string) (string, types.HandoffState, int64, bool) {
	m := r.cache.Load()
	if m == nil {
		return "", types.HandoffStateUnknown, 0, false
	}

	e, ok := (*m)[partitionID]
	if !ok || e.deleted {
		return "", types.HandoffStateUnknown, 0, false
	}

	return e.owner, e.state, e.epoch, true
}

// ForceRefreshPartition performs a best-effort on-demand refresh for a specific partition.
// It fetches the claim directly from KV and updates the local cache immediately.
// Errors are returned but safe to ignore by callers that treat this as an optimization.
func (r *ClaimBasedResolver) ForceRefreshPartition(ctx context.Context, partitionID string) error {
	key := partitionID
	if r.claimsPref != "" {
		key = r.claimsPref + partitionID
	}
	entry, err := r.kv.Get(ctx, key)
	if err != nil {
		return err
	}
	cl, err := handoff.UnmarshalClaim(entry.Value())
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	cur := r.cache.Load()
	// Check for stale refresh
	if cur != nil {
		if existing, ok := (*cur)[partitionID]; ok {
			if existing.revision >= entry.Revision() {
				return nil
			}
		}
	}

	next := make(map[string]claimEntry, len(*cur)+1)
	maps.Copy(next, *cur)
	next[partitionID] = claimEntry{
		owner:    cl.Owner,
		state:    toState(cl.State),
		epoch:    cl.Epoch,
		revision: entry.Revision(),
	}
	r.cache.Store(&next)
	if r.metrics != nil {
		r.metrics.SetCacheSize(len(next))
	}

	return nil
}

// warm loads all current claims once.
func (r *ClaimBasedResolver) warm(ctx context.Context) error {
	keys, err := r.kv.Keys(ctx)
	if err != nil {
		// Treat empty bucket as no claims rather than an error (JetStream returns an error string "nats: no keys found").
		if !strings.Contains(err.Error(), "no keys found") {
			return fmt.Errorf("kv keys: %w", err)
		}
		keys = nil
	}

	next := make(map[string]claimEntry, len(keys))
	for _, k := range keys {
		if r.claimsPref != "" && !strings.HasPrefix(k, r.claimsPref) {
			continue
		}
		entry, err := r.kv.Get(ctx, k)
		if err != nil {
			// Skip missing keys; transient errors are logged once
			continue
		}

		cl, err := handoff.UnmarshalClaim(entry.Value())
		if err != nil {
			continue
		}

		pid := strings.TrimPrefix(k, r.claimsPref)
		next[pid] = claimEntry{
			owner:    cl.Owner,
			state:    toState(cl.State),
			epoch:    cl.Epoch,
			revision: entry.Revision(),
		}
	}

	r.cache.Store(&next)
	if r.metrics != nil {
		r.metrics.SetCacheSize(len(next))
	}
	if r.logger != nil {
		r.logger.Debug("claim resolver warm cache", "count", len(next))
	}

	return nil
}

func (r *ClaimBasedResolver) startWatcher(ctx context.Context) error {
	// Use WatchAll to avoid pattern wildcard semantics differences; filter by prefix manually.
	watcher, err := r.kv.WatchAll(ctx)
	if err != nil {
		return fmt.Errorf("kv watch all: %w", err)
	}
	r.watcher = watcher
	if r.logger != nil {
		r.logger.Debug("claim resolver watcher started", "mode", "all", "prefix", r.claimsPref)
	}
	go r.processWatcher(ctx, watcher)

	return nil
}

func (r *ClaimBasedResolver) processWatcher(ctx context.Context, watcher jetstream.KeyWatcher) {
	// capture batching config (immutable within goroutine scope)
	batchWindow := r.batchWindow
	if batchWindow <= 0 {
		batchWindow = 5 * time.Millisecond
	}
	batchMaxItems := r.batchMaxItems
	if batchMaxItems <= 0 {
		batchMaxItems = 1024
	}

	pendingByPID := make(map[string]pending, 256)
	timer := time.NewTimer(batchWindow)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case upd, ok := <-watcher.Updates():
			if !ok {
				// Channel closed, watcher stopped
				return
			}
			if upd == nil {
				// Keep-alive marker; ignore
				continue
			}
			r.handleWatcherUpdate(upd, pendingByPID)
			if len(pendingByPID) >= batchMaxItems {
				r.applyPendingBatch(pendingByPID, "maxitems")
				r.stopAndResetTimer(timer, batchWindow)
			}
		case <-timer.C:
			r.applyPendingBatch(pendingByPID, "timer")
			timer.Reset(batchWindow)
		}
	}
}

// handleWatcherUpdate filters and coalesces a single KV update into pendingByPID.
func (r *ClaimBasedResolver) handleWatcherUpdate(upd jetstream.KeyValueEntry, pendingByPID map[string]pending) {
	key := upd.Key()
	if r.claimsPref != "" && !strings.HasPrefix(key, r.claimsPref) {
		return
	}
	pid := strings.TrimPrefix(key, r.claimsPref)
	if upd.Operation() == jetstream.KeyValueDelete || upd.Operation() == jetstream.KeyValuePurge {
		pendingByPID[pid] = pending{op: "delete", revision: upd.Revision()}
		return
	}
	// Last-wins coalescing per PID
	pendingByPID[pid] = pending{op: "upsert", data: upd.Value(), revision: upd.Revision()}
}

// applyPendingBatch applies coalesced updates to the cache and emits metrics; clears the batch.
func (r *ClaimBasedResolver) applyPendingBatch(pendingByPID map[string]pending, reason string) {
	if len(pendingByPID) == 0 {
		return
	}
	started := time.Now()

	r.mu.Lock()
	defer r.mu.Unlock()

	cur := r.cache.Load()
	next := make(map[string]claimEntry, len(*cur)+len(pendingByPID))
	maps.Copy(next, *cur)

	for pid, p := range pendingByPID {
		// Check for stale update vs current cache
		if existing, ok := next[pid]; ok {
			if existing.revision >= p.revision {
				continue
			}
		}

		if p.op == "delete" {
			// Use tombstone instead of deleting to preserve revision history
			// and prevent stale ForceRefresh from resurrecting the entry.
			next[pid] = claimEntry{
				revision: p.revision,
				deleted:  true,
			}
			if r.metrics != nil {
				r.metrics.IncUpdate("delete")
			}

			continue
		}

		cl, err := handoff.UnmarshalClaim(p.data)
		if err != nil {
			continue
		}
		next[pid] = claimEntry{
			owner:    cl.Owner,
			state:    toState(cl.State),
			epoch:    cl.Epoch,
			revision: p.revision,
		}
		if r.metrics != nil {
			if !cl.LastUpdated.IsZero() {
				if lag := time.Since(cl.LastUpdated); lag >= 0 {
					r.metrics.ObserveVisibilityLag(lag)
				}
			}
			r.metrics.IncUpdate("upsert")
		}
	}

	r.cache.Store(&next)
	if r.metrics != nil {
		r.metrics.SetCacheSize(len(next))
		r.metrics.ObserveBatch(len(pendingByPID), time.Since(started))
		if reason != "" {
			r.metrics.IncBatchFlush(reason)
		}
	}

	// Clear the batch in-place
	clear(pendingByPID)
}

// stopAndResetTimer drains a timer if needed, then resets it to d.
func (r *ClaimBasedResolver) stopAndResetTimer(timer *time.Timer, d time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(d)
}

// toState converts internal handoff claim state to public HandoffState.
//
//nolint:exhaustive // Abort and Unknown states map to Unknown
func toState(cs handoff.ClaimState) types.HandoffState {
	switch cs {
	case handoff.ClaimStateStable:
		return types.HandoffStateStable
	case handoff.ClaimStatePrepare:
		return types.HandoffStatePrepare
	case handoff.ClaimStateCommit:
		return types.HandoffStateCommit
	default:
		return types.HandoffStateUnknown
	}
}
