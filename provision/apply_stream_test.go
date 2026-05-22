package provision

// Same-package unit tests for the stream apply seam: applyCreateStreamAction,
// applyUpdateStreamAction, applyStampStreamMarkerAction, and the applyPlan
// wiring of the three stream action kinds. These use a fake streamManager so
// they run without a live NATS server.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// --- fake streamManager seam -------------------------------------------------

// fakeStreamManager drives the stream apply helpers deterministically. infoCfg
// is the StreamConfig StreamInfo returns; infoErr / createErr / updateErr /
// deleteErr force the corresponding seam method to fail. created / updated /
// deleted record whether the methods ran and the config they were handed.
type fakeStreamManager struct {
	infoCfg jetstream.StreamConfig
	infoErr error

	createErr error
	created   bool
	createGot jetstream.StreamConfig

	updateErr error
	updated   bool
	updateGot jetstream.StreamConfig

	deleteErr error
	deleted   bool
}

func (m *fakeStreamManager) StreamInfo(_ context.Context, _ string) (*jetstream.StreamInfo, error) {
	if m.infoErr != nil {
		return nil, m.infoErr
	}

	return &jetstream.StreamInfo{Config: m.infoCfg}, nil
}

func (m *fakeStreamManager) CreateStream(_ context.Context, cfg jetstream.StreamConfig) error {
	m.created = true
	m.createGot = cfg

	return m.createErr
}

func (m *fakeStreamManager) UpdateStream(_ context.Context, cfg jetstream.StreamConfig) error {
	m.updated = true
	m.updateGot = cfg

	return m.updateErr
}

func (m *fakeStreamManager) DeleteStream(_ context.Context, _ string) error {
	m.deleted = true

	return m.deleteErr
}

// markedAppStream returns a synthetic Parti-marked application stream config.
func markedAppStream(mutate ...func(*jetstream.StreamConfig)) jetstream.StreamConfig {
	cfg := jetstream.StreamConfig{
		Name:     "orders",
		Subjects: []string{"orders.>"},
		Storage:  jetstream.MemoryStorage,
		Metadata: BuildMarker(ComponentStream, "prod"),
	}
	for _, m := range mutate {
		m(&cfg)
	}

	return cfg
}

// ordersStreamCfg returns a StreamCfg matching markedAppStream's shape.
func ordersStreamCfg(mutate ...func(*StreamCfg)) StreamCfg {
	sc := StreamCfg{
		Name:     "orders",
		Subjects: []string{"orders.>"},
		Storage:  "memory",
		Discard:  "old",
	}
	for _, m := range mutate {
		m(&sc)
	}

	return sc
}

// --- applyCreateStreamAction -------------------------------------------------

func TestApplyCreateStream_HappyPath_CreatesStream(t *testing.T) {
	t.Parallel()

	mgr := &fakeStreamManager{}
	want := buildStreamConfig(ordersStreamCfg(), "prod")
	action := PlannedAction{Kind: ActionCreateStream, Name: "orders", Resource: want}

	executed, err := applyCreateStreamAction(context.Background(), mgr, action)
	require.NoError(t, err)
	require.Equal(t, ActionCreateStream, executed.Kind)
	require.Equal(t, "orders", executed.Name)
	require.False(t, executed.Raced)
	require.True(t, mgr.created)
	require.Equal(t, want.Name, mgr.createGot.Name)
}

func TestApplyCreateStream_Race_RacedTrue(t *testing.T) {
	t.Parallel()

	mgr := &fakeStreamManager{createErr: jetstream.ErrStreamNameAlreadyInUse}
	action := PlannedAction{
		Kind: ActionCreateStream, Name: "orders",
		Resource: buildStreamConfig(ordersStreamCfg(), "prod"),
	}

	executed, err := applyCreateStreamAction(context.Background(), mgr, action)
	require.NoError(t, err)
	require.True(t, executed.Raced, "ErrStreamNameAlreadyInUse is a Plan->Apply race, recorded as success")
}

func TestApplyCreateStream_WrongResourceType_FailsFast(t *testing.T) {
	t.Parallel()

	mgr := &fakeStreamManager{}
	// A *jetstream.StreamConfig pointer (wrong: create-stream carries a value).
	cfg := buildStreamConfig(ordersStreamCfg(), "prod")
	action := PlannedAction{Kind: ActionCreateStream, Name: "orders", Resource: &cfg}

	_, err := applyCreateStreamAction(context.Background(), mgr, action)
	require.Error(t, err)
	require.Contains(t, err.Error(), "wrong Resource type")
	require.False(t, mgr.created, "no write on a wrong-type guard failure")
}

