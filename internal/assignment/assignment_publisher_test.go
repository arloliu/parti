package assignment

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/arloliu/parti/internal/logging"
	"github.com/arloliu/parti/internal/metrics"
	partitest "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

func TestAssignmentPublisher_DiscoverHighestVersion(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-publisher-discover")

	publisher := NewAssignmentPublisher(
		assignmentKV,
		"assignment",
		logging.NewNop(),
		metrics.NewNop(),
	)

	ctx := context.Background()

	// Initially should be 0 - DiscoverHighestVersion handles empty KV gracefully
	err := publisher.DiscoverHighestVersion(ctx)
	// Empty KV may return "no keys found" error - this is acceptable
	if err != nil && !types.IsNoKeysFoundError(err) {
		require.NoError(t, err)
	}
	require.Equal(t, int64(0), publisher.CurrentVersion())

	// Add some assignments with different versions
	asgn1 := types.Assignment{Version: 5, Partitions: []types.Partition{{Keys: []string{"p1"}}}}
	data1, _ := json.Marshal(asgn1)
	_, err = assignmentKV.Put(ctx, "assignment.w1", data1)
	require.NoError(t, err)

	asgn2 := types.Assignment{Version: 10, Partitions: []types.Partition{{Keys: []string{"p2"}}}}
	data2, _ := json.Marshal(asgn2)
	_, err = assignmentKV.Put(ctx, "assignment.w2", data2)
	require.NoError(t, err)

	// Should discover version 10
	err = publisher.DiscoverHighestVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(10), publisher.CurrentVersion())
}

func TestAssignmentPublisher_DiscoverHighestVersion_IgnoresNonAssignmentKeys(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-publisher-ignore")

	publisher := NewAssignmentPublisher(
		assignmentKV,
		"assignment",
		logging.NewNop(),
		metrics.NewNop(),
	)

	ctx := context.Background()

	// Add heartbeat key (different prefix)
	_, err := assignmentKV.Put(ctx, "heartbeat.w1", []byte("alive"))
	require.NoError(t, err)

	// Add assignment with version 3
	asgn := types.Assignment{Version: 3, Partitions: []types.Partition{{Keys: []string{"p1"}}}}
	data, _ := json.Marshal(asgn)
	_, err = assignmentKV.Put(ctx, "assignment.w1", data)
	require.NoError(t, err)

	// Should only find version 3 (ignoring heartbeat)
	err = publisher.DiscoverHighestVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(3), publisher.CurrentVersion())
}

func TestAssignmentPublisher_Publish(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-publisher-publish")

	publisher := NewAssignmentPublisher(
		assignmentKV,
		"assignment",
		logging.NewNop(),
		metrics.NewNop(),
	)

	ctx := context.Background()

	// Publish assignments
	assignments := map[string][]types.Partition{
		"w1": {{Keys: []string{"p1", "p2"}}},
		"w2": {{Keys: []string{"p3"}}},
	}

	beforePublish := time.Now()
	err := publisher.Publish(ctx, []string{"w1", "w2"}, assignments, "test")
	require.NoError(t, err)

	// Version should be incremented to 1
	require.Equal(t, int64(1), publisher.CurrentVersion())

	// Last rebalance time should be recent
	require.True(t, publisher.LastRebalanceTime().After(beforePublish))

	// Verify assignments were written to KV
	entry1, err := assignmentKV.Get(ctx, "assignment.w1")
	require.NoError(t, err)

	var asgn1 types.Assignment
	err = json.Unmarshal(entry1.Value(), &asgn1)
	require.NoError(t, err)
	require.Equal(t, int64(1), asgn1.Version)
	require.Len(t, asgn1.Partitions, 1)

	entry2, err := assignmentKV.Get(ctx, "assignment.w2")
	require.NoError(t, err)

	var asgn2 types.Assignment
	err = json.Unmarshal(entry2.Value(), &asgn2)
	require.NoError(t, err)
	require.Equal(t, int64(1), asgn2.Version)
}

func TestAssignmentPublisher_Publish_IncrementsVersion(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-publisher-increment")

	publisher := NewAssignmentPublisher(
		assignmentKV,
		"assignment",
		logging.NewNop(),
		metrics.NewNop(),
	)

	ctx := context.Background()

	// First publish
	assignments1 := map[string][]types.Partition{
		"w1": {{Keys: []string{"p1"}}},
	}
	err := publisher.Publish(ctx, []string{"w1"}, assignments1, "test")
	require.NoError(t, err)
	require.Equal(t, int64(1), publisher.CurrentVersion())

	// Second publish should increment version
	assignments2 := map[string][]types.Partition{
		"w1": {{Keys: []string{"p1"}}},
		"w2": {{Keys: []string{"p2"}}},
	}
	err = publisher.Publish(ctx, []string{"w1", "w2"}, assignments2, "test")
	require.NoError(t, err)
	require.Equal(t, int64(2), publisher.CurrentVersion())
}

