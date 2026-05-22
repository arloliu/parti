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
// once, filters for KV streams (Name starts with "KV_") and application
// streams (all other names), and reads stream.Config.Metadata to decide which
// resources are Parti-managed. Unmarked resources are excluded from default
// View output.
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
		Streams:          []StreamState{},
	}

	// Short-circuit: nothing to do if no resource kinds are requested.
	// DynamicConsumers is not yet populated by View.
	if !scope.ControlPlane && !scope.PartitionSource && !scope.Streams {
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
		appendStreamInfo(&snap, scope, info)
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
	sortStreamStates(snap.Streams)

	return snap, nil
}

// appendStreamInfo classifies a single *jetstream.StreamInfo and appends it to
// the appropriate Snapshot slot if scope and filters permit.
func appendStreamInfo(snap *Snapshot, scope Scope, info *jetstream.StreamInfo) {
	if strings.HasPrefix(info.Config.Name, kvStreamPrefix) {
		// KV bucket path: existing classifier, unchanged.
		state, ok := kvBucketStateFromStreamInfo(info)
		if !ok || state.Managed == "" {
			return
		}
		if scope.Instance != "" && state.Instance != scope.Instance {
			return
		}
		// Categorize by component value. Known control-plane components and
		// unknown forward-compat values both surface under ControlPlane
		// (operators don't lose visibility of new components). PartitionSource
		// has its own slot. Anything else (dynamic-consumer, future categories)
		// is silently dropped.
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

		return
	}

	// Application stream path.
	state, ok := streamStateFromStreamInfo(info)
	if !ok {
		return
	}
	if scope.Instance != "" && state.Instance != scope.Instance {
		return
	}
	if scope.Streams {
		snap.Streams = append(snap.Streams, state)
	}
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

// streamStateFromStreamInfo decodes a *jetstream.StreamInfo into a StreamState
// for a candidate application (non-KV) stream. It returns (_, false) if the
// stream carries no Parti marker or if the marker component is not
// ComponentStream — those resources are not application streams and are dropped
// by the default View filter.
func streamStateFromStreamInfo(info *jetstream.StreamInfo) (StreamState, bool) {
	marker := ParseMarker(info.Config.Metadata)
	// Default View filter: unmarked streams are excluded.
	if !marker.IsManaged() {
		return StreamState{}, false
	}
	// Non-stream-component markers on a non-KV stream are not application
	// streams — drop them.
	if marker.ClassifyComponent() != ComponentStream {
		return StreamState{}, false
	}

	// Normalize live MaxBytes and MaxMsgs: NATS stores "no limit" as -1 at
	// the stream level, but provision convention uses 0 for "no limit".
	maxBytes := info.Config.MaxBytes
	if maxBytes == -1 {
		maxBytes = 0
	}
	maxMsgs := info.Config.MaxMsgs
	if maxMsgs == -1 {
		maxMsgs = 0
	}

	state := StreamState{
		Stream:    info.Config.Name,
		Managed:   marker.Managed,
		Component: marker.ClassifyComponent(),
		Instance:  marker.Instance,
		Subjects:  info.Config.Subjects,
		Retention: retentionName(info.Config.Retention),
		Storage:   storageName(info.Config.Storage),
		Discard:   discardName(info.Config.Discard),
		Replicas:  info.Config.Replicas,
		MaxAge:    info.Config.MaxAge,
		MaxBytes:  maxBytes,
		MaxMsgs:   maxMsgs,
	}

	return state, true
}

func retentionName(r jetstream.RetentionPolicy) string {
	switch r {
	case jetstream.LimitsPolicy:
		return "limits"
	case jetstream.WorkQueuePolicy:
		return "workqueue"
	case jetstream.InterestPolicy:
		return "interest"
	default:
		return ""
	}
}

func discardName(d jetstream.DiscardPolicy) string {
	switch d {
	case jetstream.DiscardOld:
		return "old"
	case jetstream.DiscardNew:
		return "new"
	default:
		return ""
	}
}

func sortStreamStates(s []StreamState) {
	slices.SortStableFunc(s, func(a, b StreamState) int {
		return stringsCmp(a.Stream, b.Stream)
	})
}
