package assignment

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/arloliu/parti/v2/kvutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// Protocol-key suffixes for the refs-always commit model. All protocol keys
// share an underscore-prefixed sub-component so they cannot collide with
// worker IDs (a worker named "commit" or "payload" would otherwise be
// ambiguous).
const (
	commitKeyName     = "_commit"      // singleton; full key: "<prefix>._commit"
	commitLogPrefix   = "_commit_log." // full key: "<prefix>._commit_log.<V>"
	payloadKeyPrefix  = "_payload."    // full key: "<prefix>._payload.<hex(sha256)>"
	protocolKeyMarker = "_"            // any sub-component starting with '_' is a protocol key
)

// Bounded retry budget for the legacy alias barrier (publish step 6 of §3.5).
// Three attempts with jittered backoff is the documented contract; failure
// here aborts the batch.
const (
	aliasBarrierMaxAttempts = 3
	aliasBarrierBaseBackoff = 50 * time.Millisecond
	aliasBarrierMaxJitter   = 50 * time.Millisecond
)

// LeaderCheckFunc is the contract for the publisher's pre-alias and post-alias
// leadership fences (publish steps 5 and 7 of §3.5).
//
// Production implementations MUST perform a live verification (e.g., a kv.Get
// against the election leader key and a revision comparison) and return a
// wrapped types.ErrLeadershipRevisionMismatch when the live revision differs
// from claimed. A cached/stale check is NOT acceptable: a former leader whose
// local term-revision has not yet been cleared could otherwise pass the fence
// after another worker has taken over.
//
// Returning any non-nil error aborts the batch; the publisher wraps the
// returned error with the appropriate sentinel
// (types.ErrLeadershipLostPreAlias or types.ErrLeadershipLostPostAlias).
type LeaderCheckFunc func(ctx context.Context, claimed uint64) error

// AssignmentPublisher publishes partition assignments using the refs-always
// commit model.
//
// On every Publish, the publisher:
//  1. Writes content-addressable AssignmentPayload keys ("assignment._payload.<hex(sha256)>")
//     via kv.Create with hash-verify on ErrKeyExists (immutable, dedupe-safe).
//  2. Reads heartbeats to classify legacy (CapAckV1=0) workers in the batch.
//  3. Brackets the legacy alias barrier with two leadership rechecks
//     (pre-alias and post-alias).
//  4. Writes mandatory "assignment.<W>" legacy aliases for legacy workers
//     with bounded retries (failure aborts the batch).
//  5. CAS-writes the AssignmentCommit to "assignment._commit" — this is the
//     single atomic decision point.
//  6. Best-effort writes the AssignmentCommitLog and compat-noise legacy
//     aliases for commit-capable workers.
//  7. Best-effort triggers GC.
//
// The publisher is intentionally agnostic to the strategy and source layout;
// the calculator computes the assignment map and source snapshot and passes
// them in via Publish.
type AssignmentPublisher struct {
	assignmentKV  jetstream.KeyValue
	heartbeatKV   jetstream.KeyValue // for the legacy alias barrier classification
	prefix        string             // assignment prefix, e.g. "assignment"
	keyPrefix     string             // cached "<prefix>."
	heartbeatPref string             // heartbeat prefix, e.g. "heartbeat"
	hbKeyPrefix   string             // cached "<heartbeatPref>."

	// leaderCheckFn performs a LIVE verification of the leader-key revision
	// against the value claimed in PublishInput.LeaderRevision. Used by steps
	// 5 (pre-alias) and 7 (post-alias). May be nil for tests / non-leader
	// pseudo-publishers; if nil, leadership rechecks are skipped (NOT a
	// production-safe configuration).
	leaderCheckFn LeaderCheckFunc

	// gcTriggerFn, if non-nil, is invoked best-effort after a successful
	// commit so the GC loop wakes immediately instead of waiting for its
	// next tick (publish step 12 of §3.5). Safe to leave nil.
	gcTriggerFn func()

	// isShuttingDown, when non-nil, gates the commit-CAS site so a leader
	// whose stopCh has been closed cannot land a stale commit. See
	// PublisherConfig.IsShuttingDown.
	isShuttingDown func() bool

	mu             sync.Mutex
	currentVersion int64
	lastCommitRev  uint64                  // KV revision of "<prefix>._commit" at last successful commit
	lastCommit     *types.AssignmentCommit // deep copy of the most recently observed commit; nil until first CAS or bootstrap

	// lastCommitObservedAtMono records the local monotonic-clock instant
	// at which THIS publisher observed lastCommit — either via a
	// successful CAS (Publish step 9) or via BootstrapLastCommit. The
	// audit uses this instead of commit.PublishedAt for grace-window
	// math so wall-clock skew across leader handoff cannot suppress or
	// prematurely trigger audit_repair escalation (ISSUE-007 / PR-2).
	//
	// Zero value means "no commit observed yet" (lastCommit is also nil
	// in that case). Set under p.mu.
	lastCommitObservedAtMono time.Time

	lastRebalance time.Time

	// now returns the current time; injectable so cooldown timing can be
	// driven deterministically in tests. Defaults to time.Now. It backs the
	// lastRebalance stamp, which the calculator's cooldown gate compares
	// against using the same clock.
	now func() time.Time

	// inflightRefs records the payload keys this publisher has selected for an
	// in-progress publish (after step 4 verify-back, before step 9 commit
	// CAS). The GC consults LiveRefs() so it cannot delete a payload that an
	// in-flight commit is about to reference (see §3.9 / P0-2). Populated as
	// a stable snapshot before step 5 and cleared after step 9 returns.
	inflightRefs sync.Map // map[string]struct{}

	logger  types.Logger
	metrics PublisherDependentMetrics
}

// PublisherDependentMetrics is the metrics surface the publisher (and GC) write to.
//
// The interface composes AssignmentMetrics (for legacy-style change tracking),
// PublisherMetrics (refs-always counters), and GCMetrics (payload reaping).
// Concrete MetricsCollector implementations satisfy all three.
type PublisherDependentMetrics interface {
	types.AssignmentMetrics
	types.PublisherMetrics
	types.GCMetrics
}

// PublisherConfig captures the publisher's construction-time dependencies.
//
// LeaderCheckFn is required for production correctness: it backs the
// pre-alias and post-alias leadership rechecks (publish steps 5 and 7) and
// MUST perform a live verification (see LeaderCheckFunc).
// HeartbeatKV is required for the legacy alias barrier (publish step 6).
// GCTriggerFn is optional and, if set, is called after a successful commit
// to wake the GC loop (publish step 12).
type PublisherConfig struct {
	AssignmentKV    jetstream.KeyValue
	HeartbeatKV     jetstream.KeyValue
	Prefix          string // e.g. "assignment"
	HeartbeatPrefix string // e.g. "heartbeat"
	LeaderCheckFn   LeaderCheckFunc
	GCTriggerFn     func()
	Logger          types.Logger
	Metrics         PublisherDependentMetrics

	// IsShuttingDown, when non-nil, is consulted immediately before the
	// commit CAS (step 9 of §3.5). If it returns true, Publish aborts with
	// errShuttingDown and no `_commit` write is attempted. Calculator wires
	// this to detect that its stopCh has been closed. nil means no gate
	// (test-only).
	IsShuttingDown func() bool

	// Now returns the current time and backs the lastRebalance stamp. When
	// nil, time.Now is used. Tests inject a controllable clock so the
	// calculator's cooldown gate is deterministic under load.
	Now func() time.Time
}

