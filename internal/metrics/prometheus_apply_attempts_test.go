package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func TestPrometheus_RecordApplyAttempt_BoundedLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	p := NewPrometheus(reg, "parti")

	// Same worker, different versions: must aggregate to a single series
	// (the version argument is discarded by the Prometheus impl to avoid
	// unbounded cardinality).
	p.RecordApplyAttempt("worker-3", 42)
	p.RecordApplyAttempt("worker-3", 43)
	p.RecordApplyAttempt("worker-3", 44)
	p.RecordApplyAttempt("worker-7", 42)

	expected := strings.NewReader(`
# HELP parti_manager_apply_attempts_total Total invocations of applyAssignmentWithPrev counted before the (V, LR) stale gate. A higher rate after a NATS leader re-election indicates the watcher debounce did not collapse a burst.
# TYPE parti_manager_apply_attempts_total counter
parti_manager_apply_attempts_total{worker_id="worker-3"} 3
parti_manager_apply_attempts_total{worker_id="worker-7"} 1
`)
	require.NoError(t, testutil.GatherAndCompare(reg, expected, "parti_manager_apply_attempts_total"))
}
