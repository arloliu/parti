package provision

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/nats-io/nats.go/jetstream"
)

// planStreams resolves each declared StreamCfg against live NATS state and
// appends actions / drift findings to out. It must only be called when
// len(cfg.Streams) > 0.
func planStreams(ctx context.Context, js jetstream.JetStream, cfg Config, out *PlanResult) error {
	for _, streamCfg := range cfg.Streams {
		if err := ctx.Err(); err != nil {
			return err
		}

		stream, err := js.Stream(ctx, streamCfg.Name)
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			if cfg.Policy == PolicyAdopt {
				out.Drift = append(out.Drift,
					missingUnderAdoptFinding(KindApplicationStream, streamCfg.Name, "stream"))
			} else {
				out.Actions = append(out.Actions, PlannedAction{
					Kind:     ActionCreateStream,
					Name:     streamCfg.Name,
					Resource: buildStreamConfig(streamCfg, cfg.Instance),
				})
			}

			continue
		}
		if err != nil {
			return fmt.Errorf("provision: lookup %s: %w", streamCfg.Name, err)
		}

		info, err := stream.Info(ctx)
		if err != nil {
			return fmt.Errorf("provision: info %s: %w", streamCfg.Name, err)
		}

		findings := classifyStreamDrift(streamCfg, info, cfg.Instance)
		out.Drift = append(out.Drift, findings...)

		marked := ParseMarker(info.Config.Metadata).IsManaged()

		// Recreate gate: force + ownership-proven + immutable drift +
		// AllowDeleteRecreate. Emits exactly ONE recreate-stream; the
		// update-stream gate is skipped for this resource. Drift findings coexist.
		immutableDrift, hasImmutable := immutableFindingDetail(findings)
		recreateOK := policyRecreatesImmutable(cfg.Policy) &&
			marked &&
			streamCfg.AllowDeleteRecreate
		if hasImmutable && recreateOK {
			before := cloneStreamConfig(info.Config)
			after := buildStreamConfig(streamCfg, cfg.Instance)
			out.Actions = append(out.Actions, PlannedAction{
				Kind: ActionRecreateStream,
				Name: streamCfg.Name,
				Resource: &RecreateStreamResource{
					Before:         before,
					After:          after,
					ImmutableDrift: immutableDrift,
				},
			})

			continue
		}

		// Under safe-update or force, a Parti-marked stream with mutable drift
		// also gets an update-stream action. Drift findings and the action coexist;
		// the classifier above still populates out.Drift in every policy.
		if policyReconcilesMutable(cfg.Policy) && marked {
			target := buildStreamUpdateTarget(streamCfg, cfg.Instance, info.Config)
			if !streamConfigsEqual(info.Config, target) {
				out.Actions = append(out.Actions, PlannedAction{
					Kind: ActionUpdateStream,
					Name: streamCfg.Name,
					Resource: &UpdateStreamResource{
						Before: cloneStreamConfig(info.Config),
						After:  cloneStreamConfig(target),
					},
				})
			}
		}

		// Under adopt, an unmarked stream gets a stamp-stream-marker action. The
		// classifier's "adopted" finding above still surfaces; the action and the
		// finding coexist. A marked stream is already adopted, so no stamp is emitted.
		if cfg.Policy == PolicyAdopt && !marked {
			out.Actions = append(out.Actions,
				newStampStreamMarkerAction(streamCfg.Name, cfg.Instance, info.Config))
		}
	}

	return nil
}

// newStampStreamMarkerAction builds the stamp-stream-marker PlannedAction for
// an unmarked stream. The merged metadata is computed from the live snapshot so
// non-Parti keys are preserved; PartiKeys lists exactly the keys the action
// adds or changes for operator review.
func newStampStreamMarkerAction(name, instance string, live jetstream.StreamConfig) PlannedAction {
	merged := mergeMarkerMetadata(live.Metadata, ComponentStream, instance)

	return PlannedAction{
		Kind: ActionStampStreamMarker,
		Name: name,
		Resource: &StreamStampMarkerResource{
			Stream:         name,
			MergedMetadata: merged,
			PartiKeys:      keysAddedOrChanged(live.Metadata, merged),
		},
	}
}

