package parti

import (
	"sync"
	"testing"

	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/stretchr/testify/require"
)

// recordingApplyAttempts embeds NopMetrics and overrides RecordApplyAttempt.
// Embedding picks up no-op impls for the other MetricsCollector methods
// so tests do not need to enumerate them.
type recordingApplyAttempts struct {
	*metrics.NopMetrics
	mu    sync.Mutex
	calls []applyAttemptCall
}

type applyAttemptCall struct {
	workerID string
	version  int64
}

func newRecordingApplyAttempts() *recordingApplyAttempts {
	return &recordingApplyAttempts{NopMetrics: metrics.NewNop()}
}

func (r *recordingApplyAttempts) RecordApplyAttempt(workerID string, version int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, applyAttemptCall{workerID, version})
}

func TestApplyAssignmentWithPrev_RecordsOneAttemptPerCall(t *testing.T) {
	rm := newRecordingApplyAttempts()
	m := newTestManagerWithMetrics(t, rm)

	m.handoffCoordinator = &recordingCoordinator{}

	_ = m.applyAssignment(Assignment{Version: 1})
	_ = m.applyAssignment(Assignment{Version: 2})
	_ = m.applyAssignment(Assignment{Version: 3})

	rm.mu.Lock()
	defer rm.mu.Unlock()
	require.Len(t, rm.calls, 3)
	require.Equal(t, int64(1), rm.calls[0].version)
	require.Equal(t, int64(2), rm.calls[1].version)
	require.Equal(t, int64(3), rm.calls[2].version)
	require.Equal(t, m.WorkerID(), rm.calls[0].workerID)
}
