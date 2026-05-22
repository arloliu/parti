package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/provision"
)

// jsFor opens a JetStream context against the embedded server at url and
// registers connection cleanup.
func jsFor(t *testing.T, url string) jetstream.JetStream {
	t.Helper()

	nc, err := nats.Connect(url)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	return js
}

// validControlPlaneConfig returns a statically-valid provision.Config that
// provisions the full control-plane bucket set.
func validControlPlaneConfig() provision.Config {
	return provision.Config{
		APIVersion: provision.APIVersionV1,
		Instance:   "test",
		Policy:     provision.PolicyWarn,
		ControlPlane: &provision.ControlPlaneConfig{
			WorkerIDTTL:     1 * time.Hour,
			ElectionTimeout: 30 * time.Second,
			HeartbeatTTL:    10 * time.Second,
			AssignmentTTL:   0,
		},
	}
}

func TestRunProvision_HappyPath(t *testing.T) {
	js := jsFor(t, startAnonymousNATS(t))
	cfg := validControlPlaneConfig()

	outcome, err := runProvision(t.Context(), js, cfg)
	require.NoError(t, err)
	require.Equal(t, ClassOK, outcome.Class)

	// Plan reported the create actions; Apply executed them.
	require.NotEmpty(t, outcome.Plan.Actions, "plan should report create actions for an empty server")
	require.Len(t, outcome.Report.Executed, len(outcome.Plan.Actions))
	require.Empty(t, outcome.Report.Errors)
	require.False(t, outcome.Report.Aborted)
}

func TestRunProvision_ConvergedNoOp(t *testing.T) {
	js := jsFor(t, startAnonymousNATS(t))
	cfg := validControlPlaneConfig()

	// First run provisions everything.
	first, err := runProvision(t.Context(), js, cfg)
	require.NoError(t, err)
	require.Equal(t, ClassOK, first.Class)
	require.NotEmpty(t, first.Report.Executed)

	// Second run is a converged no-op: nothing left to plan or execute.
	second, err := runProvision(t.Context(), js, cfg)
	require.NoError(t, err)
	require.Equal(t, ClassOK, second.Class)
	require.Empty(t, second.Plan.Actions, "converged environment has no planned actions")
	require.Empty(t, second.Report.Executed)
	require.Empty(t, second.Report.Errors)
}

func TestRunProvision_InvalidConfig(t *testing.T) {
	js := jsFor(t, startAnonymousNATS(t))

	// An invalid APIVersion fails static validation in step 1.
	cfg := validControlPlaneConfig()
	cfg.APIVersion = "wrong/v9"

	outcome, err := runProvision(t.Context(), js, cfg)
	require.Error(t, err)
	require.ErrorIs(t, err, provision.ErrInvalidConfig)
	require.Equal(t, ClassValidation, outcome.Class)
}

func TestRunProvision_UnreachableServer(t *testing.T) {
	// JetStream context against a server that is not listening: step 2 Plan
	// fails with a non-cancellation error.
	nc, err := nats.Connect("nats://127.0.0.1:1",
		nats.RetryOnFailedConnect(false))
	if err == nil {
		t.Cleanup(nc.Close)
	}
	// If the connect itself failed, build a js against a closed connection
	// instead so Plan still fails downstream.
	if err != nil {
		dead := jsForDead(t)
		outcome, perr := runProvision(t.Context(), dead, validControlPlaneConfig())
		require.Error(t, perr)
		require.Equal(t, ClassTransientNATS, outcome.Class)

		return
	}

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	outcome, perr := runProvision(t.Context(), js, validControlPlaneConfig())
	require.Error(t, perr)
	require.Equal(t, ClassTransientNATS, outcome.Class)
}

