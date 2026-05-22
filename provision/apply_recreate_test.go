package provision

// Same-package unit tests for the recreate apply seam:
// applyRecreateKVAction, applyRecreateStreamAction, applyRecreateConsumerAction,
// and the applyPlan / ApplyConsumers wiring of the three recreate action kinds.
// These use fake seams so they run without a live NATS server.

import (
	"context"
	"errors"
	"testing"

	"github.com/arloliu/parti/v2/internal/dynamicbuild"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// --- fake kvRecreator seam --------------------------------------------------

// fakeKVRecreator drives the recreate-kv helper deterministically. infoCfg is
// the StreamConfig StreamInfo returns; infoErr / deleteErr / createErr force the
// corresponding seam method to fail. deleted / created record whether the
// destructive / create methods ran, in order, so a test can assert the delete
// preceded the create.
type fakeKVRecreator struct {
	infoCfg jetstream.StreamConfig
	infoErr error

	deleteErr error
	deleted   bool

	createErr error
	created   bool
	createGot jetstream.KeyValueConfig

	// sequence records "delete" / "create" in call order.
	sequence []string
}

func (r *fakeKVRecreator) StreamInfo(_ context.Context, _ string) (*jetstream.StreamInfo, error) {
	if r.infoErr != nil {
		return nil, r.infoErr
	}

	return &jetstream.StreamInfo{Config: r.infoCfg}, nil
}

func (r *fakeKVRecreator) DeleteKeyValue(_ context.Context, _ string) error {
	r.deleted = true
	r.sequence = append(r.sequence, "delete")

	return r.deleteErr
}

func (r *fakeKVRecreator) CreateKeyValue(_ context.Context, cfg jetstream.KeyValueConfig) error {
	r.created = true
	r.createGot = cfg
	r.sequence = append(r.sequence, "create")

	return r.createErr
}

// --- recreate-kv fixtures ---------------------------------------------------

// recreateKVAfter returns the desired After KeyValueConfig for a control-plane
// bucket: FileStorage, History 1, marked for the control-plane-id component.
func recreateKVAfter() jetstream.KeyValueConfig {
	return jetstream.KeyValueConfig{
		Bucket:   "parti-stableid",
		Storage:  jetstream.FileStorage,
		History:  1,
		Metadata: BuildMarker(ComponentControlPlaneID, "prod"),
	}
}

// liveKVStream returns a live KV_-prefixed stream config carrying the given KV
// config's fields. By default it diverges on Storage (immutable drift present).
func liveKVStream(mutate ...func(*jetstream.StreamConfig)) jetstream.StreamConfig {
	cfg := jetstream.StreamConfig{
		Name:              "KV_parti-stableid",
		Storage:           jetstream.MemoryStorage, // diverges from FileStorage After
		MaxMsgsPerSubject: 1,
		Metadata:          BuildMarker(ComponentControlPlaneID, "prod"),
	}
	for _, m := range mutate {
		m(&cfg)
	}

	return cfg
}

func recreateKVAction(after jetstream.KeyValueConfig) PlannedAction {
	return PlannedAction{
		Kind: ActionRecreateKV,
		Name: after.Bucket,
		Resource: &RecreateKVResource{
			After:          after,
			ImmutableDrift: map[string]any{"storage": map[string]any{"want": "file", "got": "memory"}},
		},
	}
}

// --- applyRecreateKVAction --------------------------------------------------

func TestApplyRecreateKV_HappyPath_DeletesThenCreates(t *testing.T) {
	t.Parallel()

	after := recreateKVAfter()
	seam := &fakeKVRecreator{infoCfg: liveKVStream()} // live diverges on Storage
	executed, err := applyRecreateKVAction(context.Background(), seam, recreateKVAction(after))
	require.NoError(t, err)
	require.Equal(t, ActionRecreateKV, executed.Kind)
	require.Equal(t, "parti-stableid", executed.Name)
	require.False(t, executed.Raced)
	require.Equal(t, []string{"delete", "create"}, seam.sequence, "delete must precede create")
	require.Equal(t, jetstream.FileStorage, seam.createGot.Storage, "created at the desired After config")
}

func TestApplyRecreateKV_StalePlanNoOp_RacedNoDelete(t *testing.T) {
	t.Parallel()

	// The re-read live state no longer carries the immutable drift: a concurrent
	// operator already repaired the bucket. The recreate must be a raced no-op.
	after := recreateKVAfter()
	live := liveKVStream(func(c *jetstream.StreamConfig) { c.Storage = jetstream.FileStorage })
	seam := &fakeKVRecreator{infoCfg: live}

	executed, err := applyRecreateKVAction(context.Background(), seam, recreateKVAction(after))
	require.NoError(t, err)
	require.True(t, executed.Raced, "converged live state is a raced no-op")
	require.False(t, seam.deleted, "NO delete on the stale-plan no-op")
	require.False(t, seam.created)
}

func TestApplyRecreateKV_ResourceGone_CreateOnly(t *testing.T) {
	t.Parallel()

	// The bucket is already gone at re-read → recreate proceeds as create-only.
	after := recreateKVAfter()
	seam := &fakeKVRecreator{infoErr: jetstream.ErrStreamNotFound}

	executed, err := applyRecreateKVAction(context.Background(), seam, recreateKVAction(after))
	require.NoError(t, err)
	require.False(t, executed.Raced)
	require.False(t, seam.deleted, "no delete when the bucket was already gone")
	require.True(t, seam.created)
	require.Equal(t, []string{"create"}, seam.sequence)
}

func TestApplyRecreateKV_DeleteSucceededCreateFailed_RecreateInterrupted(t *testing.T) {
	t.Parallel()

	// The create step errors after the delete succeeded: a fail-fast error that
	// wraps ErrRecreateInterrupted and does NOT satisfy errors.Is(_, ctx errors).
	after := recreateKVAfter()
	seam := &fakeKVRecreator{
		infoCfg:   liveKVStream(),
		createErr: errors.New("quota exceeded"),
	}

	_, err := applyRecreateKVAction(context.Background(), seam, recreateKVAction(after))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrRecreateInterrupted)
	require.True(t, seam.deleted, "the delete succeeded")
	require.Contains(t, err.Error(), "quota exceeded")
	require.Contains(t, err.Error(), "re-run apply")
}

