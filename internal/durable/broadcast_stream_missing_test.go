package durable

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	partitesting "github.com/arloliu/parti/v2/partitest"
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

// TestBroadcastConsumer_ActionStreamMissing_LogsAndBacksOff pins the spec-
// required mapping for Broadcast's non-Dynamic ActionStreamMissing branch
// (docs/plans/self-healing/09-pr9-spec.md § "File-by-file caller contract"):
// Broadcast does not own stream lifecycle, so the stream-missing
// classification is logged for operator observability and folded into a
// backoff — it must NOT fall through to the default branch or cause the
// loop to exit silently.
func TestBroadcastConsumer_ActionStreamMissing_LogsAndBacksOff(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	const streamName = "BROADCAST_STREAM_MISSING"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{"bcsm.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	log := &captureWarnLogger{t: t}
	cfg := BroadcastConsumerConfig{
		StreamName:       streamName,
		ConsumerPrefix:   "bcsm",
		ConsumerID:       "worker-1",
		WildcardFilter:   "bcsm.>",
		BatchSize:        1,
		FetchTimeout:     1500 * time.Millisecond,
		Logger:           log,
		RecoveryStrategy: RecoverFromBeginning,
	}

	handler := messageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	bc, err := NewBroadcastConsumer(js, cfg, handler)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bc.Close(context.Background()) })

	parts := []types.Partition{{Keys: []string{"region", "us-east"}}}
	require.NoError(t, bc.UpdateWorkerConsumer(ctx, "worker-1", parts))

	durable := bc.durableName()
	require.Eventually(t, func() bool {
		_, infoErr := js.Consumer(ctx, streamName, durable)
		return infoErr == nil
	}, 5*time.Second, 25*time.Millisecond, "broadcast consumer must bind before stream deletion")

	require.NoError(t, js.DeleteStream(ctx, streamName))

	require.Eventually(t, func() bool {
		return log.hasWarn("broadcast consumer recovery classified stream missing")
	}, 10*time.Second, 50*time.Millisecond,
		"Broadcast's ActionStreamMissing case must emit the expected WARN log line; absence implies the case is unwired or fell through")
}