func TestApplyCreateStream_Cancelled_PropagatesCancellation(t *testing.T) {
	t.Parallel()

	mgr := &fakeStreamManager{createErr: context.Canceled}
	action := PlannedAction{
		Kind: ActionCreateStream, Name: "orders",
		Resource: buildStreamConfig(ordersStreamCfg(), "prod"),
	}

	_, err := applyCreateStreamAction(context.Background(), mgr, action)
	require.ErrorIs(t, err, context.Canceled)
}

func TestApplyCreateStream_ServerRejected_FailsFast(t *testing.T) {
	t.Parallel()

	mgr := &fakeStreamManager{createErr: errors.New("invalid subject")}
	action := PlannedAction{
		Kind: ActionCreateStream, Name: "orders",
		Resource: buildStreamConfig(ordersStreamCfg(), "prod"),
	}

	_, err := applyCreateStreamAction(context.Background(), mgr, action)
	require.Error(t, err)
	require.False(t, errors.Is(err, context.Canceled))
	require.Contains(t, err.Error(), "invalid subject")
}

// --- applyUpdateStreamAction -------------------------------------------------

// updateStreamAction builds an ActionUpdateStream PlannedAction with the given
// Before/After.
func updateStreamAction(before, after jetstream.StreamConfig) PlannedAction {
	return PlannedAction{
		Kind:     ActionUpdateStream,
		Name:     "orders",
		Resource: &UpdateStreamResource{Before: before, After: after},
	}
}

func ordersStreamCfgs(sc StreamCfg) map[string]StreamCfg {
	return map[string]StreamCfg{sc.Name: sc}
}

func TestApplyUpdateStream_HappyPath_WritesTarget(t *testing.T) {
	t.Parallel()

	// Live stream drifts on MaxAge (mutable); Before matches live.
	live := markedAppStream(func(c *jetstream.StreamConfig) { c.MaxAge = 99 * time.Second })
	sc := ordersStreamCfg(func(s *StreamCfg) { s.MaxAge = 10 * time.Second })
	mgr := &fakeStreamManager{infoCfg: live}

	executed, err := applyUpdateStreamAction(context.Background(), mgr,
		ordersStreamCfgs(sc), "prod", updateStreamAction(cloneStreamConfig(live), live))
	require.NoError(t, err)
	require.False(t, executed.Raced)
	require.True(t, mgr.updated)
	require.Equal(t, 10*time.Second, mgr.updateGot.MaxAge, "write target carries the desired MaxAge")
}

func TestApplyUpdateStream_NoOpConvergence_RacedTrueNoWrite(t *testing.T) {
	t.Parallel()

	// A genuinely emitted action: Before carries the stale plan-time MaxAge,
	// After the desired one. A concurrent operator converged the stream to the
	// desired state between Plan and Apply: the re-read live equals the target
	// but differs from Before. This must be a raced success, not stale-before.
	sc := ordersStreamCfg(func(s *StreamCfg) { s.MaxAge = 10 * time.Second })
	before := markedAppStream(func(c *jetstream.StreamConfig) { c.MaxAge = 99 * time.Second })
	live := markedAppStream(func(c *jetstream.StreamConfig) { c.MaxAge = 10 * time.Second })
	require.False(t, streamConfigsEqual(before, live), "test needs a genuine Before != live")

	mgr := &fakeStreamManager{infoCfg: live}
	executed, err := applyUpdateStreamAction(context.Background(), mgr,
		ordersStreamCfgs(sc), "prod", updateStreamAction(before, live))
	require.NoError(t, err)
	require.True(t, executed.Raced, "stream already at the target is a raced success")
	require.False(t, mgr.updated, "no write when already converged")
}

func TestApplyUpdateStream_StaleBefore_FailsFastNoWrite(t *testing.T) {
	t.Parallel()

	// Re-read live differs from both the target AND the plan-time Before: a
	// concurrent writer moved the stream to a third state.
	sc := ordersStreamCfg(func(s *StreamCfg) { s.MaxAge = 10 * time.Second })
	before := markedAppStream(func(c *jetstream.StreamConfig) { c.MaxAge = 99 * time.Second })
	live := markedAppStream(func(c *jetstream.StreamConfig) { c.MaxAge = 500 * time.Second })

	mgr := &fakeStreamManager{infoCfg: live}
	_, err := applyUpdateStreamAction(context.Background(), mgr,
		ordersStreamCfgs(sc), "prod", updateStreamAction(before, live))
	require.Error(t, err)
	require.Contains(t, err.Error(), "stale-before")
	require.False(t, mgr.updated, "no write on stale-before")
}