func TestApplyRecreateKV_CancelBeforeDelete_CleanAbort(t *testing.T) {
	t.Parallel()

	// Cancellation on the re-read (before the delete) → ordinary cancellation,
	// surfaced verbatim; nothing destroyed.
	after := recreateKVAfter()
	seam := &fakeKVRecreator{infoErr: context.Canceled}

	_, err := applyRecreateKVAction(context.Background(), seam, recreateKVAction(after))
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, seam.deleted, "nothing destroyed on a pre-delete cancel")
}

func TestApplyRecreateKV_CancelAfterDelete_RecreateInterruptedNotCancellation(t *testing.T) {
	t.Parallel()

	// Cancellation on the create step (AFTER the delete succeeded) → fail-fast
	// ErrRecreateInterrupted. Critically, the returned error must NOT satisfy
	// errors.Is(_, context.Canceled): a %w-wrap would misroute it through
	// foldActionResult's pure-cancellation branch.
	after := recreateKVAfter()
	seam := &fakeKVRecreator{
		infoCfg:   liveKVStream(),
		createErr: context.Canceled,
	}

	_, err := applyRecreateKVAction(context.Background(), seam, recreateKVAction(after))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrRecreateInterrupted)
	require.False(t, errors.Is(err, context.Canceled),
		"a post-delete cancellation must NOT satisfy errors.Is(_, context.Canceled)")
	require.True(t, seam.deleted)
}

func TestApplyRecreateKV_RereadServerError_FailsFast(t *testing.T) {
	t.Parallel()

	after := recreateKVAfter()
	seam := &fakeKVRecreator{infoErr: errors.New("connection reset")}

	_, err := applyRecreateKVAction(context.Background(), seam, recreateKVAction(after))
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrRecreateInterrupted), "a pre-delete error is not interrupted")
	require.False(t, errors.Is(err, context.Canceled))
	require.Contains(t, err.Error(), "connection reset")
	require.False(t, seam.deleted)
}

