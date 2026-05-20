package provision

import (
	"reflect"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// fullKVConfig constructs a jetstream.KeyValueConfig with every field that
// nats.go v1.50.0 exposes set to a non-zero / non-nil value. This is the
// forward-compatibility anchor: if nats.go adds a new pointer-bearing field,
// reflect.DeepEqual in the preservation test will detect it before cloneKVConfig
// is updated to handle it.
func fullKVConfig() jetstream.KeyValueConfig {
	now := time.Now()
	tags := []string{"region:us-east", "tier:prod"}

	return jetstream.KeyValueConfig{
		Bucket:         "test-bucket",
		Description:    "test description",
		MaxValueSize:   4096,
		History:        3,
		TTL:            10 * time.Minute,
		MaxBytes:       1024 * 1024,
		Storage:        jetstream.FileStorage,
		Replicas:       2,
		Compression:    true,
		LimitMarkerTTL: 5 * time.Minute,
		Placement: &jetstream.Placement{
			Cluster: "us-east",
			Tags:    tags,
		},
		RePublish: &jetstream.RePublish{
			Source:      "src.>",
			Destination: "dst.>",
			HeadersOnly: true,
		},
		Mirror: &jetstream.StreamSource{
			Name:          "mirror-stream",
			OptStartSeq:   42,
			OptStartTime:  &now,
			FilterSubject: "foo.>",
			SubjectTransforms: []jetstream.SubjectTransformConfig{
				{Source: "foo.>", Destination: "bar.>"},
			},
			External: &jetstream.ExternalStream{
				APIPrefix:     "ext-api",
				DeliverPrefix: "ext-deliver",
			},
		},
		Sources: []*jetstream.StreamSource{
			{
				Name:          "source-stream",
				OptStartSeq:   100,
				OptStartTime:  &now,
				FilterSubject: "baz.>",
				SubjectTransforms: []jetstream.SubjectTransformConfig{
					{Source: "baz.>", Destination: "qux.>"},
				},
				External: &jetstream.ExternalStream{
					APIPrefix:     "src-api",
					DeliverPrefix: "src-deliver",
				},
			},
		},
		Metadata: map[string]string{
			"key1": "val1",
			"key2": "val2",
		},
	}
}

// TestCloneKVConfig_ForwardCompatPreservation is the forward-compatibility anchor
// test. It builds a KeyValueConfig with every field set to non-zero/non-nil values,
// clones it, and asserts reflect.DeepEqual. If nats.go adds a new pointer-bearing
// field, this test will fail until cloneKVConfig is updated to deep-clone it.
func TestCloneKVConfig_ForwardCompatPreservation(t *testing.T) {
	t.Parallel()

	src := fullKVConfig()
	clone := cloneKVConfig(src)

	// Assert byte-equality between src and clone.
	require.True(t, reflect.DeepEqual(src, clone),
		"clone must be deeply equal to src\nsrc:   %#v\nclone: %#v", src, clone)

	// Mutate every pointer and slice element on the clone; assert src is unchanged.
	clone.Placement.Cluster = "mutated-cluster"
	clone.Placement.Tags[0] = "mutated-tag"
	clone.RePublish.Source = "mutated-src.>"
	clone.Mirror.Name = "mutated-mirror"
	clone.Mirror.SubjectTransforms[0].Source = "mutated.>"
	clone.Mirror.External.APIPrefix = "mutated-api"
	clone.Sources[0].Name = "mutated-source"
	clone.Sources[0].SubjectTransforms[0].Destination = "mutated-dst.>"
	clone.Sources[0].External.DeliverPrefix = "mutated-deliver"
	clone.Metadata["key1"] = "mutated-val"

	// src must be unchanged.
	require.Equal(t, "us-east", src.Placement.Cluster, "src.Placement.Cluster must not be mutated")
	require.Equal(t, "region:us-east", src.Placement.Tags[0], "src.Placement.Tags must not be mutated")
	require.Equal(t, "src.>", src.RePublish.Source, "src.RePublish.Source must not be mutated")
	require.Equal(t, "mirror-stream", src.Mirror.Name, "src.Mirror.Name must not be mutated")
	require.Equal(t, "foo.>", src.Mirror.SubjectTransforms[0].Source, "src.Mirror.SubjectTransforms must not be mutated")
	require.Equal(t, "ext-api", src.Mirror.External.APIPrefix, "src.Mirror.External must not be mutated")
	require.Equal(t, "source-stream", src.Sources[0].Name, "src.Sources[0].Name must not be mutated")
	require.Equal(t, "qux.>", src.Sources[0].SubjectTransforms[0].Destination, "src.Sources[0].SubjectTransforms must not be mutated")
	require.Equal(t, "src-deliver", src.Sources[0].External.DeliverPrefix, "src.Sources[0].External must not be mutated")
	require.Equal(t, "val1", src.Metadata["key1"], "src.Metadata must not be mutated")
}

// TestCloneKVConfig_NilPointers verifies that cloneKVConfig handles a config
// with all pointer/slice fields nil without panicking and returns all nil.
func TestCloneKVConfig_NilPointers(t *testing.T) {
	t.Parallel()

	src := jetstream.KeyValueConfig{
		Bucket:  "simple",
		History: 1,
		Storage: jetstream.FileStorage,
	}

	require.NotPanics(t, func() {
		clone := cloneKVConfig(src)
		require.Nil(t, clone.Placement)
		require.Nil(t, clone.RePublish)
		require.Nil(t, clone.Mirror)
		require.Nil(t, clone.Sources)
		require.Nil(t, clone.Metadata)
	})
}

// TestCloneKVConfig_MetadataIndependence verifies that mutating the clone's
// Metadata map does not affect the source.
func TestCloneKVConfig_MetadataIndependence(t *testing.T) {
	t.Parallel()

	src := jetstream.KeyValueConfig{
		Bucket:   "test",
		Metadata: map[string]string{"k": "v"},
	}
	clone := cloneKVConfig(src)
	clone.Metadata["k"] = "mutated"
	require.Equal(t, "v", src.Metadata["k"], "src.Metadata must be independent of clone.Metadata")
}

// TestCloneStreamSource_NilSrc verifies that cloneStreamSource(nil) returns nil.
func TestCloneStreamSource_NilSrc(t *testing.T) {
	t.Parallel()
	require.Nil(t, cloneStreamSource(nil))
}