func TestApplyUpdateStream_WrongResourceType_FailsFast(t *testing.T) {
	t.Parallel()

	mgr := &fakeStreamManager{infoCfg: markedAppStream()}
	action := PlannedAction{Kind: ActionUpdateStream, Name: "orders", Resource: "not-a-resource"}

	_, err := applyUpdateStreamAction(context.Background(), mgr,
		ordersStreamCfgs(ordersStreamCfg()), "prod", action)
	require.Error(t, err)
	require.Contains(t, err.Error(), "wrong Resource type")
	require.False(t, mgr.updated)
}

func TestApplyUpdateStream_MissingFromConfig_FailsFast(t *testing.T) {
	t.Parallel()

	// The action's stream is not described by streamCfgs: a defensive wiring
	// error, not a runtime race.
	live := markedAppStream()
	mgr := &fakeStreamManager{infoCfg: live}
	_, err := applyUpdateStreamAction(context.Background(), mgr,
		map[string]StreamCfg{}, "prod", updateStreamAction(cloneStreamConfig(live), live))
	require.Error(t, err)
	require.Contains(t, err.Error(), "not described by the resolved config")
	require.False(t, mgr.updated)
}

func TestApplyUpdateStream_MissingOnReread_FailsFast(t *testing.T) {
	t.Parallel()

	sc := ordersStreamCfg()
	mgr := &fakeStreamManager{infoErr: jetstream.ErrStreamNotFound}
	_, err := applyUpdateStreamAction(context.Background(), mgr,
		ordersStreamCfgs(sc), "prod", updateStreamAction(markedAppStream(), markedAppStream()))
	require.Error(t, err)
	require.Contains(t, err.Error(), "stream-missing-before-update")
	require.False(t, mgr.updated)
}

func TestApplyUpdateStream_MissingOnWrite_FailsFast(t *testing.T) {
	t.Parallel()

	// Live drifts (so a write is needed) and matches Before, but the write
	// fails with ErrStreamNotFound — the stream vanished mid-apply.
	sc := ordersStreamCfg(func(s *StreamCfg) { s.MaxAge = 10 * time.Second })
	live := markedAppStream(func(c *jetstream.StreamConfig) { c.MaxAge = 99 * time.Second })
	mgr := &fakeStreamManager{infoCfg: live, updateErr: jetstream.ErrStreamNotFound}

	_, err := applyUpdateStreamAction(context.Background(), mgr,
		ordersStreamCfgs(sc), "prod", updateStreamAction(cloneStreamConfig(live), live))
	require.Error(t, err)
	require.Contains(t, err.Error(), "stream-missing-before-update")
	require.True(t, mgr.updated, "the write was attempted before the miss surfaced")
}

func TestApplyUpdateStream_CancelledReread_PropagatesCancellation(t *testing.T) {
	t.Parallel()

	mgr := &fakeStreamManager{infoErr: context.Canceled}
	_, err := applyUpdateStreamAction(context.Background(), mgr,
		ordersStreamCfgs(ordersStreamCfg()), "prod",
		updateStreamAction(markedAppStream(), markedAppStream()))
	require.ErrorIs(t, err, context.Canceled)
}

