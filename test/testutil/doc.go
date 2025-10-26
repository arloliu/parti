// Package testutil provides shared test utilities and fixtures for integration tests.
//
// This package contains common setup code, test data, and helper functions
// that are used across multiple integration tests.
//
// # Core Components
//
// WorkerCluster: Manages multiple workers for testing scenarios
//   - AddWorker(): Add workers with state tracking
//   - StartWorkers(): Start all workers simultaneously
//   - WaitForStableState(): Wait for all workers to stabilize
//   - VerifyExactlyOneLeader(): Ensure correct leader election
//
// # Debug Logging
//
// To enable detailed logging for troubleshooting, pass a logger when adding workers:
//
//	import "github.com/arloliu/parti/internal/logger"
//
//	debugLogger := logger.NewTest(t)
//	cluster.AddWorker(ctx, debugLogger)  // With debug logs
//
// Or without logging (default):
//
//	cluster.AddWorker(ctx)  // Silent mode
//
// Debug logs include:
//   - Calculator state transitions (Idle, Scaling, Rebalancing, Emergency)
//   - Worker changes detected by polling
//   - Assignment calculations and publications
//   - Stabilization window timeouts
//
// # Examples
//
// Cold start test with 3 workers:
//
//	cluster := testutil.NewWorkerCluster(t, nc, 10)
//	for i := 0; i < 3; i++ {
//	    cluster.AddWorker(ctx)
//	}
//	cluster.StartWorkers(ctx)
//	cluster.WaitForStableState(15 * time.Second)
//
// Note: For NATS server setup, use the github.com/arloliu/parti/testing package.
// This package is specifically for integration test scenarios and helper utilities.
package testutil
