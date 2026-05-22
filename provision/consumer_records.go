package provision

import (
	"context"
	"errors"
	"fmt"

	"github.com/arloliu/parti/v2/internal/dynamicbuild"
	"github.com/nats-io/nats.go/jetstream"
)

// ErrConsumerStreamMissing is returned by PlanConsumers when the application
// stream that a precreation-opted DynamicConsumerCfg targets does not exist
// live. It wraps ErrLiveValidation, so errors.Is(err, ErrLiveValidation) holds
// and the CLI maps it to exit code 3. The consumers subcommand never creates
// the stream; the operator runs `partictl apply` to provision it first.
var ErrConsumerStreamMissing = fmt.Errorf("%w: application stream does not exist", ErrLiveValidation)

// ValidateConsumerSet performs the Phase-5 static validation for dynamic
// consumer precreation. It is called by PlanConsumers before any NATS I/O.
//
// Rules:
//   - At least one DynamicConsumerCfg has a non-empty PartitionsRef (else
//     "no dynamic consumers opted into precreation").
//   - For every target with a non-empty PartitionsRef: cfg.PartitionSource is
//     non-nil, cfg.PartitionSource.Partitions is non-empty (validated via
//     ValidatePartitionSet), and PartitionsRef == cfg.PartitionSource.Bucket.
//   - Each opted-in target's StreamName, ConsumerPrefix, and SubjectTemplate
//     pass the same checks PlanDynamicConsumers enforces: non-empty,
//     prefix charset (dynamicbuild.IsAllowedConsumerRune), template contains
//     {{.PartitionID}} and is valid (dynamicbuild.ValidateSubjectTemplate).
//
// All errors wrap ErrInvalidConfig (CLI exit 3).
func ValidateConsumerSet(cfg Config) error {
	hasOpted := false
	for _, dc := range cfg.DynamicConsumers {
		if dc.PartitionsRef == "" {
			continue
		}
		hasOpted = true

		if cfg.PartitionSource == nil {
			return fmt.Errorf(
				"%w: dynamicConsumer target %q has partitionsRef but partitionSource is not declared",
				ErrInvalidConfig, dc.ConsumerPrefix,
			)
		}
		if len(cfg.PartitionSource.Partitions) == 0 {
			return fmt.Errorf(
				"%w: dynamicConsumer target %q has partitionsRef but partitionSource.partitions is empty",
				ErrInvalidConfig, dc.ConsumerPrefix,
			)
		}
		if err := ValidatePartitionSet(cfg.PartitionSource.Partitions); err != nil {
			return err
		}
		if dc.PartitionsRef != cfg.PartitionSource.Bucket {
			return fmt.Errorf(
				"%w: dynamicConsumer target %q partitionsRef %q does not match partitionSource.bucket %q",
				ErrInvalidConfig, dc.ConsumerPrefix, dc.PartitionsRef, cfg.PartitionSource.Bucket,
			)
		}

		if dc.StreamName == "" {
			return fmt.Errorf("%w: dynamicConsumer target with partitionsRef: streamName is required", ErrInvalidConfig)
		}
		if dc.ConsumerPrefix == "" {
			return fmt.Errorf("%w: dynamicConsumer target with partitionsRef: consumerPrefix is required", ErrInvalidConfig)
		}
		for _, r := range dc.ConsumerPrefix {
			if !dynamicbuild.IsAllowedConsumerRune(r) {
				return fmt.Errorf(
					"%w: dynamicConsumer target %q consumerPrefix contains invalid characters (allowed: a-z, A-Z, 0-9, -, _)",
					ErrInvalidConfig, dc.ConsumerPrefix,
				)
			}
		}
		if dc.SubjectTemplate == "" {
			return fmt.Errorf("%w: dynamicConsumer target %q: subjectTemplate is required", ErrInvalidConfig, dc.ConsumerPrefix)
		}
		if err := dynamicbuild.ValidateSubjectTemplate(dc.SubjectTemplate, true); err != nil {
			return fmt.Errorf("%w: dynamicConsumer target %q subjectTemplate: %w", ErrInvalidConfig, dc.ConsumerPrefix, err)
		}
	}

	if !hasOpted {
		return fmt.Errorf("%w: no dynamic consumers opted into precreation (set partitionsRef on at least one dynamicConsumers entry)", ErrInvalidConfig)
	}

	return nil
}

