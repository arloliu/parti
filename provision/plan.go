package provision

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/arloliu/parti/v2/internal/kvbuckets"
	"github.com/nats-io/nats.go/jetstream"
)

// Plan computes the deterministic list of create-kv actions and drift
// findings needed to bring the live NATS environment into agreement with cfg.
//
// Plan is read-only — it never mutates NATS. It is also strict about
// boundaries:
//
//   - Static validation is performed up front (same rules as Validate).
//   - Each desired bucket is looked up by exact NATS name (KV_<bucket>);
//     marker presence is never used to authorize or skip the lookup.
//   - The partition-source key is NOT probed in Plan (that is a ValidateLive
//     concern, landing in W2).
//
// Partition-source create-kv emission and drift classification are implemented
// (W3). Dynamic-consumer alignment lands in W4. The PlanResult struct supports
// those slots already.
//
// Cancellation: ctx cancellation returns a zero-value PlanResult plus
// ctx.Err().
func Plan(ctx context.Context, js jetstream.JetStream, cfg Config) (PlanResult, error) {
	if err := ctx.Err(); err != nil {
		return PlanResult{}, err
	}
	resolved, err := normalize(cfg)
	if err != nil {
		return PlanResult{}, err
	}
	if err := validateResolved(resolved); err != nil {
		return PlanResult{}, err
	}

	out := PlanResult{
		APIVersion: APIVersionProvisionV1,
		Kind:       KindPlan,
		Actions:    []PlannedAction{},
		Drift:      []DriftFinding{},
	}

	if resolved.ControlPlane != nil {
		if err := planControlPlane(ctx, js, resolved, &out); err != nil {
			if ctx.Err() != nil {
				return PlanResult{}, ctx.Err()
			}
			return PlanResult{}, err
		}
	}

	if resolved.PartitionSource != nil {
		if err := planPartitionSource(ctx, js, resolved, &out); err != nil {
			if ctx.Err() != nil {
				return PlanResult{}, ctx.Err()
			}
			return PlanResult{}, err
		}
	}

	// DynamicConsumers Plan output is intentionally empty in W1/W3. The
	// slot exists so W4 inherits a stable shape.

	sortActions(out.Actions)
	sortDrift(out.Drift)

	return out, nil
}

// controlPlaneSpec describes one desired control-plane bucket. The slice is
// built once per Plan call from resolved cfg in a fixed component order so
// Plan output is deterministic before sorting.
type controlPlaneSpec struct {
	component string
	bucket    string
	ttl       time.Duration
	storage   jetstream.StorageType
}

func buildControlPlaneSpecs(cp ControlPlaneConfig) []controlPlaneSpec {
	specs := []controlPlaneSpec{
		{ComponentControlPlaneID, cp.StableIDBucket, cp.WorkerIDTTL, jetstream.FileStorage},
		{ComponentControlPlaneElection, cp.ElectionBucket, cp.ElectionTimeout, jetstream.MemoryStorage},
		{ComponentControlPlaneHeartbeat, cp.HeartbeatBucket, cp.HeartbeatTTL, jetstream.MemoryStorage},
		{ComponentControlPlaneAssignment, cp.AssignmentBucket, cp.AssignmentTTL, jetstream.FileStorage},
	}
	if cp.EnableTwoPhaseHandoff {
		// The handoff bucket is created with no MaxAge (ttl: 0). A bucket-level
		// TTL would age out stable ownership claims — which are written once and
		// never refreshed — and permanently suppress pull-gated consumers.
		// cp.HandoffTTL is the coordinator's advisory sweep TTL, not a bucket TTL.
		specs = append(specs, controlPlaneSpec{
			component: ComponentControlPlaneHandoff,
			bucket:    cp.HandoffBucket,
			ttl:       0,
			storage:   jetstream.FileStorage,
		})
	}

	return specs
}

func planControlPlane(ctx context.Context, js jetstream.JetStream, cfg Config, out *PlanResult) error {
	specs := buildControlPlaneSpecs(*cfg.ControlPlane)
	for _, spec := range specs {
		if err := ctx.Err(); err != nil {
			return err
		}
		streamName := kvStreamPrefix + spec.bucket
		stream, err := js.Stream(ctx, streamName)
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			out.Actions = append(out.Actions, newCreateKVAction(spec, cfg.Instance))
			continue
		}
		if err != nil {
			return fmt.Errorf("provision: lookup %s: %w", streamName, err)
		}

		info, err := stream.Info(ctx)
		if err != nil {
			return fmt.Errorf("provision: info %s: %w", streamName, err)
		}

		findings := classifyControlPlaneDrift(spec, info, cfg.Instance)
		out.Drift = append(out.Drift, findings...)
	}

	return nil
}