func TestAssignmentPublisher_Publish_RemovesOldWorkers(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-publisher-remove")

	publisher := NewAssignmentPublisher(
		assignmentKV,
		"assignment",
		logging.NewNop(),
		metrics.NewNop(),
	)

	ctx := context.Background()

	// Publish with 3 workers
	assignments1 := map[string][]types.Partition{
		"w1": {{Keys: []string{"p1"}}},
		"w2": {{Keys: []string{"p2"}}},
		"w3": {{Keys: []string{"p3"}}},
	}
	err := publisher.Publish(ctx, []string{"w1", "w2", "w3"}, assignments1, "test")
	require.NoError(t, err)

	// Verify all workers have assignments
	_, err = assignmentKV.Get(ctx, "assignment.w1")
	require.NoError(t, err)
	_, err = assignmentKV.Get(ctx, "assignment.w2")
	require.NoError(t, err)
	_, err = assignmentKV.Get(ctx, "assignment.w3")
	require.NoError(t, err)

	// Publish with only 2 workers (w3 removed)
	assignments2 := map[string][]types.Partition{
		"w1": {{Keys: []string{"p1", "p3"}}},
		"w2": {{Keys: []string{"p2"}}},
	}
	err = publisher.Publish(ctx, []string{"w1", "w2"}, assignments2, "test")
	require.NoError(t, err)

	// w1 and w2 should still exist with updated assignments
	entry1, err := assignmentKV.Get(ctx, "assignment.w1")
	require.NoError(t, err)
	var asgn1 types.Assignment
	err = json.Unmarshal(entry1.Value(), &asgn1)
	require.NoError(t, err)
	require.Len(t, asgn1.Partitions, 1)

	_, err = assignmentKV.Get(ctx, "assignment.w2")
	require.NoError(t, err)

	// w3 should be deleted (stale assignment removed)
	_, err = assignmentKV.Get(ctx, "assignment.w3")
	require.Error(t, err, "stale worker assignment should be deleted")
}

func TestAssignmentPublisher_Publish_EmptyAssignments(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-publisher-empty")

	publisher := NewAssignmentPublisher(
		assignmentKV,
		"assignment",
		logging.NewNop(),
		metrics.NewNop(),
	)

	ctx := context.Background()

	// Publish empty assignments (no workers)
	err := publisher.Publish(ctx, []string{}, map[string][]types.Partition{}, "test")
	require.NoError(t, err)

	// Version should NOT be incremented when there are no workers
	require.Equal(t, int64(0), publisher.CurrentVersion())
}

func TestAssignmentPublisher_CurrentVersion(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-publisher-version")

	publisher := NewAssignmentPublisher(
		assignmentKV,
		"assignment",
		logging.NewNop(),
		metrics.NewNop(),
	)

	// Should start at 0
	require.Equal(t, int64(0), publisher.CurrentVersion())
}

func TestAssignmentPublisher_LastRebalanceTime(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-publisher-time")

	publisher := NewAssignmentPublisher(
		assignmentKV,
		"assignment",
		logging.NewNop(),
		metrics.NewNop(),
	)

	ctx := context.Background()

	// Should start at zero time
	require.True(t, publisher.LastRebalanceTime().IsZero())

	// After publish, should be set
	assignments := map[string][]types.Partition{
		"w1": {{Keys: []string{"p1"}}},
	}
	beforePublish := time.Now()
	err := publisher.Publish(ctx, []string{"w1"}, assignments, "test")
	require.NoError(t, err)

	lastRebalance := publisher.LastRebalanceTime()
	require.False(t, lastRebalance.IsZero())
	require.True(t, lastRebalance.After(beforePublish.Add(-time.Second)))
	require.True(t, lastRebalance.Before(time.Now().Add(time.Second)))
}

func TestAssignmentPublisher_VersionMonotonicity(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-publisher-monotonic")

	// Simulate existing assignments from previous leader
	ctx := context.Background()
	asgn := types.Assignment{Version: 100, Partitions: []types.Partition{{Keys: []string{"p1"}}}}
	data, _ := json.Marshal(asgn)
	_, err := assignmentKV.Put(ctx, "assignment.w1", data)
	require.NoError(t, err)

	// New publisher (new leader) discovers existing version
	publisher := NewAssignmentPublisher(
		assignmentKV,
		"assignment",
		logging.NewNop(),
		metrics.NewNop(),
	)

	err = publisher.DiscoverHighestVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(100), publisher.CurrentVersion())

	// Next publish should be 101
	assignments := map[string][]types.Partition{
		"w1": {{Keys: []string{"p1"}}},
	}
	err = publisher.Publish(ctx, []string{"w1"}, assignments, "test")
	require.NoError(t, err)
	require.Equal(t, int64(101), publisher.CurrentVersion())
}

