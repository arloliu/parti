package provision

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
)

// buildPartitionSourceKVConfig returns a jetstream.KeyValueConfig for the
// partition-source bucket described by ps.
//
// This builder is intentionally separate from the W0 control-plane builder
// (internal/kvbuckets.BuildKeyValueConfig) because partition-source has a
// different shape: Storage, History, Replicas, and MaxValueSize are all
// configurable by the operator, whereas control-plane buckets have fixed
// Storage and History=1. The two builders must never be merged.
//
// Field mapping rules (ps must already be normalized by normalize()):
//   - Bucket: ps.Bucket (required; empty is rejected by validatePartitionSource)
//   - Storage: FileStorage when ps.Storage is "file", MemoryStorage otherwise
//   - History: ps.History (always ≥ 1 after normalization)
//   - Replicas: ps.Replicas (0 = server default; nats.go normalizes to 1 server-side)
//   - MaxValueSize: ps.MaxValueSize (0 = no limit)
//   - TTL: set only when ps.TTL > 0 (matches control-plane convention)
func buildPartitionSourceKVConfig(ps PartitionSourceConfig) jetstream.KeyValueConfig {
	cfg := jetstream.KeyValueConfig{
		Bucket:       ps.Bucket,
		History:      ps.History,
		Storage:      storageTypeFromString(ps.Storage),
		Replicas:     ps.Replicas,
		MaxValueSize: ps.MaxValueSize,
	}

	if ps.TTL > 0 {
		cfg.TTL = ps.TTL
	}

	return cfg
}

// planPartitionSource resolves the partition-source bucket against live NATS
// state and appends actions / drift findings to out. It must only be called
// when cfg.PartitionSource != nil.
func planPartitionSource(ctx context.Context, js jetstream.JetStream, cfg Config, out *PlanResult) error {
	ps := cfg.PartitionSource
	streamName := kvStreamPrefix + ps.Bucket

	stream, err := js.Stream(ctx, streamName)
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		kv := buildPartitionSourceKVConfig(*ps)
		kv.Metadata = BuildMarker(ComponentPartitionSource, cfg.Instance)
		out.Actions = append(out.Actions, PlannedAction{
			Kind:     ActionCreateKV,
			Name:     ps.Bucket,
			Resource: kv,
		})

		return nil
	}
	if err != nil {
		return fmt.Errorf("provision: lookup %s: %w", streamName, err)
	}

	info, err := stream.Info(ctx)
	if err != nil {
		return fmt.Errorf("provision: info %s: %w", streamName, err)
	}

	findings := classifyPartitionSourceDrift(ps, info, cfg.Instance)
	out.Drift = append(out.Drift, findings...)

	return nil
}

// classifyPartitionSourceDrift compares a live KV-bucket stream against the
// desired partition-source config and returns drift findings.
//
// Mutability:
//   - Immutable: History, Storage, and the parti.io/component marker key.
//   - Mutable: MaxValueSize, TTL, Replicas, and the parti.io/managed and
//     parti.io/instance marker keys. The NATS server enforces cluster
//     feasibility for Replicas changes at apply time.
//   - Preserved-from-live (not reported as drift): Description, MaxBytes.
//     These fields have no YAML representation and survive every update-kv
//     unchanged.
func classifyPartitionSourceDrift(ps *PartitionSourceConfig, info *jetstream.StreamInfo, instance string) []DriftFinding {
	marker := ParseMarker(info.Config.Metadata)
	if !marker.IsManaged() {
		return []DriftFinding{{
			Severity: SeverityAdopted,
			Kind:     KindPartitionSource,
			Name:     ps.Bucket,
			Detail: map[string]any{
				"component": ComponentPartitionSource,
				"reason":    "bucket exists without parti.io/managed marker",
			},
		}}
	}

	live := extractLiveKVConfig(&info.Config)
	wanted := wantedPartitionSourceKV(ps, instance, live)
	if kvConfigsEqual(wanted, live) {
		return []DriftFinding{{
			Severity: SeverityInformational,
			Kind:     KindPartitionSource,
			Name:     ps.Bucket,
			Detail:   map[string]any{"component": ComponentPartitionSource},
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
	if normalizeReplicas(live.Replicas) != normalizeReplicas(wanted.Replicas) {
		addMutable("replicas", map[string]any{
			"want": normalizeReplicas(wanted.Replicas),
			"got":  live.Replicas,
		})
	}
	if normalizeMaxValueSize(live.MaxValueSize) != normalizeMaxValueSize(wanted.MaxValueSize) {
		addMutable("maxValueSize", map[string]any{
			"want": wanted.MaxValueSize,
			"got":  info.Config.MaxMsgSize,
		})
	}
	if live.TTL != wanted.TTL {
		addMutable("ttl", map[string]any{
			"want": wanted.TTL.String(),
			"got":  live.TTL.String(),
		})
	}

	if marker.Managed != MarkerManagedValue {
		addMutable("managed", map[string]any{
			"want": MarkerManagedValue,
			"got":  marker.Managed,
		})
	}
	// Component mismatch is immutable: a bucket stamped for a different
	// role is a misconfiguration safe-update must not silently re-label.
	if marker.Component != ComponentPartitionSource {
		addImmutable("component", map[string]any{
			"want": ComponentPartitionSource,
			"got":  marker.Component,
		})
	}
	if marker.Instance != instance {
		addMutable("instance", map[string]any{
			"want": instance,
			"got":  marker.Instance,
		})
	}

	findings := make([]DriftFinding, 0, 2)
	if len(immutable) > 0 {
		findings = append(findings, DriftFinding{
			Severity: SeverityDriftImmutable,
			Kind:     KindPartitionSource,
			Name:     ps.Bucket,
			Detail:   immutable,
		})
	}
	if len(mutable) > 0 {
		findings = append(findings, DriftFinding{
			Severity: SeverityDriftMutable,
			Kind:     KindPartitionSource,
			Name:     ps.Bucket,
			Detail:   mutable,
		})
	}

	return findings
}

// wantedPartitionSourceKV returns the KeyValueConfig the ps implies, with
// preserved-from-live fields inherited from live.
func wantedPartitionSourceKV(ps *PartitionSourceConfig, instance string, live jetstream.KeyValueConfig) jetstream.KeyValueConfig {
	wanted := live
	wanted.Storage = storageTypeFromString(ps.Storage)
	wanted.History = ps.History
	wanted.TTL = ps.TTL
	wanted.Replicas = ps.Replicas
	wanted.MaxValueSize = ps.MaxValueSize
	wanted.Metadata = mergeMarkerMetadata(live.Metadata, ComponentPartitionSource, instance)

	return wanted
}

// storageTypeFromString maps the YAML-side string ("file"/"memory") to a
// jetstream.StorageType. Anything other than "memory" maps to FileStorage,
// matching the runtime default for partition-source buckets.
func storageTypeFromString(s string) jetstream.StorageType {
	if s == "memory" {
		return jetstream.MemoryStorage
	}

	return jetstream.FileStorage
}
