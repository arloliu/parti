package provision

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
)

// streamManager is the apply-side seam for the stream actions. The production
// implementation (jsStreamManager) wraps a live jetstream.JetStream; tests
// inject a fake to drive the re-read / no-op / stale-before / write steps
// without a NATS server.
type streamManager interface {
	// StreamInfo re-reads the live config of the named stream. A missing
	// stream surfaces as jetstream.ErrStreamNotFound.
	StreamInfo(ctx context.Context, name string) (*jetstream.StreamInfo, error)
	// CreateStream creates a stream. A concurrent creator surfaces as
	// jetstream.ErrStreamNameAlreadyInUse.
	CreateStream(ctx context.Context, cfg jetstream.StreamConfig) error
	// UpdateStream updates an existing stream. A vanished stream surfaces as
	// jetstream.ErrStreamNotFound.
	UpdateStream(ctx context.Context, cfg jetstream.StreamConfig) error
}

// jsStreamManager adapts a jetstream.JetStream to streamManager. Application
// streams use the raw NATS stream name — there is no "KV_" prefix.
type jsStreamManager struct {
	js jetstream.JetStream
}

func (m jsStreamManager) StreamInfo(ctx context.Context, name string) (*jetstream.StreamInfo, error) {
	stream, err := m.js.Stream(ctx, name)
	if err != nil {
		return nil, err
	}

	return stream.Info(ctx)
}

func (m jsStreamManager) CreateStream(ctx context.Context, cfg jetstream.StreamConfig) error {
	_, err := m.js.CreateStream(ctx, cfg)

	return err
}

func (m jsStreamManager) UpdateStream(ctx context.Context, cfg jetstream.StreamConfig) error {
	_, err := m.js.UpdateStream(ctx, cfg)

	return err
}

// streamMissingBeforeUpdate is the fail-fast error for a stream that existed
// at plan time but is gone when Apply re-reads or writes it on the
// update-stream path. Both the re-read miss and the write-time
// ErrStreamNotFound surface the same operator-facing message class.
func streamMissingBeforeUpdate(name string) error {
	return fmt.Errorf("stream-missing-before-update: %s no longer exists", name)
}

// streamMissingBeforeStamp is the fail-fast error for a stream that existed at
// plan time but is gone when Apply re-reads or writes it on the
// stamp-stream-marker path.
func streamMissingBeforeStamp(name string) error {
	return fmt.Errorf("stream-missing-before-stamp: %s no longer exists", name)
}

// applyCreateStreamAction executes one ActionCreateStream. It type-asserts the
// Resource to a jetstream.StreamConfig value (mirroring create-kv) and calls
// CreateStream.
//
// Return contract (mirrors applyUpdateKVAction):
//   - nil error → the returned ExecutedAction is recorded as-is. Raced is true
//     when the stream already existed at the moment CreateStream ran (a
//     Plan→Apply race), treated as success.
//   - context.Canceled / context.DeadlineExceeded (or an error that wraps one)
//     → the caller treats it as a mid-mutation cancellation.
//   - any other error → fail-fast resource error.
func applyCreateStreamAction(
	ctx context.Context,
	mgr streamManager,
	action PlannedAction,
) (ExecutedAction, error) {
	cfg, ok := action.Resource.(jetstream.StreamConfig)
	if !ok {
		// Defensive guard: Plan should never emit this.
		return ExecutedAction{}, fmt.Errorf(
			"create-stream action %q has wrong Resource type %T", action.Name, action.Resource)
	}

	err := mgr.CreateStream(ctx, cfg)
	switch {
	case err == nil:
		return ExecutedAction{Kind: action.Kind, Name: action.Name}, nil
	case errors.Is(err, jetstream.ErrStreamNameAlreadyInUse):
		// Plan→Apply race: treat as success.
		return ExecutedAction{Kind: action.Kind, Name: action.Name, Raced: true}, nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return ExecutedAction{}, err
	default:
		// Returned raw; foldActionResult wraps it as "provision: apply
		// create-stream %q". Mirrors applyCreateKVAction — a single-phase
		// create needs no inner phase prefix.
		return ExecutedAction{}, err
	}
}