func TestAssignmentPublisher_CleanupAllAssignments(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-publisher-cleanup-all")

	publisher := NewAssignmentPublisher(
		assignmentKV,
		"assignment",
		logging.NewNop(),
		metrics.NewNop(),
	)

	ctx := context.Background()

	// Publish initial assignments for 3 workers
	assignments := map[string][]types.Partition{
		"w1": {{Keys: []string{"p1"}}},
		"w2": {{Keys: []string{"p2"}}},
		"w3": {{Keys: []string{"p3"}}},
	}
	err := publisher.Publish(ctx, []string{"w1", "w2", "w3"}, assignments, "test")
	require.NoError(t, err)

	// Verify all assignments exist
	_, err = assignmentKV.Get(ctx, "assignment.w1")
	require.NoError(t, err)
	_, err = assignmentKV.Get(ctx, "assignment.w2")
	require.NoError(t, err)
	_, err = assignmentKV.Get(ctx, "assignment.w3")
	require.NoError(t, err)

	// Cleanup all assignments (simulating Calculator.Stop())
	err = publisher.CleanupAllAssignments(ctx)
	require.NoError(t, err)

	// Verify all assignments are deleted
	_, err = assignmentKV.Get(ctx, "assignment.w1")
	require.Error(t, err, "w1 assignment should be deleted")
	_, err = assignmentKV.Get(ctx, "assignment.w2")
	require.Error(t, err, "w2 assignment should be deleted")
	_, err = assignmentKV.Get(ctx, "assignment.w3")
	require.Error(t, err, "w3 assignment should be deleted")

	t.Log("Verified: CleanupAllAssignments removes all assignment keys")
}

func TestAssignmentPublisher_CleanupStaleAssignments_Selective(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-publisher-cleanup-selective")

	publisher := NewAssignmentPublisher(
		assignmentKV,
		"assignment",
		logging.NewNop(),
		metrics.NewNop(),
	)

	ctx := context.Background()

	// Publish initial assignments for 4 workers
	assignments1 := map[string][]types.Partition{
		"w1": {{Keys: []string{"p1"}}},
		"w2": {{Keys: []string{"p2"}}},
		"w3": {{Keys: []string{"p3"}}},
		"w4": {{Keys: []string{"p4"}}},
	}
	err := publisher.Publish(ctx, []string{"w1", "w2", "w3", "w4"}, assignments1, "test")
	require.NoError(t, err)

	// Verify all assignments exist
	for _, workerID := range []string{"w1", "w2", "w3", "w4"} {
		_, err = assignmentKV.Get(ctx, "assignment."+workerID)
		require.NoError(t, err, "assignment for %s should exist", workerID)
	}

	// Publish new assignments for only 2 workers (w1, w2)
	// This should trigger cleanup of w3 and w4
	assignments2 := map[string][]types.Partition{
		"w1": {
			{Keys: []string{"p1"}},
			{Keys: []string{"p3"}},
		}, // w1 gets partitions p1 and p3
		"w2": {
			{Keys: []string{"p2"}},
			{Keys: []string{"p4"}},
		}, // w2 gets partitions p2 and p4
	}
	err = publisher.Publish(ctx, []string{"w1", "w2"}, assignments2, "test")
	require.NoError(t, err)

	// Verify w1 and w2 still exist with updated assignments
	entry1, err := assignmentKV.Get(ctx, "assignment.w1")
	require.NoError(t, err)
	var asgn1 types.Assignment
	err = json.Unmarshal(entry1.Value(), &asgn1)
	require.NoError(t, err)
	require.Len(t, asgn1.Partitions, 2)

	entry2, err := assignmentKV.Get(ctx, "assignment.w2")
	require.NoError(t, err)
	var asgn2 types.Assignment
	err = json.Unmarshal(entry2.Value(), &asgn2)
	require.NoError(t, err)
	require.Len(t, asgn2.Partitions, 2)

	// Verify w3 and w4 are deleted (stale assignments removed)
	_, err = assignmentKV.Get(ctx, "assignment.w3")
	require.Error(t, err, "w3 assignment should be deleted")
	_, err = assignmentKV.Get(ctx, "assignment.w4")
	require.Error(t, err, "w4 assignment should be deleted")

	t.Log("Verified: Selective cleanup removes only stale workers")
}
