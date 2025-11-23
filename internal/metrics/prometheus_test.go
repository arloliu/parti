package metrics

import (
	"testing"

	"github.com/arloliu/parti/types"
	"github.com/prometheus/client_golang/prometheus"
)

func TestPrometheus_WorkerConsumer_UpdateMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	mc := NewPrometheus(reg, "parti")

	mc.RecordWorkerConsumerUpdate("success")
	mc.RecordWorkerConsumerUpdate("failure")
	mc.ObserveWorkerConsumerUpdateLatency(0.05)
	mc.ObserveWorkerConsumerUpdateLatency(0.15)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather error: %v", err)
	}
	var resultsSamples, latencySamples int
	for _, mf := range mfs {
		switch mf.GetName() {
		case "parti_worker_consumer_update_results_total":
			resultsSamples = len(mf.Metric)
		case "parti_worker_consumer_update_latency_seconds":
			latencySamples = len(mf.Metric)
		}
	}
	if resultsSamples < 2 { // success + failure
		t.Fatalf("expected >=2 samples for update_results_total, got %d", resultsSamples)
	}
	if latencySamples == 0 {
		t.Fatal("expected latency histogram to have samples")
	}
}

func TestPrometheus_Manager_StateTransition(t *testing.T) {
	reg := prometheus.NewRegistry()
	mc := NewPrometheus(reg, "parti")

	mc.RecordStateTransition(types.StateStable, types.StateScaling, 0.12)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather error: %v", err)
	}
	found := false
	for _, mf := range mfs {
		if mf.GetName() == "parti_manager_state_transitions_total" {
			found = true
			if len(mf.Metric) == 0 {
				t.Fatal("expected state_transitions_total to have samples")
			}
			break
		}
	}
	if !found {
		t.Fatal("missing metric family parti_manager_state_transitions_total")
	}
}

// Health status should toggle between healthy/unhealthy values.
func TestPrometheus_WorkerConsumer_HealthToggle(t *testing.T) {
	reg := prometheus.NewRegistry()
	mc := NewPrometheus(reg, "parti")

	mc.SetWorkerConsumerHealthStatus(true)
	mc.SetWorkerConsumerHealthStatus(false)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather error: %v", err)
	}
	var values []float64
	for _, mf := range mfs {
		if mf.GetName() == "parti_worker_consumer_health_status" {
			for _, m := range mf.Metric {
				if m.Gauge != nil {
					values = append(values, m.Gauge.GetValue())
				}
			}
		}
	}
	if len(values) == 0 {
		t.Fatal("expected at least one health status sample")
	}
	// Last sample should reflect most recent Set (false => 0)
	if values[len(values)-1] != 0 {
		t.Fatalf("expected last health status value 0 (unhealthy), got %v", values[len(values)-1])
	}
}

// Recreation sequence metrics should produce attempts, outcomes and duration samples.
func TestPrometheus_WorkerConsumer_RecreationMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	mc := NewPrometheus(reg, "parti")

	mc.IncrementWorkerConsumerRecreationAttempt("missing")
	mc.IncrementWorkerConsumerRecreationAttempt("policy_change")
	mc.RecordWorkerConsumerRecreation("success", "missing")
	mc.RecordWorkerConsumerRecreation("failure", "policy_change")
	mc.ObserveWorkerConsumerRecreationDuration(0.22)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather error: %v", err)
	}
	var attempts, outcomes, duration int
	for _, mf := range mfs {
		switch mf.GetName() {
		case "parti_worker_consumer_recreation_attempts_total":
			attempts = len(mf.Metric)
		case "parti_worker_consumer_recreations_total":
			outcomes = len(mf.Metric)
		case "parti_worker_consumer_recreation_duration_seconds":
			duration = len(mf.Metric)
		}
	}
	if attempts < 2 {
		t.Fatalf("expected >=2 recreation attempts samples, got %d", attempts)
	}
	if outcomes < 2 {
		t.Fatalf("expected >=2 recreation outcome samples, got %d", outcomes)
	}
	if duration == 0 {
		t.Fatal("expected recreation duration histogram to have samples")
	}
}

// Backoff + retry metrics should record both counters and histogram observations.
func TestPrometheus_WorkerConsumer_RetryBackoffMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	mc := NewPrometheus(reg, "parti")

	mc.IncrementWorkerConsumerControlRetry("info")
	mc.IncrementWorkerConsumerControlRetry("update")
	mc.RecordWorkerConsumerRetryBackoff("info", 0.05)
	mc.RecordWorkerConsumerRetryBackoff("update", 0.12)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather error: %v", err)
	}
	var retries, backoffs int
	for _, mf := range mfs {
		switch mf.GetName() {
		case "parti_worker_consumer_control_retries_total":
			retries = len(mf.Metric)
		case "parti_worker_consumer_retry_backoff_seconds":
			backoffs = len(mf.Metric)
		}
	}
	if retries < 2 {
		t.Fatalf("expected >=2 retry counter samples, got %d", retries)
	}
	if backoffs < 2 {
		t.Fatalf("expected >=2 backoff histogram samples, got %d", backoffs)
	}
}
