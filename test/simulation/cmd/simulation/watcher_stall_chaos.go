// Phase 2 chaos primitive: watcher_stall.
//
// Stalls the in-process KV watcher on the parti-sim-source bucket WITHOUT
// dropping the NATS connection. Each NATS KV watch attaches an ephemeral
// JetStream consumer to the KV_<bucket> stream; deleting those consumers
// causes the watcher's Next() / Updates() channel to error, which forces
// the source's bounded restart envelope (watcherMaxAttempts = 6, backoff
// 2s..30s) into action. If the stall duration outlives that envelope, the
// reconciler becomes the sole recovery path — the empirical invariant from
// project memory.
//
// Mechanism trade-offs considered (per the plan's risk note):
//   - NetworkControl client-side blackhole: the existing dialer is
//     connection-level, not subject-level. nats.go's custom dialer cannot
//     filter by subject once the TCP socket is up.
//   - Embedded-server permission injection: doable but couples the
//     primitive to the embedded-server runtime and forces a config
//     reload.
//   - Ephemeral-consumer deletion (CHOSEN): pure JetStream admin API,
//     drops NO core NATS traffic, surgically targets only the source
//     stream's watchers. Documented limitation: stalls ALL workers'
//     watchers simultaneously (one stream, N watcher consumers, all
//     killed). The `target_worker` param is recorded but not honored —
//     marked as follow-up.

package main

import (
	"context"
	"log"
	"strings"
	"time"
)

// handleWatcherStall stalls watchers on the KV stream backing the
// partition-source bucket for the given duration.
//
// Parameters:
//   - ctx: simulation root context (used as parent for the stall goroutine)
//   - subjectPrefix: e.g. "$KV.parti-sim-source.>" — used only to derive
//     the backing stream name "KV_<bucket>".
//   - duration: how long to keep deleting consumers. Should outlive the
//     watcher's natural rebind envelope (watcherBaseBackoff=2s ×
//     watcherMaxAttempts=6, capped at watcherMaxBackoff=30s, ~30s worst
//     case) so the reconciler is the only recovery path.
func handleWatcherStall(ctx context.Context, subjectPrefix string, duration time.Duration) {
	bucket := bucketFromSubjectPrefix(subjectPrefix)
	if bucket == "" {
		log.Printf("[Chaos] watcher_stall: cannot derive bucket from subject_prefix=%q", subjectPrefix)
		return
	}
	streamName := "KV_" + bucket

	js, err := freshJS()
	if err != nil {
		log.Printf("[Chaos] watcher_stall: failed to open probe JS: %v", err)
		return
	}

	log.Printf("[Chaos] watcher_stall: stalling watchers on stream %q for %v", streamName, duration)

	// Run the consumer-deletion loop in a goroutine so the chaos handler
	// returns promptly. Cadence: delete every 1s. The watcher's restart
	// backoff is 2s..30s, so a 1s sweep guarantees any newly-rebound
	// watcher consumer is killed before it can drain a single update.
	go func() {
		stallCtx, cancel := context.WithTimeout(ctx, duration)
		defer cancel()
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		deleteAll := func() {
			delCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
			defer c()
			stream, sErr := js.Stream(delCtx, streamName)
			if sErr != nil {
				// Tolerated when the source bucket has just been deleted
				// (composed scenarios). Logged at INFO; not a stall
				// failure.
				log.Printf("[Chaos] watcher_stall: Stream(%q) error (tolerated): %v", streamName, sErr)
				return
			}
			cnames := stream.ListConsumers(delCtx)
			deleted := 0
			for ci := range cnames.Info() {
				if ci == nil {
					continue
				}
				if dErr := stream.DeleteConsumer(delCtx, ci.Name); dErr != nil {
					// Race with the watcher's own restart loop is benign.
					log.Printf("[Chaos] watcher_stall: DeleteConsumer(%s) error (tolerated): %v", ci.Name, dErr)
					continue
				}
				deleted++
			}
			if err := cnames.Err(); err != nil {
				log.Printf("[Chaos] watcher_stall: ListConsumers iteration error: %v", err)
			}
			if deleted > 0 {
				log.Printf("[Chaos] watcher_stall: deleted %d ephemeral consumers from %q", deleted, streamName)
			}
		}

		deleteAll()
		for {
			select {
			case <-stallCtx.Done():
				log.Printf("[Chaos] watcher_stall: stall window elapsed on %q", streamName)
				return
			case <-ticker.C:
				deleteAll()
			}
		}
	}()
}

// bucketFromSubjectPrefix turns e.g. "$KV.parti-sim-source.>" into
// "parti-sim-source". Returns "" if the prefix does not match the
// expected `$KV.<bucket>.<...>` shape.
func bucketFromSubjectPrefix(prefix string) string {
	const kvPrefix = "$KV."
	if !strings.HasPrefix(prefix, kvPrefix) {
		return ""
	}
	rest := prefix[len(kvPrefix):]
	idx := strings.Index(rest, ".")
	if idx < 0 {
		return rest
	}

	return rest[:idx]
}