func TestApplyRecreateKV_DeleteErrors_FailsFastNotInterrupted(t *testing.T) {
	t.Parallel()

	// The delete itself fails: the bucket still exists, so this is an ordinary
	// pre-mutation fail-fast — NOT ErrRecreateInterrupted.
	after := recreateKVAfter()
	seam := &fakeKVRecreator{infoCfg: liveKVStream(), deleteErr: errors.New("delete rejected")}

	_, err := applyRecreateKVAction(context.Background(), seam, recreateKVAction(after))
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrRecreateInterrupted), "a failed delete leaves the resource intact")
	require.Contains(t, err.Error(), "delete rejected")
	require.False(t, seam.created)
}

func TestApplyRecreateKV_RaceFilledName_RacedTrue(t *testing.T) {
	t.Parallel()

	// A concurrent creator filled the just-deleted name: the create returns
	// ErrBucketExists, but the desired resource exists → raced success.
	after := recreateKVAfter()
	seam := &fakeKVRecreator{infoCfg: liveKVStream(), createErr: jetstream.ErrBucketExists}

	executed, err := applyRecreateKVAction(context.Background(), seam, recreateKVAction(after))
	require.NoError(t, err)
	require.True(t, executed.Raced)
	require.True(t, seam.deleted)
}

func TestApplyRecreateKV_ResourceGoneRaceFilledName_RacedTrue(t *testing.T) {
	t.Parallel()

	// The bucket was already gone at re-read (create-only branch) and a
	// concurrent creator then won the create race: an ordinary Plan->Apply
	// race → raced success, not a fail-fast — mirrors applyCreateKVAction.
	after := recreateKVAfter()
	seam := &fakeKVRecreator{infoErr: jetstream.ErrStreamNotFound, createErr: jetstream.ErrBucketExists}

	executed, err := applyRecreateKVAction(context.Background(), seam, recreateKVAction(after))
	require.NoError(t, err)
	require.True(t, executed.Raced)
	require.False(t, seam.deleted, "no delete when the bucket was already gone")
}

func TestApplyRecreateKV_WrongResourceType_FailsFast(t *testing.T) {
	t.Parallel()

	seam := &fakeKVRecreator{}
	// A value RecreateKVResource (wrong: recreate-kv carries a pointer).
	action := PlannedAction{Kind: ActionRecreateKV, Name: "x", Resource: RecreateKVResource{}}

	_, err := applyRecreateKVAction(context.Background(), seam, action)
	require.Error(t, err)
	require.Contains(t, err.Error(), "wrong Resource type")
	require.False(t, seam.deleted)
}

// --- recreate-stream fixtures -----------------------------------------------

// markedAppStreamFile is a Parti-marked application stream with FileStorage —
// the "live" state that diverges from the memory-storage After.
func markedAppStreamFile() jetstream.StreamConfig {
	return markedAppStream(func(c *jetstream.StreamConfig) { c.Storage = jetstream.FileStorage })
}

func recreateStreamAction(after jetstream.StreamConfig) PlannedAction {
	return PlannedAction{
		Kind: ActionRecreateStream,
		Name: after.Name,
		Resource: &RecreateStreamResource{
			After:          after,
			ImmutableDrift: map[string]any{"storage": map[string]any{"want": "memory", "got": "file"}},
		},
	}
}

// --- applyRecreateStreamAction ----------------------------------------------

func TestApplyRecreateStream_HappyPath_DeletesThenCreates(t *testing.T) {
	t.Parallel()

	after := buildStreamConfig(ordersStreamCfg(), "prod") // memory storage
	mgr := &fakeStreamManager{infoCfg: markedAppStreamFile()}

	executed, err := applyRecreateStreamAction(context.Background(), mgr, recreateStreamAction(after))
	require.NoError(t, err)
	require.Equal(t, ActionRecreateStream, executed.Kind)
	require.False(t, executed.Raced)
	require.True(t, mgr.deleted, "the stream was deleted")
	require.True(t, mgr.created, "the stream was recreated")
	require.Equal(t, jetstream.MemoryStorage, mgr.createGot.Storage)
}

