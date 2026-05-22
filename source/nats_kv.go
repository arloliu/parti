package source

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/arloliu/parti/v2/internal/partcodec"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// defaultReconcileInterval is the default reconcile ticker cadence.
	defaultReconcileInterval = 30 * time.Second

	// leaderReconcileInterval is the reconcile cadence when a leadership probe
	// reports that this instance is the current leader.
	leaderReconcileInterval = 30 * time.Second

	// followerReconcileInterval is the reconcile cadence when a leadership probe
	// reports that this instance is a follower.
	followerReconcileInterval = 5 * time.Minute

	// defaultUpdateRetries is the default maximum CAS attempts for Update/Modify.
	defaultUpdateRetries = 5

	// watcherBaseBackoff is the initial backoff for watcher restart attempts.
	watcherBaseBackoff = 2 * time.Second

	// watcherMaxBackoff is the maximum backoff for watcher restart attempts.
	watcherMaxBackoff = 30 * time.Second

	// watcherJitter is the ±fraction applied to the backoff delay.
	watcherJitter = 0.3
)

// ErrUpdateRetryExhausted is returned by Update and Modify when all CAS retry
// attempts are exhausted due to persistent concurrent conflicts.
var ErrUpdateRetryExhausted = errors.New("update retry budget exhausted")

// NatsKVOption is a functional option for configuring a NatsKV source.
type NatsKVOption func(*NatsKV)

// WithReconcileInterval sets the periodic-reconcile ticker cadence. A value of
// 0 disables polling entirely. The default is 30s. When WithLeadershipProbe is
// also set the fixed interval is ignored in favour of the leadership-driven
// cadence (leader=30s / follower=5min).
//
// Parameters:
//   - d: Reconcile interval (0 disables; default 30s)
//
// Returns:
//   - NatsKVOption: Option function
func WithReconcileInterval(d time.Duration) NatsKVOption {
	return func(s *NatsKV) {
		s.reconcileInterval = d
	}
}

// WithLeadershipProbe sets a callback that the reconcile loop calls on each
// tick to choose between leader cadence (30s) and follower cadence (5min).
// When set, the fixed reconcile interval from WithReconcileInterval is ignored.
//
// The provided function must be safe to call from any goroutine.
//
// Parameters:
//   - fn: Returns true if this instance is currently the leader
//
// Returns:
//   - NatsKVOption: Option function
func WithLeadershipProbe(fn func() bool) NatsKVOption {
	return func(s *NatsKV) {
		s.leadershipProbe = fn
	}
}

// WithUpdateRetries sets the maximum number of CAS attempts for Update and
// Modify. A value <= 0 falls back to the default of 5. After exhausting all
// attempts, Update and Modify return ErrUpdateRetryExhausted.
//
// Parameters:
//   - n: Maximum number of CAS attempts (default 5)
//
// Returns:
//   - NatsKVOption: Option function
func WithUpdateRetries(n int) NatsKVOption {
	return func(s *NatsKV) {
		if n > 0 {
			s.updateRetries = n
		}
	}
}

