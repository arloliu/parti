package provision

import (
	"maps"
	"slices"

	"github.com/nats-io/nats.go/jetstream"
)

// cloneKVConfig returns a deep clone of src. Pointer fields (Placement,
// RePublish, Mirror), the Sources slice and its pointees, and Metadata
// are all reallocated so the result is safe to mutate without affecting
// src — required because nats.go's prepareKeyValueConfig may mutate
// entries in cfg.Sources while building the underlying stream config.
func cloneKVConfig(src jetstream.KeyValueConfig) jetstream.KeyValueConfig {
	dst := src // shallow copy of scalar fields

	if src.Placement != nil {
		p := *src.Placement
		p.Tags = slices.Clone(src.Placement.Tags)
		dst.Placement = &p
	}

	if src.RePublish != nil {
		rp := *src.RePublish
		dst.RePublish = &rp
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

	dst.Metadata = maps.Clone(src.Metadata)

	return dst
}

// cloneStreamSource deep-clones a *StreamSource, including its pointer and
// slice fields (OptStartTime, External, SubjectTransforms).
func cloneStreamSource(src *jetstream.StreamSource) *jetstream.StreamSource {
	if src == nil {
		return nil
	}
	dst := *src // shallow copy

	if src.OptStartTime != nil {
		t := *src.OptStartTime
		dst.OptStartTime = &t
	}

	if src.External != nil {
		e := *src.External
		dst.External = &e
	}

	dst.SubjectTransforms = slices.Clone(src.SubjectTransforms)

	return &dst
}
