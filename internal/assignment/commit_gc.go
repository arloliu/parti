package assignment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// GC defaults per §3.9 of the partition assignment robustness plan.
const (
	// DefaultPayloadGCInterval is the default cadence at which a CommitGC
	// instance triggers a sweep when run via Start.
	DefaultPayloadGCInterval = 5 * time.Minute

	// DefaultPayloadGCRetention is the default time window during which a
	// payload key is retained even if it is unreferenced. Keys created less
	// than this duration ago are never deleted, providing forensic margin.
	DefaultPayloadGCRetention = 24 * time.Hour

	// DefaultPayloadGCKeepCommits is the default count of recent commit logs
	// scanned to compute the live payload set. Defaults to 10.
	DefaultPayloadGCKeepCommits = 10
)

// LiveRefsProvider is the contract the GC uses to consult the publisher's
// in-flight payload-ref set. The default implementation (AssignmentPublisher)
// returns the keys of every payload it has selected for an in-progress
// publish; tests can inject a fake provider to drive race scenarios
// deterministically.
type LiveRefsProvider interface {
	// LiveRefs returns a point-in-time snapshot of payload keys the publisher
	// has selected for an in-progress publish. The GC must NOT delete any
	// returned key during this sweep, even if it appears unreferenced and is
	// older than Retention.
	LiveRefs() []string
}

// CommitGCConfig configures a CommitGC instance.
//
// All fields are optional except Publisher, which provides the KV bucket and
// prefix. Zero-valued fields are filled in with the Default* constants.
type CommitGCConfig struct {
	// Publisher provides the KV bucket and key prefix to operate on. The GC
	// only ever reads protocol keys (assignment._commit, _commit_log.<V>,
	// _payload.<hash>) and deletes orphan _payload.<hash> keys.
	Publisher *AssignmentPublisher

	// LiveRefsProvider is consulted on every sweep to obtain the publisher's
	// in-flight payload-ref set, which the GC must never delete (P0-2 / §3.9).
	// When nil, defaults to the Publisher itself; tests may inject a fake.
	LiveRefsProvider LiveRefsProvider

	// Interval is the cadence at which Start triggers a sweep. Defaults to
	// DefaultPayloadGCInterval.
	Interval time.Duration

	// Retention is the minimum age a payload key must reach before it is
	// eligible for deletion (even if otherwise unreferenced). Defaults to
	// DefaultPayloadGCRetention.
	Retention time.Duration

	// KeepCommits is the number of recent commit_log.<V> entries scanned to
	// compute the live payload set. Defaults to DefaultPayloadGCKeepCommits.
	KeepCommits int

	// Logger / Metrics: optional. The publisher's logger/metrics are reused if
	// these are nil.
	Logger  types.Logger
	Metrics types.GCMetrics

	// Now is a clock injection for tests. Defaults to time.Now.
	Now func() time.Time
}

// CommitGC reaps orphan content-addressable payload keys.
//
// GC is conservative and never participates in correctness:
//   - A payload key is "live" if it appears in either the current
//     assignment._commit or any of the last KeepCommits commit_log entries.
//   - Live keys are never deleted.
//   - Non-live keys older than Retention are eligible for deletion. Failures
//     are non-fatal and surface via the IncrementPayloadDeleteErrors metric.
//
// CommitGC is safe to call concurrently with Publisher.Publish, including
// across processes: GC's delete is conditioned on the payload key's revision
// (jetstream.LastRevision) as observed immediately before the delete, and a
// publish adopting an existing key (Create → ErrKeyExists → verify-back)
// CAS-touches it to advance that same revision before treating the adoption
// as final (see createOrAdoptPayload in assignment_publisher.go). Whichever
// side's conditioned write reaches the server first wins deterministically:
// a GC delete that lands first makes the adopter's touch fail and retry
// (recreating the key), and an adopter's touch that lands first makes GC's
// delete fail and the key survives.
//
// Example:
//
//	gc := assignment.NewCommitGC(assignment.CommitGCConfig{
//	    Publisher: pub,
//	    Logger:    logger,
//	    Metrics:   metrics,
//	})
//	gc.Start(ctx) // runs in background
//	defer gc.Stop()
type CommitGC struct {
	cfg      CommitGCConfig
	pub      *AssignmentPublisher
	liveRefs LiveRefsProvider

	now func() time.Time

	// Lifecycle.
	mu        sync.Mutex
	stopCh    chan struct{}
	doneCh    chan struct{}
	triggerCh chan struct{}
	logger    types.Logger
}

