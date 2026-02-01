package jsutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsValidConsumerName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{name: "valid alphanumeric", input: "consumer1", expected: true},
		{name: "valid with hyphen", input: "my-consumer", expected: true},
		{name: "valid with underscore", input: "my_consumer", expected: true},
		{name: "valid mixed", input: "My-Consumer_123", expected: true},
		{name: "valid uppercase", input: "CONSUMER", expected: true},
		{name: "empty string", input: "", expected: true},
		{name: "invalid with space", input: "my consumer", expected: false},
		{name: "invalid with dot", input: "my.consumer", expected: false},
		{name: "invalid with asterisk", input: "consumer*", expected: false},
		{name: "invalid with greater than", input: "consumer>", expected: false},
		{name: "invalid with slash", input: "consumer/name", expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsValidConsumerName(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestCalculateBackoff(t *testing.T) {
	// Test that backoff increases with attempt number
	delays := make([]int64, 5)
	for i := range 5 {
		delay := calculateBackoff(i)
		delays[i] = delay.Milliseconds()

		// Verify bounds
		assert.GreaterOrEqual(t, delay.Milliseconds(), int64(10), "delay should be at least 10ms")
		assert.LessOrEqual(t, delay.Milliseconds(), int64(500), "delay should not exceed 500ms")
	}

	// Test general increasing trend (accounting for jitter)
	// Base values: 20, 40, 80, 160, 320 ms (before jitter and cap)
	// With ±10ms jitter: 10-30, 30-50, 70-90, 150-170, 310-330 ms
	// After cap: all capped at 500ms max
	assert.Less(t, delays[0], delays[2], "later attempts should generally have longer delays")
}
