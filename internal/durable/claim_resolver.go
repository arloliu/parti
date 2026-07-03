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
	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/arloliu/parti/v2/internal/retry"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// Watcher restart backoff knobs. Values mirror the canonical
// source-watcher implementation in source/nats_kv.go (and
// manager_assignment.go): 2s base, 30s cap, ±30% jitter, 6 attempts.
//
// Declared as vars (not consts) so integration tests in this package
// can drive the bounded-retry envelope at a sub-second cadence without
// waiting the production worst-case of ~80s. Production code must
// NEVER mutate these.
var (
	watcherBaseBackoff = 2 * time.Second
	watcherMaxBackoff  = 30 * time.Second

	// watcherMaxAttempts caps the bounded-retry envelope on the
	// watcher-restart loop in supervise. With watcherBaseBackoff=2s
	// and watcherMaxBackoff=30s, six attempts span roughly one
	// reconcile cycle. Past this point we rely on the periodic
	// reconciler for recovery and emit the watcher-exhausted
	// escalation signal.
	watcherMaxAttempts = 6
)

const (
	watcherJitter            = 0.3
	defaultReconcileInterval = 30 * time.Second
)

// errWatcherClosed signals the supervisor that the underlying watcher's
// Updates() channel closed and a new watcher must be established.
var errWatcherClosed = errors.New("claim resolver: watcher channel closed")

// Watcher-restart reason labels emitted via ResolverMetrics.IncWatcherRestart.
// Defined as constants so emit sites and tests share a single source of truth.
const (
	watcherRestartReasonChannelClosed   = "channel_closed"
	watcherRestartReasonEstablishFailed = "establish_failed"
	watcherRestartReasonDriftDetected   = "drift_detected"

	// watcherRestartReasonExhausted is emitted exactly once per
	// envelope-exhaustion event in supervise. The supervisor stops
	// retrying after this signal; recovery falls back to the periodic
	// reconciler. Operators should alert on this metric being non-zero
	// — sustained exhaustion indicates the underlying KV bucket is
	// unrecoverable from this connection (e.g., wipe with no recreate,
	// or a sustained NATS outage past the retry budget).
	watcherRestartReasonExhausted = "exhausted"
)

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
	}
}

// WithStreamPosProbe enables the reconcile scan gate by supplying a
// DEDICATED KV handle used only for stream-position probes on the
// reconciler goroutine. The handle must not be shared with any other
// goroutine's KV operations: kv.Status resolves through stream.Info,
// which mutates the handle's cached *stream state and races concurrent
// Get/Put/Watch on the same handle (the race class fixed for the epoch
// monitor by a dedicated probe handle). Passing nil leaves the gate
// disabled: every reconcile tick runs the full scan, the pre-gate
// behavior.
//
// Parameters:
//   - probeKV: Dedicated KV handle for probing; nil disables the gate.
//
// Returns:
//   - ResolverOption: Option function
func WithStreamPosProbe(probeKV jetstream.KeyValue) ResolverOption {
	return func(r *ClaimBasedResolver) {
		if probeKV == nil {
			return
		}
		r.streamPos = func(ctx context.Context) (natsutil.KVStreamPos, error) {
			return natsutil.ProbeKVStreamPos(ctx, probeKV)
		}
	}
}

// WithDriftRestartCooldown sets the minimum interval between
// reconciler-triggered watcher restarts. When reconcileScan observes drift
// between KV and the in-memory cache, it asks the supervisor to tear down
// and re-establish the watcher; this signal is rate-limited so that a
// persistently unreachable NATS server cannot trigger a restart storm.
//
// A value of 0 disables drift-driven restarts entirely: reconcileScan will
// still observe drift and rescue the cache (emitting IncReconcileRescue),
// but the watcher is NOT torn down. The default is 2 × reconcileInterval,
// floored at 60s. If reconcileInterval is 0 (reconciler disabled), drift
// restart is inert because the trigger never fires.
//
// Parameters:
//   - d: Minimum interval between drift-driven watcher restarts; 0 disables.
//
// Returns:
//   - ResolverOption: Option function
func WithDriftRestartCooldown(d time.Duration) ResolverOption {
	return func(r *ClaimBasedResolver) {
		r.driftRestartCooldown = d
		r.driftRestartCooldownSet = true
	}
}

