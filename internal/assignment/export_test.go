package assignment

import (
	"context"
	"time"
)

// PartitionRebalanceEntries returns the current value of the
// handlePartitionRebalance entry counter. Test-only signal for asserting the
// §2.2.3 / §10.3 "at most one extra cycle" duplicate-rebalance bound.
func PartitionRebalanceEntries(c *Calculator) int64 {
	return c.partitionRebalanceEntries.Load()
}

// SetPartitionRebalanceBlocker installs or clears the test-only blocking
// channel consulted by handlePartitionRebalance after the entries counter is
// incremented and before c.rebalance runs. Production callers MUST NOT use
// this.
func SetPartitionRebalanceBlocker(ch chan struct{}) {
	partitionRebalanceBlocker = ch
}

// SetPartitionRebalanceRequestTimeout overrides the request-context timeout
// allocated for the partition-lifecycle rebalance. Returns the previous value
// so the caller can restore it.
func SetPartitionRebalanceRequestTimeout(d time.Duration) time.Duration {
	prev := partitionRebalanceRequestTimeout
	partitionRebalanceRequestTimeout = d
	return prev
}

// SetPartitionTailCheckTimeout overrides the tail-check observeAndDecide
// timeout. Returns the previous value so the caller can restore it.
func SetPartitionTailCheckTimeout(d time.Duration) time.Duration {
	prev := partitionTailCheckTimeout
	partitionTailCheckTimeout = d
	return prev
}

// SetPartitionTailCheckEntryHook installs (or clears, when h is nil) the
// test-only hook that fires immediately before the tail-check observeAndDecide
// runs. The hook receives the fresh tailCtx and the rebalance reqCtx.Err()
// captured at that point so tests can prove §3.5's fresh-context invariant
// directly.
func SetPartitionTailCheckEntryHook(h func(tailCtx context.Context, reqCtxErr error)) {
	partitionTailCheckEntryHook = h
}