// abortIfShuttingDown returns errShuttingDown when isShuttingDown signals
// stopCh has closed. Records the batch-abort metric (and the visible-uncommitted
// alias counter when applicable) without touching IncrementCommitAborts —
// no CAS is issued (see types/metrics_collector.go contract).
func (p *AssignmentPublisher) abortIfShuttingDown(aliasWritten bool) error {
	if p.isShuttingDown == nil || !p.isShuttingDown() {
		return nil
	}
	p.metrics.IncrementBatchAborted("shutdown")
	if aliasWritten {
		p.metrics.IncrementAliasVisibleUncommitted()
	}

	return errShuttingDown
}

// NewAssignmentPublisher creates a publisher wired to the assignment KV bucket.
//
// LeaderCheckFn and HeartbeatKV may be nil only in narrow test contexts;
// production calls MUST supply both. When LeaderCheckFn is nil, the
// pre-alias and post-alias leadership rechecks become no-ops (every
// Publish is treated as if leadership is held). When HeartbeatKV is nil,
// the legacy alias barrier degrades into "treat all workers as commit-capable"
// — the publisher will not write mandatory aliases and the cluster cannot
// safely host pre-CapAckV1 workers.
func NewAssignmentPublisher(cfg PublisherConfig) *AssignmentPublisher {
	hbPrefix := cfg.HeartbeatPrefix
	if hbPrefix == "" {
		hbPrefix = "heartbeat"
	}
	logger := cfg.Logger
	if logger == nil {
		// Fallback to a nop logger via the existing pattern; importing logging
		// would create a cycle in some test setups, so demand a real logger
		// from callers in production.
		logger = types.Logger(noopLogger{})
	}
	metrics := cfg.Metrics
	if metrics == nil {
		metrics = nopPublisherMetrics{}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	return &AssignmentPublisher{
		assignmentKV:   cfg.AssignmentKV,
		heartbeatKV:    cfg.HeartbeatKV,
		prefix:         cfg.Prefix,
		keyPrefix:      cfg.Prefix + ".",
		heartbeatPref:  hbPrefix,
		hbKeyPrefix:    hbPrefix + ".",
		leaderCheckFn:  cfg.LeaderCheckFn,
		gcTriggerFn:    cfg.GCTriggerFn,
		isShuttingDown: cfg.IsShuttingDown,
		now:            now,
		logger:         logger,
		metrics:        metrics,
	}
}

// noopLogger is a defensive fallback when no logger is supplied. Production
// callers should always pass a real logger (logging.NewNop() is fine).
type noopLogger struct{}

func (noopLogger) Debug(_ string, _ ...any) {}
func (noopLogger) Info(_ string, _ ...any)  {}
func (noopLogger) Warn(_ string, _ ...any)  {}
func (noopLogger) Error(_ string, _ ...any) {}
func (noopLogger) Fatal(_ string, _ ...any) {}

// nopPublisherMetrics is a defensive fallback metrics implementation.
type nopPublisherMetrics struct{}

func (nopPublisherMetrics) RecordAssignmentChange(_, _ int, _ int64) {}
func (nopPublisherMetrics) IncrementPayloadsCreated()                {}
func (nopPublisherMetrics) IncrementPayloadsReused()                 {}
func (nopPublisherMetrics) ObservePayloadBytesWritten(_ int)         {}
func (nopPublisherMetrics) ObserveCommitBytesWritten(_ int)          {}
func (nopPublisherMetrics) IncrementBatchAborted(_ string)           {}
func (nopPublisherMetrics) IncrementAliasBarrierFailed()             {}
func (nopPublisherMetrics) IncrementAliasVisibleUncommitted()        {}
func (nopPublisherMetrics) IncrementCommitAborts()                   {}
func (nopPublisherMetrics) IncrementPayloadDeleteErrors()            {}

// PublishInput captures the per-batch inputs to Publish that have to come from
// the calculator (because the publisher does not own the strategy or the
// source snapshot path). Group them in a struct so future additions don't
// re-break callers.
type PublishInput struct {
	// Workers is the active worker set the assignments cover. Determines
	// AssignmentCommit.Workers (sorted internally).
	Workers []string

	// Assignments maps worker ID → its assigned partition slice.
	Assignments map[string][]types.Partition

	// SourcePartitions is the full sorted-or-unsorted partition list from the
	// source. The publisher uses it for the strict set-equality coverage check
	// at publish step 3 (see §3.8). Pass exactly what was returned from the
	// source snapshot — the publisher canonicalizes internally.
	SourcePartitions []types.Partition

	// SourceRevision and SourceRevisionKnown come from
	// types.RevisionedPartitionSource.Snapshot when available. SourceRevision=0
	// + SourceRevisionKnown=false is the legitimate non-revisioned-source
	// state.
	SourceRevision      uint64
	SourceRevisionKnown bool

	// LeaderRevision is the publisher's claimed leader epoch. Steps 5 and 7
	// re-read the live election state and abort if it disagrees.
	LeaderRevision uint64

	// Lifecycle is the rebalance reason. Diagnostic only.
	Lifecycle string

	// WorkersToRemove is the list of legacy assignment.<W> aliases the
	// publisher should sweep AFTER a successful commit. Workers not in
	// AssignmentCommit.Workers are implicitly revoked at the commit level;
	// alias sweeping is rolling-upgrade hygiene only.
	WorkersToRemove []string

	// ParkedPartitions is the set deliberately left unassigned this batch.
	// Coverage becomes: assigned ∪ parked == source AND assigned ∩ parked == ∅.
	// Nil (the default) degenerates to the pre-label set-equality check.
	ParkedPartitions []types.Partition

	// WorkerLabels maps workerID → labels-of-record for payload stamping.
	// Workers absent from the map get WorkerLabels=nil (still Known=true).
	WorkerLabels map[string][]string
}

// Publish runs the 12-step refs-always commit flow described in §3.5 of the
// partition-assignment robustness plan.
//
// The flow is:
//  1. Verify set-equality coverage of Assignments against SourcePartitions.
//  2. Build per-worker AssignmentPayloads, Create their content-addressable
//     keys, and on ErrKeyExists verify-back via sha256 of the canonical
//     uncompressed bytes.
//  3. Pre-alias leadership recheck.
//  4. Heartbeat-aware legacy alias barrier (mandatory pre-commit aliases for
//     CapAckV1=0 workers in the batch, with bounded retries).
//  5. Post-alias leadership recheck.
//  6. CAS-write AssignmentCommit (THE commit point).
//  7. Best-effort: write AssignmentCommitLog, write compat-noise aliases for
//     commit-capable workers, sweep WorkersToRemove, schedule GC if wired.
//
// Returns:
//   - error: a typed sentinel from types/errors.go (ErrCoverageMismatch,
//     ErrLeadershipLostPreAlias, ErrAliasBarrierFailed,
//     ErrLeadershipLostPostAlias, ErrCommitCASFailed,
//     ErrPayloadHashCollisionOrCorruption) wrapped with context where useful.
func (p *AssignmentPublisher) Publish(ctx context.Context, in PublishInput) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(in.Workers) == 0 {
		p.logger.Info("publish skipped: no active workers")
		return nil
	}

	// --- step 3 of §3.5: publish-time set-equality coverage check ---
	if err := p.checkCoverage(in.SourcePartitions, in.Assignments, in.ParkedPartitions); err != nil {
		p.metrics.IncrementBatchAborted("coverage_mismatch")
		return err
	}

	// Tentative version for this batch — we increment local state ONLY on
	// successful CAS at step 9. Increment now to compute the proposed V.
	proposedVersion := p.currentVersion + 1

	// --- step 4 of §3.5: write content-addressable payload keys ---
	refs, payloadByWorker, err := p.writePayloads(ctx, in.Workers, in.Assignments, in.WorkerLabels)
	if err != nil {
		// Coverage was fine but a payload write failed; classify as a generic
		// abort. We do not increment IncrementBatchAborted with a specific
		// reason here because payload write failures are KV-layer faults
		// rather than protocol-state aborts. Caller logs include the err.
		return err
	}

	// --- in-flight refs registration (P0-2 / §3.9) ---
	// Register every selected payload key BEFORE the leadership fence (and
	// before the commit CAS) so a concurrent GC computing its live set after
	// this point cannot delete a payload this in-flight commit will reference.
	// The set is cleared on return whether the publish commits or aborts —
	// aborted publishes' payloads are inert and may be reaped on the next GC
	// pass anyway, but holding them through the abort window is safe and
	// keeps the lifecycle simple.
	for _, ref := range refs {
		p.inflightRefs.Store(ref.Key, struct{}{})
	}
	defer func() {
		for _, ref := range refs {
			p.inflightRefs.Delete(ref.Key)
		}
	}()

	// --- step 5 of §3.5: pre-alias leadership fence (LIVE) ---
	if err := p.checkLeadership(ctx, in.LeaderRevision, types.ErrLeadershipLostPreAlias); err != nil {
		p.metrics.IncrementBatchAborted("leadership_lost_pre_alias")
		return err
	}

	// --- step 6 of §3.5: heartbeat-aware legacy alias barrier ---
	legacyInBatch := p.classifyLegacyWorkers(ctx, in.Workers)
	aliasWritten, err := p.runAliasBarrier(ctx, in, legacyInBatch, payloadByWorker, proposedVersion)
	if err != nil {
		return err
	}

	// --- step 7 of §3.5: post-alias leadership recheck (LIVE) ---
	if err := p.checkLeadership(ctx, in.LeaderRevision, types.ErrLeadershipLostPostAlias); err != nil {
		// This is the documented mixed-version exposure case (§3.5): some
		// aliases may already be visible to old workers, but we did not
		// attempt the commit CAS (so parti.publisher.commit_aborts does NOT
		// fire — see test #62).
		p.metrics.IncrementBatchAborted("leadership_lost_post_alias")
		if aliasWritten {
			p.metrics.IncrementAliasVisibleUncommitted()
		}

		return err
	}

	// --- step 8 of §3.5: build AssignmentCommit ---
	sortedWorkers := make([]string, len(in.Workers))
	copy(sortedWorkers, in.Workers)
	slices.Sort(sortedWorkers)
	batchDigest := computeBatchDigest(in.SourcePartitions)
	commit := types.AssignmentCommit{
		Version:             proposedVersion,
		LeaderRevision:      in.LeaderRevision,
		SourceRevision:      in.SourceRevision,
		SourceRevisionKnown: in.SourceRevisionKnown,
		PublishedAt:         time.Now().UTC(),
		Workers:             sortedWorkers,
		Payloads:            refs,
		BatchDigest:         batchDigest,
		ParkedCount:         len(in.ParkedPartitions),
		ParkedDigest:        types.PartitionSetDigest(in.ParkedPartitions),
		PrevCommitRev:       p.lastCommitRev,
		Lifecycle:           in.Lifecycle,
	}
	commitBytes, err := json.Marshal(commit)
	if err != nil {
		return fmt.Errorf("marshal commit: %w", err)
	}

	// --- step 9 of §3.5: CAS-write assignment._commit ---
	// Shutdown gate (PR-1 / ISSUE-003 Layer 2): defense-in-depth against
	// a leader whose stopCh has closed between rebalance start and this
	// commit point. See abortIfShuttingDown.
	if err := p.abortIfShuttingDown(aliasWritten); err != nil {
		return err
	}
	commitKey := p.keyPrefix + commitKeyName
	var newCommitRev uint64
	if p.lastCommitRev == 0 {
		newCommitRev, err = p.assignmentKV.Create(ctx, commitKey, commitBytes)
	} else {
		newCommitRev, err = p.assignmentKV.Update(ctx, commitKey, commitBytes, p.lastCommitRev)
	}
	if err != nil {
		// CAS lost OR Create lost (another leader created the commit first).
		// Either way: surrender. The lost batch's payload writes are inert;
		// the legacy aliases written at step 6 are documented exposure.
		p.metrics.IncrementCommitAborts()
		p.metrics.IncrementBatchAborted("commit_cas_failed")
		// Only count exposure when at least one alias actually landed.
		if aliasWritten {
			p.metrics.IncrementAliasVisibleUncommitted()
		}
		// ISSUE-001: when the failure is a true CAS conflict, refresh
		// lastCommitRev from the live _commit so the next Publish can
		// re-CAS. Transient KV errors short-circuit inside the helper.
		p.refreshLastCommitRevAfterCASFailureLocked(ctx, err)

		return fmt.Errorf("%w: %w", types.ErrCommitCASFailed, err)
	}
	// COMMIT POINT: from here on, this batch is the cluster's authoritative view.
	p.lastCommitRev = newCommitRev
	p.currentVersion = proposedVersion
	// Cache a deep copy of the freshly-written commit so the leader-side audit
	// (Phase 4 §4.2) can read it without re-fetching the KV entry on every tick.
	commitCopy := cloneAssignmentCommit(&commit)
	p.lastCommit = &commitCopy
	// Capture the local monotonic-clock instant of this observation. The
	// leader-side audit gates grace windows on this value rather than the
	// wire-clock commit.PublishedAt, so cross-leader wall-clock skew cannot
	// suppress or prematurely fire audit_repair escalation (ISSUE-007).
	p.lastCommitObservedAtMono = time.Now()
	p.metrics.ObserveCommitBytesWritten(len(commitBytes))

	// --- step 10 of §3.5: best-effort commit log ---
	if err := p.writeCommitLog(ctx, commit, refs); err != nil {
		p.logger.Warn("commit log write failed (non-fatal)", "version", commit.Version, "error", err)
	}

	// --- step 11 of §3.5: best-effort compat-noise aliases for commit-capable workers ---
	legacySet := make(map[string]struct{}, len(legacyInBatch))
	for _, w := range legacyInBatch {
		legacySet[w] = struct{}{}
	}
	for _, w := range sortedWorkers {
		if _, isLegacy := legacySet[w]; isLegacy {
			continue
		}
		payload := payloadByWorker[w]
		legacy := buildLegacyAlias(payload, commit.Version, commit.LeaderRevision, commit.SourceRevision, commit.Lifecycle, len(sortedWorkers))
		if err := p.writeLegacyAliasOnce(ctx, w, legacy); err != nil {
			p.logger.Debug("compat-noise alias write failed (non-fatal)", "worker", w, "error", err)
		}
	}

	// --- post-step: best-effort sweep of removed workers' legacy aliases ---
	p.cleanupStaleAssignmentsLocked(ctx, in.WorkersToRemove)

	// --- step 12 of §3.5: best-effort GC trigger ---
	// Wake the background GC loop so the freshly-committed payloads' deltas
	// (newly-orphaned older payloads) are reclaimed promptly. The trigger is
	// non-blocking: if no listener is wired, this is a no-op.
	if p.gcTriggerFn != nil {
		p.gcTriggerFn()
	}

	// Bookkeeping & metrics.
	p.lastRebalance = p.now()
	for _, parts := range in.Assignments {
		p.metrics.RecordAssignmentChange(len(parts), 0, commit.Version)
	}

	p.logger.Info("assignment commit written",
		"version", commit.Version,
		"workers", len(commit.Workers),
		"payloads", len(commit.Payloads),
		"commit_bytes", len(commitBytes),
		"lifecycle", commit.Lifecycle,
		"source_revision_known", commit.SourceRevisionKnown,
	)

	return nil
}

