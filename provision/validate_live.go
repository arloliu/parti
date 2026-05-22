package provision

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// ErrLiveValidation is the sentinel returned by ValidateLive for any live
// validation failure that is not a reachability or cancellation error.
// Callers can use errors.Is(err, ErrLiveValidation) to distinguish
// live-validation errors from static-validation (ErrInvalidConfig) and from
// reachability/cancellation surfaces.
//
// CLI exit-code mapping (per the master plan's Exit code precedence):
//   - reachability failure (ErrJetStreamNotEnabled / ErrJetStreamNotEnabledForAccount
//     during step 1.B), or context cancellation → exit code 4. These do NOT
//     wrap ErrLiveValidation; callers detect them via errors.Is against the
//     underlying error.
//   - ErrLiveValidation (permission-denied or generic) → exit code 3.
var ErrLiveValidation = errors.New("provision: live validation failed")

// ValidateLive performs the live (NATS-reachable) preflight for cfg without
// mutating any resource. It executes a deterministic probe sequence (see
// docs/plans/provision-sdk-cli/02-w2-apply-validatelive-spec.md §1):
//
//  1. Reachability probe via js.AccountInfo.
//  2. Per-bucket stream-info probe for every named bucket in cfg.
//     ErrStreamNotFound is NOT an error (Apply will create the bucket).
//  3. Partition-source key probe (only when cfg.PartitionSource != nil) with
//     a single retry on permission-denied to handle a live AllowDirect flip.
//
// ValidateLive normalizes cfg internally (applying the same defaults as Plan
// and Validate) so that callers are not required to pre-normalize. This makes
// ValidateLive safe to call directly from partictl validate -live or any
// standalone caller, not just from Apply.
//
// On success, ValidateLive returns a Report with empty action slices (the
// Report is informational; Apply's Report is the mutation-outcome surface).
// On failure, ValidateLive returns a Report containing a ResourceError entry
// for the failing probe plus the underlying error.
//
// Cancellation: ctx cancellation returns a zero-value Report plus ctx.Err().
func ValidateLive(ctx context.Context, js jetstream.JetStream, cfg Config) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}

	// Normalize cfg (apply bucket-name defaults, etc.) so probes target the
	// correct stream names even when the caller omits optional fields.
	// This mirrors Plan's public-entrypoint pattern (provision/plan.go:36-42)
	// and makes ValidateLive safe to call directly without pre-normalization.
	resolved, err := normalize(cfg)
	if err != nil {
		return Report{}, err
	}
	cfg = resolved

	// Step 1.B: reachability probe.
	if _, err := js.AccountInfo(ctx); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Report{}, ctxErr
		}
		// All reachability errors are surfaced as-is (no ErrLiveValidation
		// wrap); the CLI maps them to exit code 4 via errors.Is against
		// the underlying error. We still return a Report containing a
		// ResourceError so JSON callers can render the failure.
		return liveErrorReport("reachability", "$JS.API.INFO", err), fmt.Errorf("provision: validatelive: reachability: %w", err)
	}

	// Step 1.C: per-bucket info probes (control-plane and partition-source).
	if cfg.ControlPlane != nil {
		specs := buildControlPlaneSpecs(*cfg.ControlPlane)
		for _, spec := range specs {
			if err := ctx.Err(); err != nil {
				return Report{}, err
			}
			if err := probeBucketInfo(ctx, js, spec.bucket); err != nil {
				return liveErrorReport(KindControlPlaneKV, spec.bucket, err),
					fmt.Errorf("provision: validatelive: stream info %q: %w", spec.bucket, err)
			}
		}
	}

	// Step 1.D: partition-source key probe — only when configured.
	if cfg.PartitionSource != nil {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		if err := probePartitionSourceKey(ctx, js, *cfg.PartitionSource); err != nil {
			return liveErrorReport(KindPartitionSource, cfg.PartitionSource.Bucket, err),
				fmt.Errorf("provision: validatelive: partition-source key probe %q/%q: %w",
					cfg.PartitionSource.Bucket, cfg.PartitionSource.Key, err)
		}
	}

	// Step 1.E: per-stream info probes — only when one or more application
	// streams are declared. Uses the raw NATS stream name (no "KV_" prefix).
	if len(cfg.Streams) > 0 {
		for _, streamCfg := range cfg.Streams {
			if err := ctx.Err(); err != nil {
				return Report{}, err
			}
			if err := probeStreamInfo(ctx, js, streamCfg.Name); err != nil {
				return liveErrorReport(KindApplicationStream, streamCfg.Name, err),
					fmt.Errorf("provision: validatelive: stream info %q: %w", streamCfg.Name, err)
			}
		}
	}

	// Dynamic-consumer live checks. Only runs when the caller declared one or
	// more dynamic-consumer alignment targets. A failure here is a
	// config-mismatch (WorkQueuePolicy + incompatible recovery strategy),
	// so it's surfaced under ErrLiveValidation (CLI exit code 3).
	if len(cfg.DynamicConsumers) > 0 {
		if err := ValidateLiveDynamicConsumers(ctx, js, cfg.DynamicConsumers); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return Report{}, err
			}
			// Use the first cfg's stream name for the Report; the error
			// message already carries the offending stream.
			name := cfg.DynamicConsumers[0].StreamName
			return liveErrorReport(KindDynamicConsumer, name, err),
				fmt.Errorf("%w: %w", ErrLiveValidation, err)
		}
	}

	return Report{
		APIVersion: APIVersionProvisionV1,
		Kind:       KindReport,
	}, nil
}

