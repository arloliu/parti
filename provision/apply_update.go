package provision

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
)

// streamReader is the read side of the update-kv Apply path. The
// production implementation calls js.Stream(...).Info(ctx); tests
// inject a fake to deterministically interleave the re-read and write
// steps.
type streamReader interface {
	StreamInfo(ctx context.Context, bucket string) (*jetstream.StreamInfo, error)
}

// kvUpdater is the write side of the update-kv Apply path. The
// production implementation calls js.UpdateKeyValue(ctx, cfg).
type kvUpdater interface {
	UpdateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) error
}

// jsStreamReader adapts a jetstream.JetStream to streamReader.
type jsStreamReader struct {
	js jetstream.JetStream
}

func (r jsStreamReader) StreamInfo(ctx context.Context, bucket string) (*jetstream.StreamInfo, error) {
	stream, err := r.js.Stream(ctx, kvStreamPrefix+bucket)
	if err != nil {
		return nil, err
	}

	return stream.Info(ctx)
}

// jsKVUpdater adapts a jetstream.JetStream to kvUpdater.
type jsKVUpdater struct {
	js jetstream.JetStream
}

func (u jsKVUpdater) UpdateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) error {
	_, err := u.js.UpdateKeyValue(ctx, cfg)

	return err
}

// applyUpdateKVAction executes one ActionUpdateKV. It re-reads live
// state, rebuilds the write target from the re-read snapshot,
// short-circuits when the bucket already matches the target, verifies
// the re-read still matches the plan-time Before, and otherwise calls
// UpdateKeyValue.
//
// The no-op check precedes the stale-before check deliberately: a
// bucket that already equals the desired target is a converged success
// regardless of whether this operator's plan or a concurrent one got it
// there. Checking stale-before first would misreport that convergence
// as a stale plan.
//
// Return contract:
//   - nil error → the returned ExecutedAction is recorded as-is. Raced
//     is true when the bucket already matched the target on re-read.
//   - context.Canceled / context.DeadlineExceeded (or an error that
//     wraps one) → the caller treats it as a mid-mutation cancellation.
//   - any other error → fail-fast resource error; the caller records
//     err.Error() verbatim into ResourceError.Error, so the message is
//     the operator-facing classification (e.g. "stale-before: ...").
func applyUpdateKVAction(
	ctx context.Context,
	reader streamReader,
	updater kvUpdater,
	cfg Config,
	cpSpecs map[string]controlPlaneSpec,
	action PlannedAction,
) (ExecutedAction, error) {
	res, ok := action.Resource.(*UpdateKVResource)
	if !ok {
		// Defensive guard: Plan should never emit this.
		return ExecutedAction{}, fmt.Errorf(
			"update-kv action %q has wrong Resource type %T", action.Name, action.Resource)
	}

	// Step 1: re-read live state.
	info, err := reader.StreamInfo(ctx, action.Name)
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		return ExecutedAction{}, bucketMissingBeforeUpdate(action.Name)
	}
	if err != nil {
		// Cancellation surfaces here too; the caller distinguishes it
		// via errors.Is. Other errors fail-fast wrapped.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ExecutedAction{}, err
		}

		return ExecutedAction{}, fmt.Errorf("re-read %s%s: %w", kvStreamPrefix, action.Name, err)
	}
	live := streamConfigToKVConfig(info.Config)

	// Step 2: rebuild the write target from the just-re-read snapshot so
	// every preserved-from-live field carries the current live value.
	target, err := buildUpdateKVTargetForBucket(cfg, cpSpecs, action.Name, live)
	if err != nil {
		return ExecutedAction{}, err
	}

	// Step 3: no-op short-circuit. The bucket already matches the desired
	// target — whether this plan or a concurrent operator converged it,
	// the intent is realized. Recorded as a raced success.
	if kvConfigsEqual(live, target) {
		return ExecutedAction{Kind: action.Kind, Name: action.Name, Raced: true}, nil
	}

	// Step 4: stale-before check. The bucket is not yet converged; if live
	// no longer matches the plan-time Before either, a concurrent writer
	// moved it to a third state and the plan is stale. kvConfigsEqual
	// ignores preserved-from-live fields and normalizes server defaults,
	// so this fires only on a genuine operator-expressible or
	// drift-detection-only change since plan time.
	if !kvConfigsEqual(live, res.Before) {
		return ExecutedAction{}, errors.New(
			"stale-before: live state changed since plan; re-run plan")
	}

	// Step 5: write.
	if err := updater.UpdateKeyValue(ctx, target); err != nil {
		switch {
		case errors.Is(err, jetstream.ErrBucketNotFound):
			// nats.go maps a mid-write stream disappearance onto
			// ErrBucketNotFound; treat it the same as a missing re-read.
			return ExecutedAction{}, bucketMissingBeforeUpdate(action.Name)
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return ExecutedAction{}, err
		default:
			return ExecutedAction{}, fmt.Errorf("update %s%s: %w", kvStreamPrefix, action.Name, err)
		}
	}

	return ExecutedAction{Kind: action.Kind, Name: action.Name}, nil
}

// bucketMissingBeforeUpdate is the fail-fast error for a bucket that
// existed at plan time but is gone when Apply re-reads or writes it.
// Both the re-read miss and the write-time ErrBucketNotFound surface
// the same operator-facing message class.
func bucketMissingBeforeUpdate(bucket string) error {
	return fmt.Errorf("bucket-missing-before-update: %s%s no longer exists", kvStreamPrefix, bucket)
}

// buildUpdateKVTargetForBucket re-derives the desired update-kv target
// for the named bucket from the just-re-read live config. It looks the
// bucket up among the control-plane specs first, then the
// partition-source config. A bucket Plan emitted an update-kv for but
// Config no longer describes is a defensive error, not a race.
func buildUpdateKVTargetForBucket(
	cfg Config,
	cpSpecs map[string]controlPlaneSpec,
	bucket string,
	live jetstream.KeyValueConfig,
) (jetstream.KeyValueConfig, error) {
	before := cloneKVConfig(live)
	if spec, ok := cpSpecs[bucket]; ok {
		return buildControlPlaneUpdateTarget(spec, cfg.Instance, before), nil
	}
	if cfg.PartitionSource != nil && cfg.PartitionSource.Bucket == bucket {
		return buildPartitionSourceUpdateTarget(cfg.PartitionSource, cfg.Instance, before), nil
	}

	return jetstream.KeyValueConfig{}, fmt.Errorf(
		"update-kv target %q is not described by the resolved config", bucket)
}