// checkCoverage implements §3.8 widened for parked partitions (spec §4.3):
// assigned ∪ parked == source AND assigned ∩ parked == ∅. The union check is a
// strict set-equality of sorted CanonicalIDs; a raw multiset count catches
// duplicates (a partition assigned twice, or both assigned and parked), and the
// overlap check catches a partition that is simultaneously assigned and parked.
//
// A nil parked set degenerates to the pre-label check exactly: got == the
// assignment union, rawCount == len(union), and overlap is empty.
func (p *AssignmentPublisher) checkCoverage(source []types.Partition, assignments map[string][]types.Partition, parked []types.Partition) error {
	union := unionPartitions(assignments)
	assignedIDs := canonicalIDSet(union) // sorted, deduped
	parkedIDs := canonicalIDSet(parked)  // sorted, deduped
	expected := canonicalIDSet(source)

	// Disjointness: a partition may not be both assigned and parked.
	overlap := intersectSortedIDs(assignedIDs, parkedIDs)
	// Union: assigned ∪ parked must equal source as a set, and the raw counts
	// must match (multiset check catches duplicates within either set and
	// across the assigned/parked boundary).
	got := mergeSortedIDs(assignedIDs, parkedIDs) // sorted, deduped union
	rawCount := len(union) + len(parked)
	setOK := equalStringSlices(got, expected)
	multisetOK := rawCount == len(expected)
	if setOK && multisetOK && len(overlap) == 0 {
		return nil
	}

	// Concise diagnostic: count missing/extra/duplicates without dumping huge sets to logs.
	gotSet := make(map[string]struct{}, len(got))
	for _, id := range got {
		gotSet[id] = struct{}{}
	}
	expSet := make(map[string]struct{}, len(expected))
	for _, id := range expected {
		expSet[id] = struct{}{}
	}
	missing := 0
	for id := range expSet {
		if _, ok := gotSet[id]; !ok {
			missing++
		}
	}
	extra := 0
	for id := range gotSet {
		if _, ok := expSet[id]; !ok {
			extra++
		}
	}
	duplicates := rawCount - len(got)
	p.logger.Error("publish coverage mismatch",
		"source_partitions", len(expected),
		"covered_unique", len(got),
		"covered_raw", rawCount,
		"parked", len(parked),
		"overlap", len(overlap),
		"missing", missing,
		"extra", extra,
		"duplicates", duplicates,
	)

	return fmt.Errorf("%w: source=%d covered_unique=%d covered_raw=%d parked=%d overlap=%d missing=%d extra=%d duplicates=%d",
		types.ErrCoverageMismatch, len(expected), len(got), rawCount, len(parked), len(overlap), missing, extra, duplicates)
}

