package parti

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// stubCalculator satisfies assignmentCalculator but is NOT assignment.NopCalculator.
type stubCalculator struct{}

func (s *stubCalculator) Start(context.Context) error { return nil }
func (s *stubCalculator) Stop(context.Context) error  { return nil }
func (s *stubCalculator) SubscribeToStateChanges() (<-chan types.CalculatorState, func()) {
	return nil, func() {}
}
func (s *stubCalculator) TriggerRebalance(context.Context) error { return nil }
func (s *stubCalculator) GetState() types.CalculatorState        { return types.CalcStateIdle }

func TestManager_calculateAndPublish(t *testing.T) {
	t.Run("returns error for NopCalculator", func(t *testing.T) {
		m := &Manager{calculator: assignment.NewNopCalculator()}
		err := m.calculateAndPublish(t.Context())
		require.Error(t, err)
		require.Contains(t, err.Error(), "calculator not started")
	})

	t.Run("respects cancelled context", func(t *testing.T) {
		m := &Manager{calculator: &stubCalculator{}}

		ctx, cancel := context.WithCancel(t.Context())
		cancel() // Cancel immediately

		start := time.Now()
		err := m.calculateAndPublish(ctx)
		elapsed := time.Since(start)

		require.ErrorIs(t, err, context.Canceled)
		require.Less(t, elapsed, 100*time.Millisecond,
			"must return immediately on cancelled context, not sleep 500ms")
	})

	t.Run("respects context deadline", func(t *testing.T) {
		m := &Manager{calculator: &stubCalculator{}}

		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()

		start := time.Now()
		err := m.calculateAndPublish(ctx)
		elapsed := time.Since(start)

		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Less(t, elapsed, 200*time.Millisecond,
			"must return at context deadline, not sleep full 500ms")
	})

	t.Run("completes normally with active context", func(t *testing.T) {
		m := &Manager{calculator: &stubCalculator{}}

		start := time.Now()
		err := m.calculateAndPublish(t.Context())
		elapsed := time.Since(start)

		require.NoError(t, err)
		require.GreaterOrEqual(t, elapsed, 450*time.Millisecond,
			"should wait ~500ms when context is not cancelled")
	})
}
