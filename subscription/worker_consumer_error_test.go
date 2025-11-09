package subscription

import (
	"errors"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// TestClassifyConsumerError validates mapping of errors to classification reasons.
func TestClassifyConsumerError(t *testing.T) { // single test with table cases
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil error", err: nil, want: "unknown"},
		{name: "consumer not found sentinel", err: jetstream.ErrConsumerNotFound, want: "not_found"},
		{name: "api error 401", err: &nats.APIError{Code: 401}, want: "terminal_policy"},
		{name: "api error 403", err: &nats.APIError{Code: 403}, want: "terminal_policy"},
		// Note: Using generic 404 codes; ErrorCode constants omitted for portability in test env.
		{name: "api error 404 consumer", err: &nats.APIError{Code: 404}, want: "not_found"},
		{name: "api error 404 stream", err: &nats.APIError{Code: 404}, want: "not_found"},
		{name: "api error 409", err: &nats.APIError{Code: 409}, want: "throttle"},
		{name: "api error 429", err: &nats.APIError{Code: 429}, want: "throttle"},
		{name: "api error 500", err: &nats.APIError{Code: 500}, want: "transient_server"},
		{name: "generic error", err: errors.New("boom"), want: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyConsumerError(tt.err)
			if got != tt.want {
				t.Fatalf("classifyConsumerError(%v) = %s, want %s", tt.err, got, tt.want)
			}
		})
	}
}
