package provision

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/nats-io/nats.go/jetstream"
)

// consumerManager is the apply-side seam for the create-consumer action. The
// production implementation (jsConsumerManager) wraps a live
// jetstream.JetStream; tests inject a fake to drive the create / re-read steps
// without a NATS server.
type consumerManager interface {
	// CreateConsumer creates a consumer on a stream. A consumer that already
	// exists with an identical config succeeds; one that exists with a
	// differing config surfaces as jetstream.ErrConsumerExists.
	CreateConsumer(ctx context.Context, stream string, cfg jetstream.ConsumerConfig) error
	// ConsumerInfo re-reads a consumer's live config by durable name. A
	// missing consumer surfaces as jetstream.ErrConsumerNotFound. Used to
	// verify identity on a create-race.
	ConsumerInfo(ctx context.Context, stream, durable string) (*jetstream.ConsumerInfo, error)
}

// jsConsumerManager adapts a jetstream.JetStream to consumerManager. Dynamic
// consumers are looked up by their deterministic durable name on the
// application stream.
type jsConsumerManager struct {
	js jetstream.JetStream
}

func (m jsConsumerManager) CreateConsumer(ctx context.Context, stream string, cfg jetstream.ConsumerConfig) error {
	_, err := m.js.CreateConsumer(ctx, stream, cfg)

	return err
}

func (m jsConsumerManager) ConsumerInfo(ctx context.Context, stream, durable string) (*jetstream.ConsumerInfo, error) {
	consumer, err := m.js.Consumer(ctx, stream, durable)
	if err != nil {
		return nil, err
	}

	return consumer.Info(ctx)
}

// consumerStreamMissingBeforeCreate is the fail-fast error for an application
// stream that existed at plan time but is gone when ApplyConsumers calls
// CreateConsumer — the analogue of streamMissingBeforeUpdate on the
// create-consumer path.
func consumerStreamMissingBeforeCreate(stream string) error {
	return fmt.Errorf(
		"consumer-stream-missing: stream %s no longer exists; run `partictl apply` to provision it first", stream)
}

