package provision

import (
	"context"
	"errors"
	"fmt"
	"maps"
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
	replicas  int // 0 = server default (nats.go normalizes to 1)
}

func buildControlPlaneSpecs(cp ControlPlaneConfig) []controlPlaneSpec {
	specs := []controlPlaneSpec{
		{ComponentControlPlaneID, cp.StableIDBucket, cp.WorkerIDTTL, jetstream.FileStorage, cp.Replicas},
		{ComponentControlPlaneElection, cp.ElectionBucket, cp.ElectionTimeout, jetstream.MemoryStorage, cp.Replicas},
		{ComponentControlPlaneHeartbeat, cp.HeartbeatBucket, cp.HeartbeatTTL, jetstream.MemoryStorage, cp.Replicas},
		{ComponentControlPlaneAssignment, cp.AssignmentBucket, cp.AssignmentTTL, jetstream.FileStorage, cp.Replicas},
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
			replicas:  cp.Replicas,
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
// bucket. The KeyValueConfig is built by the shared kvbuckets builder
// and then stamped with the Parti ownership marker; nothing else may
// construct the KeyValueConfig (this preserves byte-equivalence with
// the runtime manager). Replicas is the only field stamped on top of
// the builder output, and only when explicitly set in config.
func newCreateKVAction(spec controlPlaneSpec, instance string) PlannedAction {
	kv := kvbuckets.BuildKeyValueConfig(spec.bucket, spec.ttl, spec.storage)
	if spec.replicas > 0 {
		kv.Replicas = spec.replicas
	}
	kv.Metadata = BuildMarker(spec.component, instance)
	return PlannedAction{
		Kind:     ActionCreateKV,
		Name:     spec.bucket,
		Resource: kv,
	}
}

// classifyControlPlaneDrift compares a live KV-bucket stream against the
// desired spec and returns one or more drift findings. Never emits
// update-* / delete-* actions, only findings.
func classifyControlPlaneDrift(spec controlPlaneSpec, info *jetstream.StreamInfo, instance string) []DriftFinding {
	marker := ParseMarker(info.Config.Metadata)
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

	live := extractLiveKVConfig(&info.Config)
	wanted := wantedControlPlaneKV(spec, instance, live)
	if kvConfigsEqual(wanted, live) {
		return []DriftFinding{{
			Severity: SeverityInformational,
			Kind:     KindControlPlaneKV,
			Name:     spec.bucket,
			Detail:   map[string]any{"component": spec.component},
		}}
	}

	var mutable, immutable map[string]any
	addImmutable := func(field string, detail map[string]any) {
		if immutable == nil {
			immutable = map[string]any{}
		}
		immutable[field] = detail
	}
	addMutable := func(field string, detail map[string]any) {
		if mutable == nil {
			mutable = map[string]any{}
		}
		mutable[field] = detail
	}

	if live.Storage != wanted.Storage {
		addImmutable("storage", map[string]any{
			"want": storageName(wanted.Storage),
			"got":  storageName(live.Storage),
		})
	}
	if live.History != wanted.History {
		addImmutable("history", map[string]any{
			"want": int64(wanted.History),
			"got":  info.Config.MaxMsgsPerSubject,
		})
	}
	if live.TTL != wanted.TTL {
		addMutable("ttl", map[string]any{
			"want": wanted.TTL.String(),
			"got":  live.TTL.String(),
		})
	}
	if normalizeReplicas(live.Replicas) != normalizeReplicas(wanted.Replicas) {
		addMutable("replicas", map[string]any{
			"want": normalizeReplicas(wanted.Replicas),
			"got":  live.Replicas,
		})
	}
	if marker.Managed != MarkerManagedValue {
		addMutable("managed", map[string]any{
			"want": MarkerManagedValue,
			"got":  marker.Managed,
		})
	}
	if marker.Instance != instance {
		addMutable("instance", map[string]any{
			"want": instance,
			"got":  marker.Instance,
		})
	}
	// Component mismatch is immutable: the safe remediation is operator-driven.
	if marker.Component != spec.component {
		addImmutable("component", map[string]any{
			"want": spec.component,
			"got":  marker.Component,
		})
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

	return findings
}

// wantedControlPlaneKV returns the KeyValueConfig the spec implies, with
// preserved-from-live fields inherited from live. The returned value is
// suitable as input to kvConfigsEqual against live.
func wantedControlPlaneKV(spec controlPlaneSpec, instance string, live jetstream.KeyValueConfig) jetstream.KeyValueConfig {
	wanted := live
	wanted.Storage = spec.storage
	wanted.History = 1
	wanted.TTL = spec.ttl
	wanted.Replicas = spec.replicas
	wanted.Metadata = mergeMarkerMetadata(live.Metadata, spec.component, instance)

	return wanted
}

// mergeMarkerMetadata returns a fresh metadata map with the Parti marker
// keys overlaid on a clone of live so non-Parti keys are preserved. When
// instance is empty the parti.io/instance key is removed, so a config
// that clears the instance label produces metadata that genuinely
// differs from a live bucket still carrying one.
func mergeMarkerMetadata(live map[string]string, component, instance string) map[string]string {
	merged := maps.Clone(live)
	if merged == nil {
		merged = map[string]string{}
	}
	for k, v := range BuildMarker(component, instance) {
		merged[k] = v
	}
	if instance == "" {
		delete(merged, MarkerInstanceKey)
	}

	return merged
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