func TestApplyUpdateStream_CancelledWrite_PropagatesCancellation(t *testing.T) {
	t.Parallel()

	sc := ordersStreamCfg(func(s *StreamCfg) { s.MaxAge = 10 * time.Second })
	live := markedAppStream(func(c *jetstream.StreamConfig) { c.MaxAge = 99 * time.Second })
	mgr := &fakeStreamManager{infoCfg: live, updateErr: context.DeadlineExceeded}

	_, err := applyUpdateStreamAction(context.Background(), mgr,
		ordersStreamCfgs(sc), "prod", updateStreamAction(cloneStreamConfig(live), live))
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestApplyUpdateStream_ServerRejected_FailsFast(t *testing.T) {
	t.Parallel()

	// A mutable update the NATS server nonetheless rejects (e.g. a Subjects
	// change it refuses) surfaces as a fail-fast resource error.
	sc := ordersStreamCfg(func(s *StreamCfg) { s.MaxAge = 10 * time.Second })
	live := markedAppStream(func(c *jetstream.StreamConfig) { c.MaxAge = 99 * time.Second })
	mgr := &fakeStreamManager{infoCfg: live, updateErr: errors.New("stream subjects overlap")}

	_, err := applyUpdateStreamAction(context.Background(), mgr,
		ordersStreamCfgs(sc), "prod", updateStreamAction(cloneStreamConfig(live), live))
	require.Error(t, err)
	require.False(t, errors.Is(err, context.Canceled))
	require.Contains(t, err.Error(), "stream subjects overlap")
}

// --- applyStampStreamMarkerAction --------------------------------------------

// unmarkedAppStream returns a synthetic application stream with no Parti marker.
func unmarkedAppStream(mutate ...func(*jetstream.StreamConfig)) jetstream.StreamConfig {
	cfg := jetstream.StreamConfig{
		Name:     "orders",
		Subjects: []string{"orders.>"},
		Storage:  jetstream.MemoryStorage,
	}
	for _, m := range mutate {
		m(&cfg)
	}

	return cfg
}

// stampStreamMarkerAction builds an ActionStampStreamMarker for the "orders"
// stream.
func stampStreamMarkerAction() PlannedAction {
	return PlannedAction{
		Kind:     ActionStampStreamMarker,
		Name:     "orders",
		Resource: &StreamStampMarkerResource{Stream: "orders"},
	}
}

func TestApplyStampStreamMarker_UnmarkedReread_WritesMergedMetadata(t *testing.T) {
	t.Parallel()

	live := unmarkedAppStream(func(c *jetstream.StreamConfig) {
		c.Metadata = map[string]string{"custom.io/team": "infra"}
	})
	mgr := &fakeStreamManager{infoCfg: live}

	executed, err := applyStampStreamMarkerAction(context.Background(), mgr, "prod",
		stampStreamMarkerAction())
	require.NoError(t, err)
	require.False(t, executed.Raced)
	require.True(t, mgr.updated)

	// Metadata is the merged map: Parti marker plus the preserved non-Parti key.
	require.Equal(t, MarkerManagedValue, mgr.updateGot.Metadata[MarkerManagedKey])
	require.Equal(t, ComponentStream, mgr.updateGot.Metadata[MarkerComponentKey])
	require.Equal(t, "prod", mgr.updateGot.Metadata[MarkerInstanceKey])
	require.Equal(t, "infra", mgr.updateGot.Metadata["custom.io/team"])
	// Non-metadata fields equal the re-read snapshot.
	require.Equal(t, jetstream.MemoryStorage, mgr.updateGot.Storage)
	require.Equal(t, "orders", mgr.updateGot.Name)
}

func TestApplyStampStreamMarker_AlreadyMarkedReread_RacedNoWrite(t *testing.T) {
	t.Parallel()

	// The stream already carries an equivalent marker: the merge is a no-op.
	mgr := &fakeStreamManager{infoCfg: markedAppStream()}
	executed, err := applyStampStreamMarkerAction(context.Background(), mgr, "prod",
		stampStreamMarkerAction())
	require.NoError(t, err)
	require.True(t, executed.Raced, "an already-marked stream is a raced no-op")
	require.False(t, mgr.updated, "no write when the marker is already present")
}

func TestApplyStampStreamMarker_WrongResourceType_FailsFast(t *testing.T) {
	t.Parallel()

	mgr := &fakeStreamManager{infoCfg: unmarkedAppStream()}
	action := PlannedAction{Kind: ActionStampStreamMarker, Name: "orders", Resource: "nope"}

	_, err := applyStampStreamMarkerAction(context.Background(), mgr, "prod", action)
	require.Error(t, err)
	require.Contains(t, err.Error(), "wrong Resource type")
	require.False(t, mgr.updated)
}

func TestApplyStampStreamMarker_MissingOnReread_FailsFast(t *testing.T) {
	t.Parallel()

	mgr := &fakeStreamManager{infoErr: jetstream.ErrStreamNotFound}
	_, err := applyStampStreamMarkerAction(context.Background(), mgr, "prod",
		stampStreamMarkerAction())
	require.Error(t, err)
	require.Contains(t, err.Error(), "stream-missing-before-stamp")
	require.False(t, mgr.updated)
}

func TestApplyStampStreamMarker_MissingOnWrite_FailsFast(t *testing.T) {
	t.Parallel()

	// Unmarked re-read so a write is needed; the write fails with
	// ErrStreamNotFound — the stream vanished mid-apply (the second miss point).
	mgr := &fakeStreamManager{infoCfg: unmarkedAppStream(), updateErr: jetstream.ErrStreamNotFound}
	_, err := applyStampStreamMarkerAction(context.Background(), mgr, "prod",
		stampStreamMarkerAction())
	require.Error(t, err)
	require.Contains(t, err.Error(), "stream-missing-before-stamp")
	require.True(t, mgr.updated, "the write was attempted before the miss surfaced")
}

func TestApplyStampStreamMarker_CancelledReread_PropagatesCancellation(t *testing.T) {
	t.Parallel()

	mgr := &fakeStreamManager{infoErr: context.Canceled}
	_, err := applyStampStreamMarkerAction(context.Background(), mgr, "prod",
		stampStreamMarkerAction())
	require.ErrorIs(t, err, context.Canceled)
}

func TestApplyStampStreamMarker_CancelledWrite_PropagatesCancellation(t *testing.T) {
	t.Parallel()

	// Unmarked re-read so a write is needed; the write itself is cancelled.
	mgr := &fakeStreamManager{infoCfg: unmarkedAppStream(), updateErr: context.Canceled}
	_, err := applyStampStreamMarkerAction(context.Background(), mgr, "prod",
		stampStreamMarkerAction())
	require.ErrorIs(t, err, context.Canceled)
}

func TestApplyStampStreamMarker_ServerRejected_FailsFast(t *testing.T) {
	t.Parallel()

	mgr := &fakeStreamManager{infoCfg: unmarkedAppStream(), updateErr: errors.New("server rejected")}
	_, err := applyStampStreamMarkerAction(context.Background(), mgr, "prod",
		stampStreamMarkerAction())
	require.Error(t, err)
	require.False(t, errors.Is(err, context.Canceled))
	require.Contains(t, err.Error(), "server rejected")
}

// --- applyPlan loop wiring ---------------------------------------------------

// applyPlanJS is a fake jetstream.JetStream covering exactly the methods
// applyPlan needs for a stream-only plan: Stream, CreateStream, UpdateStream,
// DeleteStream. streamInfo seeds StreamInfo re-reads; createErr / updateErr /
// deleteErr force the corresponding write to fail. createdNames / updatedNames /
// deletedNames record what ran.
type applyPlanJS struct {
	jetstream.JetStream
	streamInfo map[string]jetstream.StreamConfig

	createErr error
	updateErr error
	deleteErr error

	createdNames []string
	updatedNames []string
	deletedNames []string
}

func (j *applyPlanJS) Stream(_ context.Context, name string) (jetstream.Stream, error) {
	cfg, ok := j.streamInfo[name]
	if !ok {
		return nil, jetstream.ErrStreamNotFound
	}

	return &fakeStream{cfg: cfg}, nil
}

func (j *applyPlanJS) CreateStream(_ context.Context, cfg jetstream.StreamConfig) (jetstream.Stream, error) {
	if j.createErr != nil {
		return nil, j.createErr
	}
	j.createdNames = append(j.createdNames, cfg.Name)

	return &fakeStream{cfg: cfg}, nil
}

func (j *applyPlanJS) UpdateStream(_ context.Context, cfg jetstream.StreamConfig) (jetstream.Stream, error) {
	if j.updateErr != nil {
		return nil, j.updateErr
	}
	j.updatedNames = append(j.updatedNames, cfg.Name)

	return &fakeStream{cfg: cfg}, nil
}

func (j *applyPlanJS) DeleteStream(_ context.Context, name string) error {
	if j.deleteErr != nil {
		return j.deleteErr
	}
	j.deletedNames = append(j.deletedNames, name)
	delete(j.streamInfo, name)

	return nil
}

// streamApplyCfg returns a resolved Config declaring the given streams.
func streamApplyCfg(streams ...StreamCfg) Config {
	return Config{
		APIVersion: APIVersionV1,
		Instance:   "prod",
		Policy:     PolicySafeUpdate,
		Streams:    streams,
	}
}

func TestApplyPlan_CreateStream_FoldsIntoExecuted(t *testing.T) {
	t.Parallel()

	sc := ordersStreamCfg()
	js := &applyPlanJS{}
	plan := PlanResult{
		Actions: []PlannedAction{{
			Kind: ActionCreateStream, Name: "orders",
			Resource: buildStreamConfig(sc, "prod"),
		}},
	}

	rep, err := applyPlan(context.Background(), js, streamApplyCfg(sc), plan)
	require.NoError(t, err)
	require.False(t, rep.Aborted)
	require.Empty(t, rep.Errors)
	require.Len(t, rep.Executed, 1)
	require.Equal(t, ActionCreateStream, rep.Executed[0].Kind)
	require.Equal(t, []string{"orders"}, js.createdNames)
}

func TestApplyPlan_UpdateStream_FoldsIntoExecuted(t *testing.T) {
	t.Parallel()

	sc := ordersStreamCfg(func(s *StreamCfg) { s.MaxAge = 10 * time.Second })
	live := markedAppStream(func(c *jetstream.StreamConfig) { c.MaxAge = 99 * time.Second })
	js := &applyPlanJS{streamInfo: map[string]jetstream.StreamConfig{"orders": live}}
	plan := PlanResult{
		Actions: []PlannedAction{
			updateStreamAction(cloneStreamConfig(live), live),
		},
	}

	rep, err := applyPlan(context.Background(), js, streamApplyCfg(sc), plan)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)
	require.Len(t, rep.Executed, 1)
	require.Equal(t, ActionUpdateStream, rep.Executed[0].Kind)
	require.Equal(t, []string{"orders"}, js.updatedNames)
}

