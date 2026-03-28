package durable_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/durable"
	partitesting "github.com/arloliu/parti/v2/partitest"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestWorkerConsumer_WildcardSubjectTemplate verifies that a WorkerConsumer with a wildcard
// in the SubjectTemplate correctly receives messages matching that wildcard pattern.
//
// Scenario:
// - Stream: "wildcard-stream" with subjects "orders.>"
// - SubjectTemplate: "orders.{{.PartitionID}}.>"
// - Worker PartitionID: "worker1"
// - Resulting Filter: "orders.worker1.>"
//
// Expected Behavior:
// - "orders.worker1.create" -> RECEIVED
// - "orders.worker1.update" -> RECEIVED
// - "orders.worker2.create" -> IGNORED
func TestWorkerConsumer_WildcardSubjectTemplate(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}
	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	defer nc.Close()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "wildcard-stream"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"orders.>"},
	})
	require.NoError(t, err)

	var handled atomic.Int64
	receivedSubjects := make(chan string, 100)

	mh := func(c context.Context, msg jetstream.Msg) error {
		handled.Add(1)
		receivedSubjects <- msg.Subject()
		return msg.Ack()
	}

	// Configure WorkerConsumer with wildcard template
	helper, err := durable.NewWorkerConsumer(js, durable.WorkerConsumerConfig{
		StreamName:      streamName,
		ConsumerPrefix:  "wc-wildcard",
		SubjectTemplate: "orders.{{.PartitionID}}.>", // Wildcard at the end
		BatchSize:       10,
	}, mh)
	require.NoError(t, err)
	defer helper.Close(context.Background())

	// Assign "worker1" partition
	// Using "worker1" as the partition key which maps to {{.PartitionID}}
	assignments := []parti.Partition{{Keys: []string{"worker1"}}}
	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-node-1", assignments))

	// Wait for consumer to be ready
	require.Eventually(t, func() bool {
		subs := helper.WorkerSubjects()
		// Expect one subject: "orders.worker1.>"
		return len(subs) == 1 && subs[0] == "orders.worker1.>"
	}, 5*time.Second, 100*time.Millisecond)

	// Publish test messages
	messages := []struct {
		subject     string
		shouldMatch bool
	}{
		{"orders.worker1.create", true},
		{"orders.worker1.update", true},
		{"orders.worker1.deep.nested.event", true},
		{"orders.worker2.create", false}, // Wrong partition ID
		{"orders.worker1", false},
	}

	for _, m := range messages {
		_, err := js.Publish(ctx, m.subject, []byte("data"))
		require.NoError(t, err)
	}

	// Verify received messages
	timeout := time.After(2 * time.Second)
	expectedCount := 0
	for _, m := range messages {
		if m.shouldMatch {
			expectedCount++
		}
	}

	receivedCount := 0
	for receivedCount < expectedCount {
		select {
		case subj := <-receivedSubjects:
			// Verify we only get what we expect
			matchFound := false
			for _, m := range messages {
				if m.subject == subj {
					if !m.shouldMatch {
						t.Errorf("Received unexpected message for subject: %s", subj)
					}
					matchFound = true
					break
				}
			}
			if !matchFound {
				t.Errorf("Received message for unknown subject: %s", subj)
			}
			receivedCount++
		case <-timeout:
			t.Fatalf("Timeout waiting for messages. Got %d, want %d", receivedCount, expectedCount)
		}
	}

	// Ensure no extra messages leak through
	select {
	case subj := <-receivedSubjects:
		t.Errorf("Received unexpected extra message: %s", subj)
	case <-time.After(500 * time.Millisecond):
		// Clean pass
	}

	require.Equal(t, int64(expectedCount), handled.Load())
}
