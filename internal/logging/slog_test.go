package logging

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/types"
)

func TestSlogLogger_ImplementsInterface(t *testing.T) {
	t.Helper()
	var _ types.Logger = (*SlogLogger)(nil)
}

func TestNewSlog(t *testing.T) {
	buf := &bytes.Buffer{}
	handler := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := NewSlog(slog.New(handler))

	require.NotNil(t, logger)
	require.NotNil(t, logger.logger)
}

func TestNewSlogDefault(t *testing.T) {
	logger := NewSlogDefault()

	require.NotNil(t, logger)
	require.NotNil(t, logger.logger)
}

func TestSlogLogger_Debug(t *testing.T) {
	buf := &bytes.Buffer{}
	handler := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := NewSlog(slog.New(handler))

	logger.Debug("debug message", "key", "value")

	output := buf.String()
	assert.Contains(t, output, "debug message")
	assert.Contains(t, output, "key=value")
	assert.Contains(t, output, "level=DEBUG")
}

func TestSlogLogger_Info(t *testing.T) {
	buf := &bytes.Buffer{}
	handler := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := NewSlog(slog.New(handler))

	logger.Info("info message", "worker", "w-1")

	output := buf.String()
	assert.Contains(t, output, "info message")
	assert.Contains(t, output, "worker=w-1")
	assert.Contains(t, output, "level=INFO")
}

func TestSlogLogger_Warn(t *testing.T) {
	buf := &bytes.Buffer{}
	handler := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	logger := NewSlog(slog.New(handler))

	logger.Warn("warning message", "state", "scaling")

	output := buf.String()
	assert.Contains(t, output, "warning message")
	assert.Contains(t, output, "state=scaling")
	assert.Contains(t, output, "level=WARN")
}

func TestSlogLogger_Error(t *testing.T) {
	buf := &bytes.Buffer{}
	handler := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelError})
	logger := NewSlog(slog.New(handler))

	logger.Error("error message", "error", "timeout")

	output := buf.String()
	assert.Contains(t, output, "error message")
	assert.Contains(t, output, "error=timeout")
	assert.Contains(t, output, "level=ERROR")
}

func TestSlogLogger_LevelFiltering(t *testing.T) {
	buf := &bytes.Buffer{}
	handler := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	logger := NewSlog(slog.New(handler))

	// Debug and Info should be filtered out
	logger.Debug("debug message")
	logger.Info("info message")

	output := buf.String()
	assert.NotContains(t, output, "debug message")
	assert.NotContains(t, output, "info message")

	// Warn and Error should appear
	logger.Warn("warn message")
	logger.Error("error message")

	output = buf.String()
	assert.Contains(t, output, "warn message")
	assert.Contains(t, output, "error message")
}

func TestSlogLogger_MultipleKeyValues(t *testing.T) {
	buf := &bytes.Buffer{}
	handler := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := NewSlog(slog.New(handler))

	logger.Info("calculator state changed",
		"old_state", "idle",
		"new_state", "scaling",
		"worker_count", 3,
		"reason", "cold_start")

	output := buf.String()
	assert.Contains(t, output, "calculator state changed")
	assert.Contains(t, output, "old_state=idle")
	assert.Contains(t, output, "new_state=scaling")
	assert.Contains(t, output, "worker_count=3")
	assert.Contains(t, output, "reason=cold_start")
}