// WithWatcherRetryExhausted registers a callback invoked exactly once
// when the watcher-restart envelope exhausts its attempt budget. The
// callback fires from the supervise goroutine immediately before the
// supervisor exits; after exit the periodic reconciler remains as the
// sole recovery path (the nats.go KV watcher's Updates() channel does
// NOT close on a NATS server restart, so the supervisor's normal
// restart path will not fire for that trigger — see
// project_nats_watcher_empirical_finding).
//
// The hook is the escalation point operators wire into readiness logic
// (or alert on directly). Without a hook configured, exhaustion is
// still logged at Warn level and emitted via
// IncWatcherRestart("exhausted") so it is never silent.
//
// Concurrency contract: the callback runs synchronously on the
// supervise goroutine. Implementations MUST be non-blocking; a blocking
// callback delays the supervisor's exit and consequently any
// subsequent Stop().
//
// Parameters:
//   - fn: Callback invoked at exhaustion. nil clears any prior hook.
//
// Returns:
//   - ResolverOption: Option function
func WithWatcherRetryExhausted(fn func(err error)) ResolverOption {
	return func(r *ClaimBasedResolver) {
		r.onRetryExhausted = fn
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
	reconcileInterval time.Duration

	// Scan-gate state for the periodic reconciler. Owned exclusively by
	// the reconcileLoop goroutine — never touched by the watcher,
	// supervisor, or ForceRefreshPartition. A gated tick proves via two
	// stream-position probes (gateConfirmGap apart) that the bucket is
	// byte-identical to what the last clean full pass observed, and
	// skips the Keys()+Get walk entirely; any probe error or unsafe
	// bucket config fails open into a full pass.
	gateLatchValid     bool
	gateLatch          natsutil.KVStreamPos
	gateSkipsSinceFull int
	// gateMaxSkips bounds consecutive gated skips before a full pass is
	// forced regardless of the probes. Initialized to
	// natsutil.ScanGateMaxSkippedPasses by NewClaimBasedResolver;
	// only in-package tests shorten it to reach the forced-pass backstop
	// on a test timescale. Production behavior is byte-identical to
	// reading the constant directly.
	gateMaxSkips    int
	gateConfigGuard natsutil.ScanGateConfigGuard
	// gateConfirmGap is the double-probe confirmation wait; tests
	// shorten it. Production value is natsutil.ScanGateDefaultConfirmGap.
	gateConfirmGap time.Duration
	// streamPos probes the position of the bucket's backing stream.
	// nil means no probe seam is configured and the gate is inert
	// (every tick runs the full scan — pre-gate behavior). Set by
	// WithStreamPosProbe over a DEDICATED probe handle, or injected
	// directly by unit tests (jetstream.KeyValueBucketStatus cannot be
	// fabricated outside nats.go).
	streamPos func(ctx context.Context) (natsutil.KVStreamPos, error)

	// Drift-driven watcher restart configuration. The reconciler signals
	// the supervisor to tear down the current watcher when drift is
	// observed; the supervisor's existing restart path then re-establishes
	// the watcher and emits IncWatcherRestart("drift_detected"). The
	// cooldown rate-limits these restarts to avoid storms when NATS is
	// persistently unreachable.
	driftRestartCooldown    time.Duration
	driftRestartCooldownSet bool

	// driftRestartPending signals to supervise that the most recent
	// watcher close was triggered by the reconciler observing drift, so
	// the restart should be classified as "drift_detected" rather than
	// "channel_closed". Set by requestWatcherRestartFromReconcile;
	// consumed by supervise via CompareAndSwap(true, false).
	driftRestartPending atomic.Bool

	// lastDriftRestartNano records the time (UnixNano) of the most recent
	// reconciler-triggered watcher restart. Used to rate-limit drift
	// restarts to no more than one per driftRestartCooldown. A zero value
	// means "no drift restart has been triggered yet". Mutated only from
	// the single reconciler goroutine; the atomic load is needed for
	// publication, not for cross-goroutine writers.
	lastDriftRestartNano atomic.Int64

	// Lifecycle. stopCh is closed by Stop; doneCh is closed once both
	// supervisor and reconciler goroutines have exited (or inline by Start
	// on early-exit error paths so Stop never hangs).
	//
	// Lifecycle invariants:
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

	// onRetryExhausted fires exactly once per supervise lifecycle when
	// the watcher-restart envelope exhausts its attempt budget. See
	// WithWatcherRetryExhausted for the wiring contract. nil means no
	// callback is configured; exhaustion is still logged + metricized.
	onRetryExhausted func(err error)
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
		gateConfirmGap:    natsutil.ScanGateDefaultConfirmGap,
		gateMaxSkips:      natsutil.ScanGateMaxSkippedPasses,
		// Allocate lifecycle channels eagerly so Stop is safe before Start.
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}

	for _, opt := range opts {
		opt(r)
	}

	// Default drift-restart cooldown: 2 × reconcileInterval, floored at
	// 60s. If reconcileInterval is 0 the reconciler is disabled, so the
	// drift trigger never fires and the cooldown value is moot. If the
	// caller explicitly set the cooldown (including to 0 to disable
	// drift-driven restarts), respect that.
	if !r.driftRestartCooldownSet && r.reconcileInterval > 0 {
		r.driftRestartCooldown = max(2*r.reconcileInterval, 60*time.Second)
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
	// Refuse double-start and stop-before-start. Atomic CAS makes the
	// lifecycle transition observable to a concurrent Stop without a
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

		// Watcher closed — log + bounded-retry envelope re-establish.
		if r.logger != nil {
			r.logger.Warn("claim resolver watcher closed, restarting",
				"error", err,
			)
		}

		// Brief pre-attempt sleep before the envelope fires its first
		// attempt. Preserves the original supervise timing so the
		// reconciler has a window to observe drift and set
		// driftRestartPending before the restart consumes it — without
		// this delay the immediate restart races the reconciler and
		// the first restart always classifies as channel_closed.
		// Respects stopCh so Stop() unblocks promptly during this
		// pre-attempt wait.
		if !r.sleepWithStop(ctx, jittered(watcherBaseBackoff)) {
			return
		}

		nextWatcher := r.restartWatcherWithEnvelope(ctx)
		if nextWatcher == nil {
			// Either the envelope exhausted its attempt budget (the
			// permanent-failure callback already fired) or the context
			// was cancelled mid-attempt. Either way the supervisor is
			// done; the periodic reconciler remains as the sole
			// recovery path.
			return
		}

		// Classify the restart reason: a pending drift signal from the
		// reconciler takes precedence over the default channel_closed.
		// CompareAndSwap consumes the flag atomically so subsequent
		// restarts default back to channel_closed until the reconciler
		// signals again. If runWatcher had failed repeatedly above
		// (emitting establish_failed), the flag stays set until this
		// successful restart, at which point it correctly classifies as
		// drift_detected — the original cause of the close was drift.
		reason := watcherRestartReasonChannelClosed
		if r.driftRestartPending.CompareAndSwap(true, false) {
			reason = watcherRestartReasonDriftDetected
		}
		if r.metrics != nil {
			r.metrics.IncWatcherRestart(reason)
		}
		watcher = nextWatcher
	}
}