// checkLeadership implements steps 5 and 7: invoke the live leader-check
// callback and abort if it returns a non-nil error. When leaderCheckFn is
// nil (test-only), the check is a no-op — production callers MUST set it.
//
// The caller selects the sentinel (pre-alias vs post-alias) so callers and
// tests can distinguish the two phases. The callback's underlying error
// (typically wrapping types.ErrLeadershipRevisionMismatch) is preserved in
// the wrapped chain.
func (p *AssignmentPublisher) checkLeadership(ctx context.Context, claimed uint64, sentinel error) error {
	if p.leaderCheckFn == nil {
		return nil
	}
	if err := p.leaderCheckFn(ctx, claimed); err != nil {
		return fmt.Errorf("%w: claimed=%d: %w", sentinel, claimed, err)
	}
	return nil
}

// writePayloads implements step 4 of §3.5. Returns:
//   - refs: AssignmentPayloadRef map, ready to embed in the commit
//   - payloadByWorker: the canonical-form AssignmentPayloads, kept for legacy alias use
//   - err: any non-recoverable KV or hash-mismatch error
func (p *AssignmentPublisher) writePayloads(
	ctx context.Context,
	workers []string,
	assignments map[string][]types.Partition,
	labels map[string][]string,
) (map[string]types.AssignmentPayloadRef, map[string]types.AssignmentPayload, error) {
	refs := make(map[string]types.AssignmentPayloadRef, len(workers))
	payloads := make(map[string]types.AssignmentPayload, len(workers))
	// Iterate in sorted worker order for log determinism — KV order is irrelevant.
	sorted := make([]string, len(workers))
	copy(sorted, workers)
	slices.Sort(sorted)
	for _, w := range sorted {
		parts := assignments[w]
		// Canonicalize: sort partitions by CanonicalID so identical sets hash
		// to the same key regardless of strategy iteration order.
		canonical := make([]types.Partition, len(parts))
		copy(canonical, parts)
		slices.SortFunc(canonical, func(a, b types.Partition) int {
			return strings.Compare(a.CanonicalID(), b.CanonicalID())
		})
		payload := types.AssignmentPayload{
			SchemaVersion: types.AssignmentSchemaVersion,
			Partitions:    canonical,
			// Labels feed the same content-addressed sha256 as the partitions,
			// so stamp a SORTED copy — identical label sets must hash
			// identically regardless of caller order, exactly like the
			// CanonicalID sort above. Never mutates the caller's slice.
			WorkerLabels: sortedLabelsCopy(labels[w]),
			// WorkerLabelsKnown is UNCONDITIONAL: a label-aware leader always
			// records presence, including for unlabeled workers (spec §4.3).
			// The presence bit distinguishes "computed for an unlabeled worker"
			// (true + empty) from "computed by a pre-label leader" (false).
			// Mirrors the SourceRevisionKnown precedent.
			WorkerLabelsKnown: true,
		}
		canonicalBytes, err := json.Marshal(payload)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal payload worker=%s: %w", w, err)
		}
		hash := sha256.Sum256(canonicalBytes)
		hashHex := hex.EncodeToString(hash[:])
		key := p.keyPrefix + payloadKeyPrefix + hashHex

		// Compress for storage. We hash the UNCOMPRESSED canonical bytes
		// (above) so verify-on-ErrKeyExists decompresses then re-hashes. Two
		// independent leaders writing the same logical payload may produce
		// slightly different gzip headers (mtime/OS); that's fine — the hash
		// is over the inner canonical bytes.
		stored, err := gzipCompress(canonicalBytes)
		if err != nil {
			return nil, nil, fmt.Errorf("gzip payload worker=%s: %w", w, err)
		}

		setDigest := computeSetDigest(canonical)

		rev, err := p.assignmentKV.Create(ctx, key, stored)
		switch {
		case err == nil:
			p.metrics.IncrementPayloadsCreated()
			p.metrics.ObservePayloadBytesWritten(len(stored))
		case errors.Is(err, jetstream.ErrKeyExists):
			// Verify-back: fetch the existing bytes, decompress, sha256, and
			// compare with our hash. A mismatch is either a sha256 collision
			// (extraordinary) or KV corruption.
			entry, gerr := p.assignmentKV.Get(ctx, key)
			if gerr != nil {
				return nil, nil, fmt.Errorf("verify-back get worker=%s key=%s: %w", w, key, gerr)
			}
			plain, derr := gzipDecompress(entry.Value())
			if derr != nil {
				// Tolerate the case where some operator wrote uncompressed bytes
				// (unlikely in production, but be defensive): treat the raw
				// stored bytes as canonical and re-hash directly.
				plain = entry.Value()
			}
			gotHash := sha256.Sum256(plain)
			if !bytes.Equal(gotHash[:], hash[:]) {
				return nil, nil, fmt.Errorf("%w: key=%s expected=%s got=%s",
					types.ErrPayloadHashCollisionOrCorruption, key, hashHex, hex.EncodeToString(gotHash[:]))
			}
			p.metrics.IncrementPayloadsReused()
			rev = entry.Revision()
		default:
			return nil, nil, fmt.Errorf("kv create payload worker=%s key=%s: %w", w, key, err)
		}

		refs[w] = types.AssignmentPayloadRef{
			Key:         key,
			PayloadHash: hashHex,
			SetDigest:   setDigest,
			Revision:    rev,
		}
		payloads[w] = payload
	}

	return refs, payloads, nil
}