// NewCommitGC constructs a CommitGC, applying defaults to optional fields.
//
// Returns nil if cfg.Publisher is nil — the publisher is the only required
// dependency.
func NewCommitGC(cfg CommitGCConfig) *CommitGC {
	if cfg.Publisher == nil {
		return nil
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultPayloadGCInterval
	}
	if cfg.Retention <= 0 {
		cfg.Retention = DefaultPayloadGCRetention
	}
	if cfg.KeepCommits <= 0 {
		cfg.KeepCommits = DefaultPayloadGCKeepCommits
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	logger := cfg.Logger
	if logger == nil {
		logger = cfg.Publisher.logger
	}
	if cfg.Metrics == nil {
		cfg.Metrics = cfg.Publisher.metrics
	}

	liveRefs := cfg.LiveRefsProvider
	if liveRefs == nil {
		liveRefs = cfg.Publisher
	}

	return &CommitGC{
		cfg:       cfg,
		pub:       cfg.Publisher,
		liveRefs:  liveRefs,
		now:       cfg.Now,
		logger:    logger,
		triggerCh: make(chan struct{}, 1),
	}
}

// Start launches the background GC loop. Call Stop to terminate it.
//
// Returns an error if already started.
func (g *CommitGC) Start(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopCh != nil {
		return errors.New("commit GC already started")
	}
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	g.stopCh = stopCh
	g.doneCh = doneCh
	// Pass the channels into the loop so it does not race the Stop()
	// goroutine clearing g.stopCh.
	go g.loop(ctx, stopCh, doneCh)

	return nil
}

// Stop terminates the background loop and waits for the in-flight pass (if
// any) to return. Safe to call multiple times.
func (g *CommitGC) Stop() {
	g.mu.Lock()
	stopCh := g.stopCh
	doneCh := g.doneCh
	g.stopCh = nil
	g.mu.Unlock()
	if stopCh == nil {
		return
	}
	close(stopCh)
	if doneCh != nil {
		<-doneCh
	}
}

// Trigger requests an immediate GC sweep. The notification is non-blocking and
// coalesces — a backlog of triggers collapses into a single sweep, so it is
// safe to call after every successful publish without throttling.
//
// No-op when the GC has not been Started.
func (g *CommitGC) Trigger() {
	// Non-blocking send into the buffered notify channel; if a wake is already
	// pending, drop this trigger.
	select {
	case g.triggerCh <- struct{}{}:
	default:
	}
}

// loop drives RunOnce on the configured interval and on Trigger notifications.
//
// stopCh and doneCh are captured at Start time so the loop never races a
// concurrent Stop() that clears g.stopCh.
func (g *CommitGC) loop(ctx context.Context, stopCh, doneCh chan struct{}) {
	defer close(doneCh)
	ticker := time.NewTicker(g.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stopCh:
			return
		case <-ticker.C:
			if err := g.RunOnce(ctx); err != nil {
				g.logger.Warn("commit GC sweep failed (non-fatal)", "error", err)
			}
		case <-g.triggerCh:
			if err := g.RunOnce(ctx); err != nil {
				g.logger.Warn("commit GC sweep failed (non-fatal)", "error", err)
			}
		}
	}
}