func TestApplyRecreateStream_StalePlanNoOp_RacedNoDelete(t *testing.T) {
	t.Parallel()

	// The re-read live stream already has the desired storage: no immutable drift.
	after := buildStreamConfig(ordersStreamCfg(), "prod")
	mgr := &fakeStreamManager{infoCfg: markedAppStream()} // memory storage, matches After

	executed, err := applyRecreateStreamAction(context.Background(), mgr, recreateStreamAction(after))
	require.NoError(t, err)
	require.True(t, executed.Raced)
	require.False(t, mgr.deleted, "NO delete on the stale-plan no-op")
	require.False(t, mgr.created)
}

func TestApplyRecreateStream_ResourceGone_CreateOnly(t *testing.T) {
	t.Parallel()

	after := buildStreamConfig(ordersStreamCfg(), "prod")
	mgr := &fakeStreamManager{infoErr: jetstream.ErrStreamNotFound}

	_, err := applyRecreateStreamAction(context.Background(), mgr, recreateStreamAction(after))
	require.NoError(t, err)
	require.False(t, mgr.deleted, "no delete when the stream was already gone")
	require.True(t, mgr.created)
}

func TestApplyRecreateStream_DeleteSucceededCreateFailed_RecreateInterrupted(t *testing.T) {
	t.Parallel()

	after := buildStreamConfig(ordersStreamCfg(), "prod")
	mgr := &fakeStreamManager{infoCfg: markedAppStreamFile(), createErr: errors.New("invalid subject")}

	_, err := applyRecreateStreamAction(context.Background(), mgr, recreateStreamAction(after))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrRecreateInterrupted)
	require.True(t, mgr.deleted)
	require.Contains(t, err.Error(), "invalid subject")
}

func TestApplyRecreateStream_CancelAfterDelete_RecreateInterruptedNotCancellation(t *testing.T) {
	t.Parallel()

	after := buildStreamConfig(ordersStreamCfg(), "prod")
	mgr := &fakeStreamManager{infoCfg: markedAppStreamFile(), createErr: context.DeadlineExceeded}

	_, err := applyRecreateStreamAction(context.Background(), mgr, recreateStreamAction(after))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrRecreateInterrupted)
	require.False(t, errors.Is(err, context.DeadlineExceeded),
		"a post-delete cancellation must NOT satisfy errors.Is(_, context.DeadlineExceeded)")
	require.True(t, mgr.deleted)
}

func TestApplyRecreateStream_CancelBeforeDelete_CleanAbort(t *testing.T) {
	t.Parallel()

	after := buildStreamConfig(ordersStreamCfg(), "prod")
	mgr := &fakeStreamManager{infoErr: context.Canceled}

	_, err := applyRecreateStreamAction(context.Background(), mgr, recreateStreamAction(after))
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, mgr.deleted)
}

func TestApplyRecreateStream_DeleteErrors_FailsFastNotInterrupted(t *testing.T) {
	t.Parallel()

	after := buildStreamConfig(ordersStreamCfg(), "prod")
	mgr := &fakeStreamManager{infoCfg: markedAppStreamFile(), deleteErr: errors.New("delete rejected")}

	_, err := applyRecreateStreamAction(context.Background(), mgr, recreateStreamAction(after))
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrRecreateInterrupted))
	require.Contains(t, err.Error(), "delete rejected")
	require.False(t, mgr.created)
}

func TestApplyRecreateStream_RaceFilledName_RacedTrue(t *testing.T) {
	t.Parallel()

	// A concurrent creator filled the just-deleted name: ErrStreamNameAlreadyInUse
	// post-delete → raced success.
	after := buildStreamConfig(ordersStreamCfg(), "prod")
	mgr := &fakeStreamManager{infoCfg: markedAppStreamFile(), createErr: jetstream.ErrStreamNameAlreadyInUse}

	executed, err := applyRecreateStreamAction(context.Background(), mgr, recreateStreamAction(after))
	require.NoError(t, err)
	require.True(t, executed.Raced)
	require.True(t, mgr.deleted)
}

