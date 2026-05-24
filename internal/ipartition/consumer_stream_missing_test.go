package ipartition

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/internal/durable"
	partitesting "github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/partition"
	"github.com/arloliu/parti/v2/types"
)

// captureWarnLogger records WARN-level messages so a test can assert that a
// specific log line was emitted by a production code path. Optional t enables
// pass-through to the test log for diagnosis.
type captureWarnLogger struct {
	mu    sync.Mutex
	warns []string
	t     *testing.T
}

var _ types.Logger = (*captureWarnLogger)(nil)

func (l *captureWarnLogger) Debug(msg string, kv ...any) {
	if l.t != nil {
		l.t.Logf("DEBUG: %s %s", msg, fmt.Sprint(kv...))
	}
}
func (l *captureWarnLogger) Info(msg string, kv ...any) {
	if l.t != nil {
		l.t.Logf("INFO: %s %s", msg, fmt.Sprint(kv...))
	}
}
func (l *captureWarnLogger) Warn(msg string, kv ...any) {
	l.mu.Lock()
	l.warns = append(l.warns, msg)
	l.mu.Unlock()
	if l.t != nil {
		l.t.Logf("WARN: %s %s", msg, fmt.Sprint(kv...))
	}
}
func (l *captureWarnLogger) Error(msg string, kv ...any) {
	if l.t != nil {
		l.t.Logf("ERROR: %s %s", msg, fmt.Sprint(kv...))
	}
}
func (l *captureWarnLogger) Fatal(msg string, _ ...any) {
	panic("captureWarnLogger.Fatal: " + msg)
}

func (l *captureWarnLogger) hasWarn(substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, w := range l.warns {
		if strings.Contains(w, substr) {
			return true
		}
	}

	return false
}

// TestJSConsumer_ActionStreamMissing_LogsAndBacksOff pins the spec-required
// mapping for JSConsumer's non-Dynamic ActionStreamMissing branch
// (docs/plans/self-healing/09-pr9-spec.md § "File-by-file caller contract"):
// JSConsumer does not own stream lifecycle, so the stream-missing
// classification is logged for operator observability and folded into a
// backoff — it must NOT fall through to the default branch or cause the
// loop to exit silently.
func TestJSConsumer_ActionStreamMissing_LogsAndBacksOff(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	const streamName = "JS_STREAM_MISSING"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{"jsm.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	log := &captureWarnLogger{t: t}

	c, err := NewJSConsumer(js, ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  1,
			SubjectPattern: "jsm.{{partition}}",
		},
		StreamName:       streamName,
		ConsumerName:     "jsm-consumer-0",
		Partition:        0,
		FetchTimeout:     1500 * time.Millisecond,
		Logger:           log,
		RecoveryStrategy: durable.RecoverFromBeginning,
	}, messageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil }))
	require.NoError(t, err)

	require.NoError(t, c.Start(ctx))
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = c.Stop(stopCtx)
	})

	// Wait for the JS consumer to be bound on the server.
	require.Eventually(t, func() bool {
		_, infoErr := js.Consumer(ctx, streamName, "jsm-consumer-0")
		return infoErr == nil
	}, 5*time.Second, 25*time.Millisecond, "JSConsumer must bind before stream deletion")

	require.NoError(t, js.DeleteStream(ctx, streamName))

	require.Eventually(t, func() bool {
		return log.hasWarn("js consumer recovery classified stream missing")
	}, 10*time.Second, 50*time.Millisecond,
		"JSConsumer's ActionStreamMissing case must emit the expected WARN log line; absence implies the case is unwired or fell through")
}