// restartWatcherWithEnvelope drives the bounded-retry envelope that
// gates the watcher-restart loop. Returns the freshly-established
// watcher on success or nil if the envelope exhausts its attempt
// budget / the context is cancelled. Per-attempt failures emit the
// existing IncWatcherRestart("establish_failed") metric; exhaustion
// emits IncWatcherRestart("exhausted") and fires the optional
// onRetryExhausted callback exactly once.
func (r *ClaimBasedResolver) restartWatcherWithEnvelope(ctx context.Context) jetstream.KeyWatcher {
	var nextWatcher jetstream.KeyWatcher
	env := retry.New(retry.Config{
		Work: func(_ context.Context) error {
			w, runErr := r.runWatcher(ctx)
			if runErr != nil {
				if r.metrics != nil {
					r.metrics.IncWatcherRestart(watcherRestartReasonEstablishFailed)
				}
				return runErr
			}
			nextWatcher = w

			return nil
		},
		OnPermanent: r.onWatcherRetryExhausted,
		BaseBackoff: watcherBaseBackoff,
		MaxBackoff:  watcherMaxBackoff,
		MaxAttempts: watcherMaxAttempts,
		Jitter:      watcherJitter,
		OnProgress: func(attempt int, err error) {
			if r.logger != nil {
				r.logger.Warn("claim resolver watcher restart failed, retrying",
					"error", err,
					"attempt", attempt,
				)
			}
		},
		// Sleep is overridden because the envelope's default Sleep
		// only observes ctx.Done(). The resolver's documented Stop
		// contract is that Stop() unblocks the supervisor's backoff
		// sleep without cancelling the lifecycle context — closing
		// stopCh is the signal. The envelope must respect both.
		Sleep: func(sctx context.Context, d time.Duration) error {
			if d <= 0 {
				return nil
			}
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-sctx.Done():
				return sctx.Err()
			case <-r.stopCh:
				return context.Canceled
			case <-t.C:
				return nil
			}
		},
	})
	_ = env.Run(ctx) // ctx-cancel, stopCh, and exhaustion all end the supervisor

	return nextWatcher
}