// NatsKV implements a partition source backed by a NATS KeyValue bucket.
//
// It watches a specific key in the KV bucket for updates to the partition list,
// supplemented by a periodic reconcile loop that recovers from missed watcher
// events. The combination of watch + reconcile guarantees eventual convergence
// with KV state.
//
// Writes use CAS (compare-and-swap) to prevent silent lost updates from
// concurrent callers. Use Modify, AddPartitions, or RemovePartitions for
// read-modify-write operations; Update is a CAS-fenced authoritative replace.
//
// The revision and known fields enable downstream audit logic to distinguish a
// never-written source (known=false) from a written-then-deleted source (known=true,
// empty partitions), which have different implications for coverage invariants.
type NatsKV struct {
	kv     jetstream.KeyValue
	key    string
	logger types.Logger

	// options
	reconcileInterval time.Duration
	leadershipProbe   func() bool
	updateRetries     int

	// leaderInterval and followerInterval are the cadences used when
	// leadershipProbe is set. They default to the package-level constants
	// and may be overridden in tests via direct field assignment.
	leaderInterval   time.Duration
	followerInterval time.Duration

	mu         sync.RWMutex
	partitions []types.Partition
	revision   uint64 // last observed KV revision
	known      bool   // false only before any KV event; true once any event arrives (including delete/purge)
	watcher    jetstream.KeyWatcher
	ctx        context.Context    //nolint:containedctx // lifecycle context stored by design
	cancel     context.CancelFunc //nolint:containedctx // paired with ctx
	running    bool
	listeners  []*natsKVListener
	wg         sync.WaitGroup // tracks all source-owned goroutines; Stop waits on this

	// watchFn is the constructor used to start a KV watcher. Tests may replace
	// this field to inject a fake watcher. Production code uses kv.Watch.
	watchFn func(ctx context.Context) (jetstream.KeyWatcher, error)

	// onReconcileTick is called after each reconcile tick with the interval that
	// was scheduled for that tick. It is nil in production and set by tests that
	// need to observe reconcile cadence without polling test-only struct fields.
	onReconcileTick func(interval time.Duration)
}

// natsKVListener wraps a channel with a sync.Once to prevent double-close panics.
type natsKVListener struct {
	ch   chan struct{}
	once sync.Once
}

var (
	_ types.PartitionSource           = (*NatsKV)(nil)
	_ types.PartitionUpdater          = (*NatsKV)(nil)
	_ types.WatchablePartitionSource  = (*NatsKV)(nil)
	_ types.RevisionedPartitionSource = (*NatsKV)(nil)
)

// NewNatsKV creates a new NATS KV-based partition source.
//
// Existing callers that omit opts receive a 30-second reconcile interval by
// default. Pass WithReconcileInterval(0) to disable polling (useful in tests
// that want deterministic behaviour).
//
// Parameters:
//   - kv: The NATS KeyValue bucket to use
//   - key: The key where partitions are stored (JSON or gzip-compressed JSON)
//   - logger: Optional structured logger (may be nil)
//   - opts: Functional options
//
// Returns:
//   - *NatsKV: Configured source (call Start before use)
func NewNatsKV(kv jetstream.KeyValue, key string, logger types.Logger, opts ...NatsKVOption) *NatsKV {
	s := &NatsKV{
		kv:                kv,
		key:               key,
		logger:            logger,
		reconcileInterval: defaultReconcileInterval,
		updateRetries:     defaultUpdateRetries,
		leaderInterval:    leaderReconcileInterval,
		followerInterval:  followerReconcileInterval,
	}
	s.watchFn = func(ctx context.Context) (jetstream.KeyWatcher, error) {
		return s.kv.Watch(ctx, s.key)
	}
	for _, o := range opts {
		o(s)
	}

	return s
}

// Start initializes the source and starts watching for updates.
//
// It fetches the initial partition list from KV, seeds revision and known
// state, and spawns background watcher and reconcile goroutines.
//
// Returns:
//   - error: Initialization error (invalid initial data, watcher setup failure)
func (s *NatsKV) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return nil
	}

	// Seed initial state from KV.
	var initPartitions []types.Partition
	var initRevision uint64
	var initKnown bool

	entry, err := s.kv.Get(ctx, s.key)
	if err != nil {
		if !errors.Is(err, jetstream.ErrKeyNotFound) {
			return fmt.Errorf("failed to fetch initial partitions: %w", err)
		}
		// Key has never been written: empty list, unknown revision.
		initPartitions = []types.Partition{}
		initRevision = 0
		initKnown = false
	} else {
		partitions, decErr := partcodec.Decode(entry.Value())
		if decErr != nil {
			return fmt.Errorf("failed to decode partitions: %w", decErr)
		}
		slices.SortFunc(partitions, func(a, b types.Partition) int {
			return a.Compare(b)
		})
		initPartitions = partitions
		initRevision = entry.Revision()
		initKnown = true
	}

	// Apply initial state through the canonical path (we already hold the write lock).
	s.applyLocalLocked(initPartitions, initRevision, initKnown)

	// Start watcher using a separate lifecycle context.
	s.ctx, s.cancel = context.WithCancel(context.Background())
	watcher, err := s.watchFn(s.ctx)
	if err != nil {
		s.cancel()

		return fmt.Errorf("failed to start watcher: %w", err)
	}
	s.watcher = watcher
	s.running = true

	s.wg.Add(2)
	go func() { defer s.wg.Done(); s.watchLoop(s.ctx, s.watcher) }()
	go func() { defer s.wg.Done(); s.reconcileLoop(s.ctx) }()

	return nil
}

