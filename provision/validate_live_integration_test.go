package provision_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/kvbuckets"
	"github.com/arloliu/parti/v2/provision"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// streamRecordingJS wraps jetstream.JetStream and records every stream name
// passed to Stream(). Used to assert that ValidateLive probes the resolved
// (post-default) stream names, not raw empty names.
type streamRecordingJS struct {
	jetstream.JetStream
	mu      sync.Mutex
	streams []string
}

func newStreamRecordingJS(js jetstream.JetStream) *streamRecordingJS {
	return &streamRecordingJS{JetStream: js}
}

func (r *streamRecordingJS) Stream(ctx context.Context, stream string) (jetstream.Stream, error) {
	r.mu.Lock()
	r.streams = append(r.streams, stream)
	r.mu.Unlock()
	return r.JetStream.Stream(ctx, stream)
}

func (r *streamRecordingJS) recordedStreams() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]string, len(r.streams))
	copy(cp, r.streams)
	return cp
}

// validateLiveCfg builds a Config with all five control-plane buckets
// enabled and a partition-source target pointing at a Parti-style bucket.
func validateLiveCfg() provision.Config {
	return provision.Config{
		APIVersion: provision.APIVersionV1,
		Instance:   "prod",
		ControlPlane: &provision.ControlPlaneConfig{
			WorkerIDTTL:           75 * time.Second,
			ElectionTimeout:       10 * time.Second,
			HeartbeatTTL:          15 * time.Second,
			AssignmentTTL:         0,
			HandoffTTL:            2 * time.Minute,
			EnableTwoPhaseHandoff: true,
		},
		PartitionSource: &provision.PartitionSourceConfig{
			Bucket: "parti-partitions",
			Key:    "partitions/v1",
		},
	}
}

func TestValidateLive_EmptyServer_AllBucketsMissing_OK(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rep, err := provision.ValidateLive(ctx, js, validateLiveCfg())
	require.NoError(t, err)
	require.Equal(t, provision.APIVersionProvisionV1, rep.APIVersion)
	require.Equal(t, provision.KindReport, rep.Kind)
	require.Empty(t, rep.Errors)
}

func TestValidateLive_CancelledContext_ReturnsCtxErr(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, err := provision.ValidateLive(ctx, js, validateLiveCfg())
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, rep)
}

func TestValidateLive_PartitionSourceKey_AllowDirectTrue_KeyMissing_OK(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	cfg := validateLiveCfg()
	// Pre-create only the partition-source bucket. nats.go's default
	// CreateKeyValue sets AllowDirect=true (verified below).
	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:   cfg.PartitionSource.Bucket,
		History:  1,
		Storage:  jetstream.FileStorage,
		Metadata: provision.BuildMarker(provision.ComponentPartitionSource, "prod"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Sanity: confirm AllowDirect=true on the underlying stream.
	stream, err := js.Stream(ctx, "KV_"+cfg.PartitionSource.Bucket)
	require.NoError(t, err)
	info, err := stream.Info(ctx)
	require.NoError(t, err)
	require.True(t, info.Config.AllowDirect, "default CreateKeyValue should set AllowDirect=true")

	rep, err := provision.ValidateLive(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)
}

func TestValidateLive_PartitionSourceKey_AllowDirectFalse_KeyMissing_OK(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	cfg := validateLiveCfg()

	// Create the partition-source bucket via raw CreateStream with
	// AllowDirect=false. We mirror the KV stream shape (KV_ prefix,
	// MaxMsgsPerSubject=1, KV_-style subject filter, deny-delete /
	// deny-purge defaults). The shape need not be exact — only enough
	// that GetLastMsgForSubject can be invoked.
	bucket := cfg.PartitionSource.Bucket
	streamCfg := jetstream.StreamConfig{
		Name:              "KV_" + bucket,
		Subjects:          []string{"$KV." + bucket + ".>"},
		Storage:           jetstream.FileStorage,
		MaxMsgsPerSubject: 1,
		AllowDirect:       false,
		Discard:           jetstream.DiscardNew,
		DenyDelete:        true,
		DenyPurge:         false,
		AllowRollup:       true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := js.CreateStream(ctx, streamCfg)
	require.NoError(t, err)

	rep, err := provision.ValidateLive(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)
}

func TestValidateLive_PartitionSourceKey_AllowDirectFlip_RetrySucceeds(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	cfg := validateLiveCfg()
	bucket := cfg.PartitionSource.Bucket

	// Start with AllowDirect=true (the nats.go default).
	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:   bucket,
		History:  1,
		Storage:  jetstream.FileStorage,
		Metadata: provision.BuildMarker(provision.ComponentPartitionSource, "prod"),
	})

	// Flip AllowDirect=false via UpdateStream. The cached stream handle
	// the ValidateLive opens internally will see the post-flip info on
	// its first stream.Info() call, so this test mainly verifies that
	// ValidateLive does the right thing across an AllowDirect transition
	// — the retry branch is exercised when a permission failure is also
	// returned, but the post-flip key probe should still succeed (key
	// absent) when permissions allow both direct and non-direct paths.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := js.Stream(ctx, "KV_"+bucket)
	require.NoError(t, err)
	info, err := stream.Info(ctx)
	require.NoError(t, err)
	updatedCfg := info.Config
	updatedCfg.AllowDirect = false
	_, err = js.UpdateStream(ctx, updatedCfg)
	require.NoError(t, err)

	rep, err := provision.ValidateLive(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)
}