// PlanConsumers computes the per-partition consumer diff between the declared
// DynamicConsumerCfg entries that have opted into precreation (non-empty
// PartitionsRef) and the live NATS state. It is read-only — it never mutates
// NATS.
//
// Algorithm:
//
//   - Run Validate(cfg) then ValidateConsumerSet(cfg). Both complete before any
//     NATS I/O, so a malformed config fails here (CLI exit 3, ErrInvalidConfig).
//   - For each DynamicConsumerCfg with a non-empty PartitionsRef, in config
//     order: build the expected consumer set via PlanDynamicConsumers, then
//     look up each expected consumer live via js.Consumer.
//   - Missing consumer → ActionCreateConsumer + drift-mutable dynamic-consumer
//     finding.
//   - Present, identity fields match → informational dynamic-consumer finding.
//   - Present, any identity/immutable field differs → drift-immutable
//     dynamic-consumer finding whose Detail names the offending field(s).
//   - Stream not found → ErrConsumerStreamMissing (wraps ErrLiveValidation,
//     CLI exit 3).
//
// Actions and drift are sorted by (Kind, Name) before return. The returned
// PlanResult always carries non-nil Actions and Drift slices.
func PlanConsumers(ctx context.Context, js jetstream.JetStream, cfg Config) (PlanResult, error) {
	// Static validation boundary: runs before context check and any NATS I/O.
	if err := Validate(cfg); err != nil {
		return PlanResult{}, err
	}
	if err := ValidateConsumerSet(cfg); err != nil {
		return PlanResult{}, err
	}

	if err := ctx.Err(); err != nil {
		return PlanResult{}, err
	}

	out := PlanResult{
		APIVersion: APIVersionProvisionV1,
		Kind:       KindPlan,
		Actions:    []PlannedAction{},
		Drift:      []DriftFinding{},
	}

	// ValidateConsumerSet has verified cfg.PartitionSource is non-nil and
	// its Partitions slice is non-empty.
	partitions := cfg.PartitionSource.Partitions

	for _, dc := range cfg.DynamicConsumers {
		if dc.PartitionsRef == "" {
			continue
		}

		expected, err := PlanDynamicConsumers(dc.StreamName, dc.ConsumerPrefix, dc.SubjectTemplate, partitions)
		if err != nil {
			// ValidateConsumerSet already checked these; this is a safety net.
			return PlanResult{}, fmt.Errorf("provision: build expected consumers for %q: %w", dc.StreamName, err)
		}

		recreateOK := policyRecreatesImmutable(cfg.Policy) && dc.AllowDeleteRecreate
		for _, exp := range expected {
			if err := ctx.Err(); err != nil {
				return PlanResult{}, err
			}
			if err := classifyConsumer(ctx, js, exp, recreateOK, &out); err != nil {
				return PlanResult{}, err
			}
		}
	}

	sortActions(out.Actions)
	sortDrift(out.Drift)

	return out, nil
}