func TestApplyRecreateStream_ResourceGoneRaceFilledName_RacedTrue(t *testing.T) {
	t.Parallel()

	// The stream was already gone at re-read (create-only branch) and a
	// concurrent creator won the create race → raced success, not fail-fast.
	after := buildStreamConfig(ordersStreamCfg(), "prod")
	mgr := &fakeStreamManager{infoErr: jetstream.ErrStreamNotFound, createErr: jetstream.ErrStreamNameAlreadyInUse}

	executed, err := applyRecreateStreamAction(context.Background(), mgr, recreateStreamAction(after))
	require.NoError(t, err)
	require.True(t, executed.Raced)
	require.False(t, mgr.deleted, "no delete when the stream was already gone")
}

func TestApplyRecreateStream_WrongResourceType_FailsFast(t *testing.T) {
	t.Parallel()

	mgr := &fakeStreamManager{}
	action := PlannedAction{Kind: ActionRecreateStream, Name: "orders", Resource: "nope"}

	_, err := applyRecreateStreamAction(context.Background(), mgr, action)
	require.Error(t, err)
	require.Contains(t, err.Error(), "wrong Resource type")
	require.False(t, mgr.deleted)
}

// --- recreate-consumer fixtures ---------------------------------------------

func recreateConsumerAction(after PlannedConsumer) PlannedAction {
	return PlannedAction{
		Kind: ActionRecreateConsumer,
		Name: after.Durable,
		Resource: &RecreateConsumerResource{
			After:          after,
			ImmutableDrift: map[string]any{"memoryStorage": map[string]any{"want": false, "got": true}},
		},
	}
}

// --- applyRecreateConsumerAction --------------------------------------------

func TestApplyRecreateConsumer_HappyPath_DeletesThenCreates(t *testing.T) {
	t.Parallel()

	after := plannedConsumer()
	// Live consumer diverges on MemoryStorage (immutable identity drift).
	mgr := &fakeConsumerManager{
		infoCfg: liveConsumerCfg(after, func(c *jetstream.ConsumerConfig) { c.MemoryStorage = true }),
	}

	executed, err := applyRecreateConsumerAction(context.Background(), mgr, recreateConsumerAction(after))
	require.NoError(t, err)
	require.Equal(t, ActionRecreateConsumer, executed.Kind)
	require.False(t, executed.Raced)
	require.True(t, mgr.deleted, "the consumer was deleted")
	require.True(t, mgr.created, "the consumer was recreated")
	require.Equal(t, after.StreamName, mgr.createGS)
}

func TestApplyRecreateConsumer_StalePlanNoOp_RacedNoDelete(t *testing.T) {
	t.Parallel()

	// The re-read live consumer no longer carries the identity drift.
	after := plannedConsumer()
	mgr := &fakeConsumerManager{infoCfg: liveConsumerCfg(after)} // identity fields match

	executed, err := applyRecreateConsumerAction(context.Background(), mgr, recreateConsumerAction(after))
	require.NoError(t, err)
	require.True(t, executed.Raced)
	require.False(t, mgr.deleted, "NO delete on the stale-plan no-op")
	require.False(t, mgr.created)
}

func TestApplyRecreateConsumer_ResourceGone_CreateOnly(t *testing.T) {
	t.Parallel()

	after := plannedConsumer()
	mgr := &fakeConsumerManager{infoErr: jetstream.ErrConsumerNotFound}

	_, err := applyRecreateConsumerAction(context.Background(), mgr, recreateConsumerAction(after))
	require.NoError(t, err)
	require.False(t, mgr.deleted, "no delete when the consumer was already gone")
	require.True(t, mgr.created)
}

func TestApplyRecreateConsumer_DeleteSucceededCreateFailed_RecreateInterrupted(t *testing.T) {
	t.Parallel()

	after := plannedConsumer()
	mgr := &fakeConsumerManager{
		infoCfg:   liveConsumerCfg(after, func(c *jetstream.ConsumerConfig) { c.MemoryStorage = true }),
		createErr: errors.New("consumer config invalid"),
	}

	_, err := applyRecreateConsumerAction(context.Background(), mgr, recreateConsumerAction(after))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrRecreateInterrupted)
	require.True(t, mgr.deleted)
	require.Contains(t, err.Error(), "consumer config invalid")
}

