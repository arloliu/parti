package controller

import (
	"context"
	"errors"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/arloliu/parti/v2/provision"
)

// provisionClass is the single classification runProvision assigns to a
// reconcile's provisioning outcome. The W4 reconciler branches purely on this
// value — it never re-inspects the raw error — so an apply-resource failure is
// never mis-routed as a transient NATS fault and a cancelled reconcile is
// never recorded on a dead ctx.
type provisionClass int

const (
	// ClassApplyAborted means a step returned an error wrapping
	// context.Canceled / context.DeadlineExceeded — the reconcile ctx was
	// cancelled. It is checked FIRST, regardless of which step produced the
	// error or whether Report.Aborted is set. W4 skips the status write
	// entirely for this class (the ctx is dead).
	ClassApplyAborted provisionClass = iota

	// ClassOK means provision.Apply returned a nil error — the environment
	// converged (or was already converged).
	ClassOK

	// ClassValidation means step 1 (provision.Validate) failed: the Config is
	// statically invalid. W4 → InvalidSpec (no requeue).
	ClassValidation

	// ClassTransientNATS means step 2 (provision.Plan) failed with a
	// non-cancellation error, or provision.Apply returned a non-cancellation
	// error with an empty Report (an IO/connect failure before any mutation).
	// W4 → NATSUnreachable (backoff).
	ClassTransientNATS

	// ClassApplyResourceError means provision.Apply returned a
	// non-cancellation error with a non-empty Report.Errors — some
	// resource(s) were rejected. W4 → ApplyError (periodic recheck).
	ClassApplyResourceError
)

// provisionOutcome is the result of one runProvision call. It always carries
// the partial Plan and Report the SDK produced — even in an error branch — so
// the W4 reconciler can surface the executed/error counts and drift summary
// on the CR status regardless of how provisioning ended.
type provisionOutcome struct {
	// Plan is the read-only plan computed in step 2. It is the source of the
	// status drift counts. Zero-value when step 1 or step 2 failed.
	Plan provision.PlanResult

	// Report is the apply outcome from step 3. provision.Apply returns a
	// populated Report alongside a non-nil error for both a mid-apply
	// cancellation and a resource-level failure; runProvision preserves it
	// verbatim. Zero-value when provisioning stopped before step 3.
	Report provision.Report

	// Class is the single classification W4 branches on.
	Class provisionClass
}

// runProvision drives the provision SDK against a connected JetStream context:
// it validates the Config, plans (for the drift summary), and applies.
//
// The three steps:
//  1. provision.Validate(cfg)      — static validation.
//  2. provision.Plan(ctx, js, cfg) — read-only; PlanResult.Drift feeds the
//     status drift counts. provision.Apply re-plans internally; this extra
//     Plan exists solely to surface the drift summary and is cheap.
//  3. provision.Apply(ctx, js, cfg) — the mutation.
//
// Cancellation is classified FIRST. Before inspecting any Report shape,
// runProvision checks each returned error: if it wraps context.Canceled or
// context.DeadlineExceeded, the class is ClassApplyAborted — regardless of
// which step produced it or whether Report.Aborted is set. provision.Apply
// surfaces a cancelled ctx in two distinct shapes — a pre-action cancel
// inside its internal ValidateLive/Plan returns a zero Report{} plus
// ctx.Err(), while a cancel inside the action loop returns a partial
// Report{Aborted: true} plus ctx.Err() — and classifying on the error (not on
// Report.Aborted) collapses both, plus the step-2 Plan cancel, into the
// single ClassApplyAborted outcome.
//
// runProvision returns the SDK's error verbatim alongside the outcome so the
// caller may log it; the caller branches on outcome.Class, not the error.
func runProvision(ctx context.Context, js jetstream.JetStream, cfg provision.Config) (provisionOutcome, error) {
	// Step 1: static validation. provision.Validate takes no ctx, so a
	// cancelled ctx cannot turn a static-invalid config into an abort here.
	if err := provision.Validate(cfg); err != nil {
		// A validation error cannot wrap context cancellation (Validate is
		// pure), so this is unconditionally ClassValidation.
		return provisionOutcome{Class: ClassValidation}, err
	}

	// Step 2: read-only plan — the drift-summary source.
	plan, err := provision.Plan(ctx, js, cfg)
	if err != nil {
		if isCancellation(err) {
			return provisionOutcome{Class: ClassApplyAborted}, err
		}
		// A non-cancellation Plan failure on a statically-valid config is a
		// NATS/IO problem.
		return provisionOutcome{Class: ClassTransientNATS}, err
	}

	// Step 3: the mutation. provision.Apply returns a populated Report
	// alongside a non-nil error for both a mid-apply cancellation and a
	// resource-level failure; preserve it verbatim in every branch.
	report, err := provision.Apply(ctx, js, cfg)

	return provisionOutcome{
		Plan:   plan,
		Report: report,
		Class:  classifyApplyResult(report, err),
	}, err
}

// classifyApplyResult maps the (Report, error) pair provision.Apply returns
// onto a provisionClass. Cancellation is checked FIRST — before the Report
// shape — so a cancelled ctx is always ClassApplyAborted, whether Apply
// returned a zero Report{} (cancel inside its pre-action ValidateLive/Plan) or
// a partial Report{Aborted: true} (cancel inside the action loop).
func classifyApplyResult(report provision.Report, err error) provisionClass {
	switch {
	case err == nil:
		return ClassOK
	case isCancellation(err):
		return ClassApplyAborted
	case len(report.Errors) > 0:
		// A non-cancellation Apply failure with resource-level errors: some
		// resource(s) were rejected.
		return ClassApplyResourceError
	default:
		// A non-cancellation Apply failure with an empty Report: a
		// connect/IO failure before any mutation was recorded.
		return ClassTransientNATS
	}
}

// isCancellation reports whether err is (or wraps) context.Canceled or
// context.DeadlineExceeded — the signal that the reconcile ctx was cancelled.
func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