// probeBucketInfo probes live stream info for the named KV bucket.
// Returns nil on success or on ErrStreamNotFound (bucket is creatable);
// otherwise classifies the error via classifyLiveError.
func probeBucketInfo(ctx context.Context, js jetstream.JetStream, bucket string) error {
	stream, err := js.Stream(ctx, kvStreamPrefix+bucket)
	if err != nil {
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			return nil
		}
		return classifyLiveError(ctx, err)
	}
	if _, err := stream.Info(ctx); err != nil {
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			return nil
		}
		return classifyLiveError(ctx, err)
	}

	return nil
}

// probeStreamInfo probes live stream info for the named application stream.
// Returns nil on success or on ErrStreamNotFound (the stream is creatable by
// Apply); otherwise classifies the error via classifyLiveError. Unlike
// probeBucketInfo, the raw stream name is used — application streams have no
// "KV_" prefix.
func probeStreamInfo(ctx context.Context, js jetstream.JetStream, name string) error {
	stream, err := js.Stream(ctx, name)
	if err != nil {
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			return nil
		}
		return classifyLiveError(ctx, err)
	}
	if _, err := stream.Info(ctx); err != nil {
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			return nil
		}
		return classifyLiveError(ctx, err)
	}

	return nil
}

// probePartitionSourceKey implements the §1.D algorithm: read STREAM.INFO,
// probe via stream.GetLastMsgForSubject (which re-evaluates AllowDirect from
// the cached stream info), and retry exactly once after refreshing the
// stream info if the first probe returned a permission-denied surface error.
func probePartitionSourceKey(ctx context.Context, js jetstream.JetStream, ps PartitionSourceConfig) error {
	stream, err := js.Stream(ctx, kvStreamPrefix+ps.Bucket)
	if err != nil {
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			// Bucket does not exist live; the key probe is vacuously OK.
			// Apply will create the bucket.
			return nil
		}
		return classifyLiveError(ctx, err)
	}
	// Refresh stream info so the AllowDirect branch is current.
	if _, err := stream.Info(ctx); err != nil {
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			return nil
		}
		return classifyLiveError(ctx, err)
	}

	subject := "$KV." + ps.Bucket + "." + ps.Key
	probeMsg := func(ctx context.Context) error {
		_, err := stream.GetLastMsgForSubject(ctx, subject)
		return err
	}
	refreshInfo := func(ctx context.Context) error {
		_, err2 := stream.Info(ctx)
		return err2
	}

	return probeWithAllowDirectRetry(ctx, probeMsg, refreshInfo)
}

// probeWithAllowDirectRetry implements the §1.D AllowDirect retry algorithm
// with injected probe and refresh functions. This seam allows the retry
// logic to be tested independently of NATS.
//
// probe(ctx) must return nil / ErrMsgNotFound for success or a permission /
// generic error. refresh(ctx) refreshes the stream's cached info so the
// next probe re-evaluates AllowDirect. If the first probe returns a
// permission-denied surface and the refresh succeeds, the probe is retried
// exactly once; a second permission-denied result is returned classified.
func probeWithAllowDirectRetry(
	ctx context.Context,
	probe func(ctx context.Context) error,
	refresh func(ctx context.Context) error,
) error {
	err := probe(ctx)
	if err == nil || errors.Is(err, jetstream.ErrMsgNotFound) {
		return nil
	}
	if !isPermissionDeniedSurface(ctx, err) {
		return classifyLiveError(ctx, err)
	}

	// Single retry: refresh stream info (re-evaluates AllowDirect) and
	// re-probe.
	if err2 := refresh(ctx); err2 != nil {
		if errors.Is(err2, jetstream.ErrStreamNotFound) {
			return nil
		}
		return classifyLiveError(ctx, err2)
	}
	err = probe(ctx)
	if err == nil || errors.Is(err, jetstream.ErrMsgNotFound) {
		return nil
	}

	return classifyLiveError(ctx, err)
}

// isPermissionDeniedSurface returns true when err looks like a permission
// failure that the §1.D retry protocol should respond to. The ctx-live guard
// avoids misclassifying a cancellation as a permission failure.
func isPermissionDeniedSurface(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	return errors.Is(err, jetstream.ErrJetStreamNotEnabled) ||
		errors.Is(err, nats.ErrNoResponders) ||
		errors.Is(err, nats.ErrPermissionViolation)
}

// classifyLiveError wraps err into a ValidateLive-style error per the §1.E
// table. Cancellation always wins over permission classification.
func classifyLiveError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, jetstream.ErrJetStreamNotEnabled) ||
		errors.Is(err, jetstream.ErrJetStreamNotEnabledForAccount) ||
		errors.Is(err, nats.ErrNoResponders) ||
		errors.Is(err, nats.ErrPermissionViolation) {
		return fmt.Errorf("%w: permission denied: %w", ErrLiveValidation, err)
	}

	return fmt.Errorf("%w: %w", ErrLiveValidation, err)
}

// liveErrorReport builds the Report returned alongside a ValidateLive error.
// The kind/name pair identifies the resource (or "reachability"/"$JS.API.INFO"
// for step 1.B failures).
func liveErrorReport(kind, name string, err error) Report {
	return Report{
		APIVersion: APIVersionProvisionV1,
		Kind:       KindReport,
		Errors: []ResourceError{
			{Kind: kind, Name: name, Error: err.Error()},
		},
	}
}