// Stop stops the watcher and reconcile loop and waits for all source-owned
// goroutines to exit.
//
// Returns:
//   - error: Cleanup error (nil on success)
func (s *NatsKV) Stop(_ context.Context) error {
	s.mu.Lock()

	if !s.running {
		s.mu.Unlock()

		return nil
	}

	if s.cancel != nil {
		s.cancel()
	}

	var err error
	if s.watcher != nil {
		err = s.watcher.Stop()
		// Ignore "consumer not found" — cancelling the context above may
		// cause the NATS library to auto-delete the consumer before Stop() runs.
		if natsutil.IsConsumerNotFound(err) {
			err = nil
		}
	}
	s.running = false

	// Close all listeners (Once-guarded to prevent double-close panics).
	for _, l := range s.listeners {
		l.once.Do(func() { close(l.ch) })
	}
	s.listeners = nil

	s.mu.Unlock()

	// Wait for all source-owned goroutines to exit after releasing the lock
	// (goroutines may need the lock to exit cleanly).
	s.wg.Wait()

	return err
}

// Watch returns a channel that emits a signal when the partition list changes.
//
// The channel is buffered (capacity 1). If the caller is not keeping up, signals
// are dropped rather than blocking the source. The channel is closed when ctx is
// cancelled.
//
// Parameters:
//   - ctx: Context whose cancellation deregisters the watcher
//
// Returns:
//   - <-chan struct{}: Signal channel
func (s *NatsKV) Watch(ctx context.Context) <-chan struct{} {
	s.mu.Lock()

	// If the source is not running, return a pre-closed channel immediately.
	// This prevents wg.Add after Stop has begun, which would violate the
	// wg.Wait guarantee in Stop.
	if !s.running {
		s.mu.Unlock()
		ch := make(chan struct{})
		close(ch)

		return ch
	}

	l := &natsKVListener{ch: make(chan struct{}, 1)}
	s.listeners = append(s.listeners, l)
	srcCtx := s.ctx // capture source lifecycle context under lock

	// wg.Add(1) must happen while we still hold s.mu so that Stop cannot
	// transition from setting s.running=false to entering wg.Wait between
	// our running check and the Add.
	s.wg.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		// Exit when either the caller's context or the source lifecycle context cancels.
		select {
		case <-ctx.Done():
		case <-srcCtx.Done():
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		for i, listener := range s.listeners {
			if listener == l {
				s.listeners = append(s.listeners[:i], s.listeners[i+1:]...)
				l.once.Do(func() { close(l.ch) })
				break
			}
		}
	}()

	return l.ch
}

// List returns the current list of partitions (deep copy).
//
// Returns:
//   - []types.Partition: Current partition list
//   - error: Always nil
func (s *NatsKV) List(_ context.Context) ([]types.Partition, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]types.Partition, len(s.partitions))
	for i, p := range s.partitions {
		cp := types.Partition{
			Weight: p.Weight,
		}
		cp.Keys = make([]string, len(p.Keys))
		copy(cp.Keys, p.Keys)
		result[i] = cp
	}

	return result, nil
}

