package provision

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
)

// ErrRecreateInterrupted is the sentinel an applyRecreate*Action helper wraps
// when a recreate fails AFTER the delete step succeeded — the live resource is
// gone but the desired resource was not (re)created. It is a resource-level
// fail-fast error, NOT a cancellation: the helper deliberately wraps this
// sentinel even when the underlying cause is a context cancellation, so
// foldActionResult records a ResourceError (CLI exit 1) instead of routing the
// outcome through its pure-cancellation branch.
//
// Recovery from an ErrRecreateInterrupted is to fix any persistent cause of the
// create failure (permissions, quota, an invalid desired config) and re-run
// apply: a fresh Plan / PlanConsumers sees the resource missing and emits an
// ordinary create-* action.
var ErrRecreateInterrupted = errors.New("recreate interrupted after delete")

// kvRecreator is the apply-side seam for the recreate-kv action. The production
// implementation (jsKVRecreator) wraps a live jetstream.JetStream; tests inject
// a fake to drive the re-read / no-op / delete / create steps without a NATS
// server. create-kv has no unified seam (it calls js.CreateKeyValue directly),
// and update-kv uses streamReader / kvUpdater — recreate-kv needs all three
// operations on one seam so its step ordering is unit-testable.
type kvRecreator interface {
	// StreamInfo re-reads the live config of the named bucket's KV_-prefixed
	// stream. A missing bucket surfaces as jetstream.ErrStreamNotFound.
	StreamInfo(ctx context.Context, bucket string) (*jetstream.StreamInfo, error)
	// DeleteKeyValue deletes the KV bucket and every entry it holds.
	DeleteKeyValue(ctx context.Context, bucket string) error
	// CreateKeyValue creates a KV bucket at the given config.
	CreateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) error
}

// jsKVRecreator adapts a jetstream.JetStream to kvRecreator. The StreamInfo
// re-read uses the KV_-prefix convention, mirroring jsStreamReader.StreamInfo.
type jsKVRecreator struct {
	js jetstream.JetStream
}

func (r jsKVRecreator) StreamInfo(ctx context.Context, bucket string) (*jetstream.StreamInfo, error) {
	stream, err := r.js.Stream(ctx, kvStreamPrefix+bucket)
	if err != nil {
		return nil, err
	}

	return stream.Info(ctx)
}

func (r jsKVRecreator) DeleteKeyValue(ctx context.Context, bucket string) error {
	return r.js.DeleteKeyValue(ctx, bucket)
}

func (r jsKVRecreator) CreateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) error {
	_, err := r.js.CreateKeyValue(ctx, cfg)

	return err
}

// recreateInterruptedAfterDelete builds the step-5 fail-fast error: the live
// resource was deleted but the (re)create did not complete. It wraps
// ErrRecreateInterrupted with %w but includes the underlying cause with %v —
// NOT %w — deliberately: a %w-wrapped context.Canceled / DeadlineExceeded would
// make errors.Is(err, context.Canceled) true and misroute the outcome through
// foldActionResult's pure-cancellation branch, which records no ResourceError.
func recreateInterruptedAfterDelete(kind, name string, cause error) error {
	return fmt.Errorf(
		"%w: %s %q was deleted but the recreate did not complete (cause: %v); "+
			"fix any persistent cause and re-run apply",
		ErrRecreateInterrupted, kind, name, cause)
}

