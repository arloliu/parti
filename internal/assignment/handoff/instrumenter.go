package handoff

import "time"

// instrumenter centralizes handoff metrics emission to keep Apply methods
// readable. It is intentionally small and branchless for minimal overhead.
// All methods are no-ops if metrics recorder is nil.
type instrumenter struct {
	m     MetricsRecorder
	start time.Time
}

// newInstrumenter constructs an instrumenter capturing the start time.
func newInstrumenter(m MetricsRecorder) *instrumenter { return &instrumenter{m: m, start: time.Now()} }

// phase runs the provided function, measuring duration under the given phase name.
// Any returned error is propagated unchanged.
func (i *instrumenter) phase(name string, fn func() error) error {
	pstart := time.Now()
	err := fn()
	if i.m != nil {
		i.m.ObservePhaseDuration(name, time.Since(pstart))
	}
	return err
}

// finish records overall duration and success/failure outcome.
func (i *instrumenter) finish(err error) {
	if i.m == nil {
		return
	}
	i.m.ObserveHandoffDuration(time.Since(i.start))
	if err != nil {
		i.m.IncHandoffTotal("failure")
	} else {
		i.m.IncHandoffTotal("success")
	}
}
