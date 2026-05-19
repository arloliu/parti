package provision_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/provision"
)

// TestValidateLiveDynamicConsumers_NoConsumers asserts that an empty
// DynamicConsumerCfg slice returns nil without contacting the server.
func TestValidateLiveDynamicConsumers_NoConsumers(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, provision.ValidateLiveDynamicConsumers(ctx, js, nil))
	require.NoError(t, provision.ValidateLiveDynamicConsumers(ctx, js, []provision.DynamicConsumerCfg{}))
}

// TestValidateLiveDynamicConsumers_WorkQueuePolicy_DefaultRecoveryDisabled
// asserts that the v1 builder's default RecoveryDisabled passes the
// WorkQueuePolicy check (positive case).
func TestValidateLiveDynamicConsumers_WorkQueuePolicy_DefaultRecoveryDisabled(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "wq-stream",
		Subjects:  []string{"wq.>"},
		Retention: jetstream.WorkQueuePolicy,
		Storage:   jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	err = provision.ValidateLiveDynamicConsumers(ctx, js, []provision.DynamicConsumerCfg{
		{StreamName: "wq-stream", ConsumerPrefix: "p", SubjectTemplate: "wq.{{.PartitionID}}"},
	})
	require.NoError(t, err, "RecoveryDisabled must pass against WorkQueuePolicy")
}

// TestValidateLiveDynamicConsumers_NonWorkQueueStream_Success asserts that
// a standard Interest/Limits policy stream is accepted unconditionally.
func TestValidateLiveDynamicConsumers_NonWorkQueueStream_Success(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "interest-stream",
		Subjects: []string{"int.>"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	require.NoError(t, provision.ValidateLiveDynamicConsumers(ctx, js, []provision.DynamicConsumerCfg{
		{StreamName: "interest-stream", ConsumerPrefix: "p", SubjectTemplate: "int.{{.PartitionID}}"},
	}))
}

// TestValidateLiveDynamicConsumers_NonExistentStream_Success asserts the
// best-effort connectivity contract: missing streams do not cause failure
// (the runtime check silently ignores stream-info fetch failures).
func TestValidateLiveDynamicConsumers_NonExistentStream_Success(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, provision.ValidateLiveDynamicConsumers(ctx, js, []provision.DynamicConsumerCfg{
		{StreamName: "does-not-exist", ConsumerPrefix: "p", SubjectTemplate: "x.{{.PartitionID}}"},
	}))
}

// TestCheckWorkQueueRecoveryCompat_RejectsRecoverFromNew exercises the
// shared-implementation contract: provision.ValidateLiveDynamicConsumers
// and consumer.Dynamic.Update both call consumer.CheckWorkQueueRecoveryCompat,
// so the WorkQueuePolicy rejection error is byte-equivalent across both
// callsites. Provision's v1 default (RecoveryDisabled) cannot exercise the
// rejection path itself, so this test reaches directly at the shared
// helper to prove the contract.
func TestCheckWorkQueueRecoveryCompat_RejectsRecoverFromNew(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "wq-reject",
		Subjects:  []string{"wqr.>"},
		Retention: jetstream.WorkQueuePolicy,
		Storage:   jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	got := consumer.CheckWorkQueueRecoveryCompat(ctx, js, "wq-reject", consumer.RecoverFromNew)
	require.Error(t, got)
	require.ErrorIs(t, got, consumer.ErrInvalidConfig)
	require.Contains(t, got.Error(), "RecoverFromNew is incompatible with WorkQueuePolicy")
}

// TestValidateLive_WiresDynamicConsumers asserts that ValidateLive invokes
// ValidateLiveDynamicConsumers for cfg.DynamicConsumers. Since the v1
// builder's RecoveryDisabled default always passes WorkQueuePolicy, we
// assert that ValidateLive returns nil for a WorkQueuePolicy stream
// configured with a dynamic-consumer entry (proving the wiring is exercised
// without false positives).
func TestValidateLive_WiresDynamicConsumers_Pass(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "vlw-stream",
		Subjects:  []string{"vlw.>"},
		Retention: jetstream.WorkQueuePolicy,
		Storage:   jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	report, err := provision.ValidateLive(ctx, js, provision.Config{
		APIVersion: provision.APIVersionV1,
		DynamicConsumers: []provision.DynamicConsumerCfg{
			{StreamName: "vlw-stream", ConsumerPrefix: "p", SubjectTemplate: "vlw.{{.PartitionID}}"},
		},
	})
	require.NoError(t, err)
	require.Empty(t, report.Errors)
}

// TestValidateLiveDynamicConsumers_RespectsCancellation pins the
// cancellation contract: ctx.Err() is returned without further wrapping.
func TestValidateLiveDynamicConsumers_RespectsCancellation(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := provision.ValidateLiveDynamicConsumers(ctx, js, []provision.DynamicConsumerCfg{
		{StreamName: "x", ConsumerPrefix: "p", SubjectTemplate: "x.{{.PartitionID}}"},
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled), "expected ctx.Canceled, got %v", err)
}