func TestApplyPlan_StampStreamMarker_FoldsIntoExecuted(t *testing.T) {
	t.Parallel()

	sc := ordersStreamCfg()
	live := unmarkedAppStream()
	js := &applyPlanJS{streamInfo: map[string]jetstream.StreamConfig{"orders": live}}
	plan := PlanResult{
		Actions: []PlannedAction{stampStreamMarkerAction()},
	}

	rep, err := applyPlan(context.Background(), js, streamApplyCfg(sc), plan)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)
	require.Len(t, rep.Executed, 1)
	require.Equal(t, ActionStampStreamMarker, rep.Executed[0].Kind)
	require.Equal(t, []string{"orders"}, js.updatedNames)
}

func TestApplyPlan_StreamFailFast_RemainderSkippedWithPriorError(t *testing.T) {
	t.Parallel()

	// First action fails (server-rejected create-stream); the remaining
	// actions land in Skipped with prior-error, Aborted stays false.
	sc1 := ordersStreamCfg()
	sc2 := StreamCfg{Name: "events", Subjects: []string{"events.>"}, Storage: "memory", Discard: "old"}
	js := &applyPlanJS{createErr: errors.New("invalid subject")}
	plan := PlanResult{
		Actions: []PlannedAction{
			{Kind: ActionCreateStream, Name: "orders", Resource: buildStreamConfig(sc1, "prod")},
			{Kind: ActionCreateStream, Name: "events", Resource: buildStreamConfig(sc2, "prod")},
		},
	}

	rep, err := applyPlan(context.Background(), js, streamApplyCfg(sc1, sc2), plan)
	require.Error(t, err)
	require.False(t, rep.Aborted, "a non-cancellation failure is fail-fast, not aborted")
	require.Len(t, rep.Errors, 1)
	require.Equal(t, "orders", rep.Errors[0].Name)
	require.Len(t, rep.Skipped, 1)
	require.Equal(t, "events", rep.Skipped[0].Name)
	require.Equal(t, SkipReasonPriorError, rep.Skipped[0].Reason)
}

