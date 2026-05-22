package provision

import (
	"context"
	"errors"

	"github.com/nats-io/nats.go/jetstream"
)

// reReadForUpdate runs the step-1 re-read shared by the update-kv / stamp-marker
// / update-stream / stamp-stream-marker apply helpers.
//
// It calls fetch (the seam's StreamInfo) and normalizes the three outcomes
// those four helpers all handle identically:
//
//   - jetstream.ErrStreamNotFound → the resource that existed at plan time is
//     gone; returns the caller-supplied missing-before sentinel (a hard error).
//   - context.Canceled / context.DeadlineExceeded (or an error wrapping one) →
//     returned verbatim so the caller's cancellation handling routes it.
//   - any other error → wrapped via wrapErr (the phase-specific "re-read ..."
//     message).
//
// On success it returns the live *jetstream.StreamInfo and a nil error. The
// cancellation check precedes the generic wrap, exactly as the inlined blocks
// did, so a cancellation never gets buried under a re-read prefix.
func reReadForUpdate(
	fetch func() (*jetstream.StreamInfo, error),
	missingErr error,
	wrapErr func(error) error,
) (*jetstream.StreamInfo, error) {
	info, err := fetch()
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		return nil, missingErr
	}
	if err != nil {
		// Cancellation surfaces here too; the caller distinguishes it via
		// errors.Is. Other errors fail-fast wrapped.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}

		return nil, wrapErr(err)
	}

	return info, nil
}

// reReadForRecreate runs the step-1 re-read shared by the recreate-kv /
// recreate-stream / recreate-consumer apply helpers.
//
// Unlike reReadForUpdate, the recreate helpers tolerate a concurrently-deleted
// resource: a not-found re-read is not an error but a signal to fall through to
// the create-only path. fetch is the seam's info call; notFoundErrs lists the
// sentinels that mean "resource gone" (recreate-kv accepts both
// ErrStreamNotFound and ErrBucketNotFound).
//
// Outcomes:
//
//   - any error in notFoundErrs → returns (zero T, gone=true, nil): the caller
//     skips re-classify + delete and goes straight to create.
//   - context.Canceled / context.DeadlineExceeded → returned verbatim with
//     gone=false so the caller's pre-delete cancellation handling routes it.
//   - any other error → wrapped via wrapErr (the phase-specific "re-read ..."
//     message), gone=false.
//   - no error → returns (info, gone=false, nil).
//
// The cancellation check precedes the generic wrap, matching the inlined
// blocks.
func reReadForRecreate[T any](
	fetch func() (T, error),
	notFoundErrs []error,
	wrapErr func(error) error,
) (T, bool, error) {
	info, err := fetch()
	gone := false
	for _, nf := range notFoundErrs {
		if errors.Is(err, nf) {
			gone = true

			break
		}
	}
	if err != nil && !gone {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return info, false, err
		}

		return info, false, wrapErr(err)
	}

	return info, gone, nil
}
