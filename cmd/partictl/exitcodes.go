package main

import (
	"context"
	"errors"

	"github.com/arloliu/parti/v2/provision"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Exit code constants. These are stable across releases — new codes only
// appear above 4 to avoid CI breakage.
const (
	// ExitOK is success.
	ExitOK = 0
	// ExitRuntime is a runtime/generic error (operation failed after preflight).
	ExitRuntime = 1
	// ExitDrift is drift detected and not resolved (only with -fail-on-drift).
	ExitDrift = 2
	// ExitValidation is a static or live validation error, or a flag-parse error.
	ExitValidation = 3
	// ExitNATS is a NATS connect, auth, or context timeout/cancel failure.
	ExitNATS = 4
)

// classifyError maps err to the appropriate CLI exit code per the plan's
// Exit code precedence table:
//
//  1. ErrInvalidConfig (static) → 3
//  2. context.Canceled / context.DeadlineExceeded → 4
//  3. ErrLiveValidation (live) → 3
//  4. NATS permission errors (after connect) → 3
//  5. Anything else → 1
//
// NATS connect errors are not wrapped in a sentinel; callers that produce a
// NATS connect error (natsconn.go) should return ExitNATS directly rather
// than routing through this helper.
//
// Note: context-error precedence over permission errors is intentional — a
// cancelled context can surface as ErrNoResponders from nats.go.
func classifyError(err error) int {
	if err == nil {
		return ExitOK
	}
	// Static validation (highest priority of error sentinels).
	if errors.Is(err, provision.ErrInvalidConfig) {
		return ExitValidation
	}
	// Context cancellation / timeout → NATS/infra failure code.
	// Must come before permission checks: a cancelled context can surface
	// as ErrNoResponders.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ExitNATS
	}
	// Live validation.
	if errors.Is(err, provision.ErrLiveValidation) {
		return ExitValidation
	}
	// NATS permission errors after a successful connect (e.g., from
	// provision.Plan / provision.View) map to validation class (exit 3).
	// These differ from connect-time auth failures, which are handled by
	// natsconn.go and never reach classifyError.
	if errors.Is(err, nats.ErrPermissionViolation) ||
		errors.Is(err, jetstream.ErrJetStreamNotEnabled) ||
		errors.Is(err, jetstream.ErrJetStreamNotEnabledForAccount) ||
		errors.Is(err, nats.ErrNoResponders) {
		return ExitValidation
	}
	// Everything else is a runtime error.
	return ExitRuntime
}

// hasDrift reports whether a PlanResult contains any non-informational drift
// (adopted, drift-mutable, or drift-immutable severity).
func hasDrift(drift []provision.DriftFinding) bool {
	for _, d := range drift {
		switch d.Severity {
		case provision.SeverityAdopted,
			provision.SeverityDriftMutable,
			provision.SeverityDriftImmutable:
			return true
		}
	}

	return false
}
