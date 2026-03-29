package handoff

import "github.com/arloliu/parti/v2/types"

// MetricsRecorder is an alias for [types.HandoffMetricsRecorder].
//
// The canonical interface definition lives in the public types package to avoid
// leaking internal import paths through the Manager's public option API.
// Internal code continues to use this alias for brevity.
type MetricsRecorder = types.HandoffMetricsRecorder

// NopMetrics is an alias for [types.NopHandoffMetricsRecorder].
type NopMetrics = types.NopHandoffMetricsRecorder
