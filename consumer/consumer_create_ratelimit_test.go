package consumer

import (
	"context"
	"testing"

	"github.com/arloliu/parti/v2/internal/ratelimit"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Helper: build a minimal Dynamic that avoids a real NATS connection. ---

// nopHandler satisfies MessageHandler.
type nopHandler struct{}

func (h *nopHandler) Handle(_ context.Context, _ jetstream.Msg) error { return nil }

// buildDynamic constructs a Dynamic with the given options; uses mockJS.
func buildDynamic(t *testing.T, opts ...DynamicOption) (*Dynamic, error) {
	t.Helper()
	js := &mockJS{}
	return NewDynamic(
		js,
		"test-stream",
		"test-prefix",
		"test.{{.PartitionID}}",
		&nopHandler{},
		opts...,
	)
}

// --- Tests: default leaves limiter nil ---

func TestWithConsumerCreateRate_DefaultIsNil(t *testing.T) {
	d, err := buildDynamic(t)
	require.NoError(t, err)
	// The inner WorkerConsumerConfig must have nil limiter by default.
	assert.Nil(t, d.inner.ConsumerCreateLimiter(), "no rate option => limiter must be nil")
}

// --- Tests: WithConsumerCreateRate enables pacing ---

func TestWithConsumerCreateRate_EnablesPacing(t *testing.T) {
	d, err := buildDynamic(t, WithConsumerCreateRate(100, 10))
	require.NoError(t, err)
	assert.NotNil(t, d.inner.ConsumerCreateLimiter(), "WithConsumerCreateRate must set a non-nil limiter")
}

// --- Tests: non-nil injected overrides rate (any order) ---

func TestWithConsumerCreateLimiter_OverridesRate_LimiterFirst(t *testing.T) {
	injected := &stubLimiter{}
	d, err := buildDynamic(t,
		WithConsumerCreateLimiter(injected),
		WithConsumerCreateRate(100, 10),
	)
	require.NoError(t, err)
	assert.Same(t, injected, d.inner.ConsumerCreateLimiter(), "injected limiter must win over rate (limiter option first)")
}

func TestWithConsumerCreateLimiter_OverridesRate_RateFirst(t *testing.T) {
	injected := &stubLimiter{}
	d, err := buildDynamic(t,
		WithConsumerCreateRate(100, 10),
		WithConsumerCreateLimiter(injected),
	)
	require.NoError(t, err)
	assert.Same(t, injected, d.inner.ConsumerCreateLimiter(), "injected limiter must win over rate (rate option first)")
}

// --- Tests: WithConsumerCreateLimiter(nil) is a no-op ---

func TestWithConsumerCreateLimiter_NilIsNoOp_AfterRate(t *testing.T) {
	// Rate is set, then nil injected: nil must NOT clear the rate.
	d, err := buildDynamic(t,
		WithConsumerCreateRate(100, 10),
		WithConsumerCreateLimiter(nil), // no-op
	)
	require.NoError(t, err)
	assert.NotNil(t, d.inner.ConsumerCreateLimiter(), "WithConsumerCreateLimiter(nil) must not clear a configured rate")
}

func TestWithConsumerCreateLimiter_NilIsNoOp_BeforeRate(t *testing.T) {
	d, err := buildDynamic(t,
		WithConsumerCreateLimiter(nil), // no-op
		WithConsumerCreateRate(100, 10),
	)
	require.NoError(t, err)
	assert.NotNil(t, d.inner.ConsumerCreateLimiter(), "WithConsumerCreateLimiter(nil) first must not clear a rate configured after")
}

// --- Tests: validation ---

func TestWithConsumerCreateRate_ValidationRejectsBurstLessThanOne(t *testing.T) {
	_, err := buildDynamic(t, WithConsumerCreateRate(10, 0))
	require.Error(t, err, "burst < 1 with perSec > 0 must be rejected")
	assert.ErrorIs(t, err, ErrInvalidConfig)
}

func TestWithConsumerCreateRate_ValidationRejectsNegativePerSec(t *testing.T) {
	_, err := buildDynamic(t, WithConsumerCreateRate(-1, 10))
	require.Error(t, err, "negative perSec must be rejected")
	assert.ErrorIs(t, err, ErrInvalidConfig)
}

func TestWithConsumerCreateRate_ZeroPerSec_NoLimiter(t *testing.T) {
	// perSec == 0 means no rate configured; limiter must remain nil.
	d, err := buildDynamic(t, WithConsumerCreateRate(0, 0))
	require.NoError(t, err)
	assert.Nil(t, d.inner.ConsumerCreateLimiter(), "perSec == 0 must leave limiter nil")
}

// --- Test: injected limiter without rate still works ---

func TestWithConsumerCreateLimiter_NonNil_NoRate(t *testing.T) {
	injected := &stubLimiter{}
	d, err := buildDynamic(t, WithConsumerCreateLimiter(injected))
	require.NoError(t, err)
	assert.Same(t, injected, d.inner.ConsumerCreateLimiter())
}

// --- Test: WithMetrics without sidecar + WithConsumerCreateRate does not panic ---

// TestWithConsumerCreateRate_OldStyleMetricsWithoutSidecar verifies that
// resolveConsumerCreateLimiter handles a metrics value that satisfies
// types.WorkerConsumerMetrics but does NOT implement the optional
// consumerCreateThrottleObserver sidecar. The limiter must still be created
// and the observer field left nil (no panic, non-nil limiter).
func TestWithConsumerCreateRate_OldStyleMetricsWithoutSidecar(t *testing.T) {
	fake := &oldStyleMetrics{}

	d, err := buildDynamic(t,
		WithMetrics(fake),
		WithConsumerCreateRate(100, 10),
	)
	require.NoError(t, err)
	require.NotNil(t, d, "NewDynamic must succeed with non-sidecar metrics")
	assert.NotNil(t, d.inner.ConsumerCreateLimiter(),
		"ConsumerCreateLimiter must be non-nil when WithConsumerCreateRate is configured")
}

// oldStyleMetrics satisfies types.WorkerConsumerMetrics via interface embedding
// but does NOT implement IncrementConsumerCreateThrottled /
// ObserveConsumerCreateThrottleWait — the optional sidecar (D7).
// This models a pre-feature external collector that is unaffected by the new methods.
type oldStyleMetrics struct {
	types.WorkerConsumerMetrics
}

var _ types.WorkerConsumerMetrics = (*oldStyleMetrics)(nil)

// stubLimiter is a test double satisfying ratelimit.Limiter.
type stubLimiter struct{}

func (s *stubLimiter) Wait(_ context.Context) error { return nil }

var _ ratelimit.Limiter = (*stubLimiter)(nil)