// classifyConsumer looks up one expected PlannedConsumer live and appends the
// appropriate action and/or drift finding to out. recreateOK signals that the
// caller has already verified policyRecreatesImmutable(cfg.Policy) &&
// dc.AllowDeleteRecreate; when true and the consumer has drift-immutable drift, a
// recreate-consumer action is emitted instead of no action.
func classifyConsumer(ctx context.Context, js jetstream.JetStream, expected PlannedConsumer, recreateOK bool, out *PlanResult) error {
	consumer, err := js.Consumer(ctx, expected.StreamName, expected.Durable)
	if errors.Is(err, jetstream.ErrConsumerNotFound) {
		out.Actions = append(out.Actions, PlannedAction{
			Kind:     ActionCreateConsumer,
			Name:     expected.Durable,
			Resource: expected,
		})
		out.Drift = append(out.Drift, DriftFinding{
			Severity: SeverityDriftMutable,
			Kind:     KindDynamicConsumer,
			Name:     expected.Durable,
			Detail:   map[string]any{"reason": "consumer does not exist"},
		})

		return nil
	}
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		return fmt.Errorf("%w: stream %q not found; run `partictl apply` to provision it first",
			ErrConsumerStreamMissing, expected.StreamName)
	}
	if err != nil {
		return fmt.Errorf("provision: lookup consumer %q on stream %q: %w",
			expected.Durable, expected.StreamName, err)
	}

	info, err := consumer.Info(ctx)
	if err != nil {
		return fmt.Errorf("provision: info consumer %q on stream %q: %w",
			expected.Durable, expected.StreamName, err)
	}

	live := info.Config
	offending := checkConsumerIdentityFields(live, expected)
	if len(offending) == 0 {
		out.Drift = append(out.Drift, DriftFinding{
			Severity: SeverityInformational,
			Kind:     KindDynamicConsumer,
			Name:     expected.Durable,
		})
		return nil
	}

	out.Drift = append(out.Drift, DriftFinding{
		Severity: SeverityDriftImmutable,
		Kind:     KindDynamicConsumer,
		Name:     expected.Durable,
		Detail:   offending,
	})

	// Recreate gate: force + AllowDeleteRecreate + drift-immutable. Ownership for
	// dynamic consumers is config-derivation (no marker stamp), so no marked check
	// is needed here — PlanConsumers only iterates config-declared targets.
	if recreateOK {
		// Deep-clone both Before and After so the Plan output is immutable
		// regardless of later Apply or nats.go mutation (the same contract
		// the Recreate{KV,Stream}Resource Before/After clones honor).
		after := expected
		after.Config = cloneConsumerConfig(expected.Config)
		out.Actions = append(out.Actions, PlannedAction{
			Kind: ActionRecreateConsumer,
			Name: expected.Durable,
			Resource: &RecreateConsumerResource{
				Before:         cloneConsumerConfig(live),
				After:          after,
				ImmutableDrift: offending,
			},
		})
	}

	return nil
}

// checkConsumerIdentityFields compares the identity and immutable fields of a
// live consumer against the expected PlannedConsumer. It returns a Detail map
// naming each offending field, or nil when all fields match.
//
// Fields checked (Name and Durable are omitted — the lookup key proves them).
// A live consumer that deviates on any of these is reported drift-immutable;
// the repair is delete/recreate:
//   - FilterSubject — an identity field. The durable name encodes the subject
//     hash, so a name carrying a different FilterSubject is a foreign
//     (hand-created) consumer squatting the name; the repair is to delete the
//     squatter and let provision recreate. (NATS itself does permit a
//     FilterSubject update — provision simply does not update consumers.)
//   - AckPolicy, DeliverPolicy, MaxWaiting, MemoryStorage — NATS-immutable:
//     CreateOrUpdateConsumer rejects a change to any of them on an existing
//     consumer (server/consumer.go update-rejection list; MemoryStorage per
//     the storage-type rule).
func checkConsumerIdentityFields(live jetstream.ConsumerConfig, expected PlannedConsumer) map[string]any {
	var detail map[string]any

	addField := func(name string, want, got any) {
		if detail == nil {
			detail = map[string]any{}
		}
		detail[name] = map[string]any{"want": want, "got": got}
	}

	if live.FilterSubject != expected.Subject {
		addField("filterSubject", expected.Subject, live.FilterSubject)
	}
	if live.AckPolicy != jetstream.AckExplicitPolicy {
		addField("ackPolicy", jetstream.AckExplicitPolicy.String(), live.AckPolicy.String())
	}
	if live.DeliverPolicy != jetstream.DeliverAllPolicy {
		addField("deliverPolicy", "DeliverAllPolicy", live.DeliverPolicy.String())
	}
	if live.MaxWaiting != expected.Config.MaxWaiting {
		addField("maxWaiting", expected.Config.MaxWaiting, live.MaxWaiting)
	}
	if live.MemoryStorage {
		addField("memoryStorage", false, true)
	}

	return detail
}
