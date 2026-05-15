package durable

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// Watcher restart backoff constants. Values mirror the Pillar 2 source-watcher
// canonical implementation in source/nats_kv.go (and manager_assignment.go):
// 2s base, 30s cap, ±30% jitter.
const (
	watcherBaseBackoff       = 2 * time.Second
	watcherMaxBackoff        = 30 * time.Second
	watcherJitter            = 0.3
	defaultReconcileInterval = 30 * time.Second
)

// errWatcherClosed signals the supervisor that the underlying watcher's
// Updates() channel closed and a new watcher must be established.
var errWatcherClosed = errors.New("claim resolver: watcher channel closed")

// ResolverOption configures a ClaimBasedResolver.
type ResolverOption func(*ClaimBasedResolver)

// WithReconcileInterval sets the periodic reconcile ticker cadence. A value of
// 0 disables polling entirely. The default is 30s.
//
// The reconciler is a safety net that re-walks the claims bucket and applies
// any state the watcher may have missed (e.g., during restart). It funnels
// through the same revision-aware apply path as the watcher and is a no-op
// when the cache is already in sync.
//
// Parameters:
//   - d: Reconcile interval (0 disables; default 30s)
//
// Returns:
//   - ResolverOption: Option function
func WithReconcileInterval(d time.Duration) ResolverOption {
	return func(r *ClaimBasedResolver) {
		r.reconcileInterval = d
		r.reconcileIntervalSet = true
	}
}

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
//
// The resolver runs two supervised background goroutines after Start:
//
//   - a watcher supervisor that re-establishes the KV watcher with exponential
//     backoff if the Updates() channel closes (cooperative watcher.Stop, the
//     underlying nats.Conn closing, or a server-side subscription teardown);
//   - a periodic reconciler that walks the claims bucket on a fixed cadence and
//     applies any state the watcher missed.
//
// In practice the supervisor covers explicit close events; silent failure
// modes — NATS reconnect across a server restart (the nats.go KV watcher does
// not surface restarts as Updates() close), an idle JetStream consumer that
// stops emitting without closing, or any other event-drought — are recovered
// by the reconciler. Both goroutines funnel updates through the same
// revision-aware apply path, so they converge on identical cache state given
// identical KV state.
type ClaimBasedResolver struct {
	kv         jetstream.KeyValue
	claimsPref string
	cache      atomic.Pointer[map[string]claimEntry]
	logger     types.Logger
	// watcher is the initially-started watcher reference. Set once in
	// startWatcher and never reassigned, so callers (including tests) can
	// safely read this field without coordinating with the supervisor.
	// Subsequent restart-created watchers are tracked separately in
	// currentWatcher (under watcherMu).
	watcher jetstream.KeyWatcher
	metrics ResolverMetrics
	// batching controls
	batchWindow   time.Duration
	batchMaxItems int
	// mu serializes cache updates between watcher, reconciler, and force-refresh.
	mu sync.Mutex
	// lastRefresh tracks the last time a partition was force-refreshed to prevent storms.
	lastRefresh map[string]time.Time
	// refreshCooldown is the minimum time between force refreshes for a partition.
	refreshCooldown time.Duration

	// Reconcile configuration.
	reconcileInterval    time.Duration
	reconcileIntervalSet bool

	// Lifecycle. stopCh is closed by Stop; doneCh is closed once both
	// supervisor and reconciler goroutines have exited (or inline by Start
	// on early-exit error paths so Stop never hangs).
	//
	// Lifecycle invariants (P1 fix):
	//   - stopCh and doneCh are allocated in NewClaimBasedResolver, so Stop
	//     is safe to call before Start.
	//   - started records whether Start successfully spawned goroutines;
	//     Stop only waits on doneCh when started is true.
	//   - stopOnce makes Stop idempotent regardless of order vs Start.
	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
	started  atomic.Bool

	// watcherMu protects currentWatcher updates during supervised restarts.
	watcherMu      sync.Mutex
	currentWatcher jetstream.KeyWatcher
}

// Compile-time assertion that ClaimBasedResolver implements OwnershipResolver.
var _ types.OwnershipResolver = (*ClaimBasedResolver)(nil)

