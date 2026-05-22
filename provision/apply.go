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
//  4. Iterate Plan.Actions and execute each one — create-kv, update-kv,
//     stamp-marker, create-stream, update-stream, stamp-stream-marker,
//     recreate-kv, recreate-stream — mapping the result onto
//     Report.{Executed, Skipped, Errors, Aborted}.
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
	report := newReport()

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
	// Resolve stream name -> desired StreamCfg once so the update-stream path
	// can rebuild a write target from a fresh re-read. Application streams use
	// the raw NATS name, so the StreamCfg.Name is the lookup key directly.
	streamCfgs := map[string]StreamCfg{}
	for _, sc := range cfg.Streams {
		streamCfgs[sc.Name] = sc
	}
	// Production seam adapters wrap js; tests inject fakes directly into
	// applyUpdateKVAction / the stream apply / recreate helpers.
	seams := applyPlanSeams{
		js:         js,
		reader:     jsStreamReader{js: js},
		updater:    jsKVUpdater{js: js},
		streamMgr:  jsStreamManager{js: js},
		kvRec:      jsKVRecreator{js: js},
		cfg:        cfg,
		cpSpecs:    cpSpecs,
		streamCfgs: streamCfgs,
	}

	for i, action := range plan.Actions {
		// Pre-mutation / between-iteration cancellation check.
		if err := ctx.Err(); err != nil {
			report.Aborted = true
			appendSkipped(&report, plan.Actions[i:], SkipReasonContextCancelled)
			return report, err
		}

		executed, known, err := seams.dispatch(ctx, action)
		if !known {
			// Plan emits only the action kinds dispatch handles. Defensive:
			// surface an unknown kind as a fail-fast resource error so
			// operators see what was rejected.
			err := fmt.Errorf("provision: apply: unsupported action kind %q (resource %q)",
				action.Kind, action.Name)
			report.Errors = append(report.Errors, ResourceError{
				Kind: action.Kind, Name: action.Name, Error: err.Error(),
			})
			appendSkipped(&report, plan.Actions[i+1:], SkipReasonPriorError)
			return report, err
		}
		if done, ferr := foldActionResult(&report, plan.Actions, i, action, executed, err); done {
			return report, ferr
		}
	}

	return report, nil
}

// applyPlanSeams bundles the production seam adapters and resolved config the
// applyPlan dispatch needs, so dispatch can route one action without a long
// parameter list.
type applyPlanSeams struct {
	js         jetstream.JetStream
	reader     streamReader
	updater    kvUpdater
	streamMgr  streamManager
	kvRec      kvRecreator
	cfg        Config
	cpSpecs    map[string]controlPlaneSpec
	streamCfgs map[string]StreamCfg
}

// dispatch routes one PlannedAction to its apply helper. The known return is
// false for an action kind Plan never emits — the caller turns that into a
// fail-fast unsupported-kind error. For a known kind it returns the helper's
// (ExecutedAction, error) verbatim for foldActionResult.
func (s applyPlanSeams) dispatch(ctx context.Context, action PlannedAction) (ExecutedAction, bool, error) {
	switch action.Kind {
	case ActionCreateKV:
		executed, err := applyCreateKVAction(ctx, s.js, action)
		return executed, true, err
	case ActionUpdateKV:
		executed, err := applyUpdateKVAction(ctx, s.reader, s.updater, s.cfg, s.cpSpecs, action)
		return executed, true, err
	case ActionStampMarker:
		executed, err := applyStampMarkerAction(ctx, s.reader, s.updater, s.cfg, s.cpSpecs, action)
		return executed, true, err
	case ActionCreateStream:
		executed, err := applyCreateStreamAction(ctx, s.streamMgr, action)
		return executed, true, err
	case ActionUpdateStream:
		executed, err := applyUpdateStreamAction(ctx, s.streamMgr, s.streamCfgs, s.cfg.Instance, action)
		return executed, true, err
	case ActionStampStreamMarker:
		executed, err := applyStampStreamMarkerAction(ctx, s.streamMgr, s.cfg.Instance, action)
		return executed, true, err
	case ActionRecreateKV:
		executed, err := applyRecreateKVAction(ctx, s.kvRec, action)
		return executed, true, err
	case ActionRecreateStream:
		executed, err := applyRecreateStreamAction(ctx, s.streamMgr, action)
		return executed, true, err
	default:
		return ExecutedAction{}, false, nil
	}
}

