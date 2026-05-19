package assignment

import (
	"context"

	"github.com/arloliu/parti/v2/types"
)

// NopCalculator implements a no-op assignment calculator.
// It is used when the manager is not the leader or during initialization.
type NopCalculator struct{}

// NewNopCalculator creates a new no-op calculator.
func NewNopCalculator() *NopCalculator {
	return &NopCalculator{}
}

// Start implements the Calculator interface.
func (n *NopCalculator) Start(ctx context.Context) error {
	return nil
}

// Stop implements the Calculator interface.
func (n *NopCalculator) Stop(ctx context.Context) error {
	return nil
}

// SubscribeToStateChanges implements the Calculator interface.
func (n *NopCalculator) SubscribeToStateChanges() (<-chan types.CalculatorState, func()) {
	ch := make(chan types.CalculatorState)
	close(ch)
	return ch, func() {}
}

// TriggerRebalance implements the Calculator interface.
func (n *NopCalculator) TriggerRebalance(ctx context.Context) error {
	return nil
}

// GetState implements the Calculator interface. Always reports Idle so the
// manager's reconcile arm is a no-op when no real calculator is wired.
func (n *NopCalculator) GetState() types.CalculatorState {
	return types.CalcStateIdle
}