// Snapshot returns the current partition list together with the last observed
// KV revision and the known flag. This implements types.RevisionedPartitionSource.
//
// The known flag distinguishes a never-written source (known=false, revision=0)
// from a written-then-deleted source (known=true, revision=deleteRevision,
// empty partitions).
//
// Parameters:
//   - ctx: Context for cancellation (unused; snapshot is served from memory)
//
// Returns:
//   - partitions: Deep copy of current partition list
//   - revision: Last observed KV revision
//   - known: True once any KV event has been observed
//   - error: Always nil
func (s *NatsKV) Snapshot(_ context.Context) ([]types.Partition, uint64, bool, error) { //nolint:revive // function-result-limit: interface mandates 4 return values
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]types.Partition, len(s.partitions))
	for i, p := range s.partitions {
		cp := types.Partition{
			Weight: p.Weight,
		}
		cp.Keys = make([]string, len(p.Keys))
		copy(cp.Keys, p.Keys)
		result[i] = cp
	}

	return result, s.revision, s.known, nil
}

// Update replaces the entire partition list in the KV bucket using CAS.
//
// Update is an authoritative-replace primitive: it replaces whatever is
// currently in KV with exactly the provided list. It is NOT safe for
// concurrent read-modify-write patterns — use Modify for those. Two callers
// issuing Update with different lists will serialize through CAS retry;
// last-writer-wins semantics apply.
//
// On success, the local cache is updated immediately so a subsequent List()
// returns the new value without waiting for the watcher round-trip.
//
// Returns ErrUpdateRetryExhausted if all CAS retries are exhausted.
//
// Note: NATS KV buckets have a default value size limit of 1MB (MaxMsgSize).
// Large partition lists are automatically compressed with Gzip. If the
// compressed size still exceeds the limit, the update will fail.
//
// Parameters:
//   - ctx: Context for the operation
//   - partitions: New partition list (replaces existing content)
//
// Returns:
//   - error: Update error or ErrUpdateRetryExhausted
func (s *NatsKV) Update(ctx context.Context, partitions []types.Partition) error {
	// Validate and dedupe before any CAS attempt.
	clean, err := validateAndDedupe(partitions)
	if err != nil {
		return err
	}

	data, err := partcodec.Encode(clean)
	if err != nil {
		return err
	}

	for attempt := range s.updateRetries {
		s.mu.RLock()
		rev := s.revision
		known := s.known
		s.mu.RUnlock()

		var newRev uint64
		var writeErr error
		if !known {
			newRev, writeErr = s.kv.Create(ctx, s.key, data)
		} else {
			newRev, writeErr = s.kv.Update(ctx, s.key, data, rev)
		}

		if writeErr == nil {
			// Update local cache and notify listeners immediately so callers
			// using Watch() don't have to wait for the watcher round-trip.
			s.applyLocal(clean, newRev, true)

			return nil
		}

		if !isCASConflict(writeErr) {
			if len(data) > 1024*1024 {
				return fmt.Errorf("failed to update partitions in KV (compressed size %.2f MB exceeds default 1MB limit): %w", float64(len(data))/(1024*1024), writeErr)
			}

			return fmt.Errorf("failed to update partitions in KV: %w", writeErr)
		}

		// Refresh from KV so the next iteration uses a fresh revision.
		if rerr := s.refreshFromKV(ctx); rerr != nil {
			return fmt.Errorf("refresh after CAS conflict (attempt %d): %w", attempt+1, rerr)
		}
	}

	return ErrUpdateRetryExhausted
}