// classifyLegacyWorkers reads each worker's heartbeat and returns the subset
// that lack CapAckV1. A read failure or a malformed heartbeat is also
// classified as legacy: we err on the safe side (treat unknown as legacy)
// because writing an extra legacy alias for a commit-capable worker is harmless
// (it goes into the compat-noise step 11 anyway), whereas SKIPPING the alias
// for a true legacy worker silently orphans its V partitions.
func (p *AssignmentPublisher) classifyLegacyWorkers(ctx context.Context, workers []string) []string {
	if p.heartbeatKV == nil {
		// No heartbeat KV wired — we can't classify; safest behavior is to
		// treat NO worker as legacy (i.e. trust commit-only). This is the
		// test-only fallback; production callers MUST wire HeartbeatKV.
		return nil
	}
	out := make([]string, 0)
	for _, w := range workers {
		entry, err := p.heartbeatKV.Get(ctx, p.hbKeyPrefix+w)
		if err != nil {
			// Heartbeat missing or unreadable → treat as legacy (safe).
			out = append(out, w)
			continue
		}
		hb, derr := types.DecodeHeartbeat(entry.Value())
		if derr != nil {
			out = append(out, w)
			continue
		}
		if hb.Capabilities&types.CapAckV1 == 0 {
			out = append(out, w)
		}
	}

	return out
}

// runAliasBarrier executes the heartbeat-aware mandatory legacy-alias writes
// of publish step 6 with bounded retries.
//
// Returns:
//   - aliasWritten: true if AT LEAST ONE legacy alias write succeeded. The
//     caller uses this to gate the alias_visible_uncommitted exposure metric
//     so a first-attempt failure (no alias landed) does not count as exposure.
//   - err: a wrapped types.ErrAliasBarrierFailed when retries for any legacy
//     worker are exhausted; nil when all (or none, if the batch is fully
//     commit-capable) succeed.
func (p *AssignmentPublisher) runAliasBarrier(
	ctx context.Context,
	in PublishInput,
	legacyInBatch []string,
	payloadByWorker map[string]types.AssignmentPayload,
	proposedVersion int64,
) (aliasWritten bool, err error) {
	for _, w := range legacyInBatch {
		payload, ok := payloadByWorker[w]
		if !ok {
			// Worker is in the batch but has no assignment slice — degenerate;
			// we still write an empty payload alias so old workers see the
			// "you're revoked" signal at version V.
			// WorkerLabelsKnown stays true: a label-aware leader always records
			// presence, even on this (currently unreachable) revoke fallback —
			// future routing changes must not emit Known=false aliases.
			payload = types.AssignmentPayload{SchemaVersion: types.AssignmentSchemaVersion, WorkerLabelsKnown: true}
		}
		legacy := buildLegacyAlias(payload, proposedVersion, in.LeaderRevision, in.SourceRevision, in.Lifecycle, len(in.Workers))
		if werr := p.writeLegacyAliasWithRetry(ctx, w, legacy); werr != nil {
			p.metrics.IncrementAliasBarrierFailed()
			p.metrics.IncrementBatchAborted("alias_barrier_failed")
			// Only count exposure when at least one earlier alias actually
			// landed; otherwise no legacy worker can observe an uncommitted
			// view (P2 fix).
			if aliasWritten {
				p.metrics.IncrementAliasVisibleUncommitted()
			}

			return aliasWritten, fmt.Errorf("%w: worker=%s: %w", types.ErrAliasBarrierFailed, w, werr)
		}
		aliasWritten = true
	}

	return aliasWritten, nil
}