func TestApplyRecreateConsumer_CancelAfterDelete_RecreateInterruptedNotCancellation(t *testing.T) {
	t.Parallel()

	after := plannedConsumer()
	mgr := &fakeConsumerManager{
		infoCfg:   liveConsumerCfg(after, func(c *jetstream.ConsumerConfig) { c.MemoryStorage = true }),
		createErr: context.Canceled,
	}

	_, err := applyRecreateConsumerAction(context.Background(), mgr, recreateConsumerAction(after))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrRecreateInterrupted)
	require.False(t, errors.Is(err, context.Canceled),
		"a post-delete cancellation must NOT satisfy errors.Is(_, context.Canceled)")
	require.True(t, mgr.deleted)
}

func TestApplyRecreateConsumer_CancelBeforeDelete_CleanAbort(t *testing.T) {
	t.Parallel()

	after := plannedConsumer()
	mgr := &fakeConsumerManager{infoErr: context.Canceled}

	_, err := applyRecreateConsumerAction(context.Background(), mgr, recreateConsumerAction(after))
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, mgr.deleted)
}

func TestApplyRecreateConsumer_DeleteErrors_FailsFastNotInterrupted(t *testing.T) {
	t.Parallel()

	after := plannedConsumer()
	mgr := &fakeConsumerManager{
		infoCfg:   liveConsumerCfg(after, func(c *jetstream.ConsumerConfig) { c.MemoryStorage = true }),
		deleteErr: errors.New("delete rejected"),
	}

	_, err := applyRecreateConsumerAction(context.Background(), mgr, recreateConsumerAction(after))
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrRecreateInterrupted))
	require.Contains(t, err.Error(), "delete rejected")
	require.False(t, mgr.created)
}

func TestApplyRecreateConsumer_RaceFilledName_RacedTrue(t *testing.T) {
	t.Parallel()

	// A concurrent creator filled the just-deleted durable name: ErrConsumerExists
	// post-delete. The verify re-read confirms the colliding consumer's identity
	// matches the desired config → raced success.
	after := plannedConsumer()
	drift := liveConsumerCfg(after, func(c *jetstream.ConsumerConfig) { c.MemoryStorage = true })
	mgr := &fakeConsumerManager{
		infoSeq: []consumerInfoStep{
			{cfg: drift},                  // step-1 re-read: immutable drift present
			{cfg: liveConsumerCfg(after)}, // verify re-read: identity matches
		},
		createErr: jetstream.ErrConsumerExists,
	}

	executed, err := applyRecreateConsumerAction(context.Background(), mgr, recreateConsumerAction(after))
	require.NoError(t, err)
	require.True(t, executed.Raced)
	require.True(t, mgr.deleted)
}

func TestApplyRecreateConsumer_RaceFilledNameWrongIdentity_RecreateInterrupted(t *testing.T) {
	t.Parallel()

	// Post-delete, the create returns ErrConsumerExists, but the verify re-read
	// shows the colliding consumer diverges on an immutable field. It must NOT
	// be blessed as a raced success — the desired consumer does not exist, so a
	// fail-fast error wrapping ErrRecreateInterrupted.
	after := plannedConsumer()
	drift := liveConsumerCfg(after, func(c *jetstream.ConsumerConfig) { c.MemoryStorage = true })
	mgr := &fakeConsumerManager{
		infoSeq: []consumerInfoStep{
			{cfg: drift}, // step-1 re-read: immutable drift present
			{cfg: drift}, // verify re-read: colliding consumer still wrong
		},
		createErr: jetstream.ErrConsumerExists,
	}

	_, err := applyRecreateConsumerAction(context.Background(), mgr, recreateConsumerAction(after))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrRecreateInterrupted)
	require.True(t, mgr.deleted)
	require.Contains(t, err.Error(), "consumer-identity-mismatch")
}

