package provision_test

// Embedded-NATS integration tests for the application-stream apply path and
// the ValidateLive per-stream probe. Shared helpers (newJS, createStream,
// safeUpdateCtx, streamOnlyCfg, defaultStreamCfg, markedStream) live in the
// other integration test files in this package.

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/provision"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// liveOrdersStreamConfig fetches the current StreamConfig for the "orders"
// application stream (raw name, no "KV_" prefix).
func liveOrdersStreamConfig(t *testing.T, js jetstream.JetStream) jetstream.StreamConfig {
	t.Helper()
	stream, err := js.Stream(context.Background(), "orders")
	require.NoError(t, err)
	info, err := stream.Info(context.Background())
	require.NoError(t, err)

	return info.Config
}

// --- create-stream ----------------------------------------------------------

func TestApplyStream_CreateStream_CreatesMarkedStream(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	cfg := streamOnlyCfg(provision.PolicyWarn, defaultStreamCfg())
	rep, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.False(t, rep.Aborted)
	require.Empty(t, rep.Errors)
	require.Len(t, rep.Executed, 1)
	require.Equal(t, provision.ActionCreateStream, rep.Executed[0].Kind)
	require.Equal(t, "orders", rep.Executed[0].Name)

	live := liveOrdersStreamConfig(t, js)
	marker := provision.ParseMarker(live.Metadata)
	require.True(t, marker.IsManaged())
	require.Equal(t, provision.ComponentStream, marker.Component)
	require.Equal(t, "prod", marker.Instance)
	require.Equal(t, []string{"orders.>"}, live.Subjects)
}

func TestApplyStream_CreateStream_RerunIsNoOp(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	cfg := streamOnlyCfg(provision.PolicySafeUpdate, defaultStreamCfg())

	first, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, first.Errors)
	require.Len(t, first.Executed, 1)

	// Second Apply: the stream now exists and is converged, so Plan emits
	// nothing for it.
	plan, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)
	for _, a := range plan.Actions {
		require.NotEqual(t, "orders", a.Name, "a converged stream needs no action")
	}

	second, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, second.Errors)
	require.Empty(t, second.Executed, "converged stream needs no second apply")
}

// --- update-stream ----------------------------------------------------------

func TestApplyStream_SafeUpdate_MaxAgeChange_Reconciles(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	// Live stream: MaxAge = 1h, marked.
	createStream(t, js, markedStream("orders", func(c *jetstream.StreamConfig) {
		c.MaxAge = 1 * time.Hour
	}))

	// Config desires MaxAge = 2h.
	streamCfg := provision.StreamCfg{
		Name:     "orders",
		Subjects: []string{"orders.>"},
		Storage:  "memory",
		MaxAge:   2 * time.Hour,
	}
	cfg := streamOnlyCfg(provision.PolicySafeUpdate, streamCfg)

	rep, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)
	require.Len(t, rep.Executed, 1)
	require.Equal(t, provision.ActionUpdateStream, rep.Executed[0].Kind)

	live := liveOrdersStreamConfig(t, js)
	require.Equal(t, 2*time.Hour, live.MaxAge, "update-stream reconciled the MaxAge drift")
}

func TestApplyStream_ServerRejectedUpdate_SurfacesResourceError(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	// Live stream is workqueue retention, marked. A workqueue stream rejects a
	// Subjects change that would let a subject be captured by an overlapping
	// stream — but more reliably, NATS rejects shrinking subjects on a
	// workqueue stream that still has the removed subject's messages. Simplest
	// deterministic server rejection: give the config an empty/invalid update.
	//
	// Use a Subjects value the server rejects: a subject already claimed by
	// another stream. Create a second stream owning "shared.>", then try to
	// update "orders" to capture "shared.>" — NATS rejects the overlap.
	createStream(t, js, markedStream("claimer", func(c *jetstream.StreamConfig) {
		c.Subjects = []string{"shared.>"}
	}))
	createStream(t, js, markedStream("orders", func(c *jetstream.StreamConfig) {
		c.Subjects = []string{"orders.>"}
	}))

	streamCfg := provision.StreamCfg{
		Name:     "orders",
		Subjects: []string{"shared.>"}, // overlaps "claimer" — server rejects
		Storage:  "memory",
	}
	cfg := streamOnlyCfg(provision.PolicySafeUpdate, streamCfg)

	rep, err := provision.Apply(ctx, js, cfg)
	require.Error(t, err, "a server-rejected update must fail Apply")
	require.False(t, rep.Aborted, "a server rejection is a resource error, not an abort")
	require.NotEmpty(t, rep.Errors)
	require.Equal(t, provision.ActionUpdateStream, rep.Errors[0].Kind)
	require.Equal(t, "orders", rep.Errors[0].Name)
}

