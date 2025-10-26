// Package logger provides logging utilities for the Parti library.
package logger

import "github.com/arloliu/parti/types"

// NopLogger is a no-op logger that discards all log messages.
//
// Useful for:
//   - Testing without log noise
//   - Production when logging is handled externally
//   - Benchmarks to avoid I/O overhead
//
// Example:
//
// mgr := parti.NewManager(&cfg, conn, src, parti.WithLogger(logger.NewNop()))
type NopLogger struct{}

// Compile-time assertion that NopLogger implements Logger.
var _ types.Logger = (*NopLogger)(nil)

// NewNop creates a new no-op logger that discards all messages.
//
// Returns:
//   - *NopLogger: Logger that performs no operations
func NewNop() *NopLogger {
	return &NopLogger{}
}

// Debug discards the message.
func (n *NopLogger) Debug(_ /* msg */ string, _ /* keysAndValues */ ...any) {}

// Info discards the message.
func (n *NopLogger) Info(_ /* msg */ string, _ /* keysAndValues */ ...any) {}

// Warn discards the message.
func (n *NopLogger) Warn(_ /* msg */ string, _ /* keysAndValues */ ...any) {}

// Error discards the message.
func (n *NopLogger) Error(_ /* msg */ string, _ /* keysAndValues */ ...any) {}

// Fatal discards the message (does NOT call os.Exit).
//
// Note: Unlike production loggers, NopLogger does not terminate the process.
// This is intentional for testing scenarios.
func (n *NopLogger) Fatal(_ /* msg */ string, _ /* keysAndValues */ ...any) {}
