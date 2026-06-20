package provision

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/internal/dynamicbuild"
	"github.com/arloliu/parti/v2/types"
)

// PlanDynamicConsumers is a pure builder that produces a deterministic
// PlannedConsumer slice for the given (streamName, consumerPrefix,
// subjectTemplate, partitions) tuple. It performs no I/O.
//
// The builder produces the same per-subject jetstream.ConsumerConfig that
// the runtime consumer.Dynamic would create (modulo runtime-tunable fields
// supplied via Dynamic options; the equality subset is documented in the
// corresponding integration test).
//
// The builder fills the ConsumerConfig from dynamicbuild.DefaultDynamicDefaults
// — the Defaults a default-configured consumer.Dynamic uses. This makes the
// NATS-immutable fields (AckPolicy = AckExplicitPolicy, MaxWaiting = 2,
// MemoryStorage = false) and DeliverPolicy = DeliverAllPolicy match the
// runtime, so a consumer provision precreates from this output can be adopted
// by the runtime's own CreateOrUpdateConsumer. The mutable tunables also carry
// the runtime defaults; the runtime overwrites them on start. The
// runtime-defaults roundtrip test (provision/dynamic_consumers_test.go) pins
// the immutable fields against durable.WorkerConsumerConfig.SetDefaults().
//
// Output ordering is deterministic by subject (matches the runtime
// internal/durable.buildSubjects sort). Two partitions that resolve to the
// same subject deduplicate; the entries are emitted in subject-sorted
// order.
//
// Errors wrap ErrInvalidConfig and are surfaced as static-validation
// failures (CLI exit code 3):
//
//   - streamName == ""        → "%w: streamName is required"
//   - consumerPrefix == ""    → "%w: consumerPrefix is required"
//   - consumerPrefix contains characters outside [a-zA-Z0-9-_]
//   - subjectTemplate == ""   → "%w: subjectTemplate is required"
//   - subjectTemplate missing {{.PartitionID}} or fails to render
//   - len(partitions) == 0    → "%w: at least one partition is required"
//   - partition with no keys  → wrapped "partition has no keys"
func PlanDynamicConsumers(
	streamName, consumerPrefix, subjectTemplate string,
	partitions []types.Partition,
) ([]PlannedConsumer, error) {
	if streamName == "" {
		return nil, fmt.Errorf("%w: streamName is required", ErrInvalidConfig)
	}
	if consumerPrefix == "" {
		return nil, fmt.Errorf("%w: consumerPrefix is required", ErrInvalidConfig)
	}
	for _, r := range consumerPrefix {
		if !dynamicbuild.IsAllowedConsumerRune(r) {
			return nil, fmt.Errorf(
				"%w: consumer prefix %q contains invalid characters (allowed: a-z, A-Z, 0-9, -, _)",
				ErrInvalidConfig, consumerPrefix,
			)
		}
	}
	if subjectTemplate == "" {
		return nil, fmt.Errorf("%w: subjectTemplate is required", ErrInvalidConfig)
	}
	// Wildcards allowed (matches runtime: durable.WorkerConsumerConfig.Validate
	// passes allowWildcard=true to validateSubjectTemplate).
	if err := dynamicbuild.ValidateSubjectTemplate(subjectTemplate, true); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	if len(partitions) == 0 {
		return nil, fmt.Errorf("%w: at least one partition is required", ErrInvalidConfig)
	}

	subjects, err := dynamicbuild.BuildSubjects(subjectTemplate, partitions)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}

	// Template framing for partition-id extraction inside PerSubjectDurableName.
	pre, suf, _ := dynamicbuild.ParseSubjectTemplateParts(subjectTemplate)

	// Use the runtime dynamic-consumer defaults, not zero values: a consumer
	// provision precreates must carry the same NATS-immutable fields
	// (AckPolicy, MaxWaiting, MemoryStorage) the runtime will use, or the
	// runtime's CreateOrUpdateConsumer fails on startup.
	defaults := dynamicbuild.DefaultDynamicDefaults()

	out := make([]PlannedConsumer, 0, len(subjects))
	for _, subject := range subjects {
		durable := dynamicbuild.PerSubjectDurableName(consumerPrefix, subject, pre, suf)
		cfg := dynamicbuild.ConsumerConfig(durable, subject, defaults)
		out = append(out, PlannedConsumer{
			StreamName: streamName,
			Subject:    subject,
			Durable:    durable,
			Config:     cfg,
		})
	}

	return out, nil
}

// ValidateLiveDynamicConsumers performs the WorkQueuePolicy compatibility
// check that runtime consumer.Dynamic.Update would perform on first update,
// for each configured dynamic-consumer alignment target.
//
// ValidateLiveDynamicConsumers always passes consumer.RecoveryDisabled as
// the strategy, mirroring what consumer.Dynamic would produce when no
// WithRecoveryStrategy option is set. Both RecoveryDisabled and
// RecoverFromBeginning unconditionally pass the check (see
// consumer.CheckWorkQueueRecoveryCompat), so alignment never raises a
// false positive against a WorkQueuePolicy stream. A configurable strategy
// is not yet supported for this check.
//
// Errors:
//   - Returns the first error encountered (fail-fast), wrapped with the
//     offending cfg's StreamName for operator clarity.
//   - The underlying error is the verbatim
//     consumer.CheckWorkQueueRecoveryCompat error (wraps
//     consumer.ErrInvalidConfig).
//   - On context cancellation, returns ctx.Err() without further wrapping.
//   - Best-effort connectivity: stream-info fetch failures inside
//     CheckWorkQueueRecoveryCompat are silently ignored, mirroring runtime.
func ValidateLiveDynamicConsumers(
	ctx context.Context,
	js jetstream.JetStream,
	cfgs []DynamicConsumerCfg,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, cfg := range cfgs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := consumer.CheckWorkQueueRecoveryCompat(
			ctx, js, cfg.StreamName, consumer.RecoveryDisabled,
		); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			return fmt.Errorf("dynamic consumer alignment for stream %q: %w", cfg.StreamName, err)
		}
		// The helper treats JS errors as benign and returns nil. If the
		// context was cancelled during the helper call, ctx.Err() is now
		// non-nil even though the helper returned nil. Check explicitly so
		// we never return a clean result after mid-call cancellation.
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	return nil
}
