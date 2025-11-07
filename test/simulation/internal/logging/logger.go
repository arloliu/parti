package logging

import (
	"log"

	"github.com/arloliu/parti/types"
)

// NewStdLogger creates a logger that writes to standard log output.
func NewStdLogger(prefix string) types.Logger {
	return &stdLogger{prefix: prefix}
}

type stdLogger struct {
	prefix string
}

// Compile-time assertion that stdLogger implements Logger.
var _ types.Logger = (*stdLogger)(nil)

func (l *stdLogger) Debug(msg string, keysAndValues ...any) {
	log.Printf("[%s] DEBUG: %s %v", l.prefix, msg, keysAndValues)
}

func (l *stdLogger) Info(msg string, keysAndValues ...any) {
	log.Printf("[%s] INFO: %s %v", l.prefix, msg, keysAndValues)
}

func (l *stdLogger) Warn(msg string, keysAndValues ...any) {
	log.Printf("[%s] WARN: %s %v", l.prefix, msg, keysAndValues)
}

func (l *stdLogger) Error(msg string, keysAndValues ...any) {
	log.Printf("[%s] ERROR: %s %v", l.prefix, msg, keysAndValues)
}

func (l *stdLogger) Fatal(msg string, keysAndValues ...any) {
	log.Fatalf("[%s] FATAL: %s %v", l.prefix, msg, keysAndValues)
}