// applyRecreateKVAction executes one ActionRecreateKV. It performs the
// five-step re-read → re-classify → delete → create sequence internally and
// returns one ExecutedAction or one fail-fast error.
//
// Return contract:
//   - nil error → the returned ExecutedAction is recorded as-is. Raced is true
//     when the re-read live state no longer carries the immutable drift the
//     plan recorded (the stale-plan no-op guard — NO delete happens).
//   - context.Canceled / context.DeadlineExceeded BEFORE the delete → the
//     caller treats it as a mid-mutation cancellation (clean abort).
//   - any other error BEFORE the delete → ordinary fail-fast resource error.
//   - any failure AFTER the delete succeeds (a create error, a cancellation) →
//     a non-cancellation error wrapping ErrRecreateInterrupted; foldActionResult
//     records a ResourceError, Aborted stays false (CLI exit 1).
func applyRecreateKVAction(
	ctx context.Context,
	seam kvRecreator,
	action PlannedAction,
) (ExecutedAction, error) {
	res, ok := action.Resource.(*RecreateKVResource)
	if !ok {
		// Defensive guard: Plan should never emit this.
		return ExecutedAction{}, fmt.Errorf(
			"recreate-kv action %q has wrong Resource type %T", action.Name, action.Resource)
	}

	// Step 1: re-read live state.
	info, gone, err := reReadForRecreate(
		func() (*jetstream.StreamInfo, error) { return seam.StreamInfo(ctx, action.Name) },
		[]error{jetstream.ErrStreamNotFound, jetstream.ErrBucketNotFound},
		func(e error) error { return fmt.Errorf("re-read %s%s: %w", kvStreamPrefix, action.Name, e) },
	)
	if err != nil {
		return ExecutedAction{}, err
	}

	// Steps 2-3: when the bucket is still present, re-classify and delete. A
	// bucket already gone falls straight through to the create-only step 4.
	if !gone {
		// Step 2: re-classify / no-op short-circuit. If the re-read live state no
		// longer carries the immutable divergence the plan recorded, a concurrent
		// operator already repaired it (or the live config now matches): record a
		// raced success and do NOT delete.
		live := streamConfigToKVConfig(info.Config)
		if !kvImmutableDriftPresent(live, res.After) {
			return ExecutedAction{Kind: action.Kind, Name: action.Name, Raced: true}, nil
		}

		// Step 3: delete the live bucket — the point of no return.
		if err := seam.DeleteKeyValue(ctx, action.Name); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return ExecutedAction{}, err
			}

			return ExecutedAction{}, fmt.Errorf("delete %s%s: %w", kvStreamPrefix, action.Name, err)
		}
	}

	// Step 4 + 5: create the bucket at the desired After config.
	if err := seam.CreateKeyValue(ctx, res.After); err != nil {
		if errors.Is(err, jetstream.ErrBucketExists) ||
			errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
			// The bucket name is filled: a concurrent creator either won an
			// ordinary Plan->Apply create race (gone branch) or filled the
			// just-deleted name (post-delete branch). Either way the desired
			// resource exists, so the recreate succeeded — mirrors
			// applyCreateKVAction's Plan->Apply race handling.
			return ExecutedAction{Kind: action.Kind, Name: action.Name, Raced: true}, nil
		}
		if gone {
			return recreateCreateOnlyError(ActionRecreateKV, action.Name, err)
		}

		return ExecutedAction{}, recreateInterruptedAfterDelete(ActionRecreateKV, action.Name, err)
	}

	return ExecutedAction{Kind: action.Kind, Name: action.Name}, nil
}

// applyRecreateStreamAction executes one ActionRecreateStream, following the
// same five-step sequence as applyRecreateKVAction. See that helper's return
// contract — it is identical.
func applyRecreateStreamAction(
	ctx context.Context,
	mgr streamManager,
	action PlannedAction,
) (ExecutedAction, error) {
	res, ok := action.Resource.(*RecreateStreamResource)
	if !ok {
		// Defensive guard: Plan should never emit this.
		return ExecutedAction{}, fmt.Errorf(
			"recreate-stream action %q has wrong Resource type %T", action.Name, action.Resource)
	}

	// Step 1: re-read live state.
	info, gone, err := reReadForRecreate(
		func() (*jetstream.StreamInfo, error) { return mgr.StreamInfo(ctx, action.Name) },
		[]error{jetstream.ErrStreamNotFound},
		func(e error) error { return fmt.Errorf("re-read %s: %w", action.Name, e) },
	)
	if err != nil {
		return ExecutedAction{}, err
	}

	// Steps 2-3: when the stream is still present, re-classify and delete.
	if !gone {
		// Step 2: re-classify / no-op short-circuit.
		if !streamImmutableDriftPresent(info.Config, res.After) {
			return ExecutedAction{Kind: action.Kind, Name: action.Name, Raced: true}, nil
		}

		// Step 3: delete the live stream — the point of no return.
		if err := mgr.DeleteStream(ctx, action.Name); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return ExecutedAction{}, err
			}

			return ExecutedAction{}, fmt.Errorf("delete %s: %w", action.Name, err)
		}
	}

	// Step 4 + 5: create the stream at the desired After config.
	if err := mgr.CreateStream(ctx, res.After); err != nil {
		if errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
			// The stream name is filled: an ordinary Plan->Apply create race
			// (gone branch) or a concurrent creator filling the just-deleted
			// name (post-delete branch). The desired resource exists either
			// way — mirrors applyCreateStreamAction's race handling.
			return ExecutedAction{Kind: action.Kind, Name: action.Name, Raced: true}, nil
		}
		if gone {
			return recreateCreateOnlyError(ActionRecreateStream, action.Name, err)
		}

		return ExecutedAction{}, recreateInterruptedAfterDelete(ActionRecreateStream, action.Name, err)
	}

	return ExecutedAction{Kind: action.Kind, Name: action.Name}, nil
}

