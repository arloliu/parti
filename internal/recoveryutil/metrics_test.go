package recoveryutil

import (
	"testing"

	"github.com/stretchr/testify/require"
)

type mockWorkerConsumerMetrics struct {
	attemptReasons []string
	results        []string
	resultReasons  []string
	durations      []float64
}

func (m *mockWorkerConsumerMetrics) IncrementWorkerConsumerControlRetry(string)       {}
func (m *mockWorkerConsumerMetrics) RecordWorkerConsumerRetryBackoff(string, float64) {}
func (m *mockWorkerConsumerMetrics) SetWorkerConsumerSubjectsCurrent(int)             {}
func (m *mockWorkerConsumerMetrics) IncrementWorkerConsumerSubjectChange(string, int) {}
func (m *mockWorkerConsumerMetrics) IncrementWorkerConsumerGuardrailViolation(string) {}
func (m *mockWorkerConsumerMetrics) IncrementWorkerConsumerSubjectThresholdWarning()  {}
func (m *mockWorkerConsumerMetrics) RecordWorkerConsumerUpdate(string)                {}
func (m *mockWorkerConsumerMetrics) ObserveWorkerConsumerUpdateLatency(float64)       {}
func (m *mockWorkerConsumerMetrics) IncrementWorkerConsumerIteratorRestart(string)    {}
func (m *mockWorkerConsumerMetrics) IncrementWorkerConsumerIteratorEscalation(string) {}
func (m *mockWorkerConsumerMetrics) SetWorkerConsumerConsecutiveIteratorFailures(int) {}
func (m *mockWorkerConsumerMetrics) SetWorkerConsumerHealthStatus(bool)               {}
func (m *mockWorkerConsumerMetrics) IncrementWorkerConsumerPullSuppressed(string)     {}
func (m *mockWorkerConsumerMetrics) IncrementWorkerConsumerRecreationAttempt(reason string) {
	m.attemptReasons = append(m.attemptReasons, reason)
}

func (m *mockWorkerConsumerMetrics) RecordWorkerConsumerRecreation(result string, reason string) {
	m.results = append(m.results, result)
	m.resultReasons = append(m.resultReasons, reason)
}

func (m *mockWorkerConsumerMetrics) ObserveWorkerConsumerRecreationDuration(seconds float64) {
	m.durations = append(m.durations, seconds)
}

func TestBeginFinish_IteratorErrorSuccess(t *testing.T) {
	metrics := &mockWorkerConsumerMetrics{}
	attempt := Begin(metrics, "consumer_deleted")
	attempt.Finish(true)

	require.Equal(t, []string{"iterator_error"}, metrics.attemptReasons)
	require.Equal(t, []string{"success"}, metrics.results)
	require.Equal(t, []string{"iterator_error"}, metrics.resultReasons)
	require.Len(t, metrics.durations, 1)
	require.GreaterOrEqual(t, metrics.durations[0], 0.0)
}

func TestBeginFinish_NotFoundFailure(t *testing.T) {
	metrics := &mockWorkerConsumerMetrics{}
	attempt := Begin(metrics, "consumer_not_found_after_burst")
	attempt.Finish(false)

	require.Equal(t, []string{"not_found"}, metrics.attemptReasons)
	require.Equal(t, []string{"failure"}, metrics.results)
	require.Equal(t, []string{"not_found"}, metrics.resultReasons)
	require.Len(t, metrics.durations, 1)
	require.GreaterOrEqual(t, metrics.durations[0], 0.0)
}

func TestBeginFinish_UnknownReason(t *testing.T) {
	metrics := &mockWorkerConsumerMetrics{}
	attempt := Begin(metrics, "something_else")
	attempt.Finish(false)

	require.Equal(t, []string{"unknown"}, metrics.attemptReasons)
	require.Equal(t, []string{"failure"}, metrics.results)
	require.Equal(t, []string{"unknown"}, metrics.resultReasons)
}
