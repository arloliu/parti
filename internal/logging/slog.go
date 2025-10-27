package logging

import (
	"log/slog"
	"os"

	"github.com/arloliu/parti/types"
)

// SlogLogger implements types.Logger using Go's standard log/slog package.
type SlogLogger struct {
	logger *slog.Logger
}

// Compile-time assertion that SlogLogger implements Logger.
var _ types.Logger = (*SlogLogger)(nil)

// NewSlog creates a new slog-based logger.
//
// Parameters:
//   - logger: The underlying slog.Logger instance to use
//
// Returns:
//   - *SlogLogger: A new logger instance that wraps the provided slog.Logger
//
// Example:
//
//	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
//	logger := NewSlog(slog.New(handler))
//	logger.Info("application started", "version", "1.0")
func NewSlog(logger *slog.Logger) *SlogLogger {
	return &SlogLogger{logger: logger}
}

// NewSlogDefault creates a new slog-based logger with default settings.
//
// The default logger uses a text handler writing to stdout with Info level.
//
// Returns:
//   - *SlogLogger: A new logger instance with default configuration
//
// Example:
//
//	logger := NewSlogDefault()
//	logger.Info("using default logger")
func NewSlogDefault() *SlogLogger {
	return &SlogLogger{logger: slog.Default()}
}

// Debug logs a debug-level message with optional key-value pairs.
//
// Parameters:
//   - msg: The log message
//   - keysAndValues: Optional key-value pairs for structured logging
func (l *SlogLogger) Debug(msg string, keysAndValues ...any) {
	l.logger.Debug(msg, keysAndValues...)
}

// Info logs an info-level message with optional key-value pairs.
//
// Parameters:
//   - msg: The log message
//   - keysAndValues: Optional key-value pairs for structured logging
func (l *SlogLogger) Info(msg string, keysAndValues ...any) {
	l.logger.Info(msg, keysAndValues...)
}

// Warn logs a warning-level message with optional key-value pairs.
//
// Parameters:
//   - msg: The log message
//   - keysAndValues: Optional key-value pairs for structured logging
func (l *SlogLogger) Warn(msg string, keysAndValues ...any) {
	l.logger.Warn(msg, keysAndValues...)
}

// Error logs an error-level message with optional key-value pairs.
//
// Parameters:
//   - msg: The log message
//   - keysAndValues: Optional key-value pairs for structured logging
func (l *SlogLogger) Error(msg string, keysAndValues ...any) {
	l.logger.Error(msg, keysAndValues...)
}

// Fatal logs a fatal-level message with optional key-value pairs and exits.
//
// This method logs at Error level (slog doesn't have a Fatal level) and then
// calls os.Exit(1) to terminate the program.
//
// Parameters:
//   - msg: The log message
//   - keysAndValues: Optional key-value pairs for structured logging
func (l *SlogLogger) Fatal(msg string, keysAndValues ...any) {
	l.logger.Error(msg, keysAndValues...)
	os.Exit(1) //nolint:revive // Fatal should exit the program
}