// Modify atomically transforms the partition list by applying fn, retrying on
// concurrent writes until the CAS succeeds or the retry budget is exhausted.
//
// fn receives a fresh snapshot read directly from KV (not the local cache) on
// every attempt and must be deterministic and side-effect-free — it may be
// called multiple times. The function signature is:
//
//	fn(current []types.Partition) []types.Partition
//
// On ErrKeyNotFound (key never written) fn receives an empty list.
// Returns ErrUpdateRetryExhausted if all attempts fail.
//
// Example:
//
//	err := src.Modify(ctx, func(current []types.Partition) []types.Partition {
//	    return append(current, types.Partition{Keys: []string{"new"}})
//	})
//
// Parameters:
//   - ctx: Context for the operation
//   - fn: Transform function (may be called multiple times; must be side-effect-free)
//
// Returns:
//   - error: Modify error or ErrUpdateRetryExhausted
func (s *NatsKV) Modify(ctx context.Context, fn func([]types.Partition) []types.Partition) error {
	for attempt := range s.updateRetries {
		// Always read from KV directly, not local cache.
		current, rev, readErr := s.fetchFromKV(ctx)
		if readErr != nil {
			return fmt.Errorf("failed to read from KV (attempt %d): %w", attempt+1, readErr)
		}

		proposed := fn(current)

		clean, valErr := validateAndDedupe(proposed)
		if valErr != nil {
			return valErr
		}

		data, encErr := partcodec.Encode(clean)
		if encErr != nil {
			return encErr
		}

		var newRev uint64
		var writeErr error
		if rev == 0 {
			newRev, writeErr = s.kv.Create(ctx, s.key, data)
		} else {
			newRev, writeErr = s.kv.Update(ctx, s.key, data, rev)
		}

		if writeErr == nil {
			s.applyLocal(clean, newRev, true)

			return nil
		}

		if !isCASConflict(writeErr) {
			return fmt.Errorf("failed to modify partitions in KV: %w", writeErr)
		}
		// CAS conflict: retry with fresh read on next iteration.
	}

	return ErrUpdateRetryExhausted
}

// AddPartitions adds one or more partitions to the source, preserving concurrent
// additions. Duplicate partitions (by CanonicalID) are silently ignored; calling
// AddPartitions twice with the same partition is a no-op, not an error.
//
// Internally implemented via Modify, so it is safe for concurrent callers.
//
// Parameters:
//   - ctx: Context for the operation
//   - partitions: Partitions to add (validated before any KV round-trip)
//
// Returns:
//   - error: Validation error, or ErrUpdateRetryExhausted if CAS budget exhausted
func (s *NatsKV) AddPartitions(ctx context.Context, partitions ...types.Partition) error {
	// Validate inputs before any KV round-trip.
	for i, p := range partitions {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("invalid partition at index %d: %w", i, err)
		}
	}

	return s.Modify(ctx, func(current []types.Partition) []types.Partition {
		// Index existing by CanonicalID for O(n) dedupe.
		existing := make(map[string]struct{}, len(current))
		for _, p := range current {
			existing[p.CanonicalID()] = struct{}{}
		}
		result := current
		for _, p := range partitions {
			if _, dup := existing[p.CanonicalID()]; !dup {
				result = append(result, p)
				existing[p.CanonicalID()] = struct{}{}
			}
		}

		return result
	})
}

// RemovePartitions removes one or more partitions from the source, matching by
// CanonicalID. Partitions not found are silently ignored. Concurrent mutations
// are preserved; internally implemented via Modify.
//
// Parameters:
//   - ctx: Context for the operation
//   - partitions: Partitions to remove (validated before any KV round-trip)
//
// Returns:
//   - error: Validation error, or ErrUpdateRetryExhausted if CAS budget exhausted
func (s *NatsKV) RemovePartitions(ctx context.Context, partitions ...types.Partition) error {
	// Validate inputs before any KV round-trip.
	for i, p := range partitions {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("invalid partition at index %d: %w", i, err)
		}
	}

	// Build set of CanonicalIDs to remove.
	toRemove := make(map[string]struct{}, len(partitions))
	for _, p := range partitions {
		toRemove[p.CanonicalID()] = struct{}{}
	}

	return s.Modify(ctx, func(current []types.Partition) []types.Partition {
		result := current[:0:0]
		for _, p := range current {
			if _, remove := toRemove[p.CanonicalID()]; !remove {
				result = append(result, p)
			}
		}

		return result
	})
}

