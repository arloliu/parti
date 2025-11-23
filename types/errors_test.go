package types

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSentinelErrors(t *testing.T) {
	t.Run("errors.Is works correctly", func(t *testing.T) {
		// Test that errors.Is can match our sentinel errors
		require.True(t, errors.Is(ErrCalculatorAlreadyStarted, ErrCalculatorAlreadyStarted))
		require.False(t, errors.Is(ErrCalculatorAlreadyStarted, ErrCalculatorNotStarted))

		// Test that wrapped errors maintain identity
		wrapped := errors.Join(ErrCalculatorAlreadyStarted, errors.New("additional context"))
		require.True(t, errors.Is(wrapped, ErrCalculatorAlreadyStarted))
	})

	t.Run("all errors are distinct", func(t *testing.T) {
		// Collect all sentinel errors
		allErrors := []error{
			// Manager errors
			ErrInvalidConfig,
			ErrNATSConnectionRequired,
			ErrPartitionSourceRequired,
			ErrAssignmentStrategyRequired,
			ErrAlreadyStarted,
			ErrNotStarted,
			ErrNotImplemented,
			ErrNoWorkersAvailable,
			ErrInvalidWorkerID,
			ErrElectionFailed,
			ErrIDClaimFailed,
			ErrAssignmentFailed,
			// Calculator errors
			ErrCalculatorAlreadyStarted,
			ErrCalculatorNotStarted,
			// WorkerMonitor errors
			ErrWorkerMonitorAlreadyStarted,
			ErrWorkerMonitorAlreadyStopped,
			ErrWorkerMonitorNotStarted,
			ErrWatcherFailed,
			// AssignmentPublisher errors
			ErrPublishFailed,
			ErrDeleteFailed,
			// Common errors
			ErrContextCanceled,
			ErrNoKeysFound,
		}

		// Verify all errors are unique
		for i, err1 := range allErrors {
			for j, err2 := range allErrors {
				if i == j {
					require.True(t, errors.Is(err1, err2), "error should equal itself: %v", err1)
				} else {
					require.False(t, errors.Is(err1, err2), "errors should be distinct: %v vs %v", err1, err2)
				}
			}
		}
	})
}

func TestIsNoKeysFoundError(t *testing.T) {
	t.Run("returns false for nil error", func(t *testing.T) {
		require.False(t, IsNoKeysFoundError(nil))
	})

	t.Run("returns true for sentinel ErrNoKeysFound", func(t *testing.T) {
		require.True(t, IsNoKeysFoundError(ErrNoKeysFound))
	})

	t.Run("returns true for wrapped ErrNoKeysFound", func(t *testing.T) {
		wrapped := errors.Join(ErrNoKeysFound, errors.New("additional context"))
		require.True(t, IsNoKeysFoundError(wrapped))
	})

	t.Run("returns true for NATS direct error message", func(t *testing.T) {
		natsErr := errors.New("nats: no keys found")
		require.True(t, IsNoKeysFoundError(natsErr))
	})

	t.Run("returns true for wrapped NATS error message", func(t *testing.T) {
		natsErr := errors.New("failed to list KV keys: nats: no keys found")
		require.True(t, IsNoKeysFoundError(natsErr))
	})

	t.Run("returns false for unrelated error", func(t *testing.T) {
		otherErr := errors.New("some other error")
		require.False(t, IsNoKeysFoundError(otherErr))
	})

	t.Run("returns false for other sentinel errors", func(t *testing.T) {
		require.False(t, IsNoKeysFoundError(ErrContextCanceled))
		require.False(t, IsNoKeysFoundError(ErrCalculatorNotStarted))
		require.False(t, IsNoKeysFoundError(ErrInvalidConfig))
	})
}