// writeLegacyAliasWithRetry implements the bounded-retry-with-jitter alias
// barrier write of step 6.
func (p *AssignmentPublisher) writeLegacyAliasWithRetry(
	ctx context.Context,
	workerID string,
	legacy types.Assignment,
) error {
	var lastErr error
	for attempt := range aliasBarrierMaxAttempts {
		// Re-check ctx between retries so a cancelled batch aborts promptly.
		if err := ctx.Err(); err != nil {
			return err
		}
		err := p.writeLegacyAliasOnce(ctx, workerID, legacy)
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt == aliasBarrierMaxAttempts-1 {
			break
		}
		// Backoff: base * 2^attempt + jitter ∈ [0, aliasBarrierMaxJitter).
		backoff := aliasBarrierBaseBackoff << attempt
		jitter := jitterDuration(aliasBarrierMaxJitter)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff + jitter):
		}
	}

	return lastErr
}

// writeLegacyAliasOnce performs a single kv.Put for the legacy alias key.
func (p *AssignmentPublisher) writeLegacyAliasOnce(ctx context.Context, workerID string, legacy types.Assignment) error {
	data, err := json.Marshal(legacy)
	if err != nil {
		return fmt.Errorf("marshal legacy alias worker=%s: %w", workerID, err)
	}
	key := p.keyPrefix + workerID
	if _, err := p.assignmentKV.Put(ctx, key, data); err != nil {
		return fmt.Errorf("kv put legacy alias key=%s: %w", key, err)
	}

	return nil
}

// writeCommitLog implements step 10 (best-effort).
func (p *AssignmentPublisher) writeCommitLog(
	ctx context.Context,
	commit types.AssignmentCommit,
	refs map[string]types.AssignmentPayloadRef,
) error {
	keys := make([]string, 0, len(refs))
	for _, ref := range refs {
		keys = append(keys, ref.Key)
	}
	slices.Sort(keys)
	// Deduplicate (cross-commit reuse can produce identical payload keys for
	// multiple workers).
	keys = slices.Compact(keys)
	logEntry := types.AssignmentCommitLog{
		Version:        commit.Version,
		LeaderRevision: commit.LeaderRevision,
		PublishedAt:    commit.PublishedAt,
		PayloadKeys:    keys,
	}
	data, err := json.Marshal(logEntry)
	if err != nil {
		return fmt.Errorf("marshal commit log: %w", err)
	}
	key := fmt.Sprintf("%s%s%d", p.keyPrefix, commitLogPrefix, commit.Version)
	if _, err := p.assignmentKV.Create(ctx, key, data); err != nil {
		return fmt.Errorf("kv create commit log key=%s: %w", key, err)
	}

	return nil
}

// cleanupStaleAssignmentsLocked deletes legacy aliases for workers that have
// left the cluster. Callers must already hold p.mu. Protocol keys (those whose
// sub-component starts with "_") are filtered out — never delete _commit /
// _commit_log.* / _payload.* via this path.
func (p *AssignmentPublisher) cleanupStaleAssignmentsLocked(ctx context.Context, workersToRemove []string) {
	for _, workerID := range workersToRemove {
		if isProtocolSubcomponent(workerID) {
			// Defensive: a protocol marker should never appear here, but if it
			// does, refuse to act.
			p.logger.Warn("refusing to delete protocol-key during stale-alias sweep", "worker_id", workerID)
			continue
		}
		key := p.keyPrefix + workerID
		if err := kvutil.DeleteKey(ctx, p.assignmentKV, key); err != nil {
			p.logger.Warn("failed to delete stale legacy alias", "key", key, "error", err)
		}
	}
}

// DiscoverHighestVersion scans the assignment KV for the highest legacy
// assignment version and returns the set of legacy worker IDs that still have
// alias keys.
//
// Protocol keys (any sub-component starting with "_": _commit, _commit_log.*,
// _payload.*) are filtered out and never interpreted as worker IDs. This is
// the bootstrap path used by §3.7 "new-leader recovery" before the first
// commit lands; once a commit exists, the publisher seeds currentVersion
// from it instead.
//
// Returns:
//   - workerIDs: sorted list of legacy worker IDs found
//   - error: KV access failure (non-fatal in many bootstrap paths)
func (p *AssignmentPublisher) DiscoverHighestVersion(ctx context.Context) ([]string, error) {
	keys, err := kvutil.ListKeys(ctx, p.assignmentKV, p.keyPrefix, false)
	if err != nil {
		return nil, fmt.Errorf("failed to list KV keys: %w", err)
	}

	// First, seed lastCommitRev from the commit key if it exists. This handles
	// the case where a prior leader already committed; takeover should
	// continue the CAS chain instead of attempting Create and losing.
	commitSeeded := false
	commitKey := p.keyPrefix + commitKeyName
	if entry, gerr := p.assignmentKV.Get(ctx, commitKey); gerr == nil {
		var commit types.AssignmentCommit
		if jerr := json.Unmarshal(entry.Value(), &commit); jerr == nil {
			p.mu.Lock()
			p.lastCommitRev = entry.Revision()
			if commit.Version > p.currentVersion {
				p.currentVersion = commit.Version
			}
			p.mu.Unlock()
			commitSeeded = true
		}
	}

	highestVersion := int64(0)
	workerIDs := make([]string, 0, len(keys))
	for _, key := range keys {
		sub := strings.TrimPrefix(key, p.keyPrefix)
		if isProtocolSubcomponent(sub) {
			continue
		}
		if commitSeeded {
			// currentVersion is already pinned by the commit (commit.Version
			// is monotonically ≥ any legacy alias version, since every
			// post-commit assignment lands via Publish→commit). Skip the
			// per-key Get; we only need the worker-ID set for callers
			// driving legacy cleanup.
			workerIDs = append(workerIDs, sub)
			continue
		}
		asgn, _, err := kvutil.GetJSON[types.Assignment](ctx, p.assignmentKV, key)
		if err != nil || asgn == nil {
			continue
		}
		if asgn.Version > highestVersion {
			highestVersion = asgn.Version
		}
		workerIDs = append(workerIDs, sub)
	}

	p.mu.Lock()
	if highestVersion > p.currentVersion {
		p.currentVersion = highestVersion
	}
	p.mu.Unlock()

	slices.Sort(workerIDs)
	if highestVersion > 0 || len(workerIDs) > 0 {
		p.logger.Info("discovered legacy assignments",
			"highest_version", highestVersion,
			"legacy_worker_keys", len(workerIDs),
		)
	}

	return workerIDs, nil
}

