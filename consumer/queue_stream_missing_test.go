package consumer

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// captureLogger records WARN-level messages in order so a test can assert
// a specific log line was emitted by a production code path. Tee t allows
// optional test-output forwarding for diagnosis.
type captureLogger struct {
	mu    sync.Mutex
	warns []string
	t     *testing.T
}

var _ types.Logger = (*captureLogger)(nil)

func (l *captureLogger) Debug(msg string, kv ...any) {
	if l.t != nil {
		l.t.Logf("DEBUG: %s %s", msg, fmt.Sprint(kv...))
	}
}
func (l *captureLogger) Info(msg string, kv ...any) {
	if l.t != nil {
		l.t.Logf("INFO: %s %s", msg, fmt.Sprint(kv...))
	}
}
func (l *captureLogger) Warn(msg string, kv ...any) {
	l.mu.Lock()
	l.warns = append(l.warns, msg)
	l.mu.Unlock()
	if l.t != nil {
		l.t.Logf("WARN: %s %s", msg, fmt.Sprint(kv...))
	}
}
func (l *captureLogger) Error(msg string, kv ...any) {
	if l.t != nil {
		l.t.Logf("ERROR: %s %s", msg, fmt.Sprint(kv...))
	}
}
func (l *captureLogger) Fatal(msg string, _ ...any) {
	panic("captureLogger.Fatal: " + msg)
}

func (l *captureLogger) hasWarn(substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, w := range l.warns {
		if strings.Contains(w, substr) {
			return true
		}
	}

	return false
}

// TestQueue_ActionStreamMissing_LogsAndBacksOff pins the spec-required
// mapping for Queue's non-Dynamic ActionStreamMissing branch
// (docs/plans/self-healing/09-pr9-spec.md § "File-by-file caller contract"):
// Queue does not own stream lifecycle, so the stream-missing classification
// is logged for operator observability and folded into a backoff — it must
// NOT fall through to the default branch or cause the loop to exit silently.
//
// Without the case wire-up, a future refactor that deletes the
// ActionStreamMissing case would let the switch fall through with no
// backoff and no log, generating a tight recreate-call loop.
func TestQueue_ActionStreamMissing_LogsAndBacksOff(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	const streamName = "QUEUE_STREAM_MISSING"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{"qsm.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	log := &captureLogger{t: t}
	handler := MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	q, err := NewQueue(js, streamName, "qsm-workers", "qsm.>", handler,
		WithLogger(log),
		WithFetchTimeout(1*time.Second),
		WithRecoveryStrategy(RecoverFromBeginning),
	)
	require.NoError(t, err)

	require.NoError(t, q.Start(ctx))

	// Wait for the queue's consumer to be bound on the server so a
	// subsequent stream delete actually triggers the in-flight iterator
	// to surface a recovery-classified failure.
	stream, err := js.Stream(ctx, streamName)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, infoErr := stream.Consumer(ctx, "qsm-workers")
		return infoErr == nil
	}, 5*time.Second, 25*time.Millisecond, "queue consumer must bind before stream deletion")

	// Delete the stream — this drives the iterator's Next() to surface a
	// consumer-gone classification, recovery.Controller.Classify's
	// CreateOrUpdateConsumer recreate returns ErrStreamNotFound, and the
	// switch enters ActionStreamMissing.
	require.NoError(t, js.DeleteStream(ctx, streamName))

	require.Eventually(t, func() bool {
		return log.hasWarn("queue consumer recovery classified stream missing")
	}, 8*time.Second, 50*time.Millisecond,
		"Queue's ActionStreamMissing case must emit the expected WARN log line; absence implies the case is unwired or fell through")

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	require.NoError(t, q.Stop(stopCtx))
}
