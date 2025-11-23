package testutil

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/arloliu/parti"
	"github.com/nats-io/nats.go/jetstream"
)

// HandoffClaimCollector subscribes to all handoff claim changes using a KV WatchAll
// and records decoded claims for deterministic assertions without sleeps.
//
// It is designed for integration tests to verify transition sequences:
//
//	stable -> prepare -> commit -> stable
//
// across one or more partitions.
//
// Usage:
//
//	collector, _ := NewHandoffClaimCollector(ctx, js, bucket)
//	defer collector.Stop()
//	collector.WaitForAllStable(ctx, expectedIDs, 2*time.Second)
//	seq := collector.Sequence(partitionID)
//
// The collector ignores non-claim keys and skips malformed entries.
type HandoffClaimCollector struct {
	mu        sync.RWMutex
	sequences map[string][]parti.HandoffClaimState
	latest    map[string]parti.HandoffClaim
	watcher   jetstream.KeyWatcher
	js        jetstream.JetStream
	bucket    string
}

// NewHandoffClaimCollector starts a WatchAll on the given bucket.
func NewHandoffClaimCollector(ctx context.Context, js jetstream.JetStream, bucket string) (*HandoffClaimCollector, error) {
	kv, err := js.KeyValue(ctx, bucket)
	if err != nil {
		return nil, err
	}
	watcher, err := kv.WatchAll(ctx, jetstream.UpdatesOnly())
	if err != nil {
		return nil, err
	}
	c := &HandoffClaimCollector{
		sequences: make(map[string][]parti.HandoffClaimState),
		latest:    make(map[string]parti.HandoffClaim),
		watcher:   watcher,
		js:        js,
		bucket:    bucket,
	}
	go c.run(ctx)

	return c, nil
}

func (c *HandoffClaimCollector) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-c.watcher.Updates():
			if e == nil { // skip initial marker
				continue
			}
			key := e.Key()
			if !startsWithClaims(key) { // filter "claims/" prefix
				continue
			}
			var decoded parti.HandoffClaim
			if err := jsonUnmarshal(e.Value(), &decoded); err != nil {
				continue
			}
			// Record state sequence
			pid := decoded.PartitionID
			c.mu.Lock()
			c.latest[pid] = decoded
			seq := c.sequences[pid]
			if len(seq) == 0 || seq[len(seq)-1] != decoded.State {
				c.sequences[pid] = append(seq, decoded.State)
			}
			c.mu.Unlock()
		}
	}
}

// WaitForAllStable waits until all provided partition IDs are observed in stable state.
// Returns true on success before deadline, false on timeout.
func (c *HandoffClaimCollector) WaitForAllStable(ctx context.Context, ids []string, deadline time.Duration) bool {
	end := time.Now().Add(deadline)
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	for time.Now().Before(end) {
		all := true
		c.mu.RLock()
		// snapshot of latest to minimize lock hold time
		latestCopy := make(map[string]parti.HandoffClaim, len(c.latest))
		for k, v := range c.latest {
			latestCopy[k] = v
		}
		for id := range set {
			claim, ok := latestCopy[id]
			if !ok {
				// fall back to on-demand inspect for missing keys
				c.mu.RUnlock()
				claims, err := parti.InspectHandoffClaims(ctx, c.js, c.bucket)
				if err != nil {
					return false
				}
				stateMap := make(map[string]parti.HandoffClaim, len(claims))
				for _, cl := range claims {
					stateMap[cl.PartitionID] = cl
				}
				cl, ok2 := stateMap[id]
				if !ok2 || cl.State != parti.HandoffClaimStable || cl.PendingOwner != "" {
					all = false
				}
				c.mu.RLock()
				if !all {
					break
				}

				continue
			}
			if claim.State != parti.HandoffClaimStable || claim.PendingOwner != "" {
				all = false
				break
			}
		}
		c.mu.RUnlock()
		if all {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}

	return false
}

// Sequence returns the unique state transition sequence for a partition.
func (c *HandoffClaimCollector) Sequence(id string) []parti.HandoffClaimState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	seq := c.sequences[id]
	out := make([]parti.HandoffClaimState, len(seq))
	copy(out, seq)

	return out
}

// WaitForStates waits until the specified partition has observed all the states in
// the provided set (order-independent), and returns true before the deadline.
func (c *HandoffClaimCollector) WaitForStates(ctx context.Context, id string, required []parti.HandoffClaimState, deadline time.Duration) bool {
	end := time.Now().Add(deadline)
	req := make(map[parti.HandoffClaimState]struct{}, len(required))
	for _, s := range required {
		req[s] = struct{}{}
	}
	for time.Now().Before(end) {
		seq := c.Sequence(id)
		seen := make(map[parti.HandoffClaimState]struct{}, len(seq))
		for _, s := range seq {
			seen[s] = struct{}{}
		}
		ok := true
		for s := range req {
			if _, has := seen[s]; !has {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}

	return false
}

// WaitForSequence waits until the specified partition observes the exact sequence
// of states (in order). Returns true on success before deadline, false on timeout.
// The observed sequence is de-duplicated (consecutive duplicates collapsed), so
// the expected argument should be on the unique-state path, e.g.,
//
//	[stable, prepare, commit, stable]
func (c *HandoffClaimCollector) WaitForSequence(ctx context.Context, id string, expected []parti.HandoffClaimState, deadline time.Duration) bool {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		seq := c.Sequence(id)
		if hasSubsequence(seq, expected) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}

	return false
}

// hasSubsequence returns true if exp appears as a contiguous subsequence of seq.
func hasSubsequence(seq, exp []parti.HandoffClaimState) bool {
	if len(exp) == 0 {
		return true
	}
	if len(seq) < len(exp) {
		return false
	}
	for i := 0; i <= len(seq)-len(exp); i++ {
		ok := true
		for j := 0; j < len(exp); j++ {
			if seq[i+j] != exp[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}

	return false
}

// Latest returns the latest claim for a partition, and ok if present.
func (c *HandoffClaimCollector) Latest(id string) (parti.HandoffClaim, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cl, ok := c.latest[id]

	return cl, ok
}

// Stop stops the watcher.
func (c *HandoffClaimCollector) Stop() {
	if c.watcher != nil {
		_ = c.watcher.Stop()
	}
}

// Internal helpers kept small to favor inlining.
func startsWithClaims(k string) bool { return len(k) >= 7 && k[:7] == "claims/" }

// Separate unmarshal to allow easy substitution in tests if needed.
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
