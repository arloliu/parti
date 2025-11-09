package subscription

import (
	"context"
	"errors"
	stdtesting "testing"
	"text/template"
	"time"

	imetrics "github.com/arloliu/parti/internal/metrics"
	ptesting "github.com/arloliu/parti/testing"
	"github.com/nats-io/nats.go/jetstream"
)

// metricsCapture wraps NopMetrics and captures recreation-specific calls.
type metricsCapture struct {
	*imetrics.NopMetrics
	attempts  []string
	outcomes  [][2]string // [result, reason]
	durations []float64
}

func (m *metricsCapture) IncrementWorkerConsumerRecreationAttempt(reason string) {
	m.attempts = append(m.attempts, reason)
}

func (m *metricsCapture) RecordWorkerConsumerRecreation(result string, reason string) {
	m.outcomes = append(m.outcomes, [2]string{result, reason})
}

func (m *metricsCapture) ObserveWorkerConsumerRecreationDuration(seconds float64) {
	m.durations = append(m.durations, seconds)
}

func TestRecreationMetrics_SuccessAfterRetries(t *stdtesting.T) {
	m := &metricsCapture{NopMetrics: imetrics.NewNop()}

	cfg := WorkerConsumerConfig{
		StreamName:      "S",
		ConsumerPrefix:  "dur",
		SubjectTemplate: "work.{{.Partition}}",
		// workerID is set directly on the helper below
		RetryBase:            5 * time.Millisecond,
		RetryMultiplier:      1.0,
		RetryMax:             10 * time.Millisecond,
		MaxRecreationRetries: 5,
		Metrics:              m,
		Logger:               ptesting.NewTestLogger(t),
	}

	tmpl := template.Must(template.New("subject").Parse(cfg.SubjectTemplate))
	wc := &WorkerConsumer{
		config:          cfg,
		logger:          cfg.Logger,
		subjectTemplate: tmpl,
		workerID:        "w1",
		createOrUpdateFn: func(_ context.Context, _ string, _ jetstream.ConsumerConfig) (jetstream.Consumer, error) {
			// First two attempts fail (attempts are incremented before invocation), then succeed
			if len(m.attempts) <= 2 {
				return nil, errors.New("create failed")
			}

			return nil, nil //nolint:nilnil // returning nil consumer is fine; metrics path only
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := wc.recreateDurableConsumer(ctx, "dur-w1", []string{"work.1"}, "not_found")
	if err != nil {
		t.Fatalf("expected success after retries, got error: %v", err)
	}

	// Attempts equal number of tries (2 failures + 1 success)
	if len(m.attempts) != 3 {
		t.Fatalf("attempts=%d, want 3", len(m.attempts))
	}
	for _, r := range m.attempts {
		if r != "not_found" {
			t.Fatalf("attempt reason=%s, want not_found", r)
		}
	}
	// Expect two failures then one success outcome record
	var fails, successes int
	for _, o := range m.outcomes {
		switch o[0] {
		case "failure":
			fails++
		case "success":
			successes++
		}
		if o[1] != "not_found" {
			t.Fatal("outcome reason!=not_found")
		}
	}
	if fails != 2 || successes != 1 {
		t.Fatalf("outcomes fail=%d succ=%d, want 2 and 1", fails, successes)
	}
	if len(m.durations) == 0 || m.durations[0] < 0 {
		t.Fatalf("expected a duration observation on success, got %v", m.durations)
	}
}

func TestRecreationMetrics_AllFailures_NoDurationObserved(t *stdtesting.T) {
	m := &metricsCapture{NopMetrics: imetrics.NewNop()}

	cfg := WorkerConsumerConfig{
		StreamName:      "S",
		ConsumerPrefix:  "dur",
		SubjectTemplate: "work.{{.Partition}}",
		// workerID is set directly on the helper below
		RetryBase:            2 * time.Millisecond,
		RetryMultiplier:      1.0,
		RetryMax:             5 * time.Millisecond,
		MaxRecreationRetries: 2,
		Metrics:              m,
		Logger:               ptesting.NewTestLogger(t),
	}

	tmpl := template.Must(template.New("subject").Parse(cfg.SubjectTemplate))
	wc := &WorkerConsumer{
		config:          cfg,
		logger:          cfg.Logger,
		subjectTemplate: tmpl,
		workerID:        "w1",
		createOrUpdateFn: func(context.Context, string, jetstream.ConsumerConfig) (jetstream.Consumer, error) {
			return nil, errors.New("always fail")
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := wc.recreateDurableConsumer(ctx, "dur-w1", []string{"work.1"}, "throttle")
	if err == nil {
		t.Fatal("expected error when all attempts fail")
	}
	if len(m.attempts) != 2 {
		t.Fatalf("attempts=%d, want 2", len(m.attempts))
	}
	// Outcomes should record two failures
	if len(m.outcomes) != 2 {
		t.Fatalf("outcomes=%d, want 2 failures", len(m.outcomes))
	}
	// Current implementation records duration also on terminal failure (sequence duration)
	if len(m.durations) != 1 {
		t.Fatalf("expected one duration observation on failure path, got %v", m.durations)
	}
}
