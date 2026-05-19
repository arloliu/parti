package provision

// Same-package tests for classifyLiveError, isPermissionDeniedSurface, and
// probeWithAllowDirectRetry. These cover the classifier and retry algorithm
// with injected errors — no embedded NATS needed.
//
// These tests must live in package provision (not provision_test) so they can
// reach the unexported helpers.

import (
	"context"
	"errors"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// --- classifyLiveError table ---

func TestClassifyLiveError_Table(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	type tc struct {
		name      string
		ctx       context.Context
		err       error
		wantIs    []error // must match via errors.Is
		wantNotIs []error // must NOT match via errors.Is
	}

	tests := []tc{
		{
			name:   "ErrNoResponders → permission-denied (wraps ErrLiveValidation)",
			ctx:    ctx,
			err:    nats.ErrNoResponders,
			wantIs: []error{ErrLiveValidation, nats.ErrNoResponders},
		},
		{
			name:   "ErrPermissionViolation → permission-denied (wraps ErrLiveValidation)",
			ctx:    ctx,
			err:    nats.ErrPermissionViolation,
			wantIs: []error{ErrLiveValidation, nats.ErrPermissionViolation},
		},
		{
			name:   "ErrJetStreamNotEnabled → permission-denied after step1 (wraps ErrLiveValidation)",
			ctx:    ctx,
			err:    jetstream.ErrJetStreamNotEnabled,
			wantIs: []error{ErrLiveValidation, jetstream.ErrJetStreamNotEnabled},
		},
		{
			name:   "ErrJetStreamNotEnabledForAccount → permission-denied (wraps ErrLiveValidation)",
			ctx:    ctx,
			err:    jetstream.ErrJetStreamNotEnabledForAccount,
			wantIs: []error{ErrLiveValidation, jetstream.ErrJetStreamNotEnabledForAccount},
		},
		{
			name:      "ErrMsgNotFound should not be passed to classifyLiveError (but if it is → generic wrap)",
			ctx:       ctx,
			err:       jetstream.ErrMsgNotFound,
			wantIs:    []error{ErrLiveValidation},
			wantNotIs: []error{nats.ErrPermissionViolation},
		},
		{
			name:      "context.Canceled → returned as-is (no ErrLiveValidation wrap)",
			ctx:       ctx,
			err:       context.Canceled,
			wantIs:    []error{context.Canceled},
			wantNotIs: []error{ErrLiveValidation},
		},
		{
			name:      "context.DeadlineExceeded → returned as-is (no ErrLiveValidation wrap)",
			ctx:       ctx,
			err:       context.DeadlineExceeded,
			wantIs:    []error{context.DeadlineExceeded},
			wantNotIs: []error{ErrLiveValidation},
		},
		{
			name:      "cancelled ctx beats any error → ctx.Err() returned",
			ctx:       cancelledCtx,
			err:       nats.ErrPermissionViolation, // would otherwise be permission-denied
			wantIs:    []error{context.Canceled},
			wantNotIs: []error{ErrLiveValidation},
		},
		{
			name:      "generic/wrapped error → wraps ErrLiveValidation",
			ctx:       ctx,
			err:       errors.New("some nats error"),
			wantIs:    []error{ErrLiveValidation},
			wantNotIs: []error{nats.ErrPermissionViolation},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := classifyLiveError(tt.ctx, tt.err)
			require.Error(t, got)
			for _, want := range tt.wantIs {
				require.True(t, errors.Is(got, want),
					"expected errors.Is(got, %v) = true; got %v", want, got)
			}
			for _, notWant := range tt.wantNotIs {
				require.False(t, errors.Is(got, notWant),
					"expected errors.Is(got, %v) = false; got %v", notWant, got)
			}
		})
	}
}

// --- isPermissionDeniedSurface ---

func TestIsPermissionDeniedSurface_Table(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"ErrNoResponders+live ctx", ctx, nats.ErrNoResponders, true},
		{"ErrPermissionViolation+live ctx", ctx, nats.ErrPermissionViolation, true},
		{"ErrJetStreamNotEnabled+live ctx", ctx, jetstream.ErrJetStreamNotEnabled, true},
		{"generic error+live ctx", ctx, errors.New("oops"), false},
		{"ErrNoResponders+cancelled ctx", cancelledCtx, nats.ErrNoResponders, false},
		{"ErrPermissionViolation+cancelled ctx", cancelledCtx, nats.ErrPermissionViolation, false},
		{"nil error", ctx, nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isPermissionDeniedSurface(tt.ctx, tt.err)
			require.Equal(t, tt.want, got)
		})
	}
}

// --- probeWithAllowDirectRetry ---

// retryCall is a helper to build an injected probe or refresh function from a
// slice of returns: each successive call pops the next error off the slice.
type callSeq struct {
	returns []error
	calls   int
}

func newCallSeq(returns ...error) *callSeq { return &callSeq{returns: returns} }