// buildStreamConfig maps a StreamCfg to a jetstream.StreamConfig for a
// create-stream action. It sets all provision-managed fields and stamps the
// Parti ownership marker. The unlimited convention is translated: config 0
// becomes -1 for MaxBytes and MaxMsgs only; MaxAge passes through (0 stays 0).
func buildStreamConfig(streamCfg StreamCfg, instance string) jetstream.StreamConfig {
	return jetstream.StreamConfig{
		Name:        streamCfg.Name,
		Subjects:    slices.Clone(streamCfg.Subjects),
		Retention:   retentionPolicyFromString(streamCfg.Retention),
		Storage:     storageTypeFromString(streamCfg.Storage),
		Discard:     discardPolicyFromString(streamCfg.Discard),
		Replicas:    streamCfg.Replicas,
		MaxAge:      streamCfg.MaxAge,
		MaxBytes:    normalizeStreamLimit(streamCfg.MaxBytes),
		MaxMsgs:     normalizeStreamLimit(streamCfg.MaxMsgs),
		Description: streamCfg.Description,
		Metadata:    BuildMarker(ComponentStream, instance),
	}
}

// wantedStreamConfig is the classifier-side target builder. It clones live
// and overwrites every provision-managed field with the desired value —
// including the immutable ones (Storage, Retention) — so streamConfigsEqual
// detects a storage- or retention-only divergence. Metadata is merged via
// mergeMarkerMetadata so a component mismatch is detectable.
func wantedStreamConfig(streamCfg StreamCfg, instance string, live jetstream.StreamConfig) jetstream.StreamConfig {
	wanted := cloneStreamConfig(live)
	wanted.Subjects = slices.Clone(streamCfg.Subjects)
	wanted.Retention = retentionPolicyFromString(streamCfg.Retention)
	wanted.Storage = storageTypeFromString(streamCfg.Storage)
	wanted.Discard = discardPolicyFromString(streamCfg.Discard)
	wanted.Replicas = streamCfg.Replicas
	wanted.MaxAge = streamCfg.MaxAge
	wanted.MaxBytes = normalizeStreamLimit(streamCfg.MaxBytes)
	wanted.MaxMsgs = normalizeStreamLimit(streamCfg.MaxMsgs)
	wanted.Description = streamCfg.Description
	wanted.Metadata = mergeMarkerMetadata(live.Metadata, ComponentStream, instance)

	return wanted
}

// buildStreamUpdateTarget is the update-side target builder used by
// update-stream. It clones live and overwrites only the mutable fields;
// Storage and Retention are inherited from live so an update-stream never
// writes an immutable change. Metadata is merged via mergeUpdateKVMetadata
// (forces managed, sets/clears instance, preserves component verbatim).
func buildStreamUpdateTarget(streamCfg StreamCfg, instance string, live jetstream.StreamConfig) jetstream.StreamConfig {
	target := cloneStreamConfig(live)
	target.Subjects = slices.Clone(streamCfg.Subjects)
	// Storage and Retention are deliberately inherited from live (immutable).
	target.Discard = discardPolicyFromString(streamCfg.Discard)
	target.Replicas = streamCfg.Replicas
	target.MaxAge = streamCfg.MaxAge
	target.MaxBytes = normalizeStreamLimit(streamCfg.MaxBytes)
	target.MaxMsgs = normalizeStreamLimit(streamCfg.MaxMsgs)
	target.Description = streamCfg.Description
	target.Metadata = mergeUpdateKVMetadata(live.Metadata, instance)

	return target
}

