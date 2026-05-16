package parti

// CapabilityReporter is an optional interface a WorkerConsumerUpdater
// MAY implement to report runtime capabilities back to the Manager.
//
// When the registered updater (or any child of a composite updater)
// satisfies this interface, Manager queries Capabilities() after each
// handoff apply attempt and ORs the returned bits into its capability
// bitmask via SetCapability.
//
// Implementations MUST be:
//   - Safe for concurrent calls. Capabilities() may be invoked from
//     the manager-apply goroutine (which calls
//     reportConsumerCapabilities after every handoffCoordinator.Apply
//     attempt) and may race with the updater's own UpdateWorkerConsumer
//     calls. The heartbeat publisher does NOT call Capabilities()
//     directly — it reads Manager.Capabilities() (the live bitmask
//     callback) — so the race surface is reporter ↔ updater, not
//     reporter ↔ heartbeat.
//   - Non-blocking. Capabilities() is invoked on every apply attempt;
//     must not perform I/O or acquire locks held by long operations.
//     A simple atomic load is the expected shape.
//   - Monotonic for runtime-wire-up bits such as CapProcessingGate:
//     once a capability has been successfully wired (e.g., a handler
//     wrapped with the processing gate), the corresponding bit MUST
//     remain set for the rest of the updater's lifetime even if a
//     later per-subject create fails. The bit reflects "this updater
//     has at least one wired component", not "all components are
//     currently wired".
//
// Manager integration semantics for the reporter integration are
// OR-only for runtime-wire-up bits: reportConsumerCapabilities calls
// SetCapability(bit, true) for known reported bits and never clears.
// (Manager.SetCapability itself supports clearing bits via active=false,
// used by other components — but the reporter pathway only sets.)
// Implementers should not rely on a returned-zero Capabilities()
// causing the manager to clear a previously-reported bit; it won't.
//
// Returning 0 is always safe (no caps advertised).
type CapabilityReporter interface {
	// Capabilities returns the OR of capability bits this reporter has
	// successfully wired at runtime. Must be safe for concurrent use,
	// non-blocking, and monotonic for runtime-wireup bits.
	Capabilities() uint32
}
