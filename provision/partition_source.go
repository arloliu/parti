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
	var storage jetstream.StorageType
	if ps.Storage == "memory" {
		storage = jetstream.MemoryStorage
	} else {
		storage = jetstream.FileStorage
	}

	cfg := jetstream.KeyValueConfig{
		Bucket:       ps.Bucket,
		History:      ps.History,
		Storage:      storage,
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
// desired partition-source config and returns one or more drift findings. v1
// never emits update-* / delete-* actions, only findings.
//
// Mutability table (per KV field mutability rules in the plan):
//   - Immutable: History, Storage. Replicas treated as drift-immutable
//     conservatively in v1 (conditionally mutable; safe-update lands in Phase 2).
//   - Mutable: Description, Metadata (managed, component, instance), MaxBytes,
//     MaxValueSize, TTL.
func classifyPartitionSourceDrift(ps *PartitionSourceConfig, info *jetstream.StreamInfo, instance string) []DriftFinding {
	marker := ParseMarker(info.Config.Metadata)

	// Unmarked bucket named by config → "adopted" drift, no action.
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

	mutable := map[string]any{}
	immutable := map[string]any{}

	// Desired storage type.
	var wantStorage jetstream.StorageType
	if ps.Storage == "memory" {
		wantStorage = jetstream.MemoryStorage
	} else {
		wantStorage = jetstream.FileStorage
	}
	if info.Config.Storage != wantStorage {
		immutable["storage"] = map[string]any{
			"want": storageName(wantStorage),
			"got":  storageName(info.Config.Storage),
		}
	}

	// History is stored as MaxMsgsPerSubject on the underlying stream.
	wantHistory := int64(ps.History) // already ≥ 1 after normalization
	if info.Config.MaxMsgsPerSubject != wantHistory {
		immutable["history"] = map[string]any{
			"want": wantHistory,
			"got":  info.Config.MaxMsgsPerSubject,
		}
	}

	// Replicas: 0 in config means "server default" which NATS normalizes to 1.
	// Treat 0 and 1 as equivalent on the desired side to avoid spurious drift
	// on every default-replica installation.
	effectiveReplicas := ps.Replicas
	if effectiveReplicas == 0 {
		effectiveReplicas = 1
	}
	if info.Config.Replicas != effectiveReplicas {
		immutable["replicas"] = map[string]any{
			"want": effectiveReplicas,
			"got":  info.Config.Replicas,
		}
	}

	// Description is mutable in place (Phase 2). Desired is always empty string
	// (we do not set a Description on partition-source buckets by default).
	if info.Config.Description != "" {
		mutable["description"] = map[string]any{
			"want": "",
			"got":  info.Config.Description,
		}
	}

	// MaxBytes is mutable in place (Phase 2). Desired is always 0 ("no limit").
	// NATS stores "no limit" as MaxBytes=-1 server-side; normalize -1→0 before
	// comparing to avoid spurious drift on default installs.
	liveMaxBytes := info.Config.MaxBytes
	if liveMaxBytes == -1 {
		liveMaxBytes = 0
	}
	if liveMaxBytes != 0 {
		mutable["maxBytes"] = map[string]any{
			"want": int64(0),
			"got":  info.Config.MaxBytes,
		}
	}

	// MaxValueSize is mutable in place (Phase 2).
	// info.Config.MaxMsgSize is the stream-level field corresponding to KV MaxValueSize.
	// NATS stores "no limit" as MaxMsgSize=-1 server-side; config uses 0 for the same
	// semantic. Normalize -1→0 before comparing to avoid spurious drift on default installs.
	liveMaxValueSize := info.Config.MaxMsgSize
	if liveMaxValueSize == -1 {
		liveMaxValueSize = 0
	}
	if liveMaxValueSize != ps.MaxValueSize {
		mutable["maxValueSize"] = map[string]any{
			"want": ps.MaxValueSize,
			"got":  info.Config.MaxMsgSize,
		}
	}

	// TTL is stored as MaxAge on the underlying stream. Mutable (Phase 2).
	wantTTL := ps.TTL // 0 means "no expiration"
	if info.Config.MaxAge != wantTTL {
		mutable["ttl"] = map[string]any{
			"want": wantTTL.String(),
			"got":  info.Config.MaxAge.String(),
		}
	}

	// Marker fields are mutable in place (Phase 2). Report any deviation from
	// the expected v1 marker values as drift-mutable, including managed version
	// and component mismatches.
	if marker.Managed != MarkerManagedValue {
		mutable["managed"] = map[string]any{
			"want": MarkerManagedValue,
			"got":  marker.Managed,
		}
	}
	if marker.Component != ComponentPartitionSource {
		mutable["component"] = map[string]any{
			"want": ComponentPartitionSource,
			"got":  marker.Component,
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
	if len(findings) == 0 {
		findings = append(findings, DriftFinding{
			Severity: SeverityInformational,
			Kind:     KindPartitionSource,
			Name:     ps.Bucket,
			Detail: map[string]any{
				"component": ComponentPartitionSource,
			},
		})
	}

	return findings
}