// --- stamp-stream-marker ----------------------------------------------------

func TestApplyStream_Adopt_UnmarkedStream_StampsMarker(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	// Pre-create an unmarked application stream.
	createStream(t, js, jetstream.StreamConfig{
		Name:     "orders",
		Subjects: []string{"orders.>"},
		Storage:  jetstream.MemoryStorage,
	})

	cfg := streamOnlyCfg(provision.PolicyAdopt, defaultStreamCfg())
	rep, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)
	require.Len(t, rep.Executed, 1)
	require.Equal(t, provision.ActionStampStreamMarker, rep.Executed[0].Kind)

	live := liveOrdersStreamConfig(t, js)
	marker := provision.ParseMarker(live.Metadata)
	require.True(t, marker.IsManaged())
	require.Equal(t, provision.ComponentStream, marker.Component)
	require.Equal(t, "prod", marker.Instance)
}

func TestApplyStream_Adopt_PreservesLiveFields(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	// Pre-create an unmarked stream with a Description and a non-Parti key.
	createStream(t, js, jetstream.StreamConfig{
		Name:        "orders",
		Subjects:    []string{"orders.>"},
		Storage:     jetstream.MemoryStorage,
		Description: "owned by team X",
		MaxAge:      42 * time.Second,
		Metadata:    map[string]string{"custom.io/team": "infra"},
	})

	cfg := streamOnlyCfg(provision.PolicyAdopt, defaultStreamCfg())
	rep, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)

	live := liveOrdersStreamConfig(t, js)
	require.Equal(t, "owned by team X", live.Description, "Description preserved by adoption")
	require.Equal(t, 42*time.Second, live.MaxAge, "MaxAge preserved by adoption")
	require.Equal(t, "infra", live.Metadata["custom.io/team"], "non-Parti metadata preserved")
	require.True(t, provision.ParseMarker(live.Metadata).IsManaged())
}

// --- end-to-end ladder -------------------------------------------------------

func TestApplyStream_EndToEnd_CreateThenSafeUpdate(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	// Step 1: warn creates the stream.
	createCfg := streamOnlyCfg(provision.PolicyWarn, provision.StreamCfg{
		Name:     "orders",
		Subjects: []string{"orders.>"},
		Storage:  "memory",
		MaxAge:   1 * time.Hour,
	})
	rep, err := provision.Apply(ctx, js, createCfg)
	require.NoError(t, err)
	require.Len(t, rep.Executed, 1)
	require.Equal(t, provision.ActionCreateStream, rep.Executed[0].Kind)

	// Step 2: safe-update with a changed MaxAge reconciles in place.
	updateCfg := streamOnlyCfg(provision.PolicySafeUpdate, provision.StreamCfg{
		Name:     "orders",
		Subjects: []string{"orders.>"},
		Storage:  "memory",
		MaxAge:   3 * time.Hour,
	})
	rep, err = provision.Apply(ctx, js, updateCfg)
	require.NoError(t, err)
	require.Len(t, rep.Executed, 1)
	require.Equal(t, provision.ActionUpdateStream, rep.Executed[0].Kind)

	live := liveOrdersStreamConfig(t, js)
	require.Equal(t, 3*time.Hour, live.MaxAge)
}

// --- ValidateLive stream probe ----------------------------------------------

func TestValidateLive_StreamProbe_ExistingStream_OK(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	createStream(t, js, markedStream("orders", func(c *jetstream.StreamConfig) {
		c.Subjects = []string{"orders.>"}
	}))

	cfg := streamOnlyCfg(provision.PolicySafeUpdate, defaultStreamCfg())
	rep, err := provision.ValidateLive(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)
}

func TestValidateLive_StreamProbe_MissingStream_OK(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	// The stream does not exist — ErrStreamNotFound is not a probe failure
	// (Apply will create it).
	cfg := streamOnlyCfg(provision.PolicyWarn, defaultStreamCfg())
	rep, err := provision.ValidateLive(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)
}

func TestValidateLive_StreamProbe_CancelledContext_ReturnsCtxErr(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cfg := streamOnlyCfg(provision.PolicyWarn, defaultStreamCfg())
	rep, err := provision.ValidateLive(ctx, js, cfg)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, rep)
}