// NewClaimBasedResolver constructs a resolver on a KV bucket. prefix should
// match the claims prefix used in ClaimStore (e.g., "claims/").
//
// Options:
//
//   - WithReconcileInterval(d): override the default 30s reconcile cadence;
//     pass 0 to disable polling (used in tests that drive convergence purely
//     through the watcher).
//
// Existing 3-arg call sites continue to compile (the options pack is variadic).
//
// Parameters:
//   - kv:     JetStream KeyValue bucket containing claims.
//   - prefix: Key prefix for claim entries (e.g., "claims/"). Trailing "/"
//     is enforced.
//   - logger: Optional logger (may be nil).
//   - opts:   Functional options.
//
// Returns:
//   - *ClaimBasedResolver: The constructed resolver. Start must be called
//     before lookups will be served.
func NewClaimBasedResolver(
	kv jetstream.KeyValue,
	prefix string,
	logger types.Logger,
	opts ...ResolverOption,
) *ClaimBasedResolver {
	p := prefix
	if p != "" && !strings.HasSuffix(p, "/") {
		p += "/"
	}

	r := &ClaimBasedResolver{
		kv:                kv,
		claimsPref:        p,
		logger:            logger,
		batchWindow:       5 * time.Millisecond,
		batchMaxItems:     1024,
		lastRefresh:       make(map[string]time.Time),
		refreshCooldown:   1 * time.Second,
		reconcileInterval: defaultReconcileInterval,
		// Allocate lifecycle channels eagerly so Stop is safe before Start.
		// See P1 fix in Stop/Start.
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}

	for _, opt := range opts {
		opt(r)
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
//
// Start launches two supervised goroutines: the watcher supervisor and the
// periodic reconciler. Both exit when Stop is called or ctx is cancelled.
// Stop blocks until both goroutines have exited.
//
// Start is idempotent: calling Start more than once returns nil without
// re-launching goroutines. If Stop has already been called before Start,
// Start refuses to spawn goroutines and returns nil immediately.
func (r *ClaimBasedResolver) Start(ctx context.Context) error {
	// P1 fix: refuse double-start and stop-before-start. Atomic CAS makes
	// the lifecycle transition observable to a concurrent Stop without a
	// dedicated mutex.
	if !r.started.CompareAndSwap(false, true) {
		return nil
	}

	// If Stop already raced ahead of Start, do nothing more. Ensure doneCh
	// is closed inline so a concurrent Stop's <-doneCh wait returns.
	select {
	case <-r.stopCh:
		close(r.doneCh)
		return nil
	default:
	}

	if err := r.warm(ctx); err != nil {
		// Failed setup: close doneCh inline so any subsequent Stop unblocks.
		close(r.doneCh)
		return err
	}

	// Establish the initial watcher synchronously so that callers (and tests)
	// can rely on r.watcher being non-nil immediately after Start returns.
	if err := r.startWatcher(ctx); err != nil {
		close(r.doneCh)
		return err
	}

	var wg sync.WaitGroup
	wg.Add(2) //nolint:revive // sync.WaitGroup does not have Go method
	go func() {
		defer wg.Done()
		r.supervise(ctx, r.watcher)
	}()
	go func() {
		defer wg.Done()
		r.reconcileLoop(ctx)
	}()
	go func() {
		wg.Wait()
		close(r.doneCh)
	}()

	return nil
}

// Stop signals the resolver to shut down and waits for both supervised
// goroutines (watcher supervisor + reconciler) to exit. Stop is idempotent
// and safe to call from any goroutine — including before Start, or
// concurrently with Start.
//
// Stop does NOT block on watcher restart backoff: the supervisor observes
// stopCh during its backoff sleep and returns promptly.
//
// P1 invariants:
//   - Stop-before-Start: closes stopCh; the subsequent Start observes stopCh
//     closed, declines to spawn goroutines, and closes doneCh inline.
//   - Concurrent Start/Stop: started CAS in Start ensures at most one Start
//     wins; Stop always closes stopCh and waits on doneCh only if a Start
//     winner exists (tracked by r.started).
func (r *ClaimBasedResolver) Stop() {
	r.stopOnce.Do(func() {
		close(r.stopCh)
		// Stop the currently-active watcher to release NATS resources and
		// unblock any goroutine sitting in <-Updates(). The supervisor's
		// select on stopCh would unblock too, but explicitly closing the
		// watcher is required to release the underlying JetStream
		// subscription promptly.
		r.watcherMu.Lock()
		w := r.currentWatcher
		r.watcherMu.Unlock()
		if w != nil {
			_ = w.Stop()
		}
	})
	// Only wait on doneCh if a Start winner exists (or has begun and will
	// close doneCh inline on its error path). When neither Start has been
	// called yet, doneCh is allocated but never closed — skip the wait.
	if r.started.Load() {
		<-r.doneCh
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
	// Rate limit checks to prevent storms on unknown partitions
	r.mu.Lock()
	if !r.lastRefresh[partitionID].IsZero() && time.Since(r.lastRefresh[partitionID]) < r.refreshCooldown {
		r.mu.Unlock()
		return nil
	}
	// Optimistically update timestamp to block other concurrent refreshes
	r.lastRefresh[partitionID] = time.Now()
	r.mu.Unlock()

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
	r.watcherMu.Lock()
	r.currentWatcher = watcher
	r.watcherMu.Unlock()
	if r.logger != nil {
		r.logger.Debug("claim resolver watcher started", "mode", "all", "prefix", r.claimsPref)
	}

	return nil
}

// supervise drives the watcher lifecycle. It runs the current watcher's
// processing loop and, on channel close or context cancellation, decides
// whether to restart (with exponential-backoff + jitter) or exit.
//
// The initial watcher is passed in (already established by Start). On restart,
// supervise calls runWatcher which establishes a new watcher.
func (r *ClaimBasedResolver) supervise(ctx context.Context, initial jetstream.KeyWatcher) {
	watcher := initial
	backoff := watcherBaseBackoff

	for {
		err := r.processWatcher(ctx, watcher)

		// Always release the watcher we just ran with — either Stop() was
		// already called (no-op) or the channel closed (still safe to Stop).
		_ = watcher.Stop()

		// Clean shutdown: context cancelled or stopCh closed.
		if err == nil || ctx.Err() != nil {
			return
		}
		select {
		case <-r.stopCh:
			return
		default:
		}

		// Watcher closed — log + back off + reestablish.
		if r.logger != nil {
			r.logger.Warn("claim resolver watcher closed, restarting",
				"error", err,
				"backoff", backoff,
			)
		}

		if !r.sleepWithStop(ctx, jittered(backoff)) {
			return
		}

		nextWatcher, restartErr := r.runWatcher(ctx)
		if restartErr != nil {
			if r.metrics != nil {
				r.metrics.IncWatcherRestart("establish_failed")
			}
			if r.logger != nil {
				r.logger.Warn("claim resolver watcher restart failed, retrying",
					"error", restartErr,
				)
			}
			backoff = nextBackoff(backoff)

			continue
		}

		if r.metrics != nil {
			r.metrics.IncWatcherRestart("channel_closed")
		}
		watcher = nextWatcher
		backoff = watcherBaseBackoff
	}
}

// runWatcher establishes a fresh KV watcher and updates currentWatcher.
// Returns the new watcher on success.
func (r *ClaimBasedResolver) runWatcher(ctx context.Context) (jetstream.KeyWatcher, error) {
	watcher, err := r.kv.WatchAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("kv watch all: %w", err)
	}
	r.watcherMu.Lock()
	r.currentWatcher = watcher
	r.watcherMu.Unlock()
	if r.logger != nil {
		r.logger.Debug("claim resolver watcher re-established", "mode", "all", "prefix", r.claimsPref)
	}

	return watcher, nil
}

// processWatcher runs the batched update loop for a single watcher instance.
// It returns nil on clean shutdown (context cancellation or stopCh) and
// errWatcherClosed when the Updates() channel closes.
func (r *ClaimBasedResolver) processWatcher(ctx context.Context, watcher jetstream.KeyWatcher) error {
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
			return nil
		case <-r.stopCh:
			return nil
		case upd, ok := <-watcher.Updates():
			if !ok {
				// Channel closed (cooperative watcher.Stop, nats.Conn close,
				// server-side subscription teardown). Silent stalls — e.g.,
				// the nats.go KV watcher does NOT surface NATS server restarts
				// here — are recovered by the periodic reconciler, not this
				// branch. Flush whatever we have queued and ask the supervisor
				// to restart the watcher.
				r.applyPendingBatch(pendingByPID, "watcher_close")
				return errWatcherClosed
			}
			if upd == nil {
				// Keep-alive marker; ignore.
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

// reconcileLoop runs a background goroutine that periodically re-walks the
// claims bucket and applies any state the watcher missed. Disabled when the
// reconcile interval is <= 0.
func (r *ClaimBasedResolver) reconcileLoop(ctx context.Context) {
	if r.reconcileInterval <= 0 {
		return
	}
	t := time.NewTicker(r.reconcileInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		case <-t.C:
			r.reconcileOnce(ctx)
		}
	}
}

// reconcileOnce performs a single reconcile pass: re-list the bucket, build a
// pending-batch of upserts and tombstones for entries that disagree with the
// cache, and apply through the shared revision-aware apply path. A no-op when
// the cache is already in sync with KV.
//
// Tombstone semantics: when a key is absent from KV but present (non-deleted)
// in the cache, we stamp the tombstone with the cache's current revision + 1.
// The revision-aware short-circuit at applyPendingBatch (existing.revision >=
// p.revision) ensures that any later watcher event carrying the *real* delete
// revision will still apply (it will be strictly greater than the cache entry
// the reconciler just wrote). Conversely, if the watcher already saw the
// delete at the authoritative revision, the cache entry has that higher
// revision and the reconciler's synthetic revision is short-circuited away.
//
// This is the same revision-aware tombstone shape used by the watcher
// at applyPendingBatch.
func (r *ClaimBasedResolver) reconcileOnce(ctx context.Context) {
	// P0 fix: snapshot the cache BEFORE walking KV Keys(). Any cache entry
	// added by the watcher concurrently with Keys()/Get() will not appear in
	// `snap` and therefore cannot be synthesized into a tombstone by the
	// loop below. The shared apply path's revision short-circuit
	// (existing.revision >= p.revision) still defends against the reverse
	// race (watcher upsert at revision R landing after snap but before the
	// tombstone is staged): the synthetic tombstone uses
	// snap[pid].revision + 1, which is strictly less than any watcher
	// upsert revision that beat reconcile to the cache, so the tombstone is
	// short-circuited.
	//
	// We pick this "snapshot first" form over a CAS-style atomic check
	// inside applyPendingBatch because it is the minimum change to close
	// the race and keeps the tombstone-or-skip decision local to
	// reconcileOnce.
	cur := r.cache.Load()
	snap := map[string]claimEntry{}
	if cur != nil {
		snap = *cur
	}

	keys, err := r.kv.Keys(ctx)
	if err != nil {
		if !strings.Contains(err.Error(), "no keys found") {
			if r.logger != nil {
				r.logger.Debug("claim resolver reconcile: list keys failed", "error", err)
			}
			return
		}
		keys = nil
	}

	pendingByPID := make(map[string]pending)
	seen := make(map[string]struct{}, len(keys))

	for _, k := range keys {
		if r.claimsPref != "" && !strings.HasPrefix(k, r.claimsPref) {
			continue
		}
		entry, err := r.kv.Get(ctx, k)
		if err != nil {
			// Skip missing keys; the next reconcile pass will retry.
			continue
		}
		// Defensive: skip delete-operation entries; we tombstone separately
		// from the keys list below.
		if entry.Operation() == jetstream.KeyValueDelete || entry.Operation() == jetstream.KeyValuePurge {
			continue
		}
		pid := strings.TrimPrefix(k, r.claimsPref)
		seen[pid] = struct{}{}
		// Pre-filter: only stage this upsert if the snapshot is missing the
		// entry or holds a strictly older revision. This avoids reseating
		// the cache pointer in steady state.
		if existing, ok := snap[pid]; ok {
			if existing.revision >= entry.Revision() {
				continue
			}
		}
		pendingByPID[pid] = pending{op: "upsert", data: entry.Value(), revision: entry.Revision()}
	}

	// P0 fix: iterate over the pre-Keys snapshot, NOT the live cache. Any
	// entry the watcher added after `snap` was taken is invisible here, so
	// the tombstone pass cannot synthesize a delete for it.
	for pid, e := range snap {
		if e.deleted {
			continue
		}
		if _, ok := seen[pid]; ok {
			continue
		}
		// Synthetic tombstone revision: existing + 1. The shared apply
		// path's revision check (existing.revision >= p.revision) ensures
		// that if a concurrent watcher upsert landed in the cache between
		// our snapshot and applyPendingBatch with a strictly greater
		// revision than snap[pid].revision, this tombstone is short-
		// circuited. A later watcher event carrying the authoritative
		// delete revision (also strictly greater) will still apply.
		pendingByPID[pid] = pending{op: "delete", revision: e.revision + 1}
	}

	if len(pendingByPID) == 0 {
		return
	}
	r.applyPendingBatch(pendingByPID, "reconcile")
}

// sleepWithStop blocks for d or until stopCh/ctx cancellation. Returns false
// if the resolver was stopped (caller should exit).
func (r *ClaimBasedResolver) sleepWithStop(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	case <-r.stopCh:
		return false
	}
}

// jittered returns d with a uniform ±watcherJitter perturbation applied.
func jittered(d time.Duration) time.Duration {
	//nolint:gosec // jitter does not require crypto-secure random
	f := rand.Float64()
	low := 1 - watcherJitter
	high := 1 + watcherJitter
	return time.Duration(float64(d) * (low + f*(high-low)))
}

// nextBackoff doubles backoff, capped at watcherMaxBackoff.
func nextBackoff(b time.Duration) time.Duration {
	n := b * 2
	if n > watcherMaxBackoff {
		return watcherMaxBackoff
	}
	return n
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
// ClaimStateAbort and ClaimStateUnknown both map to HandoffStateUnknown.
func toState(cs handoff.ClaimState) types.HandoffState {
	switch cs {
	case handoff.ClaimStateStable:
		return types.HandoffStateStable
	case handoff.ClaimStatePrepare:
		return types.HandoffStatePrepare
	case handoff.ClaimStateCommit:
		return types.HandoffStateCommit
	case handoff.ClaimStateAbort, handoff.ClaimStateUnknown:
		return types.HandoffStateUnknown
	}

	return types.HandoffStateUnknown
}
