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
//     concern).
//
// Partition-source create-kv emission and drift classification are
// implemented. Dynamic-consumer alignment entries are not yet populated
// through Plan; use PlanDynamicConsumers directly.
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

	// DynamicConsumers Plan output is intentionally empty. The slot exists
	// so the PlanResult shape is stable for future use.

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
			// adopt does not create missing buckets: it emits no action
			// but records an informational finding so the missing bucket
			// is not silently absent from the plan. warn / safe-update
			// keep their create-kv emission.
			if cfg.Policy == PolicyAdopt {
				out.Drift = append(out.Drift, missingUnderAdoptFinding(KindControlPlaneKV, spec.bucket))
				continue
			}
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

		marked := ParseMarker(info.Config.Metadata).IsManaged()

		// Under safe-update, a Parti-marked bucket with operator-
		// expressible drift also gets an update-kv action. Drift
		// findings and the action coexist; the classifier above still
		// populates out.Drift in every policy.
		if cfg.Policy == PolicySafeUpdate && marked {
			before := cloneKVConfig(streamConfigToKVConfig(info.Config))
			after := buildControlPlaneUpdateTarget(spec, cfg.Instance, before)
			if !kvConfigsEqual(before, after) {
				out.Actions = append(out.Actions, PlannedAction{
					Kind:     ActionUpdateKV,
					Name:     spec.bucket,
					Resource: &UpdateKVResource{Before: before, After: after},
				})
			}
		}

		// Under adopt, an unmarked bucket gets a stamp-marker action.
		// The classifier's "adopted" finding above still surfaces; the
		// action and the finding coexist. A marked bucket is already
		// adopted, so no stamp-marker is emitted.
		if cfg.Policy == PolicyAdopt && !marked {
			out.Actions = append(out.Actions,
				newStampMarkerAction(spec.bucket, spec.component, cfg.Instance, info.Config))
		}
	}

	return nil
}

// missingUnderAdoptFinding builds the informational drift finding emitted
// for a config-named bucket that does not exist live when the policy is
// adopt. adopt does not create buckets, so the bucket would otherwise
// vanish silently from the plan; this finding keeps it visible.
func missingUnderAdoptFinding(kind, bucket string) DriftFinding {
	return DriftFinding{
		Severity: SeverityInformational,
		Kind:     kind,
		Name:     bucket,
		Detail: map[string]any{
			"reason": "bucket missing; adopt does not create — run apply with warn or safe-update",
		},
	}
}

// newStampMarkerAction builds the stamp-marker PlannedAction for one
// unmarked bucket. The merged metadata is computed from the live
// snapshot so non-Parti keys are preserved; PartiKeys lists exactly the
// keys the action adds or changes for operator review.
func newStampMarkerAction(bucket, component, instance string, live jetstream.StreamConfig) PlannedAction {
	liveKV := streamConfigToKVConfig(live)
	merged := mergeMarkerMetadata(liveKV.Metadata, component, instance)

	return PlannedAction{
		Kind: ActionStampMarker,
		Name: bucket,
		Resource: &StampMarkerResource{
			Bucket:         bucket,
			MergedMetadata: merged,
			PartiKeys:      keysAddedOrChanged(liveKV.Metadata, merged),
		},
	}
}

// keysAddedOrChanged returns the sorted list of metadata keys whose value
// differs between live and merged, in either direction: keys added to
// merged, keys whose value changed, and keys removed from live (present
// in live but absent from merged). A removal is a real change an
// operator must see — an unmarked bucket carrying a stray
// parti.io/instance adopted under an empty cfg.Instance has that key
// deleted by mergeMarkerMetadata.
func keysAddedOrChanged(live, merged map[string]string) []string {
	changed := map[string]struct{}{}
	for k, mv := range merged {
		if lv, ok := live[k]; !ok || lv != mv {
			changed[k] = struct{}{}
		}
	}
	for k := range live {
		if _, ok := merged[k]; !ok {
			changed[k] = struct{}{}
		}
	}
	keys := make([]string, 0, len(changed))
	for k := range changed {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	return keys
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
//
// This helper sets parti.io/component to the desired value so the drift
// classifier gate detects a component mismatch. It is intentionally
// distinct from mergeUpdateKVMetadata, which PRESERVES the live
// component — do not consolidate the two helpers.
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

// mergeUpdateKVMetadata returns the Metadata map for an update-kv
// target. It clones live, forces parti.io/managed to the current schema
// value, sets or removes parti.io/instance per the desired instance,
// and PRESERVES parti.io/component verbatim from live — component is
// drift-immutable and update-kv never re-labels a bucket's role.
// Non-Parti keys are preserved.
//
// This helper is intentionally distinct from mergeMarkerMetadata, which
// rewrites parti.io/component to the desired value for classifier drift
// detection. Do not consolidate the two helpers: doing so reintroduces
// the silent component-rewrite bug, where safe-update would re-label a
// bucket stamped for a different role instead of flagging it.
func mergeUpdateKVMetadata(live map[string]string, instance string) map[string]string {
	merged := maps.Clone(live)
	if merged == nil {
		merged = map[string]string{}
	}
	merged[MarkerManagedKey] = MarkerManagedValue
	if instance != "" {
		merged[MarkerInstanceKey] = instance
	} else {
		delete(merged, MarkerInstanceKey)
	}
	// parti.io/component is left exactly as cloned from live.

	return merged
}

// buildControlPlaneUpdateTarget returns the desired update-kv target for
// a control-plane bucket: a deep clone of before with only the
// operator-expressible fields overwritten. Every other field —
// drift-detection-only (History, Storage), preserved-from-live
// (Description, MaxBytes, MaxValueSize, Placement, RePublish, Mirror,
// Sources, Compression, LimitMarkerTTL), and Bucket — is inherited from
// before verbatim. Control-plane has no MaxValueSize config field, so
// that field is preserved-from-live here.
func buildControlPlaneUpdateTarget(spec controlPlaneSpec, instance string, before jetstream.KeyValueConfig) jetstream.KeyValueConfig {
	after := cloneKVConfig(before)
	after.TTL = spec.ttl
	after.Replicas = spec.replicas
	after.Metadata = mergeUpdateKVMetadata(before.Metadata, instance)

	return after
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
