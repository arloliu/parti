package provision

// Same-package unit tests for the consumer apply seam:
// applyCreateConsumerAction, verifyRacedConsumer, and the ApplyConsumers loop
// wiring. These use a fake consumerManager so they run without a live NATS
// server.

import (
	"context"
	"errors"
	"testing"

	"github.com/arloliu/parti/v2/internal/dynamicbuild"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// --- fake consumerManager seam ----------------------------------------------

// fakeConsumerManager drives the consumer apply helpers deterministically.
// createErr forces CreateConsumer to fail; infoCfg / infoErr drive the
// ConsumerInfo re-read. created records whether CreateConsumer ran and the
// (stream, cfg) it was handed.
type fakeConsumerManager struct {
	createErr error
	created   bool
	createGot jetstream.ConsumerConfig
	createGS  string

	infoCfg jetstream.ConsumerConfig
	infoErr error
	infoGot bool
}

func (m *fakeConsumerManager) CreateConsumer(_ context.Context, stream string, cfg jetstream.ConsumerConfig) error {
	m.created = true
	m.createGS = stream
	m.createGot = cfg

	return m.createErr
}

func (m *fakeConsumerManager) ConsumerInfo(_ context.Context, _, _ string) (*jetstream.ConsumerInfo, error) {
	m.infoGot = true
	if m.infoErr != nil {
		return nil, m.infoErr
	}

	return &jetstream.ConsumerInfo{Config: m.infoCfg}, nil
}

// plannedConsumer returns a synthetic PlannedConsumer built from the shared
// dynamicbuild helpers with the runtime defaults — exactly what PlanConsumers
// emits. mutate can override fields for the mismatch tests.
func plannedConsumer(mutate ...func(*jetstream.ConsumerConfig)) PlannedConsumer {
	const (
		stream  = "orders"
		subject = "orders.a"
		durable = "ord-worker_a_deadbeefdeadbeef"
	)
	cfg := dynamicbuild.ConsumerConfig(durable, subject, dynamicbuild.DefaultDynamicDefaults())
	for _, m := range mutate {
		m(&cfg)
	}

	return PlannedConsumer{
		StreamName: stream,
		Subject:    subject,
		Durable:    durable,
		Config:     cfg,
	}
}

// createConsumerAction wraps a PlannedConsumer in an ActionCreateConsumer.
func createConsumerAction(pc PlannedConsumer) PlannedAction {
	return PlannedAction{Kind: ActionCreateConsumer, Name: pc.Durable, Resource: pc}
}

// liveConsumerCfg returns the live ConsumerConfig a matching consumer carries —
// the desired identity / immutable fields, optionally mutated.
func liveConsumerCfg(pc PlannedConsumer, mutate ...func(*jetstream.ConsumerConfig)) jetstream.ConsumerConfig {
	cfg := pc.Config
	for _, m := range mutate {
		m(&cfg)
	}

	return cfg
}

// --- applyCreateConsumerAction ----------------------------------------------

func TestApplyCreateConsumer_HappyPath_CreatesConsumer(t *testing.T) {
	t.Parallel()

	pc := plannedConsumer()
	mgr := &fakeConsumerManager{}

	executed, err := applyCreateConsumerAction(context.Background(), mgr, createConsumerAction(pc))
	require.NoError(t, err)
	require.Equal(t, ActionCreateConsumer, executed.Kind)
	require.Equal(t, pc.Durable, executed.Name)
	require.False(t, executed.Raced)
	require.True(t, mgr.created)
	require.Equal(t, pc.StreamName, mgr.createGS)
	require.Equal(t, pc.Config.Durable, mgr.createGot.Durable)
	require.False(t, mgr.infoGot, "no re-read on a clean create")
}

func TestApplyCreateConsumer_ConsumerExists_IdentityMatches_RacedTrue(t *testing.T) {
	t.Parallel()

	pc := plannedConsumer()
	// The live consumer matches every identity / immutable field; only the
	// runtime-owned mutable tunables differ.
	mgr := &fakeConsumerManager{
		createErr: jetstream.ErrConsumerExists,
		infoCfg: liveConsumerCfg(pc, func(c *jetstream.ConsumerConfig) {
			c.AckWait = 99 // a mutable tunable — must not block the raced success
			c.MaxDeliver = 3
		}),
	}

	executed, err := applyCreateConsumerAction(context.Background(), mgr, createConsumerAction(pc))
	require.NoError(t, err)
	require.True(t, executed.Raced, "ErrConsumerExists with matching identity fields is a raced success")
	require.True(t, mgr.infoGot, "the create-race re-read ran")
}

func TestApplyCreateConsumer_NameAlreadyInUse_IdentityMatches_RacedTrue(t *testing.T) {
	t.Parallel()

	pc := plannedConsumer()
	mgr := &fakeConsumerManager{
		createErr: jetstream.ErrConsumerNameAlreadyInUse,
		infoCfg:   liveConsumerCfg(pc),
	}

	executed, err := applyCreateConsumerAction(context.Background(), mgr, createConsumerAction(pc))
	require.NoError(t, err)
	require.True(t, executed.Raced, "ErrConsumerNameAlreadyInUse with matching identity is a raced success")
}

func TestApplyCreateConsumer_ConsumerExists_IdentityMismatch_FailsFast(t *testing.T) {
	t.Parallel()

	// Each case mutates one identity / immutable field of the live consumer
	// and asserts the fail-fast error names exactly that field.
	cases := []struct {
		name  string
		field string
		mut   func(*jetstream.ConsumerConfig)
	}{
		{"filterSubject", "filterSubject", func(c *jetstream.ConsumerConfig) { c.FilterSubject = "orders.WRONG" }},
		{"ackPolicy", "ackPolicy", func(c *jetstream.ConsumerConfig) { c.AckPolicy = jetstream.AckNonePolicy }},
		{"deliverPolicy", "deliverPolicy", func(c *jetstream.ConsumerConfig) { c.DeliverPolicy = jetstream.DeliverLastPolicy }},
		{"maxWaiting", "maxWaiting", func(c *jetstream.ConsumerConfig) { c.MaxWaiting = 17 }},
		{"memoryStorage", "memoryStorage", func(c *jetstream.ConsumerConfig) { c.MemoryStorage = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pc := plannedConsumer()
			mgr := &fakeConsumerManager{
				createErr: jetstream.ErrConsumerExists,
				infoCfg:   liveConsumerCfg(pc, tc.mut),
			}

			_, err := applyCreateConsumerAction(context.Background(), mgr, createConsumerAction(pc))
			require.Error(t, err)
			require.False(t, errors.Is(err, context.Canceled))
			require.Contains(t, err.Error(), "consumer-identity-mismatch")
			require.Contains(t, err.Error(), tc.field, "the error names the offending field")
		})
	}
}

func TestApplyCreateConsumer_ConsumerExists_RereadNotFound_FailsFast(t *testing.T) {
	t.Parallel()

	// The consumer was deleted between the failed create and the re-read.
	pc := plannedConsumer()
	mgr := &fakeConsumerManager{
		createErr: jetstream.ErrConsumerExists,
		infoErr:   jetstream.ErrConsumerNotFound,
	}

	_, err := applyCreateConsumerAction(context.Background(), mgr, createConsumerAction(pc))
	require.Error(t, err)
	require.False(t, errors.Is(err, context.Canceled))
	require.Contains(t, err.Error(), "consumer-deleted-during-apply")
}

func TestApplyCreateConsumer_ConsumerExists_RereadCancelled_PropagatesCancellation(t *testing.T) {
	t.Parallel()

	pc := plannedConsumer()
	mgr := &fakeConsumerManager{
		createErr: jetstream.ErrConsumerExists,
		infoErr:   context.Canceled,
	}

	_, err := applyCreateConsumerAction(context.Background(), mgr, createConsumerAction(pc))
	require.ErrorIs(t, err, context.Canceled)
}

func TestApplyCreateConsumer_ConsumerExists_RereadServerError_FailsFast(t *testing.T) {
	t.Parallel()

	pc := plannedConsumer()
	mgr := &fakeConsumerManager{
		createErr: jetstream.ErrConsumerExists,
		infoErr:   errors.New("connection reset"),
	}

	_, err := applyCreateConsumerAction(context.Background(), mgr, createConsumerAction(pc))
	require.Error(t, err)
	require.False(t, errors.Is(err, context.Canceled))
	require.Contains(t, err.Error(), "re-read consumer")
}

func TestApplyCreateConsumer_WrongResourceType_FailsFast(t *testing.T) {
	t.Parallel()

	mgr := &fakeConsumerManager{}
	// A *PlannedConsumer pointer (wrong: create-consumer carries a value).
	pc := plannedConsumer()
	action := PlannedAction{Kind: ActionCreateConsumer, Name: pc.Durable, Resource: &pc}

	_, err := applyCreateConsumerAction(context.Background(), mgr, action)
	require.Error(t, err)
	require.Contains(t, err.Error(), "wrong Resource type")
	require.False(t, mgr.created, "no write on a wrong-type guard failure")
}

func TestApplyCreateConsumer_StreamNotFound_ConsumerStreamMissing(t *testing.T) {
	t.Parallel()

	pc := plannedConsumer()
	mgr := &fakeConsumerManager{createErr: jetstream.ErrStreamNotFound}

	_, err := applyCreateConsumerAction(context.Background(), mgr, createConsumerAction(pc))
	require.Error(t, err)
	require.False(t, errors.Is(err, context.Canceled))
	require.Contains(t, err.Error(), "consumer-stream-missing")
}

func TestApplyCreateConsumer_Cancelled_PropagatesCancellation(t *testing.T) {
	t.Parallel()

	pc := plannedConsumer()
	mgr := &fakeConsumerManager{createErr: context.Canceled}

	_, err := applyCreateConsumerAction(context.Background(), mgr, createConsumerAction(pc))
	require.ErrorIs(t, err, context.Canceled)
}

func TestApplyCreateConsumer_ServerRejected_FailsFast(t *testing.T) {
	t.Parallel()

	pc := plannedConsumer()
	mgr := &fakeConsumerManager{createErr: errors.New("consumer config invalid")}

	_, err := applyCreateConsumerAction(context.Background(), mgr, createConsumerAction(pc))
	require.Error(t, err)
	require.False(t, errors.Is(err, context.Canceled))
	require.Contains(t, err.Error(), "consumer config invalid")
}

// --- ApplyConsumers loop wiring ---------------------------------------------

// applyConsumersJS is a fake jetstream.JetStream covering exactly the methods
// ApplyConsumers needs via jsConsumerManager: CreateConsumer and Consumer.
// createErr forces every create to fail; createdNames records what ran.
type applyConsumersJS struct {
	jetstream.JetStream
	createErr    error
	createdNames []string
}

func (j *applyConsumersJS) CreateConsumer(
	_ context.Context, _ string, cfg jetstream.ConsumerConfig,
) (jetstream.Consumer, error) {
	if j.createErr != nil {
		return nil, j.createErr
	}
	j.createdNames = append(j.createdNames, cfg.Durable)

	return nil, nil //nolint:nilnil // fake: ApplyConsumers discards the Consumer.
}

func TestApplyConsumers_NoAction_EnvelopeReport(t *testing.T) {
	t.Parallel()

	// nil JetStream: a reached NATS call would panic, proving the no-action
	// path performs no I/O.
	report, err := ApplyConsumers(context.Background(), nil, PlanResult{Actions: []PlannedAction{}})
	require.NoError(t, err)
	require.Equal(t, APIVersionProvisionV1, report.APIVersion)
	require.Equal(t, KindReport, report.Kind)
	require.NotNil(t, report.Executed)
	require.NotNil(t, report.Skipped)
	require.NotNil(t, report.Errors)
	require.Empty(t, report.Executed)
}

func TestApplyConsumers_CreateConsumer_FoldsIntoExecuted(t *testing.T) {
	t.Parallel()

	pc := plannedConsumer()
	js := &applyConsumersJS{}
	plan := PlanResult{Actions: []PlannedAction{createConsumerAction(pc)}}

	rep, err := ApplyConsumers(context.Background(), js, plan)
	require.NoError(t, err)
	require.False(t, rep.Aborted)
	require.Empty(t, rep.Errors)
	require.Len(t, rep.Executed, 1)
	require.Equal(t, ActionCreateConsumer, rep.Executed[0].Kind)
	require.Equal(t, []string{pc.Durable}, js.createdNames)
}

func TestApplyConsumers_UnsupportedKind_FailsFast(t *testing.T) {
	t.Parallel()

	// PlanConsumers emits only create-consumer; a foreign kind is a defensive
	// fail-fast.
	plan := PlanResult{Actions: []PlannedAction{
		{Kind: ActionCreateKV, Name: "x"},
	}}
	rep, err := ApplyConsumers(context.Background(), nil, plan)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported action kind")
	require.Len(t, rep.Errors, 1)
	require.False(t, rep.Aborted)
}

func TestApplyConsumers_FailFast_RemainderSkippedWithPriorError(t *testing.T) {
	t.Parallel()

	// First action fails (server-rejected create); the remaining action lands
	// in Skipped with prior-error, Aborted stays false.
	pc1 := plannedConsumer()
	pc2 := plannedConsumer(func(c *jetstream.ConsumerConfig) {
		c.Name = "ord-worker_b_cafecafecafecafe"
		c.Durable = "ord-worker_b_cafecafecafecafe"
		c.FilterSubject = "orders.b"
	})
	pc2.Durable = "ord-worker_b_cafecafecafecafe"
	pc2.Subject = "orders.b"

	js := &applyConsumersJS{createErr: errors.New("consumer config invalid")}
	plan := PlanResult{Actions: []PlannedAction{
		createConsumerAction(pc1),
		createConsumerAction(pc2),
	}}

	rep, err := ApplyConsumers(context.Background(), js, plan)
	require.Error(t, err)
	require.False(t, rep.Aborted, "a non-cancellation failure is fail-fast, not aborted")
	require.Len(t, rep.Errors, 1)
	require.Equal(t, pc1.Durable, rep.Errors[0].Name)
	require.Len(t, rep.Skipped, 1)
	require.Equal(t, pc2.Durable, rep.Skipped[0].Name)
	require.Equal(t, SkipReasonPriorError, rep.Skipped[0].Reason)
}

func TestApplyConsumers_CancelledMidApply_AbortedAndSkipped(t *testing.T) {
	t.Parallel()

	// The first create-consumer is cancelled mid-write; it plus the remaining
	// action go to Skipped with context-cancelled, Aborted is true.
	pc1 := plannedConsumer()
	pc2 := plannedConsumer(func(c *jetstream.ConsumerConfig) {
		c.Name = "ord-worker_b_cafecafecafecafe"
		c.Durable = "ord-worker_b_cafecafecafecafe"
		c.FilterSubject = "orders.b"
	})
	pc2.Durable = "ord-worker_b_cafecafecafecafe"
	pc2.Subject = "orders.b"

	js := &applyConsumersJS{createErr: context.Canceled}
	plan := PlanResult{Actions: []PlannedAction{
		createConsumerAction(pc1),
		createConsumerAction(pc2),
	}}

	rep, err := ApplyConsumers(context.Background(), js, plan)
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, rep.Aborted)
	require.Empty(t, rep.Errors, "a cancellation is not a resource error")
	require.Len(t, rep.Skipped, 2, "the cancelled action and the remainder are skipped")
	for _, s := range rep.Skipped {
		require.Equal(t, SkipReasonContextCancelled, s.Reason)
	}
}

func TestApplyConsumers_PreMutationCancel_AllSkipped(t *testing.T) {
	t.Parallel()

	// ctx is already cancelled before the loop: every action is skipped with
	// context-cancelled, no write attempted.
	pc := plannedConsumer()
	js := &applyConsumersJS{}
	plan := PlanResult{Actions: []PlannedAction{createConsumerAction(pc)}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rep, err := ApplyConsumers(ctx, js, plan)
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, rep.Aborted)
	require.Len(t, rep.Skipped, 1)
	require.Equal(t, SkipReasonContextCancelled, rep.Skipped[0].Reason)
	require.Empty(t, js.createdNames, "no write on a pre-mutation cancel")
}
