package types

// Logger defines methods for structured logging.
//
// Compatible with zap.SugaredLogger and other structured loggers.
// All methods accept key-value pairs for structured fields.
type Logger interface {
	// Debug logs a message at DebugLevel.
	// The message includes any fields passed at the log site, as well as any fields accumulated on the logger.
	Debug(msg string, keysAndValues ...any)

	// Info logs a message at InfoLevel.
	// The message includes any fields passed at the log site, as well as any fields accumulated on the logger.
	Info(msg string, keysAndValues ...any)

	// Warn logs a message at WarnLevel.
	// The message includes any fields passed at the log site, as well as any fields accumulated on the logger.
	Warn(msg string, keysAndValues ...any)

	// Error logs a message at ErrorLevel.
	// The message includes any fields passed at the log site, as well as any fields accumulated on the logger.
	Error(msg string, keysAndValues ...any)

	// Fatal logs a message at FatalLevel and calls os.Exit(1).
	// The message includes any fields passed at the log site, as well as any fields accumulated on the logger.
	//
	// The logger then calls os.Exit(1), even if logging at FatalLevel is disabled.
	Fatal(msg string, keysAndValues ...any)
}
