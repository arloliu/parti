package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// TestAssignmentMetricsCaching verifies cached values reflect setters/increments.
func TestAssignmentMetricsCaching(t *testing.T) {
	c := NewCollectorWithRegistry(prometheus.NewRegistry())

	c.SetUnassignedPartitions(5)
	c.SetLocalityRatio(0.75)
	c.IncMovedPartitions(3)

	u, l, m := c.GetAssignmentMetrics()
	if u != 5 || l != 0.75 || m != 3 {
		t.Fatalf("unexpected assignment metrics cache: unassigned=%d locality=%f moved=%d", u, l, m)
	}

	c.IncMovedPartitions(2)
	_, _, m = c.GetAssignmentMetrics()
	if m != 5 { // 3 + 2
		t.Fatalf("moved partitions total expected 5 got %d", m)
	}
}

// TestLatencyAndRecoverySummaries verifies histogram sample count & sum via summary helpers.
func TestLatencyAndRecoverySummaries(t *testing.T) {
	c := NewCollectorWithRegistry(prometheus.NewRegistry())
	// Initially zero
	lc, ls := c.PublishLatencySummary()
	if lc != 0 || ls != 0 {
		t.Fatalf("expected initial latency summary zero got count=%d sum=%f", lc, ls)
	}
	rc, rs := c.RecoveryDurationSummary()
	if rc != 0 || rs != 0 {
		t.Fatalf("expected initial recovery summary zero got count=%d sum=%f", rc, rs)
	}

	c.ObservePublishToConsumeLatency(10 * time.Millisecond)
	c.ObservePublishToConsumeLatency(20 * time.Millisecond)
	c.ObserveRecoveryDuration(2 * time.Second)
	c.ObserveRecoveryDuration(3 * time.Second)

	lc, ls = c.PublishLatencySummary()
	if lc != 2 || ls <= 0.029 || ls >= 0.031 { // approx 0.03s
		t.Fatalf("unexpected latency summary count=%d sum=%f", lc, ls)
	}
	rc, rs = c.RecoveryDurationSummary()
	if rc != 2 || rs < 4.9 || rs > 5.1 { // approx 5s total
		t.Fatalf("unexpected recovery summary count=%d sum=%f", rc, rs)
	}
}
