package logger

import (
	"testing"

	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

func TestNopLogger(t *testing.T) {
	logger := NewNop()

	// Verify it implements the interface
	var _ types.Logger = logger

	// All methods should be callable without panicking
	require.NotPanics(t, func() {
		logger.Debug("test message", "key", "value")
		logger.Info("test message", "key", "value")
		logger.Warn("test message", "key", "value")
		logger.Error("test message", "key", "value")
		logger.Fatal("test message", "key", "value") // Should NOT exit
	})
}

func TestNopLogger_NoSideEffects(t *testing.T) {
	logger := NewNop()

	// Should handle nil and empty arguments
	require.NotPanics(t, func() {
		logger.Debug("")
		logger.Info("", nil)
		logger.Warn("message")
		logger.Error("message", "single")
		logger.Fatal("message", "k1", "v1", "k2", "v2")
	})
}

func TestNopLoggerImplementsLogger(_ *testing.T) {
	var _ types.Logger = (*NopLogger)(nil)
}

func TestNewNop(t *testing.T) {
	logger := NewNop()

	require.NotNil(t, logger)
	require.IsType(t, &NopLogger{}, logger)
}

func BenchmarkNopLogger(b *testing.B) {
	logger := NewNop()

	for b.Loop() {
		logger.Debug("benchmark message", "key1", "value1", "key2", 42)
	}
}