func TestApplyRecreateConsumer_ResourceGoneRaceFilledName_RacedTrue(t *testing.T) {
	t.Parallel()

	// The consumer was already gone at re-read (create-only branch) and a
	// concurrent creator won the create race. The verify re-read confirms the
	// colliding consumer's identity matches → raced success, not fail-fast.
	after := plannedConsumer()
	mgr := &fakeConsumerManager{
		infoSeq: []consumerInfoStep{
			{err: jetstream.ErrConsumerNotFound}, // step-1 re-read: consumer already gone
			{cfg: liveConsumerCfg(after)},        // verify re-read: identity matches
		},
		createErr: jetstream.ErrConsumerExists,
	}

	executed, err := applyRecreateConsumerAction(context.Background(), mgr, recreateConsumerAction(after))
	require.NoError(t, err)
	require.True(t, executed.Raced)
	require.False(t, mgr.deleted, "no delete when the consumer was already gone")
}

func TestApplyRecreateConsumer_ResourceGoneRaceFilledNameWrongIdentity_CreateOnlyFailFast(t *testing.T) {
	t.Parallel()

	// The consumer was already gone at re-read (create-only branch) and a
	// concurrent creator won the create race, but the colliding consumer
	// diverges on an immutable field. Nothing was destroyed, so this is an
	// ordinary fail-fast — it must NOT wrap ErrRecreateInterrupted.
	after := plannedConsumer()
	drift := liveConsumerCfg(after, func(c *jetstream.ConsumerConfig) { c.MemoryStorage = true })
	mgr := &fakeConsumerManager{
		infoSeq: []consumerInfoStep{
			{err: jetstream.ErrConsumerNotFound}, // step-1 re-read: consumer already gone
			{cfg: drift},                         // verify re-read: colliding consumer is wrong
		},
		createErr: jetstream.ErrConsumerExists,
	}

	_, err := applyRecreateConsumerAction(context.Background(), mgr, recreateConsumerAction(after))
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrRecreateInterrupted), "nothing was destroyed on the gone branch")
	require.False(t, mgr.deleted)
	require.Contains(t, err.Error(), "consumer-identity-mismatch")
}

func TestApplyRecreateConsumer_WrongResourceType_FailsFast(t *testing.T) {
	t.Parallel()

	mgr := &fakeConsumerManager{}
	// A value RecreateConsumerResource (wrong: recreate-consumer carries a pointer).
	action := PlannedAction{Kind: ActionRecreateConsumer, Name: "x", Resource: RecreateConsumerResource{}}

	_, err := applyRecreateConsumerAction(context.Background(), mgr, action)
	require.Error(t, err)
	require.Contains(t, err.Error(), "wrong Resource type")
	require.False(t, mgr.deleted)
}

// --- applyPlan / ApplyConsumers loop wiring ---------------------------------

func TestApplyRecreateKV_FoldsCleanlyIntoReport(t *testing.T) {
	t.Parallel()

	// recreate-kv routes through jsKVRecreator, which uses Stream + Info to
	// re-read and DeleteKeyValue + CreateKeyValue to recreate. applyPlanJS
	// covers Stream / DeleteStream but not DeleteKeyValue / CreateKeyValue, so
	// this confirms the helper's clean result folds into the report; the full
	// applyPlan recreate-kv wiring is exercised end-to-end by
	// TestApplyRecreate_KV_EndToEnd (integration).
	after := recreateKVAfter()
	seam := &fakeKVRecreator{infoCfg: liveKVStream()}
	executed, err := applyRecreateKVAction(context.Background(), seam, recreateKVAction(after))
	require.NoError(t, err)
	done, ferr := foldActionResult(&Report{}, nil, 0, recreateKVAction(after), executed, err)
	require.False(t, done)
	require.NoError(t, ferr)
}

func TestApplyPlan_RecreateStream_FoldsIntoExecuted(t *testing.T) {
	t.Parallel()

	after := buildStreamConfig(ordersStreamCfg(), "prod")
	js := &applyPlanJS{streamInfo: map[string]jetstream.StreamConfig{"orders": markedAppStreamFile()}}
	plan := PlanResult{Actions: []PlannedAction{recreateStreamAction(after)}}

	rep, err := applyPlan(context.Background(), js, streamApplyCfg(ordersStreamCfg()), plan)
	require.NoError(t, err)
	require.False(t, rep.Aborted)
	require.Empty(t, rep.Errors)
	require.Len(t, rep.Executed, 1)
	require.Equal(t, ActionRecreateStream, rep.Executed[0].Kind)
	require.Equal(t, []string{"orders"}, js.deletedNames)
	require.Equal(t, []string{"orders"}, js.createdNames)
}

