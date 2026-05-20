package provision

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
)

// Apply reconciles the live NATS environment to match cfg.
//
// The high-level sequence is:
//
//  1. Validate(cfg) — static validation. Failure returns Report{} + error
//     (CLI maps to exit 3 via ErrInvalidConfig).
//  2. ValidateLive(ctx, js, cfg) — reachability + per-bucket info probes.
//     Failure returns Report{} + error (CLI maps the underlying error to
//     exit 3 or 4 per the precedence table; no mutation has started yet).
//  3. Plan(ctx, js, cfg) — compute the deterministic action list.
//     Failure returns Report{} + error.
//  4. Iterate Plan.Actions and call js.CreateKeyValue for each create-kv,
//     mapping the result onto Report.{Executed, Skipped, Errors, Aborted}.
//
// Cancellation semantics:
//   - Pre-mutation cancel → Report{Aborted: true, Skipped: every-action
//     with reason "context-cancelled"}, error = ctx.Err(); CLI exit 4.
//   - Mid-mutation cancel → partial Report with Aborted=true, Executed
//     holds completed actions, current+remaining go to Skipped with
//     reason "context-cancelled"; error = ctx.Err(); CLI exit 4.
//
// Non-cancellation failure is fail-fast: the failing action goes to Errors
// once, remaining go to Skipped with reason "prior-error", Aborted stays
// false; CLI exit 1.
//
// Plan→Apply race: if js.CreateKeyValue returns ErrBucketExists /
// ErrStreamNameAlreadyInUse, Apply treats the outcome as success and marks
// the ExecutedAction with Raced=true. Apply does NOT re-read live state to
// drift-check the racing bucket; the next `plan` run reports any adopted
// drift.
//
// Apply does NOT echo Plan.Drift into the returned Report. Drift findings
// are a Plan concern; the CLI reads plan.Drift separately from apply
// output.
func Apply(ctx context.Context, js jetstream.JetStream, cfg Config) (Report, error) {
	// Step 1: static validation (normalize once, validate the resolved
	// form, and reuse it for the live + plan steps).
	resolved, err := normalize(cfg)
	if err != nil {
		return Report{}, err
	}
	if err := validateResolved(resolved); err != nil {
		return Report{}, err
	}

	// Step 2: live validation. Return Report{} (zero value) on failure:
	// no mutation has been attempted yet, so a populated Report would
	// misleadingly resemble a mutation-outcome Report.
	if _, err := ValidateLive(ctx, js, resolved); err != nil {
		return Report{}, err
	}

	// Step 3: plan. Same zero-value return contract: no mutation started.
	planResult, err := Plan(ctx, js, resolved)
	if err != nil {
		return Report{}, err
	}

	return applyPlan(ctx, js, resolved, planResult)
}