// CleanupAllAssignments removes ALL legacy per-worker assignment aliases
// from KV, including aliases for workers that may still be active in the
// cluster.
//
// Protocol keys (assignment._commit, assignment._commit_log.*,
// assignment._payload.*) are NEVER deleted by this method — they are the
// authoritative cluster state, and a successor leader needs them to continue
// the CAS chain and to GC payloads safely.
//
// Intended use: admin tooling / whole-cluster teardown where every parti
// instance is being decommissioned together. NOT safe to call on
// leader-step-down or single-node shutdown: it will yank aliases for
// workers that are still serving traffic on peer leaders, which would
// then have to wait for the next rebalance to be re-published. There is
// currently no production caller; the method exists for tests and
// operator scripts.
//
// A leader-step-down-safe variant would need a separate
// CleanupInactiveAssignments(activeWorkers []string) method that
// preserves aliases of currently-live workers — out of scope here.
func (p *AssignmentPublisher) CleanupAllAssignments(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.logger.Info("cleaning up legacy assignment aliases (protocol keys preserved)")

	keys, err := p.assignmentKV.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) || types.IsNoKeysFoundError(err) {
			return nil
		}

		return fmt.Errorf("failed to list keys for full cleanup: %w", err)
	}

	workersToRemove := make([]string, 0, len(keys))
	for _, key := range keys {
		sub, ok := strings.CutPrefix(key, p.keyPrefix)
		if !ok {
			continue
		}
		if isProtocolSubcomponent(sub) {
			continue
		}
		workersToRemove = append(workersToRemove, sub)
	}

	p.cleanupStaleAssignmentsLocked(ctx, workersToRemove)

	return nil
}

// CurrentVersion returns the highest assignment version this publisher has
// observed (either via DiscoverHighestVersion or via a successful commit).
//
// This method is safe to call concurrently.
func (p *AssignmentPublisher) CurrentVersion() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.currentVersion
}

// LastCommitRev returns the KV revision of the most recently CAS-written
// assignment._commit. Used by the GC loop to identify the live commit.
//
// This method is safe to call concurrently.
func (p *AssignmentPublisher) LastCommitRev() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastCommitRev
}

// LastCommit returns a defensive copy of the most recently observed
// AssignmentCommit, populated either by a successful CAS or by
// BootstrapLastCommit.
//
// Returns nil when no commit has been observed yet (pre-first-commit
// bootstrap path against an empty assignment bucket).
//
// The returned struct is a deep copy: callers may iterate or mutate Workers
// and Payloads without affecting publisher state.
//
// Safe to call concurrently with Publish.
//
// Returns:
//   - *types.AssignmentCommit: Defensive copy, or nil if no commit observed.
func (p *AssignmentPublisher) LastCommit() *types.AssignmentCommit {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastCommit == nil {
		return nil
	}
	out := cloneAssignmentCommit(p.lastCommit)

	return &out
}

// LastCommitObservedAt returns the local monotonic-clock instant at which
// this publisher observed the current lastCommit. Returns the zero time
// when lastCommit is nil.
//
// The leader-side audit uses this for grace-window math instead of the
// wire-clock commit.PublishedAt so cross-leader wall-clock skew (e.g.,
// after takeover or container suspend/resume) cannot suppress or
// prematurely trigger audit_repair escalation.
//
// Callers use time.Since on the return value; subtracting wall-clock
// times against this value is meaningless and will silently lose the
// monotonic reading.
func (p *AssignmentPublisher) LastCommitObservedAt() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.lastCommitObservedAtMono
}

// BootstrapLastCommit seeds the publisher's in-memory lastCommit cache from
// the live "<prefix>._commit" KV entry, if one exists. Called once at
// calculator startup so the audit loop can run from t=0 without waiting for
// the publisher to issue its own commit.
//
// Best-effort: KV access failures, missing entries, and unmarshal errors all
// leave lastCommit nil and return nil. Errors are logged for ops visibility
// but do not propagate.
//
// Parameters:
//   - ctx: Context for the KV Get operation.
//
// Returns:
//   - error: Always nil; the bootstrap path is intentionally non-fatal.
func (p *AssignmentPublisher) BootstrapLastCommit(ctx context.Context) error {
	commitKey := p.keyPrefix + commitKeyName
	entry, err := p.assignmentKV.Get(ctx, commitKey)
	if err != nil {
		p.logger.Debug("bootstrap lastCommit: no commit entry yet", "key", commitKey, "error", err)
		return nil
	}
	var commit types.AssignmentCommit
	if jerr := json.Unmarshal(entry.Value(), &commit); jerr != nil {
		p.logger.Warn("bootstrap lastCommit: unmarshal failed", "key", commitKey, "error", jerr)
		return nil
	}
	p.mu.Lock()
	commitCopy := cloneAssignmentCommit(&commit)
	p.lastCommit = &commitCopy
	// Local monotonic observation; see Publish step 9 for the rationale.
	p.lastCommitObservedAtMono = time.Now()
	if entry.Revision() > p.lastCommitRev {
		p.lastCommitRev = entry.Revision()
	}
	if commit.Version > p.currentVersion {
		p.currentVersion = commit.Version
	}
	p.mu.Unlock()
	p.logger.Debug("bootstrap lastCommit seeded",
		"version", commit.Version,
		"leader_revision", commit.LeaderRevision,
		"workers", len(commit.Workers),
	)

	return nil
}

// refreshLastCommitRevAfterCASFailureLocked re-reads the live _commit KV
// revision and updates p.lastCommitRev so the next Publish can re-CAS.
//
// Gated on writeErr being jetstream.ErrKeyExists — the sentinel NATS
// JetStream returns for both Create (key already exists) and Update
// (revision mismatch). Both indicate another writer beat us; any other
// error class is a transient KV failure whose recovery is the existing
// retry-on-next-rebalance-event path, and refreshing on transient errors
// would skew lastCommitRev independently of currentVersion / lastCommit.
//
// Called from Publish's commit-write error branch — Publish already holds
// p.mu, so this method MUST NOT acquire it (BootstrapLastCommit cannot be
// reused here because it would deadlock on the same mutex).
//
// Best-effort: KV errors and missing entries leave lastCommitRev unchanged.
// The next CAS failure triggers another refresh attempt; a Calculator
// restart is the ultimate recovery path.
func (p *AssignmentPublisher) refreshLastCommitRevAfterCASFailureLocked(ctx context.Context, writeErr error) {
	if !errors.Is(writeErr, jetstream.ErrKeyExists) {
		return
	}
	commitKey := p.keyPrefix + commitKeyName
	entry, err := p.assignmentKV.Get(ctx, commitKey)
	if err != nil {
		p.logger.Debug("post-CAS-failure refresh: could not read live _commit",
			"key", commitKey, "error", err)
		return
	}
	if entry.Revision() > p.lastCommitRev {
		p.lastCommitRev = entry.Revision()
	}
}

// LastRebalanceTime returns the time of the last successful commit.
//
// This method is safe to call concurrently.
func (p *AssignmentPublisher) LastRebalanceTime() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastRebalance
}

// AssignmentKV returns the KV bucket the publisher writes into. Exposed so the
// GC loop and tests can list and inspect protocol keys without re-opening the
// bucket.
func (p *AssignmentPublisher) AssignmentKV() jetstream.KeyValue {
	return p.assignmentKV
}

// LiveRefs returns a snapshot of the payload keys this publisher has selected
// for an in-progress publish (after step 4 verify-back, before step 9 commit
// CAS returns). The GC consults this set to avoid deleting a payload that an
// in-flight commit is about to reference (§3.9 / P0-2).
//
// The returned slice is a point-in-time copy: callers may iterate without
// holding any lock. An empty slice is a normal steady-state value when no
// publish is in flight.
//
// This method is safe to call concurrently with Publish.
func (p *AssignmentPublisher) LiveRefs() []string {
	out := make([]string, 0)
	p.inflightRefs.Range(func(key, _ any) bool {
		k, ok := key.(string)
		if ok {
			out = append(out, k)
		}
		return true
	})

	return out
}

