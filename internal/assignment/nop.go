package assignment

import (
	"context"

	"github.com/arloliu/parti/types"
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
