package assignment

import (
	"sync"

	"github.com/arloliu/parti/types"
)

// stateSubscriber is a helper for managing state change subscriptions.
type stateSubscriber struct {
	ch     chan types.CalculatorState
	mu     sync.Mutex
	closed bool
}

// trySend sends a state update to the subscriber's channel without blocking.
func (s *stateSubscriber) trySend(state types.CalculatorState, metricsCollector types.MetricsCollector) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}

	select {
	case s.ch <- state:
	default:
		// Subscriber is slow or not ready; they will get the next update.
		// TODO: Add metricsCollector.RecordSlowSubscriber() when available
		_ = metricsCollector // Avoid unused parameter warning
	}
}

// close safely closes the subscriber's channel.
func (s *stateSubscriber) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.ch)
}
