package testing

import (
	"testing"

	"github.com/arloliu/parti/types"
)

// NewTestLogger creates a new logger instance that writes to the testing.T logger.
// This is useful for seeing log output during test runs.
func NewTestLogger(t *testing.T) types.Logger {
	return &testLogger{t: t}
}

type testLogger struct {
	t *testing.T
}

var _ types.Logger = (*testLogger)(nil)

func (l *testLogger) Debug(msg string, keysAndValues ...any) {
	l.t.Logf("DEBUG: %s %v", msg, keysAndValues)
}

func (l *testLogger) Info(msg string, keysAndValues ...any) {
	l.t.Logf("INFO: %s %v", msg, keysAndValues)
}

func (l *testLogger) Warn(msg string, keysAndValues ...any) {
	l.t.Logf("WARN: %s %v", msg, keysAndValues)
}

func (l *testLogger) Error(msg string, keysAndValues ...any) {
	l.t.Logf("ERROR: %s %v", msg, keysAndValues)
}

func (l *testLogger) Fatal(msg string, keysAndValues ...any) {
	l.t.Fatalf("FATAL: %s %v", msg, keysAndValues)
}