// applyUpdateStreamAction executes one ActionUpdateStream. It re-reads live
// state, rebuilds the write target from the re-read snapshot,
// short-circuits when the stream already matches the target, verifies the
// re-read still matches the plan-time Before, and otherwise calls
// UpdateStream.
//
// The no-op check precedes the stale-before check deliberately, exactly as
// applyUpdateKVAction documents: a stream already equal to the desired
// target is a converged success regardless of which operator's plan got it
// there.
//
// Return contract mirrors applyUpdateKVAction:
//   - nil error → the returned ExecutedAction is recorded as-is. Raced is true
//     when the stream already matched the target on re-read.
//   - context.Canceled / context.DeadlineExceeded (or an error that wraps
//     one) → the caller treats it as a mid-mutation cancellation.
//   - any other error → fail-fast resource error; the caller records
//     err.Error() verbatim into ResourceError.Error.
func applyUpdateStreamAction(
	ctx context.Context,
	mgr streamManager,
	streamCfgs map[string]StreamCfg,
	instance string,
	action PlannedAction,
) (ExecutedAction, error) {
	res, ok := action.Resource.(*UpdateStreamResource)
	if !ok {
		// Defensive guard: Plan should never emit this.
		return ExecutedAction{}, fmt.Errorf(
			"update-stream action %q has wrong Resource type %T", action.Name, action.Resource)
	}

	// Step 1: re-read live state.
	info, err := mgr.StreamInfo(ctx, action.Name)
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		return ExecutedAction{}, streamMissingBeforeUpdate(action.Name)
	}
	if err != nil {
		// Cancellation surfaces here too; the caller distinguishes it via
		// errors.Is. Other errors fail-fast wrapped.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ExecutedAction{}, err
		}

		return ExecutedAction{}, fmt.Errorf("re-read %s: %w", action.Name, err)
	}
	live := info.Config

	// Step 2: rebuild the write target from the just-re-read snapshot so every
	// preserved-from-live field carries the current live value. A stream Plan
	// emitted an update-stream for but Config no longer describes is a
	// defensive wiring error, not a runtime race.
	streamCfg, ok := streamCfgs[action.Name]
	if !ok {
		return ExecutedAction{}, fmt.Errorf(
			"update-stream target %q is not described by the resolved config", action.Name)
	}
	target := buildStreamUpdateTarget(streamCfg, instance, live)

	// Step 3: no-op short-circuit. The stream already matches the desired
	// target — whether this plan or a concurrent operator converged it, the
	// intent is realized. Recorded as a raced success.
	if streamConfigsEqual(live, target) {
		return ExecutedAction{Kind: action.Kind, Name: action.Name, Raced: true}, nil
	}

	// Step 4: stale-before check. The stream is not yet converged; if live no
	// longer matches the plan-time Before either, a concurrent writer moved it
	// to a third state and the plan is stale.
	if !streamConfigsEqual(live, res.Before) {
		return ExecutedAction{}, errors.New(
			"stale-before: live state changed since plan; re-run plan")
	}

	// Step 5: write.
	if err := mgr.UpdateStream(ctx, target); err != nil {
		switch {
		case errors.Is(err, jetstream.ErrStreamNotFound):
			// The stream vanished between the re-read and this write.
			return ExecutedAction{}, streamMissingBeforeUpdate(action.Name)
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return ExecutedAction{}, err
		default:
			return ExecutedAction{}, fmt.Errorf("update %s: %w", action.Name, err)
		}
	}

	return ExecutedAction{Kind: action.Kind, Name: action.Name}, nil
}

// applyStampStreamMarkerAction executes one ActionStampStreamMarker. It
// re-reads live state, recomputes the merged metadata against the re-read
// snapshot, short-circuits when the merge is already a no-op, and otherwise
// writes the re-read snapshot back with only Metadata changed.
//
// Unlike applyUpdateStreamAction, stamp-stream-marker has no stale-before
// check: it carries no plan-time expectation of the stream's non-metadata
// fields, exactly as applyStampMarkerAction documents for KV buckets.
//
// Return contract mirrors applyUpdateStreamAction:
//   - nil error → the returned ExecutedAction is recorded as-is. Raced is true
//     when the stream already carries an equivalent marker on re-read.
//   - context.Canceled / context.DeadlineExceeded (or an error that wraps
//     one) → the caller treats it as a mid-mutation cancellation.
//   - any other error → fail-fast resource error.
func applyStampStreamMarkerAction(
	ctx context.Context,
	mgr streamManager,
	instance string,
	action PlannedAction,
) (ExecutedAction, error) {
	if _, ok := action.Resource.(*StreamStampMarkerResource); !ok {
		// Defensive guard: Plan should never emit this.
		return ExecutedAction{}, fmt.Errorf(
			"stamp-stream-marker action %q has wrong Resource type %T", action.Name, action.Resource)
	}

	// Step 1: re-read live state.
	info, err := mgr.StreamInfo(ctx, action.Name)
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		return ExecutedAction{}, streamMissingBeforeStamp(action.Name)
	}
	if err != nil {
		// Cancellation surfaces here too; the caller distinguishes it via
		// errors.Is. Other errors fail-fast wrapped.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ExecutedAction{}, err
		}

		return ExecutedAction{}, fmt.Errorf("re-read %s: %w", action.Name, err)
	}
	live := info.Config

	// Step 2: recompute the merge against the re-read metadata. A non-Parti
	// key added between plan and apply flows into the written map. The stream
	// component is always ComponentStream — no per-resource derivation.
	merged := mergeMarkerMetadata(live.Metadata, ComponentStream, instance)

	// Step 3: no-op short-circuit. The target is the re-read snapshot with
	// only Metadata replaced. If it already equals live, the stream was
	// concurrently adopted or already carries an equivalent marker — recorded
	// as a raced success with no write.
	target := cloneStreamConfig(live)
	target.Metadata = merged
	if streamConfigsEqual(live, target) {
		return ExecutedAction{Kind: action.Kind, Name: action.Name, Raced: true}, nil
	}

	// Step 4: write.
	if err := mgr.UpdateStream(ctx, target); err != nil {
		switch {
		case errors.Is(err, jetstream.ErrStreamNotFound):
			// The stream vanished between the re-read and this write.
			return ExecutedAction{}, streamMissingBeforeStamp(action.Name)
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return ExecutedAction{}, err
		default:
			return ExecutedAction{}, fmt.Errorf("stamp %s: %w", action.Name, err)
		}
	}

	return ExecutedAction{Kind: action.Kind, Name: action.Name}, nil
}
