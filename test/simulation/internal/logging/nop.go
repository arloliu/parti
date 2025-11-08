package logging

import "github.com/arloliu/parti/types"

// NewNop creates a no-op logger that discards all log output.
func NewNop() types.Logger {
	return &nopLogger{}
}

type nopLogger struct{}

// Compile-time assertion that nopLogger implements Logger.
var _ types.Logger = (*nopLogger)(nil)

func (l *nopLogger) Debug(string, ...any) {}
func (l *nopLogger) Info(string, ...any)  {}
func (l *nopLogger) Warn(string, ...any)  {}
func (l *nopLogger) Error(string, ...any) {}
func (l *nopLogger) Fatal(string, ...any) {}