// applyLocal applies a new partition state to the in-memory cache under the
// write lock. It is used by watchLoop, reconcileOnce, Update, and Modify.
// All callers set known=true; the never-written path (known=false) is handled
// by applyEmptyPreservingKnown, and initial startup uses applyLocalLocked directly.
//
// It sorts the incoming partitions, diffs against the current state, updates
// s.partitions/s.revision/s.known atomically, and fans out to listeners if
// notify is true and the state actually changed.
func (s *NatsKV) applyLocal(partitions []types.Partition, revision uint64, notify bool) {
	s.mu.Lock()
	changed := s.applyLocalLocked(partitions, revision, true)
	listeners := s.listeners
	if notify && changed {
		for _, l := range listeners {
			select {
			case l.ch <- struct{}{}:
			default:
				// Skip if channel full — listener is not keeping up.
			}
		}
	}
	s.mu.Unlock()
}

// applyLocalLocked applies partition state while the caller already holds s.mu.
// It deep-copies and sorts the incoming partitions, diffs against current state,
// and updates s.partitions/s.revision/s.known. Returns true if state changed.
// Caller is responsible for acquiring and releasing s.mu.
//
// Stale events are ignored. A watcher event whose revision is older than the
// last revision we already applied represents a delayed re-delivery from the
// watcher's Updates() channel (the local apply ran first, then the watcher
// goroutine drained the corresponding event later). Applying it would (a) regress
// s.revision, and (b) spuriously fire a "change" signal because s.partitions
// has already been advanced past this revision's content via the later local
// apply or watcher event.
//
// revision == 0 is reserved for Start()'s never-written initial-seed path
// (ErrKeyNotFound returns a zero revision). It always bypasses the gate so the
// initial empty-state seed succeeds when s.revision is still its zero value.
// All other callers (Update/Modify after CAS, watcher entry.Revision(),
// refreshFromKV/reconcileOnce after kv.Get) MUST pass a positive revision —
// passing 0 with content would let stale data clobber a known state.
func (s *NatsKV) applyLocalLocked(partitions []types.Partition, revision uint64, known bool) bool {
	if revision != 0 && revision < s.revision {
		return false
	}

	sorted := deepCopyPartitions(partitions)
	slices.SortFunc(sorted, func(a, b types.Partition) int {
		return a.Compare(b)
	})

	changed := !partitionsEqual(s.partitions, sorted)
	if changed {
		s.partitions = sorted
	}
	s.revision = revision
	s.known = known

	return changed
}

// applyEmptyPreservingKnown applies an empty partition list without overwriting
// revision or known when the source is already in a known state. It is called by
// reconcileOnce and refreshFromKV when they observe ErrKeyNotFound: the watcher
// will eventually deliver the delete event with the proper revision, so reconcile
// must not overwrite the known state with a less-informed view.
//
// Behavior:
//   - If known is already true: applies empty partitions (listeners may fire if
//     changing from non-empty), but does NOT touch revision or known.
//   - If known is still false: leaves everything unchanged (initial-never-written
//     state stays initial-never-written until the watcher delivers an event).
func (s *NatsKV) applyEmptyPreservingKnown() {
	s.mu.Lock()
	if !s.known {
		// Never-written: leave revision=0, known=false, partitions empty.
		s.mu.Unlock()

		return
	}
	// Known state: apply empty partitions but preserve revision and known.
	changed := !partitionsEqual(s.partitions, nil)
	if changed {
		s.partitions = []types.Partition{}
	}
	listeners := s.listeners
	if changed {
		for _, l := range listeners {
			select {
			case l.ch <- struct{}{}:
			default:
			}
		}
	}
	s.mu.Unlock()
}

