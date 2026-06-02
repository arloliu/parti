package parti

import (
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/stretchr/testify/require"
)

// TestCalculateAlertLevel_BoundaryTable characterizes calculateAlertLevel across
// every alert level and both sides of each threshold band. The function had zero
// coverage; this pins the duration->level mapping AND the highest-first cascade
// order (Critical -> Error -> Warn -> Info) so a consolidation refactor cannot
// reorder the checks or remap a band undetected.
//
// Note on what is (not) locked: time.Since always returns threshold+epsilon for a
// degradedSince computed as now-threshold, so the exact >=/> boundary at a single
// nanosecond is below the observable resolution and is intentionally NOT asserted
// (no operator degrades for exactly 30.000000000s). The load-bearing behavior is
// the band mapping and the cascade ORDER, both of which this table locks.
func TestCalculateAlertLevel_BoundaryTable(t *testing.T) {
	t.Parallel()
	m, _, _, _ := newTestManager(t)
	m.cfg.DegradedAlert = DegradedAlertConfig{
		InfoThreshold:     30 * time.Second,
		WarnThreshold:     2 * time.Minute,
		ErrorThreshold:    5 * time.Minute,
		CriticalThreshold: 10 * time.Minute,
		AlertInterval:     time.Minute,
	}

	belowInfo := AlertLevelInfo - 1
	cases := []struct {
		name     string
		dur      time.Duration
		expected AlertLevel
	}{
		{"zero", 0, belowInfo},
		{"below-info", 29 * time.Second, belowInfo},
		{"at-info", 30 * time.Second, AlertLevelInfo},
		{"info-band", 90 * time.Second, AlertLevelInfo},
		{"just-below-warn", 119 * time.Second, AlertLevelInfo},
		{"at-warn", 2 * time.Minute, AlertLevelWarn},
		{"warn-band", 4 * time.Minute, AlertLevelWarn},
		{"just-below-error", 299 * time.Second, AlertLevelWarn},
		{"at-error", 5 * time.Minute, AlertLevelError},
		{"error-band", 8 * time.Minute, AlertLevelError},
		{"just-below-critical", 599 * time.Second, AlertLevelError},
		{"at-critical", 10 * time.Minute, AlertLevelCritical},
		{"above-critical", time.Hour, AlertLevelCritical},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := m.calculateAlertLevel(time.Now().Add(-tc.dur))
			require.Equal(t, tc.expected, got, "duration %s", tc.dur)
		})
	}
}

// alertRecordingMetrics captures the alert-emission metric calls so the
// level->name mapping in emitDegradedAlert is observable without timing.
type alertRecordingMetrics struct {
	*metrics.NopMetrics
	lastLevel int
	lastName  string
	emitted   int
}

func (r *alertRecordingMetrics) SetAlertLevel(level int)           { r.lastLevel = level }
func (r *alertRecordingMetrics) IncrementAlertEmitted(name string) { r.lastName = name; r.emitted++ }

// TestEmitDegradedAlert_LevelNameMapping pins emitDegradedAlert's level->name
// mapping and that it records both the numeric level metric and the named
// counter exactly once. Observable via the metrics collector seam.
func TestEmitDegradedAlert_LevelNameMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		level AlertLevel
		name  string
	}{
		{AlertLevelInfo, "info"},
		{AlertLevelWarn, "warn"},
		{AlertLevelError, "error"},
		{AlertLevelCritical, "critical"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, _, _, _ := newTestManager(t)
			rec := &alertRecordingMetrics{NopMetrics: metrics.NewNop()}
			m.metrics = rec

			m.emitDegradedAlert(tc.level, time.Now().Add(-time.Minute))

			require.Equal(t, int(tc.level), rec.lastLevel, "numeric alert level metric")
			require.Equal(t, tc.name, rec.lastName, "named alert counter")
			require.Equal(t, 1, rec.emitted, "exactly one emission")
		})
	}
}