// ApplyConsumers executes every create-consumer action in plan against js. It
// precreates the per-partition durable consumers PlanConsumers found missing.
//
// Consumer creation is idempotent on identity: a consumer that already exists
// with the same identity / immutable fields is the desired outcome, so there
// is no --prune-style gate and no stale-before dance — the design is closer to
// the create-kv / create-stream paths than to update-stream. The one re-read
// it performs is verification-only, on the ErrConsumerExists create-race path
// (see applyCreateConsumerAction).
//
// ApplyConsumers does NOT re-run Plan; it executes a caller-supplied plan,
// exactly as ApplyPartitions does. The CLI runs PlanConsumers then
// ApplyConsumers.
//
// Failure-return contract (every returned Report carries the envelope fields):
//
//   - Resource-level failure — a wrong Resource type, an identity mismatch on
//     a create-race, a concurrent delete, a missing stream, or any other
//     server-rejected create: the failing action goes to Errors once, the
//     remaining actions go to Skipped with prior-error, Aborted stays false,
//     and a non-nil error is returned → CLI exit 1.
//   - Context cancellation: Aborted true, no ResourceError, the current and
//     remaining actions go to Skipped with context-cancelled, ctx.Err() is
//     returned → CLI exit 4.
//
// The no-action plan returns the envelope Report with nothing executed.
func ApplyConsumers(ctx context.Context, js jetstream.JetStream, plan PlanResult) (Report, error) {
	report := newReport()

	// Production seam adapter built once before the loop; tests inject a fake
	// consumerManager directly into applyCreateConsumerAction.
	mgr := jsConsumerManager{js: js}

	for i, action := range plan.Actions {
		// Pre-mutation / between-iteration cancellation check.
		if err := ctx.Err(); err != nil {
			report.Aborted = true
			appendSkipped(&report, plan.Actions[i:], SkipReasonContextCancelled)

			return report, err
		}

		switch action.Kind {
		case ActionCreateConsumer:
			executed, err := applyCreateConsumerAction(ctx, mgr, action)
			if done, ferr := foldActionResult(&report, plan.Actions, i, action, executed, err); done {
				return report, ferr
			}

		default:
			// PlanConsumers emits only create-consumer. Defensive: surface an
			// unknown kind as a fail-fast resource error so operators see what
			// was rejected.
			err := fmt.Errorf("provision: apply consumers: unsupported action kind %q (resource %q)",
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

// applyCreateConsumerAction executes one ActionCreateConsumer. It type-asserts
// the Resource to a PlannedConsumer value (mirroring create-kv / create-stream)
// and calls CreateConsumer.
//
// Unlike applyCreateStreamAction — which trusts ErrStreamNameAlreadyInUse
// outright — the ErrConsumerExists branch re-reads the colliding consumer to
// verify its identity (see verifyRacedConsumer): a durable name encodes the
// subject hash, so a name collision must not be blessed without checking the
// immutable fields.
//
// Return contract (mirrors applyCreateStreamAction):
//   - nil error → the returned ExecutedAction is recorded as-is. Raced is true
//     when the consumer already existed and a re-read confirmed its identity /
//     immutable fields match the desired config (a Plan→Apply race), treated
//     as success.
//   - context.Canceled / context.DeadlineExceeded (or an error that wraps one)
//     → the caller treats it as a mid-mutation cancellation.
//   - any other error → fail-fast resource error: a wrong Resource type, an
//     identity divergence on a create-race, a concurrent delete, a missing
//     stream, or a server-rejected create.
func applyCreateConsumerAction(
	ctx context.Context,
	mgr consumerManager,
	action PlannedAction,
) (ExecutedAction, error) {
	res, ok := action.Resource.(PlannedConsumer)
	if !ok {
		// Defensive guard: PlanConsumers should never emit this.
		return ExecutedAction{}, fmt.Errorf(
			"create-consumer action %q has wrong Resource type %T", action.Name, action.Resource)
	}

	err := mgr.CreateConsumer(ctx, res.StreamName, res.Config)
	switch {
	case err == nil:
		return ExecutedAction{Kind: action.Kind, Name: action.Name}, nil

	case errors.Is(err, jetstream.ErrConsumerExists),
		errors.Is(err, jetstream.ErrConsumerNameAlreadyInUse):
		// A consumer with this durable name already exists. ErrConsumerExists
		// specifically means "exists with a differing config" — the difference
		// might be in an identity field (a hand-created durable-name collision
		// carrying a wrong FilterSubject, or a runtime that created the
		// consumer with a non-AckExplicitPolicy policy), not only in a
		// runtime-owned tunable. Verify identity before blessing it as a raced
		// success.
		return verifyRacedConsumer(ctx, mgr, action, res)

	case errors.Is(err, jetstream.ErrStreamNotFound):
		// The application stream was deleted between plan and apply.
		return ExecutedAction{}, consumerStreamMissingBeforeCreate(res.StreamName)

	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return ExecutedAction{}, err

	default:
		// Returned raw; foldActionResult wraps it as "provision: apply
		// create-consumer %q". Mirrors applyCreateStreamAction.
		return ExecutedAction{}, err
	}
}

// verifyRacedConsumer handles the ErrConsumerExists / ErrConsumerNameAlreadyInUse
// create-race: it re-reads the colliding consumer and compares its identity /
// immutable fields against the desired config.
//
//   - All identity / immutable fields match → the only differences are
//     runtime-owned mutable tunables → raced success (Raced: true).
//   - Any identity / immutable field differs → fail-fast ResourceError naming
//     the offending field(s) — an identity divergence the operator resolves
//     via the Phase 6 repair path.
//   - The re-read itself returns ErrConsumerNotFound (the consumer was deleted
//     between the failed create and the re-read) → fail-fast error.
//   - The re-read is cancelled → cancellation surfaced to the caller.
func verifyRacedConsumer(
	ctx context.Context,
	mgr consumerManager,
	action PlannedAction,
	res PlannedConsumer,
) (ExecutedAction, error) {
	info, err := mgr.ConsumerInfo(ctx, res.StreamName, res.Durable)
	switch {
	case errors.Is(err, jetstream.ErrConsumerNotFound):
		// The consumer was deleted between the failed create and this re-read:
		// a genuine concurrent-delete race. The operator re-runs `consumers
		// plan`.
		return ExecutedAction{}, fmt.Errorf(
			"consumer-deleted-during-apply: consumer %q on stream %q vanished between create and re-read; "+
				"re-run `partictl consumers plan`", res.Durable, res.StreamName)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return ExecutedAction{}, err
	case err != nil:
		return ExecutedAction{}, fmt.Errorf(
			"re-read consumer %q on stream %q: %w", res.Durable, res.StreamName, err)
	}

	// The colliding consumer may diverge on an immutable field, not just a
	// runtime-owned tunable; checkConsumerIdentityFields names any that do.
	if offending := checkConsumerIdentityFields(info.Config, res); len(offending) > 0 {
		return ExecutedAction{}, fmt.Errorf(
			"consumer-identity-mismatch: consumer %q on stream %q diverges on immutable field(s) %s; "+
				"repair requires delete/recreate", res.Durable, res.StreamName, offendingFieldList(offending))
	}

	// Identity / immutable fields all match — only runtime-owned mutable
	// tunables may differ. Raced success.
	return ExecutedAction{Kind: action.Kind, Name: action.Name, Raced: true}, nil
}

// offendingFieldList renders the sorted field names of an offending-field
// Detail map (as produced by checkConsumerIdentityFields) into a deterministic
// "[a, b, c]" string for the fail-fast error message.
func offendingFieldList(detail map[string]any) string {
	names := make([]string, 0, len(detail))
	for name := range detail {
		names = append(names, name)
	}
	slices.Sort(names)

	return "[" + strings.Join(names, ", ") + "]"
}
