package parti

// Test helpers for the degraded-record-pointer state. Production code holds the
// degraded {since, reason} pair as one atomic.Pointer[degradedRecord]; these keep
// the white-box tests that previously poked the two separate atomics concise.

// markDegraded forces the degraded record directly, mirroring the production
// single-swap publish of {since, reason}. Tests that drive enterDegraded through
// the real path do not need this; it is for tests that set up a degraded record
// without a state transition.
func (m *Manager) markDegraded(sinceNano int64, reason string) {
	m.degraded.Store(&degradedRecord{since: sinceNano, reason: reason})
}

// degradedSinceNano returns the degrade-entry UnixNano (0 if not degraded),
// preserving the require.Zero / require.NotZero assertion style that previously
// read the degradedSince atomic directly.
func (m *Manager) degradedSinceNano() int64 {
	if rec := m.degraded.Load(); rec != nil {
		return rec.since
	}

	return 0
}
