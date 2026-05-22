package provision_test

// Live-NATS integration tests for ApplyConsumers: the PlanConsumers →
// ApplyConsumers precreation path, idempotency, and the create-race identity
// verification. These reuse the harness helpers from
// consumer_records_integration_test.go (newJS, createOrdersStream,
// createConsumer, crConfig, crPartitions, durableName).

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/internal/dynamicbuild"
	"github.com/arloliu/parti/v2/provision"
)

// TestApplyConsumers_EndToEnd_PrecreatesConsumers verifies the core path:
// PlanConsumers finds the consumers missing, ApplyConsumers creates them, and
// each expected durable resolves live with the expected FilterSubject.
func TestApplyConsumers_EndToEnd_PrecreatesConsumers(t *testing.T) {
	js := newJS(t)
	createOrdersStream(t, js)

	parts := crPartitions("a", "b")
	cfg := crConfig(parts...)

	plan, err := provision.PlanConsumers(t.Context(), js, cfg)
	require.NoError(t, err)
	require.Len(t, plan.Actions, 2, "two consumers missing")

	report, err := provision.ApplyConsumers(t.Context(), js, plan)
	require.NoError(t, err)
	require.Len(t, report.Executed, 2)
	require.Empty(t, report.Errors)
	require.Empty(t, report.Skipped)
	for _, ex := range report.Executed {
		require.Equal(t, provision.ActionCreateConsumer, ex.Kind)
		require.False(t, ex.Raced, "a clean create is not raced")
	}

	// Each expected durable resolves live with the expected FilterSubject.
	pre, suf, _ := dynamicbuild.ParseSubjectTemplateParts(crTemplate)
	for _, p := range parts {
		subject := "orders." + p.Keys[0]
		durable := dynamicbuild.PerSubjectDurableName(crPrefix, subject, pre, suf)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		consumer, cerr := js.Consumer(ctx, crStream, durable)
		cancel()
		require.NoError(t, cerr)

		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		info, ierr := consumer.Info(ctx2)
		cancel2()
		require.NoError(t, ierr)
		require.Equal(t, subject, info.Config.FilterSubject)
	}
}

// TestApplyConsumers_EndToEnd_Idempotent verifies a second PlanConsumers →
// ApplyConsumers cycle after convergence is a no-op: PlanConsumers emits no
// create-consumer action, so ApplyConsumers has nothing to execute.
func TestApplyConsumers_EndToEnd_Idempotent(t *testing.T) {
	js := newJS(t)
	createOrdersStream(t, js)

	cfg := crConfig(crPartitions("a", "b")...)

	plan1, err := provision.PlanConsumers(t.Context(), js, cfg)
	require.NoError(t, err)
	_, err = provision.ApplyConsumers(t.Context(), js, plan1)
	require.NoError(t, err)

	// Re-plan against the converged state: no diff, no action.
	plan2, err := provision.PlanConsumers(t.Context(), js, cfg)
	require.NoError(t, err)
	require.Empty(t, plan2.Actions, "no create-consumer action when all consumers exist")

	report, err := provision.ApplyConsumers(t.Context(), js, plan2)
	require.NoError(t, err)
	require.Empty(t, report.Executed)
	require.Equal(t, provision.APIVersionProvisionV1, report.APIVersion)
	require.Equal(t, provision.KindReport, report.Kind)
}

// TestApplyConsumers_EndToEnd_RacedSuccess drives the ErrConsumerExists
// create-race verify path against a live server: a stale plan (built before the
// consumer existed) is applied after the consumer has been created with a
// differing *mutable* tunable. CreateConsumer returns ErrConsumerExists, the
// re-read confirms the identity / immutable fields match, and ApplyConsumers
// records a raced success.
func TestApplyConsumers_EndToEnd_RacedSuccess(t *testing.T) {
	js := newJS(t)
	createOrdersStream(t, js)

	cfg := crConfig(crPartitions("a")...)

	// Build the plan while the consumer is still missing.
	plan, err := provision.PlanConsumers(t.Context(), js, cfg)
	require.NoError(t, err)
	require.Len(t, plan.Actions, 1)

	// Hand-create the consumer with the runtime defaults but a differing
	// mutable tunable (AckWait): the identity / immutable fields still match,
	// so CreateConsumer in ApplyConsumers returns ErrConsumerExists and the
	// re-read must classify it as a raced success.
	durable := durableName()
	racedCfg := dynamicbuild.ConsumerConfig("", "orders.a", dynamicbuild.DefaultDynamicDefaults())
	racedCfg.Name = durable
	racedCfg.Durable = durable
	racedCfg.AckWait = 90 * time.Second // a mutable tunable — differs from the plan
	createConsumer(t, js, racedCfg)

	report, err := provision.ApplyConsumers(t.Context(), js, plan)
	require.NoError(t, err)
	require.Len(t, report.Executed, 1)
	require.True(t, report.Executed[0].Raced,
		"a pre-existing consumer with matching identity fields is a raced success")
	require.Empty(t, report.Errors)
}

// TestApplyConsumers_EndToEnd_CreateRaceIdentityMismatch verifies the apply-time
// re-read catches an identity divergence: a consumer hand-created with one of
// the planned durable names but a wrong FilterSubject must fail that action
// with a ResourceError rather than being blessed as a raced success.
func TestApplyConsumers_EndToEnd_CreateRaceIdentityMismatch(t *testing.T) {
	js := newJS(t)
	createOrdersStream(t, js)

	cfg := crConfig(crPartitions("a")...)

	plan, err := provision.PlanConsumers(t.Context(), js, cfg)
	require.NoError(t, err)
	require.Len(t, plan.Actions, 1)

	// Hand-create a consumer squatting the planned durable name but carrying a
	// wrong FilterSubject — an identity divergence.
	durable := durableName()
	squatterCfg := dynamicbuild.ConsumerConfig("", "orders.WRONG", dynamicbuild.DefaultDynamicDefaults())
	squatterCfg.Name = durable
	squatterCfg.Durable = durable
	createConsumer(t, js, squatterCfg)

	report, err := provision.ApplyConsumers(t.Context(), js, plan)
	require.Error(t, err)
	require.Contains(t, err.Error(), "consumer-identity-mismatch")
	require.Contains(t, err.Error(), "filterSubject")
	require.Len(t, report.Errors, 1)
	require.Equal(t, provision.ActionCreateConsumer, report.Errors[0].Kind)
	require.False(t, report.Aborted)
	require.Empty(t, report.Executed)
}

// TestApplyConsumers_EndToEnd_StreamMissing verifies that a create against a
// non-existent application stream fails fast with the consumer-stream-missing
// error.
func TestApplyConsumers_EndToEnd_StreamMissing(t *testing.T) {
	js := newJS(t)
	createOrdersStream(t, js)

	cfg := crConfig(crPartitions("a")...)
	plan, err := provision.PlanConsumers(t.Context(), js, cfg)
	require.NoError(t, err)
	require.Len(t, plan.Actions, 1)

	// Delete the stream between plan and apply.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	require.NoError(t, js.DeleteStream(ctx, crStream))
	cancel()

	report, err := provision.ApplyConsumers(t.Context(), js, plan)
	require.Error(t, err)
	require.Contains(t, err.Error(), "consumer-stream-missing")
	require.Len(t, report.Errors, 1)
	require.False(t, report.Aborted)
}