// applyCreateKVAction executes one ActionCreateKV. It type-asserts the
// Resource to a jetstream.KeyValueConfig value and calls js.CreateKeyValue.
//
// Return contract mirrors the stream apply helpers:
//   - nil error → the returned ExecutedAction is recorded as-is. Raced is true
//     when the bucket already existed (a Plan→Apply race), treated as success.
//   - context.Canceled / context.DeadlineExceeded → mid-mutation cancellation.
//   - any other error → fail-fast resource error.
func applyCreateKVAction(
	ctx context.Context,
	js jetstream.JetStream,
	action PlannedAction,
) (ExecutedAction, error) {
	kvCfg, ok := action.Resource.(jetstream.KeyValueConfig)
	if !ok {
		// Defensive guard: Plan should never emit this.
		return ExecutedAction{}, fmt.Errorf(
			"create-kv action %q has wrong Resource type %T", action.Name, action.Resource)
	}

	_, err := js.CreateKeyValue(ctx, kvCfg)
	switch {
	case err == nil:
		return ExecutedAction{Kind: action.Kind, Name: action.Name}, nil
	case errors.Is(err, jetstream.ErrBucketExists),
		errors.Is(err, jetstream.ErrStreamNameAlreadyInUse):
		// Plan→Apply race: treat as success.
		return ExecutedAction{Kind: action.Kind, Name: action.Name, Raced: true}, nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return ExecutedAction{}, err
	default:
		return ExecutedAction{}, err
	}
}

// foldActionResult folds the (executed, err) result of one re-read/write apply
// helper into report, applying the uniform cancellation / fail-fast / success
// contract every action case in applyPlan shares:
//
//   - nil err → executed is appended to report.Executed; done is false so the
//     loop continues to the next action.
//   - context.Canceled / context.DeadlineExceeded → report.Aborted is set, the
//     current and remaining actions go to Skipped with context-cancelled, and
//     done is true with err returned verbatim (CLI exit 4).
//   - any other err → the failing action goes to Errors once, the remaining
//     actions go to Skipped with prior-error, and done is true with a wrapped
//     fail-fast error (CLI exit 1).
//
// actions is the full plan action slice and i is the index of the current
// action, so the helper can slice the cancelled / skipped tails consistently.
func foldActionResult(
	report *Report,
	actions []PlannedAction,
	i int,
	action PlannedAction,
	executed ExecutedAction,
	err error,
) (bool, error) {
	switch {
	case err == nil:
		report.Executed = append(report.Executed, executed)
		return false, nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		report.Aborted = true
		appendSkipped(report, actions[i:], SkipReasonContextCancelled)
		return true, err
	default:
		// Non-cancellation failure: fail-fast.
		report.Errors = append(report.Errors, ResourceError{
			Kind: action.Kind, Name: action.Name, Error: err.Error(),
		})
		appendSkipped(report, actions[i+1:], SkipReasonPriorError)
		return true, fmt.Errorf("provision: apply %s %q: %w", action.Kind, action.Name, err)
	}
}

// newReport returns a Report with the inherited envelope fields set and
// non-nil (empty) Executed / Skipped / Errors slices, so every Apply-path
// return honors the parti.io/provision/v1 output schema — "empty Report"
// never means a zero-value struct.
func newReport() Report {
	return Report{
		APIVersion: APIVersionProvisionV1,
		Kind:       KindReport,
		Executed:   []ExecutedAction{},
		Skipped:    []SkippedAction{},
		Errors:     []ResourceError{},
	}
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
