package provision

import (
	"errors"
	"fmt"
)

// ErrInvalidConfig is the sentinel returned by Validate for any static
// validation failure. Callers can use errors.Is(err, ErrInvalidConfig) to
// distinguish static-validation errors from I/O failures.
var ErrInvalidConfig = errors.New("provision: invalid config")

// Validate performs pure static validation of cfg. It does not mutate the
// caller's Config — defaulting is performed on an internal copy.
//
// Validation rules (see docs/plans/provision-sdk-cli for the authoritative
// spec):
//
//   - APIVersion must equal APIVersionV1.
//   - Policy defaults to PolicyWarn when empty; "safe-update" and "force"
//     are rejected as not-supported-in-v1.
//   - ControlPlane bucket name fields default to runtime defaults; any
//     bucket name still empty after defaulting is rejected.
//   - ControlPlane.WorkerIDTTL, .ElectionTimeout, .HeartbeatTTL must be > 0.
//   - ControlPlane.AssignmentTTL may be 0 (runtime default: no expiration).
//   - When ControlPlane.EnableTwoPhaseHandoff is true, HandoffTTL must be > 0.
//
// PartitionSource static validation is active (W3): Bucket, Key, Storage,
// Replicas, MaxValueSize, and TTL are all validated. DynamicConsumers deeper
// validation lands in W4.
func Validate(cfg Config) error {
	resolved, err := normalize(cfg)
	if err != nil {
		return err
	}
	return validateResolved(resolved)
}

// ApplyDefaults returns a defaulted copy of cfg without running any
// validation. Callers who want to inspect the post-default values (e.g.
// CLI dry-run renderers) use this directly. The returned Config is a deep
// copy; the input is not mutated.
//
// ApplyDefaults does NOT validate; an APIVersion mismatch returns an error
// because defaulting an unknown schema is unsafe.
func ApplyDefaults(cfg Config) (Config, error) {
	return normalize(cfg)
}

// normalize deep-copies cfg, applies runtime defaults, and rejects the
// `apiVersion` mismatch up front. Returns a new Config; the input is never
// mutated.
func normalize(cfg Config) (Config, error) {
	if cfg.APIVersion != APIVersionV1 {
		return Config{}, fmt.Errorf("%w: apiVersion %q is not supported (expected %q)",
			ErrInvalidConfig, cfg.APIVersion, APIVersionV1)
	}

	out := cfg // shallow copy of value fields

	// Default Policy.
	if out.Policy == "" {
		out.Policy = PolicyWarn
	}

	// Deep-copy ControlPlane before defaulting so the caller's pointer
	// target is not mutated.
	if cfg.ControlPlane != nil {
		cp := *cfg.ControlPlane
		if cp.StableIDBucket == "" {
			cp.StableIDBucket = defaultStableIDBucket
		}
		if cp.ElectionBucket == "" {
			cp.ElectionBucket = defaultElectionBucket
		}
		if cp.HeartbeatBucket == "" {
			cp.HeartbeatBucket = defaultHeartbeatBucket
		}
		if cp.AssignmentBucket == "" {
			cp.AssignmentBucket = defaultAssignmentBucket
		}
		if cp.HandoffBucket == "" {
			cp.HandoffBucket = defaultHandoffBucket
		}
		out.ControlPlane = &cp
	}

	// Deep-copy PartitionSource and apply defaults.
	if cfg.PartitionSource != nil {
		ps := *cfg.PartitionSource
		if ps.Storage == "" {
			ps.Storage = "file"
		}
		if ps.History == 0 {
			ps.History = 1
		}
		out.PartitionSource = &ps
	}

	// DynamicConsumers is a slice of value structs; copy the backing array
	// so caller-owned entries are not mutated by later phases.
	if len(cfg.DynamicConsumers) > 0 {
		dc := make([]DynamicConsumerCfg, len(cfg.DynamicConsumers))
		copy(dc, cfg.DynamicConsumers)
		out.DynamicConsumers = dc
	}

	return out, nil
}

