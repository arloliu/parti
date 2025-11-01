package assignment

import (
	"context"
	"testing"
	"time"

	partitest "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// mockSource implements types.PartitionSource for testing.
//
// Provides configurable partition lists and error simulation.
type mockSource struct {
	partitions []types.Partition
	err        error
}

// ListPartitions returns the configured partitions or error.
func (m *mockSource) ListPartitions(_ context.Context) ([]types.Partition, error) {
	if m.err != nil {
		return nil, m.err
	}

	return m.partitions, nil
}

// mockStrategy implements types.AssignmentStrategy for testing.
//
// Provides configurable assignment maps and error simulation.
// If no custom assignments are provided, uses simple round-robin.
type mockStrategy struct {
	assignments map[string][]types.Partition
	err         error
}

// Assign returns the configured assignments or performs round-robin.
func (m *mockStrategy) Assign(workers []string, partitions []types.Partition) (map[string][]types.Partition, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.assignments != nil {
		return m.assignments, nil
	}

	// Simple round-robin assignment
	result := make(map[string][]types.Partition)
	for i, part := range partitions {
		workerIdx := i % len(workers)
		worker := workers[workerIdx]
		result[worker] = append(result[worker], part)
	}

	return result, nil
}

// testSetup contains common test dependencies for Calculator tests.
//
// This helper eliminates duplicate NATS/KV setup code across test files
// and provides standardized test fixtures.
//
//nolint:unused // Reserved for future test refactoring
type testSetup struct {
	AssignmentKV jetstream.KeyValue
	HeartbeatKV  jetstream.KeyValue
	Source       *mockSource
	Strategy     *mockStrategy
}

// newTestSetup creates a standard test setup with embedded NATS.
//
// This creates two KV buckets (assignment and heartbeat) and provides
// default mock implementations for PartitionSource and AssignmentStrategy.
//
// Parameters:
//   - t: Test instance
//   - testName: Unique name for this test (used for KV bucket names)
//
// Returns:
//   - *testSetup: Configured test setup ready for use
//
// Example:
//
//	setup := newTestSetup(t, "test-calculator-start")
//	calc := setup.newCalculator(t)
//	defer calc.Stop()
//
//nolint:unused // Reserved for future test refactoring
func newTestSetup(t *testing.T, testName string) *testSetup {
	t.Helper()
	_, nc := partitest.StartEmbeddedNATS(t)

	return &testSetup{
		AssignmentKV: partitest.CreateJetStreamKV(t, nc, testName+"-assignment"),
		HeartbeatKV:  partitest.CreateJetStreamKV(t, nc, testName+"-heartbeat"),
		Source:       &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}}},
		Strategy:     &mockStrategy{},
	}
}

// newCalculator creates a calculator with test defaults.
//
// Provides sensible defaults for all required configuration fields.
// Custom configuration can be provided via functional options.
//
// Parameters:
//   - t: Test instance
//   - opts: Optional configuration overrides
//
// Returns:
//   - *Calculator: Configured calculator instance
//
// Example:
//
//	setup := newTestSetup(t, "test-name")
//	calc := setup.newCalculator(t, func(cfg *Config) {
//	    cfg.HeartbeatTTL = 10 * time.Second
//	})
//
//nolint:unused // Reserved for future test refactoring
func (s *testSetup) newCalculator(t *testing.T, opts ...func(*Config)) *Calculator {
	t.Helper()

	cfg := &Config{
		AssignmentKV:         s.AssignmentKV,
		HeartbeatKV:          s.HeartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               s.Source,
		Strategy:             s.Strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         6 * time.Second,
		EmergencyGracePeriod: 3 * time.Second,
	}

	// Apply custom configuration
	for _, opt := range opts {
		opt(cfg)
	}

	calc, err := NewCalculator(cfg)

	require.NoError(t, err)

	return calc
}

// publishTestHeartbeat publishes a heartbeat for a test worker.
//
// Helper function to simulate worker heartbeats in tests.
//
// Parameters:
//   - t: Test instance
//   - kv: Heartbeat KV bucket
//   - prefix: Heartbeat key prefix
//   - workerID: Worker identifier
//
//nolint:unused // Reserved for future test refactoring
func publishTestHeartbeat(t *testing.T, kv jetstream.KeyValue, prefix, workerID string) {
	t.Helper()
	key := prefix + "." + workerID
	_, err := kv.PutString(context.Background(), key, "alive")
	require.NoError(t, err)
}

// deleteTestHeartbeat deletes a heartbeat for a test worker.
//
// Helper function to simulate worker disappearance in tests.
//
// Parameters:
//   - t: Test instance
//   - kv: Heartbeat KV bucket
//   - prefix: Heartbeat key prefix
//   - workerID: Worker identifier
//
//nolint:unused // Reserved for future test refactoring
func deleteTestHeartbeat(t *testing.T, kv jetstream.KeyValue, prefix, workerID string) {
	t.Helper()
	key := prefix + "." + workerID
	err := kv.Delete(context.Background(), key)
	require.NoError(t, err)
}
