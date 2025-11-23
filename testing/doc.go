// Package testing provides test utilities for the Parti library.
//
// This package offers helpers for setting up test environments, particularly
// embedded NATS servers for integration testing. It follows Go's convention
// of providing testing utilities in a dedicated package (similar to net/http/httptest).
//
// Key utilities:
//   - StartEmbeddedNATS: Single NATS server with JetStream
//   - StartEmbeddedNATSCluster: 3-node NATS cluster for HA testing
//   - CreateJetStreamKV: Convenience wrapper for KV bucket creation
//
// Example usage:
//
//	import (
//	    "testing"
//	    partitest "github.com/arloliu/parti/testing"
//	)
//
//	func TestMyComponent(t *testing.T) {
//	    _, nc := partitest.StartEmbeddedNATS(t)
//	    // Use nc for your tests
//	}
package testing
