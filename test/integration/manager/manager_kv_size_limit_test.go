package manager_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestKVSizeLimit_AssignmentPublishFails reproduces the user-reported symptom
// where manually-created KV buckets with MaxValueSize=512KiB cause pods to
// hang on restart, while parti-auto-created buckets (unlimited) work.
//
// Hypothesis: with many partitions, the leader's per-worker Assignment JSON
// exceeds MaxValueSize. publisher.Publish fails at kv.Put; rebalance returns
// an error; calc.Start returns an error; the new leader ends up with no
// calculator and followers hang on waitForAssignment.
//
// To reproduce we pre-create the parti-assignment bucket with a tight
// MaxValueSize, then attempt to start a cluster. If the hypothesis is correct
// we should see leader Start fail (cold start) or takeover hang (restart).
func TestKVSizeLimit_AssignmentPublishFails(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Pre-create the assignment bucket with a tight MaxValueSize so publishes
	// exceeding it will fail. Mirrors the user's manual-bucket setup.
	const tightMaxValueSize = 4 * 1024 // 4 KiB — deliberately small
	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:       "parti-assignment",
		History:      1,
		MaxValueSize: tightMaxValueSize,
	})
	require.NoError(t, err)

	cfg := testutil.IntegrationTestConfig()

	// Build a partition list large enough that a single worker's assignment
	// JSON (if all partitions are routed to that worker) exceeds 4 KiB.
	// Each partition has a Keys slice with one ~100-byte key, so about
	// ~140 bytes of JSON per partition. 200 partitions ≈ 28 KiB — far beyond
	// the 4 KiB bucket limit for a single-worker-gets-all cold_start_immediate.
	partitions := make([]types.Partition, 200)
	for i := range partitions {
		partitions[i] = types.Partition{
			Keys:   []string{fmt.Sprintf("p-%05d-%s", i, longKeySuffix())},
			Weight: 1,
		}
	}
	src := source.NewStatic(partitions)
	assignStrat := strategy.NewConsistentHash()

	mgr, err := parti.NewManager(&cfg, js, src, assignStrat)
	require.NoError(t, err)

	startCtx, startCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer startCancel()

	startErr := mgr.Start(startCtx)
	t.Logf("Start returned: %v", startErr)

	// Document the observed behavior. If Start returns a clear error that
	// mentions the publish failure, callers can diagnose. If Start hangs and
	// then times out, the operator sees "context deadline exceeded" with no
	// hint about the real cause — that's the user's production experience.
	if startErr != nil {
		t.Logf("symptom: Start failed with %v — operator sees this error at startup", startErr)
	} else {
		t.Logf("symptom: Start succeeded unexpectedly — either hypothesis is wrong or partitions weren't large enough")
	}

	// Clean up regardless.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	_ = mgr.Stop(stopCtx)

	// The test is deliberately assertion-light for now — it's a diagnostic
	// scaffold to confirm the failure mode. Once we know what error surface
	// we want, we can tighten it.
	require.Error(t, startErr, "hypothesis: tight MaxValueSize should cause Start to fail because publisher.Publish is rejected by NATS")
}

// longKeySuffix returns a ~100-char string so each partition's JSON is large
// enough to make 200 partitions' total serialized assignment exceed 4 KiB.
func longKeySuffix() string {
	return "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
}
