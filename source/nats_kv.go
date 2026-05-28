package source

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/arloliu/parti/v2/internal/partcodec"
	"github.com/arloliu/parti/v2/internal/retry"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
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

	// watcherJitter is the ±fraction applied to the backoff delay.
	watcherJitter = 0.3
)

// Watcher retry-envelope parameters. Declared as vars (not consts) so
// integration tests in this package can drive the bounded-retry envelope
// at a sub-second cadence without waiting the production 60-second
// cycle. Production code must NEVER mutate these.
var (
	// watcherBaseBackoff is the initial backoff for watcher restart attempts.
	watcherBaseBackoff = 2 * time.Second

	// watcherMaxBackoff is the maximum backoff for watcher restart attempts.
	watcherMaxBackoff = 30 * time.Second

	// watcherMaxAttempts caps the bounded-retry envelope on the watcher-restart
	// loop. With watcherBaseBackoff=2s and watcherMaxBackoff=30s, six
	// attempts span roughly one reconcile cycle (≤ defaultReconcileInterval).
	// Past this point we rely on the periodic reconciler for recovery and
	// emit the onWatcherRetryExhausted callback.
	watcherMaxAttempts = 6
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
// Disabling the reconciler (d <= 0 with no leadership probe wired)
// disables the load-bearing recovery path for the source watcher: the
// nats.go KV watcher's Updates() channel does NOT close on a NATS
// server restart, and the periodic reconcile is the only mechanism
// that re-syncs source state after a silently stalled watcher. Disable
// polling only with full awareness — Start emits a one-shot WARN log
// line when this state is detected so the operational consequence is
// visible at first deploy.
//
// Disabling the reconciler also disables bucket delete+recreate recovery:
// see reconcileOnce for the recovery boundary.
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

// SourceUnavailableHook fires when the partition-source bucket is observed to
// be missing on the live connection. The reconciler and watcher-restart paths
// each see a distinct error from the nats.go surface — [jetstream.ErrBucketNotFound]
// from a fresh [jetstream.JetStream.KeyValue] lookup, [jetstream.ErrStreamNotFound]
// from a cached watcher rebind, or [nats.ErrNoResponders] from a cached
// [jetstream.KeyValue.Get] — and all three classify to bucket-loss internally.
// The err argument hands the original sentinel to the hook so implementations
// can log or surface the raw cause, but **do not gate readiness on a single
// sentinel type**: when the hook fires, the bucket is missing.
//
// The library does not recreate the bucket — it is caller-owned, and rebuilding
// it requires the operator's provisioning flow. Wire this hook into your
// readiness logic (e.g. flip a readiness flag and let the orchestrator rotate
// the pod) so the loss becomes an actionable signal rather than a silent stall.
//
// Concurrency contract: the hook is invoked synchronously from the source's
// reconcile / restart goroutine. Implementations MUST be non-blocking and
// MUST NOT call back into the source (recursive Get / Update / Stop will
// deadlock against the watcher's restart path).
//
// Rate-limiting: repeated observations within a cooldown window (default
// 30s, matching the default reconcile cadence) suppress the hook so the
// callback is not flooded. The companion [SourceMetrics] gauge stays set
// for the full duration of the outage and clears once a subsequent
// operation succeeds.
type SourceUnavailableHook func(err error)

// WithUnavailableHook registers a [SourceUnavailableHook] that fires when
// the partition-source bucket is observed to be missing. See
// [SourceUnavailableHook] for the deadlock contract and rate-limiting
// behavior. Without a hook, the loss is still logged and the metric is
// set (when [WithMetrics] is configured), but no escalation runs — the
// library cannot recreate a user-owned bucket on its own.
//
// Parameters:
//   - h: Callback invoked on bucket-loss detection
//
// Returns:
//   - NatsKVOption: Option function
func WithUnavailableHook(h SourceUnavailableHook) NatsKVOption {
	return func(s *NatsKV) {
		s.unavailableHook = h
	}
}

// WithMetrics wires a [types.SourceMetrics] collector. The source uses the
// collector to expose its `parti_source_bucket_missing` gauge (set to 1 on
// the first bucket-loss observation — see [SourceUnavailableHook] for the
// empirical error surface — cleared on the next successful operation). The
// default is [types.NopSourceMetrics].
//
// Parameters:
//   - m: SourceMetrics collector
//
// Returns:
//   - NatsKVOption: Option function
func WithMetrics(m types.SourceMetrics) NatsKVOption {
	return func(s *NatsKV) {
		if m != nil {
			s.metrics = m
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
	// applyGen increments under s.mu every time applyLocalLocked accepts an entry
	// (passes the stale gate), regardless of whether content changed. It lets the
	// reconcile reset path detect that local state advanced between its
	// confirmatory read and the reset lock — even when the new value lands at a
	// revision numerically equal to the old baseline (the bucket-recreate ABA),
	// which a revision-number comparison alone cannot distinguish.
	applyGen  uint64
	known     bool // false only before any KV event; true once any event arrives (including delete/purge)
	watcher   jetstream.KeyWatcher
	ctx       context.Context    //nolint:containedctx // lifecycle context stored by design
	cancel    context.CancelFunc //nolint:containedctx // paired with ctx
	running   bool
	listeners []*natsKVListener
	wg        sync.WaitGroup // tracks all source-owned goroutines; Stop waits on this

	// watchFn is the constructor used to start a KV watcher. Tests may replace
	// this field to inject a fake watcher. Production code uses kv.Watch.
	watchFn func(ctx context.Context) (jetstream.KeyWatcher, error)

	// decodeFn decodes a KV value into a partition list. Indirected through a
	// field (like watchFn) so tests can observe decode invocations. Production
	// uses partcodec.Decode.
	decodeFn func(data []byte) ([]types.Partition, error)

	// Reconcile skip-guard cache. Accessed ONLY by reconcileOnce, which runs on
	// the single reconcileLoop goroutine, so these need no mutex. They identify
	// the last entry this loop applied so an unchanged tick can skip the decode.
	// The pair (rev, created) — not rev alone — is required: a recreated bucket
	// reuses revision numbers from 1, but the entry creation timestamp does not
	// collide across a real recreate.
	reconcileRev     uint64
	reconcileCreated time.Time
	reconcileKnown   bool

	// onReconcileTick is called after each reconcile tick with the interval that
	// was scheduled for that tick. It is nil in production and set by tests that
	// need to observe reconcile cadence without polling test-only struct fields.
	onReconcileTick func(interval time.Duration)

	// Bucket-unavailability escalation:
	//
	// unavailableHook fires (rate-limited) when the watcher-restart loop or
	// the reconciler observes any error in the bucket-loss surface — see
	// isBucketUnavailableErr for the empirical set (currently
	// jetstream.ErrBucketNotFound, jetstream.ErrStreamNotFound, and
	// nats.ErrNoResponders).
	// metrics receives a SetSourceBucketMissing(true/false) call on the
	// degradation/recovery edges.
	// unavailableMu serialises lastUnavailableAt and bucketMissing — the two
	// fields that govern rate-limiting and the gauge state.
	// unavailableCooldown is the minimum interval between successive hook
	// invocations under sustained bucket-loss; the default 30s matches the
	// reconcile cadence.
	unavailableHook     SourceUnavailableHook
	metrics             types.SourceMetrics
	unavailableMu       sync.Mutex
	lastUnavailableAt   time.Time
	unavailableCooldown time.Duration
	bucketMissing       bool
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
		kv:                  kv,
		key:                 key,
		logger:              logger,
		reconcileInterval:   defaultReconcileInterval,
		updateRetries:       defaultUpdateRetries,
		leaderInterval:      leaderReconcileInterval,
		followerInterval:    followerReconcileInterval,
		metrics:             types.NopSourceMetrics{},
		unavailableCooldown: defaultReconcileInterval, // 30s
	}
	s.watchFn = func(ctx context.Context) (jetstream.KeyWatcher, error) {
		return s.kv.Watch(ctx, s.key)
	}
	s.decodeFn = partcodec.Decode
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

	// Warn when the reconciler is disabled with no leadership probe wired.
	// reconcileLoop (below) exits immediately on this condition; without a
	// running reconciler, a silent NATS server restart leaves the source
	// watcher stalled with no recovery path. See the
	// project_nats_watcher_empirical_finding memory pin.
	if s.reconcileInterval <= 0 && s.leadershipProbe == nil {
		s.logWarn("source reconciler disabled; server-restart recovery will not work — call source.WithReconcileInterval with a positive duration (or wire a leadership probe via source.WithLeadershipProbe)")
	}

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
		// Ignore benign teardown signals — cancelling the context above can
		// trigger nats.go's internal ctx-cancel goroutine to unsubscribe the
		// watcher first (race -> ErrBadSubscription) or the server may have
		// already deleted the ephemeral consumer (ErrConsumerNotFound).
		if natsutil.IsBenignWatcherStopErr(err) {
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
//
// It returns whether the entry was accepted (passed the stale gate and became
// the current revision). The reconcile path uses this to update its skip cache
// only with entries that were actually applied — recording a gate-dropped entry
// would let the skip-guard wrongly skip a later re-read of that same identity.
// Other callers ignore the result.
func (s *NatsKV) applyLocal(partitions []types.Partition, revision uint64, notify bool) bool {
	s.mu.Lock()
	accepted := revision == 0 || revision >= s.revision
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

	return accepted
}

// markReconcileApplied records the entry identity the reconcile loop last
// accepted, for the next tick's skip-guard. Called ONLY after an apply that was
// actually accepted (never after a dropped or unconfirmed entry). Reconcile-loop
// goroutine only; no lock.
func (s *NatsKV) markReconcileApplied(revision uint64, created time.Time) {
	s.reconcileRev = revision
	s.reconcileCreated = created
	s.reconcileKnown = true
}

// applyReconcileReset accepts an entry that a confirmatory reconcile read has
// proven belongs to a new bucket incarnation (its revision is below the baseline
// we observed before the confirmatory read, yet it is the current latest). It
// clears the stale-gate baseline under the lock so the lower revision is
// accepted, then fans out exactly like applyLocal. The shared
// watcher/Update/Start paths are unaffected.
//
// baseline/gen are s.revision and s.applyGen as observed before the confirmatory
// read. The baseline clear is applied ONLY if s.applyGen is still gen — i.e. no
// apply has been accepted in between. A revision check alone is insufficient: a
// recreated bucket can advance back to a revision numerically equal to the old
// baseline (the ABA case), so s.revision == baseline can hold even though local
// state already moved to newer recreated content. If anything was accepted in
// the window we must NOT bypass the stale gate; we fall through to a normal gated
// apply that drops the lower revision instead of rolling state backward.
//
// It returns whether the entry was accepted (the reset cleared the baseline, or
// the fall-through gated apply still accepted it), so the caller updates its skip
// cache only on a real apply.
func (s *NatsKV) applyReconcileReset(partitions []types.Partition, revision, baseline, gen uint64) bool {
	s.mu.Lock()
	if s.revision == baseline && s.applyGen == gen {
		s.revision = 0 // confirmed incarnation reset: drop baseline so the lower revision applies
	}
	accepted := revision == 0 || revision >= s.revision
	changed := s.applyLocalLocked(partitions, revision, true)
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

	return accepted
}

// applyReconcileEntry applies a freshly fetched reconcile entry, handling bucket
// incarnation reset. A reconcile kv.Get returns the current latest revision, so
// an entry revision below our cached baseline is ambiguous: a stale read racing
// a concurrent Update (same incarnation), or a deleted+recreated bucket. We
// disambiguate with ONE confirmatory latest read before bypassing the stale
// gate; we never roll state back on a single possibly-stale read.
func (s *NatsKV) applyReconcileEntry(ctx context.Context, partitions []types.Partition, entry jetstream.KeyValueEntry) {
	s.mu.RLock()
	cachedRev := s.revision
	cachedGen := s.applyGen
	s.mu.RUnlock()

	if entry.Revision() == 0 || entry.Revision() >= cachedRev {
		// Not backwards: ordinary apply (stale gate no-ops on equal/higher).
		// Update the skip cache only if the gate accepted the entry: a concurrent
		// writer may have advanced s.revision past it, in which case it was dropped
		// and must not be recorded as applied.
		if s.applyLocal(partitions, entry.Revision(), true) {
			s.markReconcileApplied(entry.Revision(), entry.Created())
		}

		return
	}

	// Backwards revision: confirm with one authoritative latest read.
	confirm, err := s.kv.Get(ctx, s.key)
	if err != nil {
		// Cannot confirm: do not guess, do not update the skip cache. The next
		// tick re-evaluates. Mirror reconcileOnce's bucket-missing posture.
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return
		}
		if s.noteBucketUnavailable(err) {
			s.logError("reconcile: confirmatory read found bucket missing", "error", err)
		}

		return
	}
	confirmParts, decErr := s.decodeFn(confirm.Value())
	if decErr != nil {
		s.logError("reconcile: failed to decode confirmatory entry", "error", decErr)

		return
	}

	// Compare the confirmed latest against the pre-confirm baseline (cachedRev),
	// NOT the live s.revision. In a single incarnation the latest revision only
	// grows, so a confirmed latest below cachedRev can only mean the bucket was
	// recreated. Using live s.revision here would misread a concurrent
	// same-incarnation Update (which advances s.revision past the confirmed
	// revision) as a reset and roll state backward.
	var accepted bool
	if confirm.Revision() != 0 && confirm.Revision() < cachedRev {
		// Confirmed latest is below the prior baseline -> genuine incarnation reset.
		accepted = s.applyReconcileReset(confirmParts, confirm.Revision(), cachedRev, cachedGen)
	} else {
		// First read was stale (a concurrent writer advanced the revision);
		// apply the confirmed latest through the normal gate.
		accepted = s.applyLocal(confirmParts, confirm.Revision(), true)
	}
	// Record the entry in the skip cache only if it was actually applied. A
	// dropped confirm that is still the latest readable entry would otherwise
	// make the next tick's skip-guard skip it forever (recovery never happens).
	if accepted {
		s.markReconcileApplied(confirm.Revision(), confirm.Created())
	}
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
	s.applyGen++ // entry accepted: advance the apply generation (see field doc)

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
//
// The retry loop is bounded by an internal/retry envelope. After
// watcherMaxAttempts consecutive failures the envelope fires
// onWatcherRetryExhausted (logged at WARN) and the goroutine exits.
// The periodic reconciler then remains as the sole recovery path
// (load-bearing because the nats.go KV watcher's Updates() channel
// does NOT close on a NATS server restart). Without the bound, a
// vanished bucket or a sustained NATS outage produced infinite
// watcher-create API load and no escalation signal.
func (s *NatsKV) restartWatcher(ctx context.Context) {
	env := retry.New(retry.Config{
		Work:        s.tryRebindWatcher(ctx),
		OnPermanent: s.onWatcherRetryExhausted,
		BaseBackoff: watcherBaseBackoff,
		MaxBackoff:  watcherMaxBackoff,
		MaxAttempts: watcherMaxAttempts,
		Jitter:      watcherJitter,
		OnProgress: func(attempt int, err error) {
			s.logError("failed to restart watcher, will retry", "error", err, "attempt", attempt)
		},
	})
	_ = env.Run(ctx) // ctx-cancel and exhaustion both terminate the goroutine
}

// tryRebindWatcher captures the lifecycle ctx and returns the Work function
// the retry envelope drives. Each successful rebind installs the new watcher,
// spawns the watchLoop goroutine, and clears the bucket-missing gauge;
// the envelope then returns nil from Run and restartWatcher exits cleanly.
// On error the per-attempt path classifies the failure via
// noteBucketUnavailable (sets the gauge + fires the rate-limited
// SourceUnavailableHook) before returning the error to the envelope for
// the bounded-retry decision.
func (s *NatsKV) tryRebindWatcher(loopCtx context.Context) func(context.Context) error {
	return func(_ context.Context) error {
		// Watch uses the source-lifetime context (loopCtx) so the watcher
		// outlives this single retry attempt — the envelope's per-attempt
		// ctx would cancel it on success.
		watcher, err := s.watchFn(loopCtx)
		if err != nil {
			// Classify the rebind error. noteBucketUnavailable fires
			// the SourceUnavailableHook (rate-limited) + sets the
			// SourceBucketMissing gauge on a wrapped bucket-unavailable
			// error. The envelope still drives the bounded retry; this
			// just makes the loss observable to operators during the
			// retry window.
			s.noteBucketUnavailable(err)
			return err
		}
		s.mu.Lock()
		s.watcher = watcher
		s.mu.Unlock()
		s.wg.Go(func() { s.watchLoop(loopCtx, watcher) })
		// A successful rebind confirms the bucket is reachable.
		s.noteBucketAvailable()

		return nil
	}
}

// onWatcherRetryExhausted is the envelope's permanent-failure callback.
// The reconcileLoop remains running and will continue to converge state on
// each tick; operators should treat sustained watcher-restart exhaustion
// as a signal that the source bucket needs investigation (the
// SourceUnavailableHook, when wired, is the escalation path).
func (s *NatsKV) onWatcherRetryExhausted(err error) {
	s.logWarn("source watcher restart attempt budget exhausted; relying on reconciler for recovery",
		"error", err,
		"max_attempts", watcherMaxAttempts)
}

// reconcileLoop runs a background goroutine that periodically fetches the
// partition list from KV and applies any changes missed by the watcher.
//
// When WithLeadershipProbe is set, the cadence switches between leader (30s)
// and follower (5min) on each tick — the fixed WithReconcileInterval value
// is ignored, including a 0 sentinel. Polling is disabled only when
// WithReconcileInterval(0) is set AND no leadership probe is wired.
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
//
// Bucket delete+recreate recovery is reconcile-driven. After an operator
// deletes and recreates the source bucket, its revision namespace resets to 1;
// the watcher may transiently drop that recreated rev-1 value through the stale
// gate, but a later reconcile tick re-applies it via applyReconcileEntry's
// confirmatory read. With WithReconcileInterval(0) and no leadership probe the
// reconciler is disabled, so bucket-recreate recovery does not occur.
func (s *NatsKV) reconcileOnce(ctx context.Context) {
	entry, err := s.kv.Get(ctx, s.key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			// Key does not exist but the bucket does — confirm the
			// bucket-available gauge and apply the empty state.
			s.noteBucketAvailable()
			// Preserve revision/known if already known — the watcher will deliver
			// the delete event with the correct revision. Only the
			// initial-never-written case (known=false) leaves state unchanged.
			s.applyEmptyPreservingKnown()
			s.reconcileKnown = false // empty state: invalidate the skip cache

			return
		}
		// Bucket-missing escalation: see noteBucketUnavailable Godoc.
		if s.noteBucketUnavailable(err) {
			s.logError("reconcile: source bucket missing", "error", err)

			return
		}
		s.logError("reconcile: failed to fetch partitions", "error", err)

		return
	}

	// Successful kv.Get confirms the bucket is reachable.
	s.noteBucketAvailable()

	// Skip the decode when this entry is the one we last applied. Match on
	// (revision, created): revision alone is unsafe because a recreated bucket
	// reuses revision numbers, while the creation timestamp does not collide
	// across a real recreate. reconcileKnown==false (first tick) never skips.
	if s.reconcileKnown &&
		entry.Revision() == s.reconcileRev &&
		entry.Created().Equal(s.reconcileCreated) {
		return
	}

	partitions, err := s.decodeFn(entry.Value())
	if err != nil {
		s.logError("reconcile: failed to decode partitions", "error", err)

		return
	}
	s.applyReconcileEntry(ctx, partitions, entry)
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

func (s *NatsKV) logWarn(msg string, args ...any) {
	if s.logger != nil {
		s.logger.Warn(msg, args...)
	}
}

// isBucketUnavailableErr classifies an error from a cached KV / watcher
// operation as "the source bucket is unavailable from this connection's
// point of view". Empirical surface (verified against nats.go v1.50.0):
//
//   - jetstream.ErrBucketNotFound: returned by js.KeyValue(...) when the
//     bucket has been deleted (lookup path).
//   - jetstream.ErrStreamNotFound: returned by kv.Watch on a CACHED kv
//     handle after the bucket was deleted under it. The KV bucket is
//     backed by a stream named KV_<bucket>; once the stream is gone the
//     watcher's bind fails with this error.
//   - nats.ErrNoResponders: returned by kv.Get on a cached handle after
//     the bucket was deleted — the request reaches the JetStream subject
//     but no responder is listening.
//
// The latter two are what production paths actually see (the reconciler
// uses cached kv.Get; restartWatcher uses cached kv.Watch). The first is
// included for completeness and for any future code path that re-binds
// the bucket via js.KeyValue.
//
// nats.ErrNoResponders is broader than "bucket deleted" — it also fires
// during a transient NATS reconnect when no JetStream node has surfaced
// the subject yet. The cooldown + gauge-clears-on-success design tolerates
// the false-positive: the hook fires once, the gauge clears on the next
// successful op, and the operator's readiness probe sees the brief blip.
// This is preferable to missing the genuine bucket-loss case.
func isBucketUnavailableErr(err error) bool {
	return errors.Is(err, jetstream.ErrBucketNotFound) ||
		errors.Is(err, jetstream.ErrStreamNotFound) ||
		errors.Is(err, nats.ErrNoResponders)
}

// noteBucketUnavailable handles the bucket-missing escalation path. Returns
// true iff err is classified as a bucket-unavailable error (see
// isBucketUnavailableErr), so the caller can substitute a more specific
// log line for the existing generic one.
//
// On a match:
//   - Sets the SourceBucketMissing gauge to true (idempotent — no-op if
//     already set).
//   - Invokes the configured SourceUnavailableHook, rate-limited by
//     unavailableCooldown so a sustained outage does not flood the
//     caller's escalation logic.
//
// The hook is invoked OUTSIDE unavailableMu so a long-running caller hook
// does not block the reconcile / restart goroutine's next tick.
func (s *NatsKV) noteBucketUnavailable(err error) bool {
	if !isBucketUnavailableErr(err) {
		return false
	}
	s.unavailableMu.Lock()
	var fire bool
	if s.unavailableHook != nil && time.Since(s.lastUnavailableAt) >= s.unavailableCooldown {
		s.lastUnavailableAt = time.Now()
		fire = true
	}
	gaugeChanged := !s.bucketMissing
	if gaugeChanged {
		s.bucketMissing = true
	}
	s.unavailableMu.Unlock()

	if gaugeChanged {
		s.metrics.SetSourceBucketMissing(true)
	}
	if fire {
		s.unavailableHook(err)
	}

	return true
}

// noteBucketAvailable clears the SourceBucketMissing gauge after a
// successful watcher-restart or kv.Get. The hook is NOT re-fired on
// recovery — escalation is one-way; the gauge carries the recovery
// signal. Cheap: a no-op if the gauge is already clear.
func (s *NatsKV) noteBucketAvailable() {
	s.unavailableMu.Lock()
	gaugeChanged := s.bucketMissing
	if gaugeChanged {
		s.bucketMissing = false
	}
	s.unavailableMu.Unlock()

	if gaugeChanged {
		s.metrics.SetSourceBucketMissing(false)
	}
}