// jsForDead returns a JetStream context bound to a connection that is closed
// immediately, so every JetStream call fails with a non-cancellation error.
func jsForDead(t *testing.T) jetstream.JetStream {
	t.Helper()

	nc := jsForDeadConn(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	return js
}

func jsForDeadConn(t *testing.T) *nats.Conn {
	t.Helper()

	nc, err := nats.Connect(startAnonymousNATS(t))
	require.NoError(t, err)
	nc.Close()

	return nc
}

func TestRunProvision_ApplyResourceError(t *testing.T) {
	js := jsFor(t, startAnonymousNATS(t))

	// A stream with Replicas: 3 against a single-node embedded server is
	// rejected by NATS — a non-cancellation error that populates
	// Report.Errors. The config is statically valid (Replicas >= 0).
	cfg := provision.Config{
		APIVersion: provision.APIVersionV1,
		Instance:   "test",
		Policy:     provision.PolicyWarn,
		Streams: []provision.StreamCfg{
			{
				Name:     "resource-error-stream",
				Subjects: []string{"resource.error.>"},
				Replicas: 3,
			},
		},
	}

	outcome, err := runProvision(t.Context(), js, cfg)
	require.Error(t, err)
	require.Equal(t, ClassApplyResourceError, outcome.Class)

	// The partial Report is preserved even on the error branch.
	require.NotEmpty(t, outcome.Report.Errors, "resource error must be preserved in the Report")
	require.False(t, outcome.Report.Aborted, "a resource error is not an abort")
}

// cancelAfterFirstKV wraps a JetStream context and cancels the test context
// the instant the first KV bucket is created — i.e. once provision.Apply has
// executed exactly one action and is back at the top of its action loop. The
// next loop-iteration ctx.Err() check then fires deterministically, with no
// sleep or polling. Only CreateKeyValue is overridden; every other method is
// promoted from the embedded real JetStream, including the read calls Apply
// makes during its pre-loop Validate/ValidateLive/Plan phase.
type cancelAfterFirstKV struct {
	jetstream.JetStream

	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelAfterFirstKV) CreateKeyValue(
	ctx context.Context, cfg jetstream.KeyValueConfig,
) (jetstream.KeyValue, error) {
	kv, err := c.JetStream.CreateKeyValue(ctx, cfg)
	if err == nil {
		c.once.Do(c.cancel)
	}

	return kv, err
}

func TestRunProvision_AbortedMidApply(t *testing.T) {
	js := jsFor(t, startAnonymousNATS(t))
	cfg := validControlPlaneConfig()

	// Pin the action-loop branch deterministically: the wrapping JetStream
	// cancels the ctx the instant the first control-plane bucket is created,
	// so provision.Apply's next loop-iteration ctx.Err() check aborts with a
	// partial Report{Aborted: true} — one action executed, the rest skipped.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	wrapped := &cancelAfterFirstKV{JetStream: js, cancel: cancel}

	outcome, err := runProvision(ctx, wrapped, cfg)

	require.Error(t, err)
	require.True(t, isCancellation(err), "error must wrap context cancellation")
	require.Equal(t, ClassApplyAborted, outcome.Class)
	require.True(t, outcome.Report.Aborted,
		"a cancel inside the action loop surfaces a partial Report with Aborted set")
	require.NotEmpty(t, outcome.Report.Executed,
		"the bucket created before cancellation is recorded as executed")
}

// TestClassifyApplyResult covers the full step-3 classification matrix
// deterministically, with no NATS server — including the zero-Report
// cancellation case (Apply cancelled inside its pre-action ValidateLive/Plan)
// that the embedded-server tests cannot force on demand.
func TestClassifyApplyResult(t *testing.T) {
	tests := []struct {
		name   string
		report provision.Report
		err    error
		want   provisionClass
	}{
		{
			name: "nil error is ClassOK",
			err:  nil, want: ClassOK,
		},
		{
			name:   "cancellation with a zero Report is ClassApplyAborted",
			report: provision.Report{}, err: context.Canceled,
			want: ClassApplyAborted,
		},
		{
			name:   "cancellation with a partial aborted Report is ClassApplyAborted",
			report: provision.Report{Aborted: true}, err: context.Canceled,
			want: ClassApplyAborted,
		},
		{
			name: "deadline-exceeded is ClassApplyAborted",
			err:  context.DeadlineExceeded, want: ClassApplyAborted,
		},
		{
			name:   "non-cancellation error with resource errors is ClassApplyResourceError",
			report: provision.Report{Errors: []provision.ResourceError{{}}},
			err:    errors.New("apply failed"), want: ClassApplyResourceError,
		},
		{
			name: "non-cancellation error with an empty Report is ClassTransientNATS",
			err:  errors.New("connection lost"), want: ClassTransientNATS,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, classifyApplyResult(tt.report, tt.err))
		})
	}
}

func TestRunProvision_AbortedPreAction(t *testing.T) {
	js := jsFor(t, startAnonymousNATS(t))
	cfg := validControlPlaneConfig() // valid, so cancellation is the only failure

	// Cancel up front: step-2 provision.Plan returns ctx.Err() from its
	// entry guard with a zero PlanResult. The class must still be
	// ClassApplyAborted — proving the error-based check, not Report.Aborted,
	// drives the classification.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	outcome, err := runProvision(ctx, js, cfg)
	require.Error(t, err)
	require.True(t, isCancellation(err), "error must wrap context cancellation")
	require.Equal(t, ClassApplyAborted, outcome.Class)
	require.False(t, outcome.Report.Aborted,
		"pre-action cancel yields a zero Report{} — Report.Aborted is false, yet class is ClassApplyAborted")
}