func (s *NatsKV) watchLoop(ctx context.Context, watcher jetstream.KeyWatcher) {
	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-watcher.Updates():
			if !ok {
				// Channel closed — re-establish watcher with backoff.
				// The reconcile loop continues as the correctness safety net.
				s.restartWatcher(ctx)

				return // exit; restartWatcher spawns a new watchLoop
			}

			if entry == nil {
				continue
			}

			if entry.Operation() == jetstream.KeyValueDelete || entry.Operation() == jetstream.KeyValuePurge {
				// Preserve the delete entry's revision (non-zero) and set known=true.
				// This distinguishes "key deleted" from "key never written".
				s.applyLocal(nil, entry.Revision(), true)

				continue
			}

			partitions, err := partcodec.Decode(entry.Value())
			if err != nil {
				s.logError("failed to decode partitions update", "error", err)

				continue
			}

			s.applyLocal(partitions, entry.Revision(), true)
		}
	}
}

// restartWatcher establishes a new KV watcher with exponential backoff and
// jitter, mirroring the pattern in manager_assignment.go:monitorAssignmentChanges.
// On success it spawns a new watchLoop and returns. On context cancellation
// it returns immediately. The reconcile loop is the correctness safety net
// while the watcher is being re-established.
func (s *NatsKV) restartWatcher(ctx context.Context) {
	backoff := watcherBaseBackoff
	for {
		if ctx.Err() != nil {
			return
		}

		watcher, err := s.watchFn(ctx)
		if err == nil {
			s.mu.Lock()
			s.watcher = watcher
			s.mu.Unlock()
			s.wg.Go(func() { s.watchLoop(ctx, watcher) })

			return
		}

		s.logError("failed to restart watcher, will retry", "error", err, "backoff", backoff)

		//nolint:gosec // jitter does not require crypto-secure random
		f := rand.Float64()
		low := 1 - watcherJitter
		high := 1 + watcherJitter
		delay := time.Duration(float64(backoff) * (low + f*(high-low)))

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		backoff = min(backoff*2, watcherMaxBackoff)
	}
}

// reconcileLoop runs a background goroutine that periodically fetches the
// partition list from KV and applies any changes missed by the watcher.
//
// When WithLeadershipProbe is set, the cadence switches between leader (30s)
// and follower (5min) on each tick. WithReconcileInterval(0) disables polling.
func (s *NatsKV) reconcileLoop(ctx context.Context) {
	if s.reconcileInterval <= 0 && s.leadershipProbe == nil {
		return
	}

	// Choose the initial interval.
	interval := s.nextReconcileInterval()
	if interval <= 0 {
		return
	}

	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.reconcileOnce(ctx)
			// Re-evaluate interval for the next tick (leadership may have changed).
			next := s.nextReconcileInterval()
			if next <= 0 {
				return
			}
			// Notify test hook (if any) with the computed interval. Called outside
			// s.mu to avoid deadlock when the hook acquires its own locks.
			if s.onReconcileTick != nil {
				s.onReconcileTick(next)
			}
			timer.Reset(next)
		}
	}
}

// nextReconcileInterval returns the interval for the next reconcile tick.
// Returns 0 if reconciliation is disabled.
func (s *NatsKV) nextReconcileInterval() time.Duration {
	if s.leadershipProbe != nil {
		if s.leadershipProbe() {
			return s.leaderInterval
		}

		return s.followerInterval
	}

	return s.reconcileInterval
}

// reconcileOnce performs a single reconcile pass: reads KV, decodes, and calls
// applyLocal so that missed watcher events are recovered. If the local state
// already matches KV, this is a no-op (no listener signal emitted).
func (s *NatsKV) reconcileOnce(ctx context.Context) {
	entry, err := s.kv.Get(ctx, s.key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			// Key does not exist. Preserve revision/known if already known — the
			// watcher will deliver the delete event with the correct revision.
			// Only the initial-never-written case (known=false) leaves state unchanged.
			s.applyEmptyPreservingKnown()

			return
		}
		s.logError("reconcile: failed to fetch partitions", "error", err)

		return
	}

	partitions, err := partcodec.Decode(entry.Value())
	if err != nil {
		s.logError("reconcile: failed to decode partitions", "error", err)

		return
	}

	s.applyLocal(partitions, entry.Revision(), true)
}

