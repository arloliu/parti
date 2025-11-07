package logging

import "github.com/arloliu/parti/types"

// NewNop creates a no-op logger that discards all log output.
func NewNop() types.Logger {
	return &nopLogger{}
}

type nopLogger struct{}

// Compile-time assertion that nopLogger implements Logger.
var _ types.Logger = (*nopLogger)(nil)

func (l *nopLogger) Debug(msg string, keysAndValues ...any) {}
func (l *nopLogger) Info(msg string, keysAndValues ...any)  {}
func (l *nopLogger) Warn(msg string, keysAndValues ...any)  {}
func (l *nopLogger) Error(msg string, keysAndValues ...any) {}
func (l *nopLogger) Fatal(msg string, keysAndValues ...any) {}
