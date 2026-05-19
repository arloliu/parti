package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/arloliu/parti/v2/provision"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func TestClassifyError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		wantCode int
	}{
		{"nil", nil, ExitOK},
		{"ErrInvalidConfig", provision.ErrInvalidConfig, ExitValidation},
		{"ErrInvalidConfig wrapped", fmt.Errorf("wrap: %w", provision.ErrInvalidConfig), ExitValidation},
		{"context.Canceled", context.Canceled, ExitNATS},
		{"context.DeadlineExceeded", context.DeadlineExceeded, ExitNATS},
		{"ErrLiveValidation", provision.ErrLiveValidation, ExitValidation},
		{"ErrLiveValidation wrapped", fmt.Errorf("wrap: %w", provision.ErrLiveValidation), ExitValidation},
		{"generic error", errors.New("some runtime error"), ExitRuntime},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyError(tc.err)
			if got != tc.wantCode {
				t.Errorf("classifyError(%v) = %d, want %d", tc.err, got, tc.wantCode)
			}
		})
	}
}

// TestClassifyError_NATSPermissionDenied verifies that NATS permission errors
// returned after a successful connect (e.g., from provision.Plan) map to
// ExitValidation (exit 3), not ExitRuntime (exit 1).
func TestClassifyError_NATSPermissionDenied(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{"ErrPermissionViolation direct", nats.ErrPermissionViolation},
		{"ErrPermissionViolation wrapped", fmt.Errorf("provision: lookup KV_foo: %w", nats.ErrPermissionViolation)},
		{"ErrJetStreamNotEnabled", jetstream.ErrJetStreamNotEnabled},
		{"ErrJetStreamNotEnabled wrapped", fmt.Errorf("provision: info KV_bar: %w", jetstream.ErrJetStreamNotEnabled)},
		{"ErrJetStreamNotEnabledForAccount", jetstream.ErrJetStreamNotEnabledForAccount},
		{"ErrNoResponders", nats.ErrNoResponders},
		{"ErrNoResponders wrapped", fmt.Errorf("provision: lookup KV_baz: %w", nats.ErrNoResponders)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyError(tc.err)
			if got != ExitValidation {
				t.Errorf("classifyError(%v) = %d, want %d (ExitValidation)", tc.err, got, ExitValidation)
			}
		})
	}
}

func TestHasDrift(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		drift []provision.DriftFinding
		want  bool
	}{
		{"empty", nil, false},
		{"informational only", []provision.DriftFinding{{Severity: provision.SeverityInformational}}, false},
		{"adopted", []provision.DriftFinding{{Severity: provision.SeverityAdopted}}, true},
		{"drift-mutable", []provision.DriftFinding{{Severity: provision.SeverityDriftMutable}}, true},
		{"drift-immutable", []provision.DriftFinding{{Severity: provision.SeverityDriftImmutable}}, true},
		{"mixed informational + drift", []provision.DriftFinding{
			{Severity: provision.SeverityInformational},
			{Severity: provision.SeverityDriftMutable},
		}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := hasDrift(tc.drift)
			if got != tc.want {
				t.Errorf("hasDrift() = %v, want %v", got, tc.want)
			}
		})
	}
}