// applyRecreateConsumerAction executes one ActionRecreateConsumer, following the
// same five-step sequence. The re-classify (step 2) re-uses
// checkConsumerIdentityFields — the exact identity check the consumer planner
// used to emit the recreate-consumer in the first place.
func applyRecreateConsumerAction(
	ctx context.Context,
	mgr consumerManager,
	action PlannedAction,
) (ExecutedAction, error) {
	res, ok := action.Resource.(*RecreateConsumerResource)
	if !ok {
		// Defensive guard: PlanConsumers should never emit this.
		return ExecutedAction{}, fmt.Errorf(
			"recreate-consumer action %q has wrong Resource type %T", action.Name, action.Resource)
	}

	stream := res.After.StreamName
	durable := res.After.Durable

	// Step 1: re-read live state.
	info, gone, err := reReadForRecreate(
		func() (*jetstream.ConsumerInfo, error) { return mgr.ConsumerInfo(ctx, stream, durable) },
		[]error{jetstream.ErrConsumerNotFound},
		func(e error) error {
			return fmt.Errorf("re-read consumer %q on stream %q: %w", durable, stream, e)
		},
	)
	if err != nil {
		return ExecutedAction{}, err
	}

	// Steps 2-3: when the consumer is still present, re-classify and delete.
	if !gone {
		// Step 2: re-classify / no-op short-circuit. checkConsumerIdentityFields
		// returns no offending field when the live consumer no longer carries the
		// immutable divergence the plan recorded.
		if len(checkConsumerIdentityFields(info.Config, res.After)) == 0 {
			return ExecutedAction{Kind: action.Kind, Name: action.Name, Raced: true}, nil
		}

		// Step 3: delete the live consumer — the point of no return.
		if err := mgr.DeleteConsumer(ctx, stream, durable); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return ExecutedAction{}, err
			}

			return ExecutedAction{}, fmt.Errorf(
				"delete consumer %q on stream %q: %w", durable, stream, err)
		}
	}

	// Step 4 + 5: create the consumer at the desired After config.
	if err := mgr.CreateConsumer(ctx, stream, res.After.Config); err != nil {
		if errors.Is(err, jetstream.ErrConsumerExists) ||
			errors.Is(err, jetstream.ErrConsumerNameAlreadyInUse) {
			// The durable name is filled: an ordinary Plan->Apply create race
			// (gone branch) or a concurrent creator filling the just-deleted
			// name (post-delete branch). A durable name encodes the subject
			// hash, so the colliding consumer can carry a wrong immutable
			// config — verify its identity before blessing it as raced,
			// exactly as applyCreateConsumerAction does. A verified match is a
			// raced success; a rejection (identity mismatch, the consumer
			// vanished, a re-read failure) falls through to the shared
			// post-create error mapping below.
			executed, verr := verifyRacedConsumer(ctx, mgr, action, res.After)
			if verr == nil {
				return executed, nil
			}
			err = verr
		}
		if gone {
			return recreateCreateOnlyError(ActionRecreateConsumer, action.Name, err)
		}

		return ExecutedAction{}, recreateInterruptedAfterDelete(ActionRecreateConsumer, action.Name, err)
	}

	return ExecutedAction{Kind: action.Kind, Name: action.Name}, nil
}

// recreateCreateOnlyError maps a create failure on the resource-already-gone
// branch (step 1 found the resource absent, so step 3's delete never ran) onto
// the helper return contract. With no delete performed it is an ordinary
// pre-mutation failure: a cancellation surfaces verbatim for the caller's
// cancellation handling; any other error fails fast wrapped — there is no
// destroyed resource, so it must NOT wrap ErrRecreateInterrupted.
func recreateCreateOnlyError(kind, name string, err error) (ExecutedAction, error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ExecutedAction{}, err
	}

	return ExecutedAction{}, fmt.Errorf("create %s %q: %w", kind, name, err)
}

// kvImmutableDriftPresent reports whether the re-read live KV config still
// diverges from the desired After config on an immutable field — Storage,
// History, or the parti.io/component marker. These are exactly the immutable
// fields classifyControlPlaneDrift / classifyPartitionSourceDrift route to a
// drift-immutable finding. When this returns false the recreate is a no-op: a
// concurrent operator already repaired the bucket (or the live config matches).
func kvImmutableDriftPresent(live, after jetstream.KeyValueConfig) bool {
	if live.Storage != after.Storage {
		return true
	}
	if live.History != after.History {
		return true
	}

	return live.Metadata[MarkerComponentKey] != after.Metadata[MarkerComponentKey]
}

// streamImmutableDriftPresent reports whether the re-read live stream config
// still diverges from the desired After config on an immutable field —
// Storage, Retention, or the parti.io/component marker. These are exactly the
// immutable fields routeStreamFieldDrift routes to a drift-immutable finding.
func streamImmutableDriftPresent(live, after jetstream.StreamConfig) bool {
	if live.Storage != after.Storage {
		return true
	}
	if live.Retention != after.Retention {
		return true
	}

	return live.Metadata[MarkerComponentKey] != after.Metadata[MarkerComponentKey]
}