func validateResolved(cfg Config) error {
	// Policy: reject reserved-future values by string match; anything else
	// not PolicyWarn is also rejected.
	switch string(cfg.Policy) {
	case string(PolicyWarn):
		// ok
	case reservedPolicySafeUpdate:
		return fmt.Errorf("%w: policy %q is not supported in v1 (lands in Phase 2)",
			ErrInvalidConfig, reservedPolicySafeUpdate)
	case reservedPolicyForce:
		return fmt.Errorf("%w: policy %q is not supported in v1 (lands in Phase 6)",
			ErrInvalidConfig, reservedPolicyForce)
	default:
		return fmt.Errorf("%w: policy %q is not recognized (v1 supports only %q)",
			ErrInvalidConfig, cfg.Policy, PolicyWarn)
	}

	if cfg.ControlPlane != nil {
		if err := validateControlPlane(*cfg.ControlPlane); err != nil {
			return err
		}
	}

	if cfg.PartitionSource != nil {
		if err := validatePartitionSource(*cfg.PartitionSource); err != nil {
			return err
		}
	}

	// DynamicConsumers: deeper validation lands in W4.
	return nil
}

// validatePartitionSource validates a PartitionSourceConfig that has already
// had Storage and History defaults applied by normalize.
func validatePartitionSource(ps PartitionSourceConfig) error {
	if ps.Bucket == "" {
		return fmt.Errorf("%w: partitionSource.bucket must not be empty", ErrInvalidConfig)
	}
	if ps.Key == "" {
		return fmt.Errorf("%w: partitionSource.key must not be empty", ErrInvalidConfig)
	}
	switch ps.Storage {
	case "file", "memory":
		// valid; normalize already set the default
	default:
		return fmt.Errorf("%w: partitionSource.storage %q is not valid (must be %q or %q)",
			ErrInvalidConfig, ps.Storage, "file", "memory")
	}
	// ps.History is uint8, so negative values are impossible by type; no check needed.
	// ps.Replicas is int; 0 means "server default" (accepted).
	if ps.Replicas < 0 {
		return fmt.Errorf("%w: partitionSource.replicas must be >= 0", ErrInvalidConfig)
	}
	if ps.MaxValueSize < 0 {
		return fmt.Errorf("%w: partitionSource.maxValueSize must be >= 0", ErrInvalidConfig)
	}
	if ps.TTL < 0 {
		return fmt.Errorf("%w: partitionSource.ttl must be >= 0", ErrInvalidConfig)
	}

	return nil
}

func validateControlPlane(cp ControlPlaneConfig) error {
	if cp.StableIDBucket == "" {
		return fmt.Errorf("%w: controlPlane.stableIdBucket must not be empty", ErrInvalidConfig)
	}
	if cp.ElectionBucket == "" {
		return fmt.Errorf("%w: controlPlane.electionBucket must not be empty", ErrInvalidConfig)
	}
	if cp.HeartbeatBucket == "" {
		return fmt.Errorf("%w: controlPlane.heartbeatBucket must not be empty", ErrInvalidConfig)
	}
	if cp.AssignmentBucket == "" {
		return fmt.Errorf("%w: controlPlane.assignmentBucket must not be empty", ErrInvalidConfig)
	}
	if cp.EnableTwoPhaseHandoff && cp.HandoffBucket == "" {
		// Unreachable from YAML because HandoffBucket defaults; here as a
		// defense against API callers explicitly zeroing the field.
		return fmt.Errorf("%w: controlPlane.handoffBucket must not be empty when enableTwoPhaseHandoff is true",
			ErrInvalidConfig)
	}

	if cp.WorkerIDTTL <= 0 {
		return fmt.Errorf("%w: controlPlane.workerIdTtl must be > 0", ErrInvalidConfig)
	}
	if cp.ElectionTimeout <= 0 {
		return fmt.Errorf("%w: controlPlane.electionTimeout must be > 0", ErrInvalidConfig)
	}
	if cp.HeartbeatTTL <= 0 {
		return fmt.Errorf("%w: controlPlane.heartbeatTtl must be > 0", ErrInvalidConfig)
	}
	// AssignmentTTL == 0 is valid (matches runtime default: no expiration).
	if cp.AssignmentTTL < 0 {
		return fmt.Errorf("%w: controlPlane.assignmentTtl must be >= 0", ErrInvalidConfig)
	}
	if cp.EnableTwoPhaseHandoff && cp.HandoffTTL <= 0 {
		return fmt.Errorf("%w: controlPlane.handoffTtl must be > 0 when enableTwoPhaseHandoff is true",
			ErrInvalidConfig)
	}

	return nil
}