// streamConfigsEqual reports whether two jetstream.StreamConfig values agree on
// the provision-managed fields plus Metadata, normalizing server defaults:
//   - Subjects compared as a set (sorted copies).
//   - Storage: empty (FileStorage==0) is equivalent to FileStorage.
//   - Retention: empty (LimitsPolicy==0) is equivalent to LimitsPolicy.
//   - Discard: empty (DiscardOld==0) is equivalent to DiscardOld.
//   - Replicas: 0 equivalent to 1 (via normalizeReplicas).
//   - MaxBytes, MaxMsgs: 0 equivalent to -1 (normalizeStreamLimit).
//   - MaxAge: compared directly (no 0↔-1 normalization; server keeps 0 as unlimited).
//   - Metadata: compared as a map.
//
// All other StreamConfig fields are ignored (preserved-from-live).
func streamConfigsEqual(a, b jetstream.StreamConfig) bool {
	// Subjects as set: sort copies, do not mutate inputs.
	aSubs := slices.Clone(a.Subjects)
	bSubs := slices.Clone(b.Subjects)
	slices.Sort(aSubs)
	slices.Sort(bSubs)
	if !slices.Equal(aSubs, bSubs) {
		return false
	}

	// Storage / Retention / Discard: the NATS server default for each is the
	// zero value (FileStorage / LimitsPolicy / DiscardOld), so a config that
	// omits the field already compares equal to a server-defaulted live one.
	if a.Storage != b.Storage {
		return false
	}
	if a.Retention != b.Retention {
		return false
	}
	if a.Discard != b.Discard {
		return false
	}
	if normalizeReplicas(a.Replicas) != normalizeReplicas(b.Replicas) {
		return false
	}
	// MaxAge compared directly: server keeps 0 as unlimited (no 0↔-1 rewrite).
	if a.MaxAge != b.MaxAge {
		return false
	}
	if normalizeStreamLimit(a.MaxBytes) != normalizeStreamLimit(b.MaxBytes) {
		return false
	}
	if normalizeStreamLimit(a.MaxMsgs) != normalizeStreamLimit(b.MaxMsgs) {
		return false
	}
	if a.Description != b.Description {
		return false
	}
	if !maps.Equal(a.Metadata, b.Metadata) {
		return false
	}

	return true
}

// classifyStreamDrift compares a live stream against the desired StreamCfg and
// returns one or more drift findings. Never emits update-* / delete-* actions.
func classifyStreamDrift(streamCfg StreamCfg, info *jetstream.StreamInfo, instance string) []DriftFinding {
	marker := ParseMarker(info.Config.Metadata)
	if !marker.IsManaged() {
		return []DriftFinding{{
			Severity: SeverityAdopted,
			Kind:     KindApplicationStream,
			Name:     streamCfg.Name,
			Detail: map[string]any{
				"component": ComponentStream,
				"reason":    "stream exists without parti.io/managed marker",
			},
		}}
	}

	live := info.Config
	wanted := wantedStreamConfig(streamCfg, instance, live)
	if streamConfigsEqual(wanted, live) {
		return []DriftFinding{{
			Severity: SeverityInformational,
			Kind:     KindApplicationStream,
			Name:     streamCfg.Name,
			Detail:   map[string]any{"component": ComponentStream},
		}}
	}

	var b driftBuilder
	routeStreamFieldDrift(live, wanted, marker, instance, &b)

	return b.findings(KindApplicationStream, streamCfg.Name)
}