// applyPlan executes a pre-computed plan against js per the semantics laid
// out in the package-level Apply docstring.
//
// cfg is the resolved Config. The update-kv path needs it to re-derive
// the desired spec for a named bucket and rebuild the write target from
// a fresh re-read of live state (the create-kv path does not consult it).
func applyPlan(ctx context.Context, js jetstream.JetStream, cfg Config, plan PlanResult) (Report, error) {
	report := Report{
		APIVersion: APIVersionProvisionV1,
		Kind:       KindReport,
		Executed:   []ExecutedAction{},
		Skipped:    []SkippedAction{},
		Errors:     []ResourceError{},
	}

	// Resolve bucket name -> desired control-plane spec once so the
	// update-kv path can rebuild a write target from a fresh re-read.
	// Built from the same buildControlPlaneSpecs helper Plan uses, so
	// Plan and Apply stay in lockstep.
	cpSpecs := map[string]controlPlaneSpec{}
	if cfg.ControlPlane != nil {
		for _, spec := range buildControlPlaneSpecs(*cfg.ControlPlane) {
			cpSpecs[spec.bucket] = spec
		}
	}
	// Production seam adapters wrap js; tests inject fakes directly into
	// applyUpdateKVAction.
	reader := jsStreamReader{js: js}
	updater := jsKVUpdater{js: js}

	for i, action := range plan.Actions {
		// Pre-mutation / between-iteration cancellation check.
		if err := ctx.Err(); err != nil {
			report.Aborted = true
			appendSkipped(&report, plan.Actions[i:], SkipReasonContextCancelled)
			return report, err
		}

		switch action.Kind {
		case ActionCreateKV:
			kvCfg, ok := action.Resource.(jetstream.KeyValueConfig)
			if !ok {
				// Defensive guard: Plan should never emit this.
				err := fmt.Errorf("provision: apply: create-kv action %q has wrong Resource type %T",
					action.Name, action.Resource)
				report.Errors = append(report.Errors, ResourceError{
					Kind: action.Kind, Name: action.Name, Error: err.Error(),
				})
				appendSkipped(&report, plan.Actions[i+1:], SkipReasonPriorError)
				return report, err
			}

			_, err := js.CreateKeyValue(ctx, kvCfg)
			switch {
			case err == nil:
				report.Executed = append(report.Executed, ExecutedAction{
					Kind: action.Kind, Name: action.Name,
				})
			case errors.Is(err, jetstream.ErrBucketExists),
				errors.Is(err, jetstream.ErrStreamNameAlreadyInUse):
				// Plan→Apply race: treat as success.
				report.Executed = append(report.Executed, ExecutedAction{
					Kind: action.Kind, Name: action.Name, Raced: true,
				})
			case errors.Is(err, context.Canceled),
				errors.Is(err, context.DeadlineExceeded):
				report.Aborted = true
				appendSkipped(&report, plan.Actions[i:], SkipReasonContextCancelled)
				return report, err
			default:
				// Non-cancellation failure: fail-fast.
				report.Errors = append(report.Errors, ResourceError{
					Kind: action.Kind, Name: action.Name, Error: err.Error(),
				})
				appendSkipped(&report, plan.Actions[i+1:], SkipReasonPriorError)
				return report, fmt.Errorf("provision: apply %s %q: %w",
					action.Kind, action.Name, err)
			}

		case ActionUpdateKV:
			executed, err := applyUpdateKVAction(ctx, reader, updater, cfg, cpSpecs, action)
			switch {
			case err == nil:
				report.Executed = append(report.Executed, executed)
			case errors.Is(err, context.Canceled),
				errors.Is(err, context.DeadlineExceeded):
				report.Aborted = true
				appendSkipped(&report, plan.Actions[i:], SkipReasonContextCancelled)
				return report, err
			default:
				// Non-cancellation failure: fail-fast.
				report.Errors = append(report.Errors, ResourceError{
					Kind: action.Kind, Name: action.Name, Error: err.Error(),
				})
				appendSkipped(&report, plan.Actions[i+1:], SkipReasonPriorError)
				return report, fmt.Errorf("provision: apply %s %q: %w",
					action.Kind, action.Name, err)
			}

		case ActionStampMarker:
			executed, err := applyStampMarkerAction(ctx, reader, updater, cfg, cpSpecs, action)
			switch {
			case err == nil:
				report.Executed = append(report.Executed, executed)
			case errors.Is(err, context.Canceled),
				errors.Is(err, context.DeadlineExceeded):
				report.Aborted = true
				appendSkipped(&report, plan.Actions[i:], SkipReasonContextCancelled)
				return report, err
			default:
				// Non-cancellation failure: fail-fast.
				report.Errors = append(report.Errors, ResourceError{
					Kind: action.Kind, Name: action.Name, Error: err.Error(),
				})
				appendSkipped(&report, plan.Actions[i+1:], SkipReasonPriorError)
				return report, fmt.Errorf("provision: apply %s %q: %w",
					action.Kind, action.Name, err)
			}

		default:
			// Plan emits only create-kv, update-kv, and stamp-marker.
			// Defensive: surface an unknown kind as a fail-fast resource
			// error so operators see what was rejected.
			err := fmt.Errorf("provision: apply: unsupported action kind %q (resource %q)",
				action.Kind, action.Name)
			report.Errors = append(report.Errors, ResourceError{
				Kind: action.Kind, Name: action.Name, Error: err.Error(),
			})
			appendSkipped(&report, plan.Actions[i+1:], SkipReasonPriorError)
			return report, err
		}
	}

	return report, nil
}

// appendSkipped appends every action in actions to report.Skipped with the
// given reason. It is a no-op when actions is empty.
func appendSkipped(report *Report, actions []PlannedAction, reason string) {
	for _, a := range actions {
		report.Skipped = append(report.Skipped, SkippedAction{
			Kind: a.Kind, Name: a.Name, Reason: reason,
		})
	}
}