func TestValidateLive_PartitionSourceMissingBucket_OK(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := validateLiveCfg()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Partition-source bucket is not pre-created — ValidateLive must
	// short-circuit the key probe (the bucket will be created by W3 /
	// Phase 3).
	rep, err := provision.ValidateLive(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)
}

func TestValidateLive_NoPartitionSource_SkipsKeyProbe(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := validateLiveCfg()
	cfg.PartitionSource = nil

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rep, err := provision.ValidateLive(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)
}

func TestValidateLive_PartitionSourceKey_PresentValue_OK(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := validateLiveCfg()
	bucket := cfg.PartitionSource.Bucket

	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:   bucket,
		History:  1,
		Storage:  jetstream.FileStorage,
		Metadata: provision.BuildMarker(provision.ComponentPartitionSource, "prod"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Write a value to the key so the probe sees data, not ErrMsgNotFound.
	kv, err := js.KeyValue(ctx, bucket)
	require.NoError(t, err)
	_, err = kv.Put(ctx, cfg.PartitionSource.Key, []byte(`{"v":1}`))
	require.NoError(t, err)

	rep, err := provision.ValidateLive(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)
}

func TestValidateLive_StreamInfoUsesSharedBuilderShape(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := validateLiveCfg()

	// Pre-create one of the control-plane buckets so stream.Info actually
	// runs (rather than short-circuiting on ErrStreamNotFound).
	live := kvbuckets.BuildKeyValueConfig("parti-election", 10*time.Second, jetstream.MemoryStorage)
	live.Metadata = provision.BuildMarker(provision.ComponentControlPlaneElection, "prod")
	createKV(t, js, live)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rep, err := provision.ValidateLive(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)
}

// TestValidateLive_DefaultBuckets_ProbesResolvedNames verifies the P0 fix:
// when the caller omits control-plane bucket names, ValidateLive must
// normalize cfg internally and probe the default names, not "KV_" (empty
// suffix). This test would fail on the pre-fix code that used raw cfg.
func TestValidateLive_DefaultBuckets_ProbesResolvedNames(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	// Config with NO explicit bucket names (the common operator case).
	// After normalization the control-plane defaults must be applied.
	cfg := provision.Config{
		APIVersion: provision.APIVersionV1,
		Instance:   "prod",
		ControlPlane: &provision.ControlPlaneConfig{
			WorkerIDTTL:     75 * time.Second,
			ElectionTimeout: 10 * time.Second,
			HeartbeatTTL:    15 * time.Second,
		},
	}

	rec := newStreamRecordingJS(js)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rep, err := provision.ValidateLive(ctx, rec, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)

	// Verify that the probed stream names are the resolved default names —
	// NOT "KV_" (which would indicate that normalization was skipped).
	probed := rec.recordedStreams()
	slices.Sort(probed)

	// The 4 control-plane buckets (handoff disabled → 4 not 5).
	wantStreams := []string{
		"KV_parti-assignment",
		"KV_parti-election",
		"KV_parti-heartbeat",
		"KV_parti-stableid",
	}

	require.Equal(t, wantStreams, probed,
		"ValidateLive must probe resolved default bucket names, not empty ones")

	// Additionally verify that "KV_" (empty suffix) is never probed.
	for _, s := range probed {
		require.NotEqual(t, "KV_", s,
			"ValidateLive must not probe KV_ (empty suffix); normalization missing")
	}
}

// TestValidateLive_Stream_ProbesRawName pins the application-stream probe to
// the RAW stream name — application streams have no "KV_" prefix. A regression
// that prefixed the name would record "KV_app-events" here. A black-box
// success/failure assertion cannot catch a wrong prefix (probeStreamInfo
// treats a not-found stream as OK), so only the recorded name pins it.
func TestValidateLive_Stream_ProbesRawName(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	createStream(t, js, jetstream.StreamConfig{
		Name:     "app-events",
		Subjects: []string{"app.events.>"},
		Metadata: provision.BuildMarker(provision.ComponentStream, "prod"),
	})

	cfg := provision.Config{
		APIVersion: provision.APIVersionV1,
		Instance:   "prod",
		Streams: []provision.StreamCfg{
			{Name: "app-events", Subjects: []string{"app.events.>"}},
		},
	}

	rec := newStreamRecordingJS(js)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rep, err := provision.ValidateLive(ctx, rec, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)

	probed := rec.recordedStreams()
	require.Contains(t, probed, "app-events",
		"ValidateLive must probe the raw application-stream name")
	require.NotContains(t, probed, "KV_app-events",
		"ValidateLive must not prefix an application-stream name with KV_")
}
