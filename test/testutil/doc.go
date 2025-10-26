// Package testutil provides shared test utilities and fixtures for integration tests.
//
// This package contains common setup code, test data, and helper functions
// that are used across multiple integration tests.
//
// Examples of utilities that belong here:
//   - Common test fixtures (predefined configurations, partitions, etc.)
//   - Setup helpers (create multiple managers, wait for conditions, etc.)
//   - Assertion helpers (verify leader state, check assignments, etc.)
//   - Test data generators (random partitions, worker IDs, etc.)
//
// Note: For NATS server setup, use the github.com/arloliu/parti/testing package.
// This package is specifically for integration test scenarios and helper utilities.
package testutil
