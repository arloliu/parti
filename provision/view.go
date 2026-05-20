package provision

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// kvStreamPrefix is the prefix JetStream stamps on every KV-bucket stream.
const kvStreamPrefix = "KV_"

// View returns a Snapshot of every Parti-marked NATS resource visible to js,
// filtered by scope.
//
// The implementation walks $JS.API.STREAM.LIST (via jetstream.ListStreams)
// once, filters for KV streams (Name starts with "KV_"), and reads
// stream.Config.Metadata to decide which buckets are Parti-managed. Unmarked
// buckets are excluded from default View output.
//
// Cancellation: ctx cancellation returns a zero-value Snapshot plus ctx.Err().
func View(ctx context.Context, js jetstream.JetStream, scope Scope) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}

	snap := Snapshot{
		APIVersion:       APIVersionProvisionV1,
		Kind:             KindSnapshot,
		ObservedAt:       time.Now().UTC(),
		ControlPlane:     []KVBucketState{},
		PartitionSource:  []KVBucketState{},
		DynamicConsumers: []ConsumerState{},
	}

	// Short-circuit: nothing to do if no KV kinds are requested.
	// DynamicConsumers is not yet populated by View.
	if !scope.ControlPlane && !scope.PartitionSource {
		return snap, nil
	}

	lister := js.ListStreams(ctx)
	for info := range lister.Info() {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		if info == nil {
			continue
		}
		state, ok := kvBucketStateFromStreamInfo(info)
		if !ok {
			continue
		}
		// Default View filter: skip resources without a Parti marker.
		if state.Managed == "" {
			continue
		}
		// Instance filter: when set, drop entries with a mismatching
		// (or absent) instance value.
		if scope.Instance != "" && state.Instance != scope.Instance {
			continue
		}

		// Categorize by component value. Known control-plane components
		// and unknown forward-compat values both surface under
		// ControlPlane (operators don't lose visibility of new
		// components). PartitionSource has its own slot. Anything else
		// (dynamic-consumer, future categories) is silently dropped.
		switch state.Component {
		case ComponentPartitionSource:
			if scope.PartitionSource {
				snap.PartitionSource = append(snap.PartitionSource, state)
			}
		case ComponentControlPlaneID,
			ComponentControlPlaneElection,
			ComponentControlPlaneHeartbeat,
			ComponentControlPlaneAssignment,
			ComponentControlPlaneHandoff,
			ComponentUnknown:
			if scope.ControlPlane {
				snap.ControlPlane = append(snap.ControlPlane, state)
			}
		}
	}
	if err := lister.Err(); err != nil {
		// Distinguish ctx cancellation (handled at top) from server-side
		// errors. The latter we propagate to the caller.
		if ctx.Err() != nil {
			return Snapshot{}, ctx.Err()
		}
		return Snapshot{}, err
	}

	sortBucketStates(snap.ControlPlane)
	sortBucketStates(snap.PartitionSource)

	return snap, nil
}

// kvBucketStateFromStreamInfo decodes a *jetstream.StreamInfo into a
// KVBucketState if the underlying stream is a KV bucket (its Name starts with
// "KV_"). Returns (state, false) for non-KV streams.
func kvBucketStateFromStreamInfo(info *jetstream.StreamInfo) (KVBucketState, bool) {
	name := info.Config.Name
	if !strings.HasPrefix(name, kvStreamPrefix) {
		return KVBucketState{}, false
	}
	bucket := strings.TrimPrefix(name, kvStreamPrefix)
	marker := ParseMarker(info.Config.Metadata)

	// Normalize MaxMsgSize: NATS stores "no limit" as -1 at the stream
	// level, but provision convention (and Plan's drift classifier) use 0
	// for "no limit". Surfacing -1 here would create an asymmetry between
	// what View reports and what Plan classifies as drift-free.
	maxValueSize := info.Config.MaxMsgSize
	if maxValueSize == -1 {
		maxValueSize = 0
	}

	state := KVBucketState{
		Bucket:       bucket,
		Managed:      marker.Managed,
		Component:    marker.ClassifyComponent(),
		Instance:     marker.Instance,
		History:      uint8MaxClamp(info.Config.MaxMsgsPerSubject),
		Storage:      storageName(info.Config.Storage),
		TTL:          info.Config.MaxAge,
		Replicas:     info.Config.Replicas,
		MaxBytes:     info.Config.MaxBytes,
		MaxValueSize: maxValueSize,
	}

	return state, true
}

// uint8MaxClamp converts a JetStream history-equivalent int64 to a uint8.
// nats.go stores KV "History" via MaxMsgsPerSubject (always 1..64 for KV);
// values outside that window indicate the stream was not created by nats.go
// as a KV bucket, but we still want a sensible report.
func uint8MaxClamp(v int64) uint8 {
	if v <= 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

func storageName(s jetstream.StorageType) string {
	switch s {
	case jetstream.FileStorage:
		return "file"
	case jetstream.MemoryStorage:
		return "memory"
	default:
		return ""
	}
}

func sortBucketStates(s []KVBucketState) {
	slices.SortStableFunc(s, func(a, b KVBucketState) int {
		if c := stringsCmp(a.Component, b.Component); c != 0 {
			return c
		}

		return stringsCmp(a.Bucket, b.Bucket)
	})
}