// newCreateKVAction builds the create-kv PlannedAction for one control-plane
// bucket. The KeyValueConfig is built by the shared kvbuckets builder and
// then stamped with the Parti ownership marker; nothing else may construct
// the KeyValueConfig (this preserves the byte-equivalence invariant).
func newCreateKVAction(spec controlPlaneSpec, instance string) PlannedAction {
	kv := kvbuckets.BuildKeyValueConfig(spec.bucket, spec.ttl, spec.storage)
	kv.Metadata = BuildMarker(spec.component, instance)
	return PlannedAction{
		Kind:     ActionCreateKV,
		Name:     spec.bucket,
		Resource: kv,
	}
}

// classifyControlPlaneDrift compares a live KV-bucket stream against the
// desired spec and returns one or more drift findings. v1 never emits
// update-* / delete-* actions, only findings.
func classifyControlPlaneDrift(spec controlPlaneSpec, info *jetstream.StreamInfo, instance string) []DriftFinding {
	marker := ParseMarker(info.Config.Metadata)

	// Unmarked bucket named by config → "adopted" drift, no action.
	if !marker.IsManaged() {
		return []DriftFinding{{
			Severity: SeverityAdopted,
			Kind:     KindControlPlaneKV,
			Name:     spec.bucket,
			Detail: map[string]any{
				"component": spec.component,
				"reason":    "bucket exists without parti.io/managed marker",
			},
		}}
	}

	mutable := map[string]any{}
	immutable := map[string]any{}

	if info.Config.Storage != spec.storage {
		immutable["storage"] = map[string]any{
			"want": storageName(spec.storage),
			"got":  storageName(info.Config.Storage),
		}
	}
	// KV History is stored on the underlying stream as MaxMsgsPerSubject.
	// The shared builder always sets History=1, so any non-1 value is
	// immutable drift.
	if info.Config.MaxMsgsPerSubject != 1 {
		immutable["history"] = map[string]any{
			"want": int64(1),
			"got":  info.Config.MaxMsgsPerSubject,
		}
	}
	// KV TTL is stored on the underlying stream as MaxAge.
	if info.Config.MaxAge != spec.ttl {
		mutable["ttl"] = map[string]any{
			"want": spec.ttl.String(),
			"got":  info.Config.MaxAge.String(),
		}
	}
	// Instance mismatch on a managed bucket is surfaced as drift-mutable
	// since Metadata is live-editable in Phase 2.
	if marker.Instance != instance {
		mutable["instance"] = map[string]any{
			"want": instance,
			"got":  marker.Instance,
		}
	}
	// Component mismatch: a bucket marked as one Parti component but named
	// in config under a different role. Treat as immutable drift — the
	// safe remediation is operator-driven.
	if marker.Component != spec.component {
		immutable["component"] = map[string]any{
			"want": spec.component,
			"got":  marker.Component,
		}
	}

	findings := make([]DriftFinding, 0, 2)
	if len(immutable) > 0 {
		findings = append(findings, DriftFinding{
			Severity: SeverityDriftImmutable,
			Kind:     KindControlPlaneKV,
			Name:     spec.bucket,
			Detail:   immutable,
		})
	}
	if len(mutable) > 0 {
		findings = append(findings, DriftFinding{
			Severity: SeverityDriftMutable,
			Kind:     KindControlPlaneKV,
			Name:     spec.bucket,
			Detail:   mutable,
		})
	}
	if len(findings) == 0 {
		findings = append(findings, DriftFinding{
			Severity: SeverityInformational,
			Kind:     KindControlPlaneKV,
			Name:     spec.bucket,
			Detail: map[string]any{
				"component": spec.component,
			},
		})
	}

	return findings
}

func sortActions(s []PlannedAction) {
	slices.SortStableFunc(s, func(a, b PlannedAction) int {
		if c := stringsCmp(a.Kind, b.Kind); c != 0 {
			return c
		}

		return stringsCmp(a.Name, b.Name)
	})
}

func sortDrift(s []DriftFinding) {
	slices.SortStableFunc(s, func(a, b DriftFinding) int {
		if c := stringsCmp(a.Kind, b.Kind); c != 0 {
			return c
		}

		return stringsCmp(a.Name, b.Name)
	})
}

// stringsCmp returns -1/0/1 to satisfy slices.SortStableFunc's int contract.
func stringsCmp(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
