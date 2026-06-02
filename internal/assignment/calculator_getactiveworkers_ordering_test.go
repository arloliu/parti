package assignment

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/stretchr/testify/require"
)

// TestGetActiveWorkers_EnumRecoverySignalsBeforeCredibilityGate locks the stage
// ORDER of the getActiveWorkers funnel: the enumeration-failure reset (which
// fires OnEnumerationSuccess) runs on a successful Keys scan BEFORE the F10-A
// suspicious-shrink credibility gate (calculator.go: resetEnumerationFailures at
// ~1233, the suspicious block at ~1246). So a scan that RECOVERS the enumeration
// stall but is ALSO sharply shrunk must still signal enumeration recovery, even
// though the credibility gate degrades it to the cached worker set.
//
// The observable signal is the OnEnumerationSuccess callback. A refactor that
// linearizes the funnel with the suspicious gate's early return ahead of the
// reset would swallow the recovery signal on a recovered-but-suspicious scan;
// this test catches that via the success counter.
func TestGetActiveWorkers_EnumRecoverySignalsBeforeCredibilityGate(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	const seedWorkers = 5
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "ordering-assign-"+t.Name())
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "ordering-hb-"+t.Name())
	for i := range seedWorkers {
		key := fmt.Sprintf("worker-hb.worker-%03d", i)
		_, err := heartbeatKV.Put(ctx, key, []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)
	}

	fault := &keysTimeoutKV{KeyValue: heartbeatKV}
	var enumErr, enumOK atomic.Int64
	calc, err := NewCalculator(&Config{
		AssignmentKV:                         assignmentKV,
		HeartbeatKV:                          fault,
		AssignmentPrefix:                     "assignment",
		Source:                               &mutableSource{partitions: makePartitions(seedWorkers)},
		Strategy:                             &mockStrategy{},
		HeartbeatPrefix:                      "worker-hb",
		HeartbeatTTL:                         5 * time.Second,
		EmergencyGracePeriod:                 1 * time.Second,
		ColdStartWindow:                      10 * time.Millisecond,
		PlannedScaleWindow:                   10 * time.Millisecond,
		Cooldown:                             0,
		EnumerationFailureThreshold:          2,
		WorkerShrinkConfirmationCount:        2,
		WorkerShrinkConfirmationThresholdPct: 50,
		OnEnumerationError:                   func(error) { enumErr.Add(1) },
		OnEnumerationSuccess:                 func() { enumOK.Add(1) },
	})
	require.NoError(t, err)

	// Seed rebalance (fault disarmed) establishes lastKnownWorkerCount=seedWorkers
	// and primes the worker cache, so a later shrink reads as suspicious.
	require.NoError(t, calc.rebalance(ctx, "test-seed"))
	require.Equal(t, seedWorkers, calc.lastKnownWorkerCount)

	// Sustained enumeration stall: EnumerationFailureThreshold consecutive Keys
	// timeouts cross the threshold and fire OnEnumerationError.
	fault.armed.Store(true)
	for range 2 {
		_, _, err = calc.getActiveWorkers(ctx)
		require.ErrorIs(t, err, context.DeadlineExceeded)
	}
	require.GreaterOrEqual(t, enumErr.Load(), int64(1), "sustained stall must fire OnEnumerationError")

	// Recover enumeration AND shrink the set in the same scan: Keys succeeds (stall
	// recovered) but observes 1 of 5 workers (a >50% shrink), so the credibility
	// gate marks it suspicious-and-unconfirmed and degrades to the cached set.
	fault.armed.Store(false)
	for i := 1; i < seedWorkers; i++ {
		require.NoError(t, heartbeatKV.Delete(ctx, fmt.Sprintf("worker-hb.worker-%03d", i)))
	}

	okBefore := enumOK.Load()
	workers, fresh, err := calc.getActiveWorkers(ctx)
	require.NoError(t, err, "the enumeration scan itself succeeded")
	require.False(t, fresh, "a suspicious shrink must degrade to the cached set (fresh=false)")
	require.Len(t, workers, seedWorkers, "the cached pre-shrink worker set is returned, not the shrunk view")

	require.Equal(t, okBefore+1, enumOK.Load(),
		"OnEnumerationSuccess must fire on the recovered-but-suspicious scan: the enumeration "+
			"reset runs BEFORE the credibility gate's early return")
}
