package logger

import (
	"fmt"
	"testing"

	"github.com/arloliu/parti/types"
)

// TestLogger implements types.Logger using testing.T for output.
// This ensures log messages appear in test output.
type TestLogger struct {
	t *testing.T
}

// Compile-time assertion that TestLogger implements Logger.
var _ types.Logger = (*TestLogger)(nil)

// NewTest creates a new test logger that writes to testing.T.
//
// Parameters:
//   - t: The testing.T instance to write logs to
//
// Returns:
//   - *TestLogger: A new logger instance that uses t.Logf()
//
// Example:
//
//	func TestSomething(t *testing.T) {
//	    logger := NewTest(t)
//	    logger.Info("test started", "id", 123)
//	}
func NewTest(t *testing.T) *TestLogger {
	return &TestLogger{t: t}
}

// Debug logs a debug-level message with optional key-value pairs.
func (l *TestLogger) Debug(msg string, keysAndValues ...any) {
	l.t.Logf("DEBUG: %s %s", msg, formatKeyValues(keysAndValues))
}

// Info logs an info-level message with optional key-value pairs.
func (l *TestLogger) Info(msg string, keysAndValues ...any) {
	l.t.Logf("INFO: %s %s", msg, formatKeyValues(keysAndValues))
}

// Warn logs a warning-level message with optional key-value pairs.
func (l *TestLogger) Warn(msg string, keysAndValues ...any) {
	l.t.Logf("WARN: %s %s", msg, formatKeyValues(keysAndValues))
}

// Error logs an error-level message with optional key-value pairs.
func (l *TestLogger) Error(msg string, keysAndValues ...any) {
	l.t.Logf("ERROR: %s %s", msg, formatKeyValues(keysAndValues))
}

// Fatal logs a fatal-level message and fails the test.
func (l *TestLogger) Fatal(msg string, keysAndValues ...any) {
	l.t.Fatalf("FATAL: %s %s", msg, formatKeyValues(keysAndValues))
}

// formatKeyValues formats key-value pairs for logging.
func formatKeyValues(keysAndValues []any) string {
	if len(keysAndValues) == 0 {
		return ""
	}

	result := ""
	for i := 0; i < len(keysAndValues); i += 2 {
		if i+1 < len(keysAndValues) {
			result += fmt.Sprintf("%v=%v ", keysAndValues[i], keysAndValues[i+1])
		} else {
			result += fmt.Sprintf("%v=<missing> ", keysAndValues[i])
		}
	}

	return result
}
