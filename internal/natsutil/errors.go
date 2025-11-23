package natsutil

import (
	"errors"
	"strings"

	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// IsConnectivityError checks if an error is caused by connectivity issues.
//
// This includes NATS timeouts, connection refused, disconnections, etc.
// Used to determine when to enter degraded mode and use cached data.
//
// Kept in internal/natsutil to avoid importing NATS dependencies in types/ package.
//
// Parameters:
//   - err: Error to check
//
// Returns:
//   - bool: true if error indicates connectivity issue
func IsConnectivityError(err error) bool {
	if err == nil {
		return false
	}

	// Check for known connectivity error types
	return errors.Is(err, types.ErrConnectivity) ||
		errors.Is(err, nats.ErrTimeout) ||
		errors.Is(err, nats.ErrNoServers) ||
		errors.Is(err, nats.ErrDisconnected) ||
		errors.Is(err, nats.ErrConnectionClosed) ||
		errors.Is(err, jetstream.ErrNoStreamResponse) ||
		strings.Contains(err.Error(), "connection refused") ||
		strings.Contains(err.Error(), "i/o timeout")
}