// refreshFromKV reads the current entry from KV and updates the local revision
// cache so the next CAS attempt uses a fresh revision. Used by Update's retry loop.
func (s *NatsKV) refreshFromKV(ctx context.Context) error {
	entry, err := s.kv.Get(ctx, s.key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			// Preserve revision/known if already known — the watcher will deliver
			// the delete event with the correct revision. Resetting known to false
			// would cause the next Update attempt to use Create instead of Update,
			// which would either create a new revision or fail with ErrKeyExists.
			s.applyEmptyPreservingKnown()

			return nil
		}

		return err
	}

	partitions, err := partcodec.Decode(entry.Value())
	if err != nil {
		return err
	}

	s.applyLocal(partitions, entry.Revision(), false)

	return nil
}

// fetchFromKV reads the current KV entry and returns the decoded partition list
// with the associated revision. Returns (nil/empty, 0, nil) on ErrKeyNotFound.
// Used by Modify's retry loop which always reads fresh from KV.
func (s *NatsKV) fetchFromKV(ctx context.Context) ([]types.Partition, uint64, error) {
	entry, err := s.kv.Get(ctx, s.key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return []types.Partition{}, 0, nil
		}

		return nil, 0, err
	}

	partitions, err := partcodec.Decode(entry.Value())
	if err != nil {
		return nil, 0, err
	}

	return partitions, entry.Revision(), nil
}

// deepCopyPartitions returns a deep copy of partitions, duplicating each Keys
// slice so callers cannot alias the stored state.
func deepCopyPartitions(partitions []types.Partition) []types.Partition {
	result := make([]types.Partition, len(partitions))
	for i, p := range partitions {
		cp := types.Partition{Weight: p.Weight}
		cp.Keys = make([]string, len(p.Keys))
		copy(cp.Keys, p.Keys)
		result[i] = cp
	}

	return result
}

// partitionsEqual checks if two partition slices are identical (assumes both
// are canonically sorted).
func partitionsEqual(a, b []types.Partition) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Weight != b[i].Weight {
			return false
		}
		if len(a[i].Keys) != len(b[i].Keys) {
			return false
		}
		for j := range a[i].Keys {
			if a[i].Keys[j] != b[i].Keys[j] {
				return false
			}
		}
	}

	return true
}

// validateAndDedupe validates each partition and dedupes by CanonicalID.
// Returns an error on the first invalid partition; duplicate partitions
// (same CanonicalID) also return an error because they indicate a bug in
// the caller.
//
// The returned slice deep-copies each partition's Keys slice so callers cannot
// alias the returned data — this protects the encode→KV-write window against
// concurrent caller mutations.
func validateAndDedupe(partitions []types.Partition) ([]types.Partition, error) {
	seen := make(map[string]struct{}, len(partitions))
	result := make([]types.Partition, 0, len(partitions))
	for i, p := range partitions {
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("invalid partition at index %d: %w", i, err)
		}
		cid := p.CanonicalID()
		if _, dup := seen[cid]; dup {
			return nil, fmt.Errorf("duplicate partition at index %d (canonical_id=%q)", i, cid)
		}
		seen[cid] = struct{}{}
		// Deep-copy Keys to protect the encode→write window against caller mutation.
		cp := types.Partition{Weight: p.Weight}
		cp.Keys = make([]string, len(p.Keys))
		copy(cp.Keys, p.Keys)
		result = append(result, cp)
	}

	return result, nil
}

// isCASConflict reports whether err is a CAS conflict that should be retried.
// Covers both kv.Create collisions (ErrKeyExists when key already exists) and
// kv.Update wrong-revision errors (also surfaced as ErrKeyExists in nats.go).
func isCASConflict(err error) bool {
	return errors.Is(err, jetstream.ErrKeyExists)
}

func (s *NatsKV) logError(msg string, args ...any) {
	if s.logger != nil {
		s.logger.Error(msg, args...)
	}
}