// RunOnce performs a single synchronous GC sweep and returns the number of
// keys deleted (and the count of delete errors observed). Tests use this to
// drive deterministic GC behavior.
//
// Failures during the sweep are NOT fatal: errors are logged and surfaced via
// the IncrementPayloadDeleteErrors metric, and RunOnce continues to the next
// candidate.
func (g *CommitGC) RunOnce(ctx context.Context) error {
	kv := g.pub.AssignmentKV()
	keyPrefix := g.pub.Prefix() + "."
	live, currentVersion, err := g.computeLiveSet(ctx, kv, keyPrefix)
	if err != nil {
		return fmt.Errorf("compute live set: %w", err)
	}

	// Fold in publisher-held in-flight refs LAST, after the commit/log walk,
	// so a publish that adopts an existing payload key via ErrKeyExists and
	// is about to CAS its commit cannot lose its referenced payload to GC
	// (P0-2 / §3.9). LiveRefs is a stable snapshot of the publisher's
	// inflightRefs sync.Map and is safe to read concurrently with publish.
	if g.liveRefs != nil {
		for _, k := range g.liveRefs.LiveRefs() {
			live[k] = struct{}{}
		}
	}

	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) || types.IsNoKeysFoundError(err) {
			return nil
		}

		return fmt.Errorf("list keys: %w", err)
	}

	payloadFullPrefix := keyPrefix + payloadKeyPrefix
	deletes := 0
	errs := 0
	now := g.now()
	for _, k := range keys {
		if !strings.HasPrefix(k, payloadFullPrefix) {
			continue
		}
		if _, isLive := live[k]; isLive {
			continue
		}
		// Age gate: never delete keys younger than Retention.
		entry, gerr := kv.Get(ctx, k)
		if gerr != nil {
			// Key may have been raced away; ignore.
			continue
		}
		if g.cfg.Retention > 0 && now.Sub(entry.Created()) < g.cfg.Retention {
			continue
		}
		// Re-check in-flight refs immediately before deleting: a
		// same-process publish that began AFTER the snapshot above and
		// adopted this key via ErrKeyExists can have registered its ref in
		// the publisher's set by now. This is a best-effort fast path only
		// — LiveRefs is process-local (invisible to a concurrent publisher
		// in another process) and even same-process the ref is registered
		// only after writePayloads returns, a window after the adopter's
		// touch below has already landed. The conditioned Delete below is
		// the actual correctness mechanism; this check just avoids the
		// round trip for the common same-process case.
		if g.liveRefs != nil {
			stillLive := slices.Contains(g.liveRefs.LiveRefs(), k)
			if stillLive {
				continue
			}
		}
		// Conditioned delete: only remove the key if its revision still
		// matches what we just observed above (entry.Revision()). This is
		// the fencing token that closes the payload-GC-vs-adoption race —
		// see createOrAdoptPayload's doc in assignment_publisher.go. If an
		// adopter CAS-touched this key (Create→ErrKeyExists→verify-back→
		// Update) anywhere between our Get above and this Delete, the
		// revision no longer matches and the delete is rejected instead of
		// silently destroying a payload a fresh commit now references. The
		// opposite ordering — a touch landing BEFORE our Get above — is
		// covered by the age gate, not by this condition: the touch writes a
		// fresh entry, so the Get sees Created within Retention and skips
		// the key entirely. Both halves assume a publish commits within
		// Retention of its payload write (the standing retention lease;
		// see createOrAdoptPayload's doc).
		if err := kv.Delete(ctx, k, jetstream.LastRevision(entry.Revision())); err != nil {
			switch {
			case errors.Is(err, jetstream.ErrKeyNotFound):
				// Key already gone — another GC pass (or a delete that
				// otherwise raced ahead of us) got there first.
				continue
			case errors.Is(err, jetstream.ErrKeyExists):
				// Revision-conditioned delete lost: nats.go maps a
				// wrong-last-sequence rejection to the same sentinel Create
				// uses for "key already exists" (both are
				// JSErrCodeStreamWrongLastSequence). It proves only that the
				// key's revision moved after our Get — most likely an
				// adopter's CAS-touch, though any concurrent write to the
				// key produces the same rejection. Either way the key is
				// live again; skip it. Kept distinguishable from the
				// already-deleted case above so debug logs can tell the two
				// loser classifications apart.
				g.logger.Debug("payload GC delete lost: revision moved after age-gate read", "key", k)
				continue
			default:
				errs++
				g.cfg.Metrics.IncrementPayloadDeleteErrors()
				g.logger.Warn("payload GC delete failed (non-fatal)", "key", k, "error", err)
				continue
			}
		}
		deletes++
	}
	if deletes > 0 || errs > 0 {
		g.logger.Info("payload GC sweep",
			"current_version", currentVersion,
			"live_keys", len(live),
			"deleted", deletes,
			"errors", errs,
		)
	}

	return nil
}

// computeLiveSet builds the set of payload keys that must be retained.
//
// The set is the union of:
//   - payload keys referenced by the current assignment._commit (always
//     included; defensive against an absent commit_log)
//   - payload keys referenced by the last KeepCommits assignment._commit_log.<V>
//     entries (consecutive starting from currentVersion - 0 down to
//     currentVersion - KeepCommits + 1; missing logs are skipped)
//
// Returns the live set and the current commit's Version (or 0 if no commit
// exists yet).
func (g *CommitGC) computeLiveSet(ctx context.Context, kv jetstream.KeyValue, keyPrefix string) (map[string]struct{}, int64, error) {
	live := make(map[string]struct{})

	currentVersion := int64(0)
	commitKey := keyPrefix + commitKeyName
	entry, err := kv.Get(ctx, commitKey)
	switch {
	case err == nil:
		var commit types.AssignmentCommit
		if jerr := json.Unmarshal(entry.Value(), &commit); jerr != nil {
			g.logger.Warn("commit GC: failed to decode commit", "error", jerr)
		} else {
			currentVersion = commit.Version
			for _, ref := range commit.Payloads {
				live[ref.Key] = struct{}{}
			}
		}
	case errors.Is(err, jetstream.ErrKeyNotFound):
		// No commit yet; nothing extra to retain. The commit-log walk below
		// also yields nothing (all log Get calls return ErrKeyNotFound).
	default:
		// Treat as transient; conservative behavior is to not delete anything.
		return nil, 0, fmt.Errorf("get commit key: %w", err)
	}

	// Walk the last KeepCommits commit_log entries.
	for i := 0; i < g.cfg.KeepCommits; i++ {
		v := currentVersion - int64(i)
		if v <= 0 {
			break
		}
		logKey := fmt.Sprintf("%s%s%d", keyPrefix, commitLogPrefix, v)
		entry, err := kv.Get(ctx, logKey)
		if err != nil {
			// Missing log → conservative behavior: we don't know which payloads
			// this version referenced, so retention WIDENS implicitly because
			// we still have the time-window guard. Skip and continue.
			continue
		}
		var logEntry types.AssignmentCommitLog
		if jerr := json.Unmarshal(entry.Value(), &logEntry); jerr != nil {
			g.logger.Warn("commit GC: failed to decode commit log", "version", v, "error", jerr)
			continue
		}
		for _, k := range logEntry.PayloadKeys {
			live[k] = struct{}{}
		}
	}

	return live, currentVersion, nil
}