// sleepWithStop blocks for d or until stopCh / ctx cancellation. Returns
// false if the resolver was stopped (caller should exit). Used by
// supervise for the pre-attempt sleep before the envelope's first
// restart attempt — see the inline comment at the call site for why
// the timing preserves drift-detection semantics.
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

// onWatcherRetryExhausted is the envelope's permanent-failure callback.
// Fires exactly once per supervise lifecycle when restartWatcherWithEnvelope
// consumes its attempt budget. The reconciler remains running and will
// continue to converge state on each tick; operators should treat
// sustained watcher-restart exhaustion as a signal that the underlying
// KV bucket needs investigation.
func (r *ClaimBasedResolver) onWatcherRetryExhausted(err error) {
	if r.metrics != nil {
		r.metrics.IncWatcherRestart(watcherRestartReasonExhausted)
	}
	if r.logger != nil {
		r.logger.Warn("claim resolver watcher restart attempt budget exhausted; "+
			"relying on reconciler for recovery",
			"error", err,
			"max_attempts", watcherMaxAttempts,
		)
	}
	if hook := r.onRetryExhausted; hook != nil {
		hook(err)
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

// reconcileScan is the full reconcile pass body (list + per-key read +
// tombstone synthesis + apply). It reports whether the pass was CLEAN —
// the list succeeded (the "no keys found" empty-bucket normalization
// counts as success) and every listed key was readable — which is the
// gate's precondition for latching a bucket position. Cleanliness is
// about read health, not about whether the pass applied changes.
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
func (r *ClaimBasedResolver) reconcileScan(ctx context.Context) bool {
	// Snapshot the cache BEFORE walking KV Keys(). Any cache entry
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
	// reconcileScan.
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
			return false
		}
		keys = nil
	}

	pendingByPID := make(map[string]pending)
	seen := make(map[string]struct{}, len(keys))
	// Keys we listed but could not Get this pass (transient read failure, e.g.
	// a quorum-lost bucket returning DeadlineExceeded/ErrNoResponders while the
	// connection stays CONNECTED). These must NOT be tombstoned: the key still
	// exists, we just could not read it now; a later pass re-reads it. Without
	// this set, an unreadable key would fall through to the tombstone pass below
	// and be staged as a synthetic delete at R+1 — an irreversible poison.
	unreadable := make(map[string]struct{})
	// firstReadErr keeps one sample of the per-key Get errors for the aggregated
	// observability log below (F-D2c). Per-key logging would be a storm during a
	// whole-bucket read fault, so we log once per pass with the count.
	var firstReadErr error

	for _, k := range keys {
		if r.claimsPref != "" && !strings.HasPrefix(k, r.claimsPref) {
			continue
		}
		entry, err := r.kv.Get(ctx, k)
		if err != nil {
			// Listed-but-unreadable: record the pid so the tombstone pass
			// skips it. The next reconcile pass retries the read.
			unreadable[strings.TrimPrefix(k, r.claimsPref)] = struct{}{}
			if firstReadErr == nil {
				firstReadErr = err
			}
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

	// Observability (F-D2c): surface the previously-swallowed per-key read
	// failures. A non-zero count means the bucket is listing keys but failing to
	// read some of them (the quorum-loss "Keys-ok/Get-fail" window) — the signal
	// that, before the F-D2a fix, was silently converted into permanent
	// tombstones. One aggregated line per pass avoids a log storm under a
	// whole-bucket read fault.
	if len(unreadable) > 0 && r.logger != nil {
		r.logger.Warn("claim resolver reconcile: listed keys were unreadable this pass",
			"unreadable_count", len(unreadable), "sample_error", firstReadErr)
	}

	// Iterate over the pre-Keys snapshot, NOT the live cache. Any
	// entry the watcher added after `snap` was taken is invisible here, so
	// the tombstone pass cannot synthesize a delete for it.
	for pid, e := range snap {
		if e.deleted {
			continue
		}
		if _, ok := seen[pid]; ok {
			continue
		}
		// Listed but unreadable this pass — a transient read failure, not a
		// deletion. Skip; the next reconcile re-reads it. (Genuine deletions —
		// absent from Keys, or a delete/purge op — are not in `unreadable` and
		// still tombstone below.)
		if _, ok := unreadable[pid]; ok {
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
		return len(unreadable) == 0
	}
	// Emit the rescue metric BEFORE applying so the event is observable
	// even if applyPendingBatch were ever to panic.
	if r.metrics != nil {
		r.metrics.IncReconcileRescue()
	}
	r.applyPendingBatch(pendingByPID, "reconcile")

	// Drift observed: the watcher missed events, almost certainly because
	// its Updates() channel is silently stalled (the nats.go KV watcher
	// does not surface NATS server restarts as channel close). Ask the
	// supervisor to tear down the current watcher; its existing restart
	// path will re-establish via WatchAll and re-deliver historical state
	// through the initial replay. Rate-limited by driftRestartCooldown.
	r.requestWatcherRestartFromReconcile()

	return len(unreadable) == 0
}

// gateProbe takes one stream-position probe and applies the fail-open
// and config-validation rules: any probe error or unsafe bucket config
// invalidates the latch, so no skip can happen until a clean full pass
// re-latches. Returns the probe result, whether it is usable for
// gating/latching, and the full-pass reason label when it is not.
func (r *ClaimBasedResolver) gateProbe(ctx context.Context) (natsutil.KVStreamPos, bool, string) {
	pos, err := r.streamPos(ctx)
	if err != nil {
		r.gateLatchValid = false
		if r.logger != nil {
			r.logger.Debug("claim resolver reconcile: stream position probe failed", "error", err)
		}

		return natsutil.KVStreamPos{}, false, "probe_error"
	}
	if !r.gateConfigGuard.Check(pos, r.logger) {
		r.gateLatchValid = false

		return pos, false, "unsafe_config"
	}

	return pos, true, ""
}

// reconcileOnce gates a reconcile tick behind a stream-position probe:
// when two probes gateConfirmGap apart both equal the position latched
// by the last clean full pass, the bucket is provably byte-identical to
// what that pass observed and the tick is skipped (no snapshot, no
// Keys(), no Gets, no apply, no drift signal). Anything else — probe
// error, unsafe config, position mismatch, unlatched state, or the
// forced-pass budget running out — runs the full reconcileScan.
func (r *ClaimBasedResolver) reconcileOnce(ctx context.Context) {
	if r.streamPos == nil {
		// No probe seam configured (no dedicated probe handle was
		// supplied): the gate is inert. Today's behavior, byte-for-byte
		// — no probes, no gate bookkeeping, no gate logging.
		_ = r.reconcileScan(ctx)

		return
	}

	pos, ok, reason := r.gateProbe(ctx)
	if ok && r.gateLatchValid && pos.Same(r.gateLatch) {
		if r.gateSkipsSinceFull >= r.gateMaxSkips {
			reason = "forced"
		} else {
			// Double-probe confirmation (stop/ctx-aware wait; on stop,
			// end the pass without scanning or latching).
			if !r.sleepWithStop(ctx, r.gateConfirmGap) {
				return
			}
			pos2, ok2, reason2 := r.gateProbe(ctx)
			if ok2 && pos2.Same(r.gateLatch) {
				r.gateSkipsSinceFull++
				if r.gateSkipsSinceFull == 1 && r.logger != nil {
					r.logger.Debug("claim resolver reconcile gate engaged")
				}

				return
			}
			// Confirmation failed: fall through to the full pass. The
			// freshest usable probe (if any) becomes the latch candidate.
			pos, ok = pos2, ok2
			reason = reason2
			if reason == "" {
				reason = "mismatch"
			}
		}
	} else if ok && reason == "" {
		if r.gateLatchValid {
			reason = "mismatch"
		} else {
			reason = "unlatched"
		}
	}

	clean := r.reconcileScan(ctx)

	if ok && clean {
		// Latch the pre-scan probe. Conservative by construction: any
		// mutation landing between this probe and the scan's reads makes
		// the NEXT probe differ from the latch, forcing a full pass —
		// the scan may observe newer state than the latch claims, never
		// older.
		r.gateLatch = pos
		r.gateLatchValid = true
	} else {
		r.gateLatchValid = false
	}
	if r.logger != nil {
		r.logger.Debug("claim resolver reconcile full pass",
			"gated_skips_since_last_full", r.gateSkipsSinceFull,
			"reason", reason,
		)
	}
	r.gateSkipsSinceFull = 0
}

// requestWatcherRestartFromReconcile is called by reconcileScan when it has
// applied any change to the cache. The reconciler engaging means the watcher
// missed events — likely a silent stall (the nats.go KV watcher does not
// surface server restarts as Updates() channel close). The supervisor's
// restart path then re-establishes the watcher and re-delivers historical
// state via WatchAll's initial replay.
//
// Rate-limited to one drift-driven restart per driftRestartCooldown (default
// 2 × reconcileInterval, minimum 60s). The reconciler will still rescue
// subsequent drifts at its normal cadence; rate-limiting prevents restart
// storms when the watcher cannot successfully re-establish (e.g., persistent
// NATS unreachable). A cooldown of 0 disables drift-driven restart entirely.
//
// Invariant: called only from the single reconciler goroutine. The
// load-then-store on lastDriftRestartNano is therefore not racy with itself;
// the atomic operations are for publication to readers (e.g., metrics or
// future tests) rather than concurrent writers.
func (r *ClaimBasedResolver) requestWatcherRestartFromReconcile() {
	cooldown := r.driftRestartCooldown
	if cooldown <= 0 {
		return // drift-driven restart disabled
	}

	if last := r.lastDriftRestartNano.Load(); last != 0 {
		if time.Since(time.Unix(0, last)) < cooldown {
			return
		}
	}
	r.lastDriftRestartNano.Store(time.Now().UnixNano())

	// Mark the next channel-close as drift-triggered for the supervise
	// reason emission. Note: a NATS-level (non-drift) close may race with
	// this Store and be misclassified as drift_detected, or a real drift
	// close may race the supervisor's CAS and be classified as
	// channel_closed. Either misclassification is benign — operators see
	// roughly the right reason in aggregate, and the recovery path is
	// identical in both cases.
	r.driftRestartPending.Store(true)

	// Stop the current watcher under watcherMu. Closing the Updates()
	// channel makes processWatcher return errWatcherClosed, which lets
	// supervise re-establish via runWatcher. The Stop call is offloaded
	// to its own goroutine because nats.go's KeyWatcher.Stop drains the
	// underlying JetStream subscription, which is a synchronous NATS
	// round-trip that can block on slow or degraded connections —
	// precisely the conditions under which drift restarts fire. Stopping
	// here in line would stall the reconciler goroutine and delay the
	// next reconcile tick.
	r.watcherMu.Lock()
	w := r.currentWatcher
	r.watcherMu.Unlock()
	if w != nil {
		// w may already be replaced or stopped by the supervisor by the
		// time this goroutine runs; KeyWatcher.Stop is idempotent.
		go func() { _ = w.Stop() }()
	}
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