// Prefix returns the assignment key prefix. Exposed for the GC loop.
func (p *AssignmentPublisher) Prefix() string {
	return p.prefix
}

// ----- helpers (unexported) -----

// isProtocolSubcomponent returns true if a sub-component (the part after the
// "<prefix>." delimiter) belongs to the refs-always protocol namespace and
// must NOT be interpreted as a worker ID.
func isProtocolSubcomponent(sub string) bool {
	return strings.HasPrefix(sub, protocolKeyMarker)
}

// canonicalIDSet returns a sorted, deduplicated slice of CanonicalIDs.
func canonicalIDSet(parts []types.Partition) []string {
	if len(parts) == 0 {
		return nil
	}
	ids := make([]string, 0, len(parts))
	for _, p := range parts {
		ids = append(ids, p.CanonicalID())
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)

	return ids
}

// unionPartitions returns the concatenation of all assignment slices. The
// caller is expected to canonicalize via canonicalIDSet, which sorts and
// dedupes — so a duplicate across two workers will collapse and be detected
// by set-equality against the source.
func unionPartitions(assignments map[string][]types.Partition) []types.Partition {
	total := 0
	for _, parts := range assignments {
		total += len(parts)
	}
	out := make([]types.Partition, 0, total)
	for _, parts := range assignments {
		out = append(out, parts...)
	}

	return out
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// intersectSortedIDs returns the sorted intersection of two sorted, deduped
// string slices (as produced by canonicalIDSet). Returns nil when the inputs
// share no elements. Used by checkCoverage to detect assigned∩parked overlap.
func intersectSortedIDs(a, b []string) []string {
	var out []string
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}

	return out
}

// mergeSortedIDs returns the sorted, deduped union of two sorted, deduped
// string slices (as produced by canonicalIDSet). Used by checkCoverage to
// compare assigned∪parked against the source set.
func mergeSortedIDs(a, b []string) []string {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	out := make([]string, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case a[i] < b[j]:
			out = append(out, a[i])
			i++
		default:
			out = append(out, b[j])
			j++
		}
	}
	out = append(out, a[i:]...)
	out = append(out, b[j:]...)

	return out
}

// sortedLabelsCopy returns a sorted, de-duplicated copy of the given label set
// for payload stamping. Labels are part of the canonical payload bytes (and
// therefore the content-addressed PayloadHash), so ordering must be
// deterministic regardless of caller order — the "heartbeat labels arrive
// pre-sorted and deduped" invariant is an unenforced cross-package assumption,
// so this helper enforces it locally (sort + compact) to stay symmetric with
// normalizeWorkerLabels. Returns nil for empty input and never mutates the
// caller's slice.
func sortedLabelsCopy(labels []string) []string {
	if len(labels) == 0 {
		return nil
	}
	out := make([]string, len(labels))
	copy(out, labels)
	slices.Sort(out)

	return slices.Compact(out)
}

// computeSetDigest delegates to types.PartitionSetDigest so the publisher and
// the manager-side apply ack share a single source of truth for the
// SetDigest / AppliedDigest values. Kept as a thin local alias for readability
// at call sites.
func computeSetDigest(parts []types.Partition) uint64 {
	return types.PartitionSetDigest(parts)
}

// cloneAssignmentCommit returns a deep copy of the given commit. The
// Workers slice and Payloads map are independently allocated so callers can
// iterate / mutate the returned struct without affecting the source.
func cloneAssignmentCommit(c *types.AssignmentCommit) types.AssignmentCommit {
	out := *c
	if c.Workers != nil {
		out.Workers = make([]string, len(c.Workers))
		copy(out.Workers, c.Workers)
	}
	if c.Payloads != nil {
		out.Payloads = make(map[string]types.AssignmentPayloadRef, len(c.Payloads))
		maps.Copy(out.Payloads, c.Payloads)
	}

	return out
}

// computeBatchDigest is identical in structure to computeSetDigest but
// operates on the full batch (publisher-side coverage proof). Kept as a
// distinct function for readability at call sites.
func computeBatchDigest(parts []types.Partition) uint64 {
	return computeSetDigest(parts)
}

// gzipCompress compresses bytes with gzip.BestCompression. Determinism of the
// gzip header is NOT required for correctness — the verify-back path
// decompresses and re-hashes the inner canonical bytes — so we don't go out of
// our way to normalize the header.
func gzipCompress(in []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(in); err != nil {
		_ = w.Close()
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// gzipDecompress decompresses gzip-encoded bytes. Returns an error if the
// input is not valid gzip.
func gzipDecompress(in []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(in))
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	var out bytes.Buffer
	if _, err := out.ReadFrom(r); err != nil {
		return nil, err
	}

	return out.Bytes(), nil
}

// jitterDuration returns a random duration in [0, max). Uses crypto/rand for
// jitter so chaotic test environments don't get correlated retries.
func jitterDuration(maxJitter time.Duration) time.Duration {
	if maxJitter <= 0 {
		return 0
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0
	}
	n := binary.LittleEndian.Uint64(b[:])
	// maxJitter is a positive int64 (time.Duration) by the guard above; the
	// uint64 conversion is safe and the modulo result is < uint64(maxJitter)
	// which fits comfortably in int64. gosec G115 only warns on the cast back
	// to int64 — bound it explicitly first.
	bounded := n % uint64(maxJitter)

	return time.Duration(bounded) //nolint:gosec // G115: bounded < uint64(maxJitter), and maxJitter is int64 by type.
}

// buildLegacyAlias constructs the wire-compatible types.Assignment that gets
// written to "<prefix>.<W>" for legacy (and compat-noise) aliases.
//
// The legacy format mirrors what pre-refs-always workers expect: a single
// JSON envelope carrying the partition list plus version/leader/source
// metadata. New workers under §3.6 may also read this alias as a fallback
// when no commit exists at a higher LeaderRevision (Phase 4).
// SourceRevision intentionally unused on the legacy envelope: it is not part
// of the pre-refs-always wire contract. New workers reading the alias under
// §3.6 fall back to commit.SourceRevision when they later observe a commit
// with a fresher LeaderRevision.
func buildLegacyAlias(
	payload types.AssignmentPayload,
	version int64,
	leaderRevision uint64,
	_ /* sourceRevision */ uint64,
	lifecycle string,
	totalWorkers int,
) types.Assignment {
	return types.Assignment{
		Version:           version,
		Lifecycle:         lifecycle,
		Partitions:        payload.Partitions,
		LeaderRevision:    leaderRevision,
		TotalWorkers:      totalWorkers,
		WorkerLabels:      payload.WorkerLabels,
		WorkerLabelsKnown: payload.WorkerLabelsKnown,
	}
}