func TestApplyPlan_RecreateStream_PostDeleteFailure_ResourceErrorNotAborted(t *testing.T) {
	t.Parallel()

	// The create step fails after the delete succeeded: foldActionResult records
	// a ResourceError (exit 1), Aborted stays false.
	after := buildStreamConfig(ordersStreamCfg(), "prod")
	js := &applyPlanJS{
		streamInfo: map[string]jetstream.StreamConfig{"orders": markedAppStreamFile()},
		createErr:  errors.New("invalid subject"),
	}
	plan := PlanResult{Actions: []PlannedAction{recreateStreamAction(after)}}

	rep, err := applyPlan(context.Background(), js, streamApplyCfg(ordersStreamCfg()), plan)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrRecreateInterrupted)
	require.False(t, rep.Aborted, "a post-delete failure is a resource error, not an abort")
	require.Len(t, rep.Errors, 1)
	require.Equal(t, "orders", rep.Errors[0].Name)
	require.Equal(t, []string{"orders"}, js.deletedNames, "the delete ran")
}

func TestApplyConsumers_RecreateConsumer_FoldsIntoExecuted(t *testing.T) {
	t.Parallel()

	after := plannedConsumer()
	// Seed the live consumer with identity drift so the recreate is not a no-op.
	live := liveConsumerCfg(after, func(c *jetstream.ConsumerConfig) { c.MemoryStorage = true })
	js := &applyConsumersJS{consumerInfo: map[string]jetstream.ConsumerConfig{after.Durable: live}}
	plan := PlanResult{Actions: []PlannedAction{recreateConsumerAction(after)}}

	rep, err := ApplyConsumers(context.Background(), js, plan)
	require.NoError(t, err)
	require.False(t, rep.Aborted)
	require.Empty(t, rep.Errors)
	require.Len(t, rep.Executed, 1)
	require.Equal(t, ActionRecreateConsumer, rep.Executed[0].Kind)
	require.Equal(t, []string{after.Durable}, js.deletedNames)
	require.Equal(t, []string{after.Durable}, js.createdNames)
}

func TestApplyConsumers_RecreateConsumer_StalePlanNoOp_RacedNoDelete(t *testing.T) {
	t.Parallel()

	// The live consumer matches the desired identity: ApplyConsumers records a
	// raced success without deleting.
	after := plannedConsumer()
	js := &applyConsumersJS{
		consumerInfo: map[string]jetstream.ConsumerConfig{after.Durable: liveConsumerCfg(after)},
	}
	plan := PlanResult{Actions: []PlannedAction{recreateConsumerAction(after)}}

	rep, err := ApplyConsumers(context.Background(), js, plan)
	require.NoError(t, err)
	require.Len(t, rep.Executed, 1)
	require.True(t, rep.Executed[0].Raced)
	require.Empty(t, js.deletedNames, "NO delete on the stale-plan no-op")
}

// recreateConsumerForDurable builds a PlannedConsumer with the dynamicbuild
// defaults — used to confirm the consumer fixtures stay in sync with the
// builder PlanConsumers uses.
func recreateConsumerForDurable(stream, subject, durable string) PlannedConsumer {
	return PlannedConsumer{
		StreamName: stream,
		Subject:    subject,
		Durable:    durable,
		Config:     dynamicbuild.ConsumerConfig(durable, subject, dynamicbuild.DefaultDynamicDefaults()),
	}
}

func TestApplyRecreateConsumer_BuilderDerivedConfig_NoOpWhenIdentical(t *testing.T) {
	t.Parallel()

	// A consumer whose live config exactly equals the builder output has no
	// identity drift — the recreate is a no-op even though the plan emitted it.
	after := recreateConsumerForDurable("orders", "orders.a", "ord-worker_a_deadbeefdeadbeef")
	mgr := &fakeConsumerManager{infoCfg: after.Config}

	executed, err := applyRecreateConsumerAction(context.Background(), mgr, recreateConsumerAction(after))
	require.NoError(t, err)
	require.True(t, executed.Raced)
	require.False(t, mgr.deleted)
}
