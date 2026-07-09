// label_chaos.go implements the label_heartbeat_takeover chaos primitive:
// see coordinator.LabelHeartbeatTakeoverEvent's doc comment for the full
// rationale (why this replaces a kill+respawn approach).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/test/simulation/internal/coordinator"
	"github.com/arloliu/parti/v2/test/simulation/internal/worker"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
)

// stringSliceFromParam coerces a chaos param value into []string. YAML
// nested lists inside a Params map[string]any decode to []any (each
// element itself an any wrapping a string), not []string directly — this
// mirrors the durationFromParams coercion-helper pattern used elsewhere in
// this package for the same params-map YAML-decoding quirk.
func stringSliceFromParam(v any) []string {
	switch vv := v.(type) {
	case []string:
		return vv
	case []any:
		out := make([]string, 0, len(vv))
		for _, e := range vv {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}

		return out
	default:
		return nil
	}
}

// handleLabelHeartbeatTakeover resolves targetWorker (sim ID, e.g.
// "worker-2"; "" or "random" picks the first live labeled worker) to its
// current heartbeat KV entry, decodes it, replaces Labels with newLabels,
// refreshes Timestamp to now, and Puts it back onto the SAME key
// (heartbeat prefix is hardcoded "heartbeat" — see manager_assignment.go's
// HeartbeatPrefix: "heartbeat" — bucket is
// parti.DefaultConfig().KVBuckets.HeartbeatBucket, default
// "parti-heartbeat"). The target worker process is never touched — it
// keeps running and remains unaware its own heartbeat key was rewritten.
func handleLabelHeartbeatTakeover(ctx context.Context, registry *coordinator.GoroutineRegistry, targetWorker string, newLabels []string) {
	js, err := freshJS()
	if err != nil {
		log.Printf("[Chaos] label_heartbeat_takeover: failed to open probe JS: %v", err)
		return
	}

	pc := parti.DefaultConfig()
	bucket := pc.KVBuckets.HeartbeatBucket
	const heartbeatPrefix = "heartbeat" // matches manager_assignment.go's hardcoded HeartbeatPrefix

	workers := registry.GetByType(coordinator.WorkerGoroutine)
	if len(workers) == 0 {
		log.Println("[Chaos] label_heartbeat_takeover: no live workers")
		return
	}
	var simID, stableID string
	if targetWorker == "" || targetWorker == "random" {
		for _, info := range workers {
			if wobj, ok := info.Obj.(*worker.Worker); ok {
				if id := wobj.StableWorkerID(); id != "" {
					simID, stableID = info.ID, id
					break
				}
			}
		}
	} else {
		for _, info := range workers {
			if info.ID != targetWorker {
				continue
			}
			if wobj, ok := info.Obj.(*worker.Worker); ok {
				stableID = wobj.StableWorkerID()
			}
			simID = info.ID

			break
		}
	}
	if stableID == "" {
		log.Printf("[Chaos] label_heartbeat_takeover: could not resolve stable worker ID (targetWorker=%q)", targetWorker)
		return
	}

	kv, kerr := js.KeyValue(ctx, bucket)
	if kerr != nil {
		log.Printf("[Chaos] label_heartbeat_takeover: UNEXPECTED KeyValue error: %v", kerr)
		return
	}

	key := heartbeatPrefix + "." + stableID
	getCtx, getCancel := context.WithTimeout(ctx, 5*time.Second)
	entry, gerr := kv.Get(getCtx, key)
	getCancel()
	if gerr != nil {
		log.Printf("[Chaos] label_heartbeat_takeover: heartbeat key %q not found (worker not yet published?): %v", key, gerr)
		return
	}

	hb, derr := types.DecodeHeartbeat(entry.Value())
	if derr != nil {
		log.Printf("[Chaos] label_heartbeat_takeover: failed to decode existing heartbeat at %q: %v", key, derr)
		return
	}
	hb.Labels = newLabels
	hb.Timestamp = time.Now()

	newValue, merr := json.Marshal(hb)
	if merr != nil {
		log.Printf("[Chaos] label_heartbeat_takeover: failed to encode new heartbeat: %v", merr)
		return
	}

	putCtx, putCancel := context.WithTimeout(ctx, 5*time.Second)
	defer putCancel()
	// Plain Put (not Update-with-revision): this must look exactly like a
	// normal live heartbeat refresh to the watcher, which is the point —
	// checkLabelChange doesn't care how the PUT arrived, only that the
	// decoded Labels differ from its retained fingerprint for this key.
	if _, perr := kv.Put(putCtx, key, newValue); perr != nil {
		if errors.Is(perr, nats.ErrNoResponders) {
			log.Printf("[Chaos] label_heartbeat_takeover: ErrNoResponders (tolerated): %v", perr)
			return
		}
		log.Printf("[Chaos] label_heartbeat_takeover: UNEXPECTED Put error: %v", perr)
		return
	}
	log.Printf("[Chaos] label_heartbeat_takeover: rewrote heartbeat key %q (sim_id=%s stable_id=%s) with labels=%v",
		key, simID, stableID, newLabels)

	// Tell the LabelAffinityOracle about the tamper: it reads labels via
	// WorkerObserver.WorkerLabels() (the worker's own Manager/Config), which
	// this out-of-band KV rewrite never touches, so without this the oracle
	// would judge affinity by a label set that's now stale relative to what
	// production actually reads from KV. See SetLiveLabelOverride's doc
	// comment for the full rationale.
	if aioCoord != nil {
		if o := aioCoord.LabelAffinityOracle(); o != nil {
			o.SetLiveLabelOverride(simID, newLabels)
		}
	}
}
