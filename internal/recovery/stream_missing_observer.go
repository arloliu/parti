package recovery

// StreamMissingObserver is implemented by any consumer-side component
// that can route a stream-missing recovery-exhaustion event to a
// supervising observer (e.g. the Parti manager's
// enterDegraded("stream-missing-recovery-exhausted") path).
//
// The manager installs its closure via SetOnStreamMissingError after
// the consumer has been registered as a WorkerConsumerUpdater. The
// callback fires exactly once per partition consumer when the
// stream-missing recovery envelope exhausts its attempt budget; the
// error chain satisfies errors.Is(err, types.ErrStreamMissing).
//
// Setting fn = nil clears any previously-installed callback.
type StreamMissingObserver interface {
	// SetOnStreamMissingError registers the supervising observer.
	// Passing fn == nil clears any previously-installed callback.
	SetOnStreamMissingError(fn func(streamName string, err error))
}