func TestApplyPlan_StreamCancelledMidApply_AbortedAndSkipped(t *testing.T) {
	t.Parallel()

	// The first create-stream is cancelled mid-write; it plus the remaining
	// actions go to Skipped with context-cancelled, Aborted is true.
	sc1 := ordersStreamCfg()
	sc2 := StreamCfg{Name: "events", Subjects: []string{"events.>"}, Storage: "memory", Discard: "old"}
	js := &applyPlanJS{createErr: context.Canceled}
	plan := PlanResult{
		Actions: []PlannedAction{
			{Kind: ActionCreateStream, Name: "orders", Resource: buildStreamConfig(sc1, "prod")},
			{Kind: ActionCreateStream, Name: "events", Resource: buildStreamConfig(sc2, "prod")},
		},
	}

	rep, err := applyPlan(context.Background(), js, streamApplyCfg(sc1, sc2), plan)
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, rep.Aborted)
	require.Empty(t, rep.Errors, "a cancellation is not a resource error")
	require.Len(t, rep.Skipped, 2, "the cancelled action and the remainder are skipped")
	for _, s := range rep.Skipped {
		require.Equal(t, SkipReasonContextCancelled, s.Reason)
	}
}

func TestApplyPlan_StreamPreMutationCancel_AllSkipped(t *testing.T) {
	t.Parallel()

	// ctx is already cancelled before the loop: every action is skipped with
	// context-cancelled, no write attempted.
	sc := ordersStreamCfg()
	js := &applyPlanJS{}
	plan := PlanResult{
		Actions: []PlannedAction{
			{Kind: ActionCreateStream, Name: "orders", Resource: buildStreamConfig(sc, "prod")},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rep, err := applyPlan(ctx, js, streamApplyCfg(sc), plan)
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, rep.Aborted)
	require.Len(t, rep.Skipped, 1)
	require.Equal(t, SkipReasonContextCancelled, rep.Skipped[0].Reason)
	require.Empty(t, js.createdNames, "no write on a pre-mutation cancel")
}

// --- probeStreamInfo --------------------------------------------------------

// probeStreamJS is a fake jetstream.JetStream for probeStreamInfo: streamErr
// forces the Stream lookup to fail; infoErr forces stream.Info to fail.
type probeStreamJS struct {
	jetstream.JetStream
	streamErr error
	infoErr   error
}

func (j *probeStreamJS) Stream(_ context.Context, _ string) (jetstream.Stream, error) {
	if j.streamErr != nil {
		return nil, j.streamErr
	}

	return &probeStreamHandle{infoErr: j.infoErr}, nil
}

// probeStreamHandle is a fake jetstream.Stream whose Info can fail.
type probeStreamHandle struct {
	jetstream.Stream
	infoErr error
}

func (s *probeStreamHandle) Info(_ context.Context, _ ...jetstream.StreamInfoOpt) (*jetstream.StreamInfo, error) {
	if s.infoErr != nil {
		return nil, s.infoErr
	}

	return &jetstream.StreamInfo{Config: jetstream.StreamConfig{Name: "orders"}}, nil
}

func TestProbeStreamInfo_ExistingStream_OK(t *testing.T) {
	t.Parallel()

	err := probeStreamInfo(context.Background(), &probeStreamJS{}, "orders")
	require.NoError(t, err)
}

func TestProbeStreamInfo_StreamNotFound_OK(t *testing.T) {
	t.Parallel()

	// ErrStreamNotFound is not a probe failure — Apply will create the stream.
	err := probeStreamInfo(context.Background(),
		&probeStreamJS{streamErr: jetstream.ErrStreamNotFound}, "orders")
	require.NoError(t, err)
}

func TestProbeStreamInfo_PermissionDeniedOnLookup_ErrLiveValidation(t *testing.T) {
	t.Parallel()

	// A permission failure on the Stream lookup classifies as ErrLiveValidation.
	err := probeStreamInfo(context.Background(),
		&probeStreamJS{streamErr: nats.ErrPermissionViolation}, "orders")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrLiveValidation)
}

func TestProbeStreamInfo_PermissionDeniedOnInfo_ErrLiveValidation(t *testing.T) {
	t.Parallel()

	// A permission failure on stream.Info also classifies as ErrLiveValidation.
	err := probeStreamInfo(context.Background(),
		&probeStreamJS{infoErr: nats.ErrPermissionViolation}, "orders")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrLiveValidation)
}

func TestProbeStreamInfo_InfoStreamNotFound_OK(t *testing.T) {
	t.Parallel()

	// A stream that vanishes between the lookup and Info is treated the same as
	// a never-existing stream — not a probe failure.
	err := probeStreamInfo(context.Background(),
		&probeStreamJS{infoErr: jetstream.ErrStreamNotFound}, "orders")
	require.NoError(t, err)
}

func TestProbeStreamInfo_CancelledContext_ReturnsCtxErr(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// classifyLiveError returns ctx.Err() directly when the context is done.
	err := probeStreamInfo(ctx, &probeStreamJS{streamErr: nats.ErrPermissionViolation}, "orders")
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrLiveValidation)
}