func (s *callSeq) call(_ context.Context) error {
	if s.calls < len(s.returns) {
		err := s.returns[s.calls]
		s.calls++
		return err
	}
	return errors.New("callSeq: no more returns configured")
}

func TestProbeWithAllowDirectRetry_Success_FirstCall(t *testing.T) {
	t.Parallel()
	probe := newCallSeq(nil)
	refresh := newCallSeq() // should not be called
	err := probeWithAllowDirectRetry(context.Background(), probe.call, refresh.call)
	require.NoError(t, err)
	require.Equal(t, 1, probe.calls, "probe called once")
	require.Equal(t, 0, refresh.calls, "refresh not called on success")
}

func TestProbeWithAllowDirectRetry_ErrMsgNotFound_Success(t *testing.T) {
	t.Parallel()
	probe := newCallSeq(jetstream.ErrMsgNotFound)
	refresh := newCallSeq()
	err := probeWithAllowDirectRetry(context.Background(), probe.call, refresh.call)
	require.NoError(t, err)
	require.Equal(t, 1, probe.calls)
	require.Equal(t, 0, refresh.calls, "refresh not called: ErrMsgNotFound is success")
}

func TestProbeWithAllowDirectRetry_PermissionDenied_RetrySucceeds(t *testing.T) {
	t.Parallel()
	// First probe: permission denied. After refresh: success.
	probe := newCallSeq(nats.ErrPermissionViolation, nil)
	refresh := newCallSeq(nil)
	err := probeWithAllowDirectRetry(context.Background(), probe.call, refresh.call)
	require.NoError(t, err)
	require.Equal(t, 2, probe.calls, "probe called twice (initial + retry)")
	require.Equal(t, 1, refresh.calls, "refresh called once between attempts")
}

func TestProbeWithAllowDirectRetry_PermissionDenied_RetryAlsoFails(t *testing.T) {
	t.Parallel()
	// Both probes: permission denied → classified as ErrLiveValidation.
	probe := newCallSeq(nats.ErrPermissionViolation, nats.ErrPermissionViolation)
	refresh := newCallSeq(nil)
	err := probeWithAllowDirectRetry(context.Background(), probe.call, refresh.call)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrLiveValidation),
		"second permission denial must surface as ErrLiveValidation")
	require.Equal(t, 2, probe.calls)
	require.Equal(t, 1, refresh.calls)
}

func TestProbeWithAllowDirectRetry_PermissionDenied_RefreshFails(t *testing.T) {
	t.Parallel()
	// First probe: permission denied. Refresh fails with generic error.
	probe := newCallSeq(nats.ErrNoResponders)
	refresh := newCallSeq(errors.New("refresh error"))
	err := probeWithAllowDirectRetry(context.Background(), probe.call, refresh.call)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrLiveValidation),
		"refresh failure must also surface as ErrLiveValidation (generic wrap)")
	require.Equal(t, 1, probe.calls)
	require.Equal(t, 1, refresh.calls)
}

func TestProbeWithAllowDirectRetry_PermissionDenied_RefreshFindsStreamGone(t *testing.T) {
	t.Parallel()
	// First probe: permission denied. Refresh returns ErrStreamNotFound → OK.
	probe := newCallSeq(nats.ErrNoResponders)
	refresh := newCallSeq(jetstream.ErrStreamNotFound)
	err := probeWithAllowDirectRetry(context.Background(), probe.call, refresh.call)
	require.NoError(t, err, "ErrStreamNotFound on refresh is treated as success (stream gone)")
	require.Equal(t, 1, probe.calls)
	require.Equal(t, 1, refresh.calls)
}

func TestProbeWithAllowDirectRetry_GenericError_NoRetry(t *testing.T) {
	t.Parallel()
	// Non-permission generic error: no retry, classify immediately.
	probe := newCallSeq(errors.New("timeout or similar"))
	refresh := newCallSeq()
	err := probeWithAllowDirectRetry(context.Background(), probe.call, refresh.call)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrLiveValidation))
	require.Equal(t, 1, probe.calls)
	require.Equal(t, 0, refresh.calls, "no retry on non-permission error")
}

func TestProbeWithAllowDirectRetry_CancelledCtx_NoRetry(t *testing.T) {
	t.Parallel()
	// Cancelled context: isPermissionDeniedSurface returns false, so no retry.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	// Even a permission-surface error must not trigger retry when ctx is done.
	probe := newCallSeq(nats.ErrPermissionViolation)
	refresh := newCallSeq()
	err := probeWithAllowDirectRetry(cancelledCtx, probe.call, refresh.call)
	require.Error(t, err)
	// classifyLiveError with a cancelled ctx returns ctx.Err() directly.
	require.True(t, errors.Is(err, context.Canceled))
	require.Equal(t, 1, probe.calls)
	require.Equal(t, 0, refresh.calls, "no retry when context is cancelled")
}