// routeStreamFieldDrift compares individual provision-managed fields of live and
// wanted, routing each differing field to the driftBuilder as immutable or
// mutable drift.
//
// Immutable: Storage, Retention, parti.io/component mismatch.
// Mutable: Subjects, Discard, Replicas, MaxAge, MaxBytes, MaxMsgs, Description,
// parti.io/managed and parti.io/instance marker fields.
func routeStreamFieldDrift(
	live, wanted jetstream.StreamConfig,
	marker MarkerInfo,
	instance string,
	b *driftBuilder,
) {
	if live.Storage != wanted.Storage {
		b.addImmutable("storage", map[string]any{
			"want": storageName(wanted.Storage),
			"got":  storageName(live.Storage),
		})
	}
	// Every retention divergence is treated as immutable: a conservative
	// policy, since NATS accepts limits<->interest but rejects workqueue
	// transitions and couples the update to consumer-replica state.
	if live.Retention != wanted.Retention {
		b.addImmutable("retention", map[string]any{
			"want": retentionName(wanted.Retention),
			"got":  retentionName(live.Retention),
		})
	}

	liveSubs := slices.Clone(live.Subjects)
	wantedSubs := slices.Clone(wanted.Subjects)
	slices.Sort(liveSubs)
	slices.Sort(wantedSubs)
	if !slices.Equal(liveSubs, wantedSubs) {
		b.addMutable("subjects", map[string]any{
			"want": wanted.Subjects,
			"got":  live.Subjects,
		})
	}
	if live.Discard != wanted.Discard {
		b.addMutable("discard", map[string]any{
			"want": discardName(wanted.Discard),
			"got":  discardName(live.Discard),
		})
	}
	if normalizeReplicas(live.Replicas) != normalizeReplicas(wanted.Replicas) {
		b.addMutable("replicas", map[string]any{
			"want": normalizeReplicas(wanted.Replicas),
			"got":  live.Replicas,
		})
	}
	if live.MaxAge != wanted.MaxAge {
		b.addMutable("maxAge", map[string]any{
			"want": wanted.MaxAge.String(),
			"got":  live.MaxAge.String(),
		})
	}
	if normalizeStreamLimit(live.MaxBytes) != normalizeStreamLimit(wanted.MaxBytes) {
		b.addMutable("maxBytes", map[string]any{
			"want": wanted.MaxBytes,
			"got":  live.MaxBytes,
		})
	}
	if normalizeStreamLimit(live.MaxMsgs) != normalizeStreamLimit(wanted.MaxMsgs) {
		b.addMutable("maxMsgs", map[string]any{
			"want": wanted.MaxMsgs,
			"got":  live.MaxMsgs,
		})
	}
	if live.Description != wanted.Description {
		b.addMutable("description", map[string]any{
			"want": wanted.Description,
			"got":  live.Description,
		})
	}

	// Marker field drift.
	if marker.Managed != MarkerManagedValue {
		b.addMutable("managed", map[string]any{
			"want": MarkerManagedValue,
			"got":  marker.Managed,
		})
	}
	if marker.Instance != instance {
		b.addMutable("instance", map[string]any{
			"want": instance,
			"got":  marker.Instance,
		})
	}
	// Component mismatch is immutable: the safe remediation is operator-driven.
	if marker.Component != ComponentStream {
		b.addImmutable("component", map[string]any{
			"want": ComponentStream,
			"got":  marker.Component,
		})
	}
}

// cloneStreamConfig returns a deep clone of src. The Subjects slice, the
// Metadata map, the pointer fields (Placement, RePublish, SubjectTransform,
// Mirror), and the Sources slice with its pointees are all reallocated, so
// the result is safe to mutate without affecting src — the
// UpdateStreamResource audit surface depends on that immutability.
func cloneStreamConfig(src jetstream.StreamConfig) jetstream.StreamConfig {
	dst := src // shallow copy of scalar fields

	dst.Subjects = slices.Clone(src.Subjects)
	dst.Metadata = maps.Clone(src.Metadata)

	if src.Placement != nil {
		p := *src.Placement
		p.Tags = slices.Clone(src.Placement.Tags)
		dst.Placement = &p
	}
	if src.RePublish != nil {
		rp := *src.RePublish
		dst.RePublish = &rp
	}
	if src.SubjectTransform != nil {
		st := *src.SubjectTransform
		dst.SubjectTransform = &st
	}
	if src.Mirror != nil {
		dst.Mirror = cloneStreamSource(src.Mirror)
	}
	if src.Sources != nil {
		dst.Sources = make([]*jetstream.StreamSource, len(src.Sources))
		for i, ss := range src.Sources {
			dst.Sources[i] = cloneStreamSource(ss)
		}
	}

	return dst
}

// normalizeStreamLimit translates the provision "unlimited" convention to the
// NATS wire form for MaxBytes and MaxMsgs: config 0 becomes -1 (unlimited).
// A live -1 is unchanged. This normalizer must NOT be applied to MaxAge —
// the server keeps 0 as unlimited for MaxAge and rejects negative values.
func normalizeStreamLimit(v int64) int64 {
	if v == 0 {
		return -1
	}

	return v
}

// retentionPolicyFromString maps the YAML-side string to a jetstream.RetentionPolicy.
// "workqueue" → WorkQueuePolicy, "interest" → InterestPolicy, anything else → LimitsPolicy.
func retentionPolicyFromString(s string) jetstream.RetentionPolicy {
	switch s {
	case "workqueue":
		return jetstream.WorkQueuePolicy
	case "interest":
		return jetstream.InterestPolicy
	default:
		return jetstream.LimitsPolicy
	}
}

// discardPolicyFromString maps the YAML-side string to a jetstream.DiscardPolicy.
// "new" → DiscardNew, anything else → DiscardOld.
func discardPolicyFromString(s string) jetstream.DiscardPolicy {
	if s == "new" {
		return jetstream.DiscardNew
	}

	return jetstream.DiscardOld
}
