package provision

// Same-package unit tests for streamStateFromStreamInfo. These tests construct
// synthetic *jetstream.StreamInfo values so they run without a live NATS server
// and can exercise the -1→0 normalization, unmarked-drop, and wrong-component
// drop paths.

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// makeStreamInfo constructs a *jetstream.StreamInfo with the given name and
// config mutators applied.
func makeStreamInfo(name string, fns ...func(*jetstream.StreamConfig)) *jetstream.StreamInfo {
	cfg := jetstream.StreamConfig{Name: name}
	for _, fn := range fns {
		fn(&cfg)
	}
	return &jetstream.StreamInfo{Config: cfg}
}

func TestStreamStateFromStreamInfo_Unmarked_ReturnsFalse(t *testing.T) {
	t.Parallel()
	info := makeStreamInfo("app-events")
	_, ok := streamStateFromStreamInfo(info)
	require.False(t, ok)
}

func TestStreamStateFromStreamInfo_WrongComponent_ReturnsFalse(t *testing.T) {
	t.Parallel()
	// A non-KV stream carrying a control-plane marker is not an application stream.
	info := makeStreamInfo("KV-lookalike", func(cfg *jetstream.StreamConfig) {
		cfg.Metadata = BuildMarker(ComponentControlPlaneElection, "prod")
	})
	_, ok := streamStateFromStreamInfo(info)
	require.False(t, ok)
}

func TestStreamStateFromStreamInfo_PartitionSourceComponent_ReturnsFalse(t *testing.T) {
	t.Parallel()
	info := makeStreamInfo("some-stream", func(cfg *jetstream.StreamConfig) {
		cfg.Metadata = BuildMarker(ComponentPartitionSource, "prod")
	})
	_, ok := streamStateFromStreamInfo(info)
	require.False(t, ok)
}

func TestStreamStateFromStreamInfo_MarkedStream_ReturnsState(t *testing.T) {
	t.Parallel()
	subjects := []string{"events.>", "orders.*"}
	info := makeStreamInfo("app-events", func(cfg *jetstream.StreamConfig) {
		cfg.Metadata = BuildMarker(ComponentStream, "prod")
		cfg.Subjects = subjects
		cfg.Retention = jetstream.LimitsPolicy
		cfg.Storage = jetstream.FileStorage
		cfg.Discard = jetstream.DiscardOld
		cfg.Replicas = 3
		cfg.MaxAge = 24 * time.Hour
		cfg.MaxBytes = 1024
		cfg.MaxMsgs = 500
	})
	state, ok := streamStateFromStreamInfo(info)
	require.True(t, ok)
	require.Equal(t, "app-events", state.Stream)
	require.Equal(t, ComponentStream, state.Component)
	require.Equal(t, "prod", state.Instance)
	require.Equal(t, "v1", state.Managed)
	require.Equal(t, subjects, state.Subjects)
	require.Equal(t, "limits", state.Retention)
	require.Equal(t, "file", state.Storage)
	require.Equal(t, "old", state.Discard)
	require.Equal(t, 3, state.Replicas)
	require.Equal(t, 24*time.Hour, state.MaxAge)
	require.Equal(t, int64(1024), state.MaxBytes)
	require.Equal(t, int64(500), state.MaxMsgs)
}

func TestStreamStateFromStreamInfo_MaxBytesMinusOne_NormalizesToZero(t *testing.T) {
	t.Parallel()
	info := makeStreamInfo("app-events", func(cfg *jetstream.StreamConfig) {
		cfg.Metadata = BuildMarker(ComponentStream, "")
		cfg.Subjects = []string{"events.>"}
		cfg.MaxBytes = -1
		cfg.MaxMsgs = 100
	})
	state, ok := streamStateFromStreamInfo(info)
	require.True(t, ok)
	require.Equal(t, int64(0), state.MaxBytes)
	require.Equal(t, int64(100), state.MaxMsgs)
}

func TestStreamStateFromStreamInfo_MaxMsgsMinusOne_NormalizesToZero(t *testing.T) {
	t.Parallel()
	info := makeStreamInfo("app-events", func(cfg *jetstream.StreamConfig) {
		cfg.Metadata = BuildMarker(ComponentStream, "")
		cfg.Subjects = []string{"events.>"}
		cfg.MaxBytes = 512
		cfg.MaxMsgs = -1
	})
	state, ok := streamStateFromStreamInfo(info)
	require.True(t, ok)
	require.Equal(t, int64(512), state.MaxBytes)
	require.Equal(t, int64(0), state.MaxMsgs)
}

func TestStreamStateFromStreamInfo_BothMaxFieldsMinusOne_NormalizesToZero(t *testing.T) {
	t.Parallel()
	info := makeStreamInfo("app-events", func(cfg *jetstream.StreamConfig) {
		cfg.Metadata = BuildMarker(ComponentStream, "")
		cfg.Subjects = []string{"events.>"}
		cfg.MaxBytes = -1
		cfg.MaxMsgs = -1
	})
	state, ok := streamStateFromStreamInfo(info)
	require.True(t, ok)
	require.Equal(t, int64(0), state.MaxBytes)
	require.Equal(t, int64(0), state.MaxMsgs)
}

func TestStreamStateFromStreamInfo_MaxAgeZero_NotNormalized(t *testing.T) {
	t.Parallel()
	// MaxAge 0 means unlimited; it should be preserved as 0 (not treated as -1).
	info := makeStreamInfo("app-events", func(cfg *jetstream.StreamConfig) {
		cfg.Metadata = BuildMarker(ComponentStream, "")
		cfg.Subjects = []string{"events.>"}
		cfg.MaxAge = 0
	})
	state, ok := streamStateFromStreamInfo(info)
	require.True(t, ok)
	require.Equal(t, time.Duration(0), state.MaxAge)
}

func TestStreamStateFromStreamInfo_WorkQueueRetention(t *testing.T) {
	t.Parallel()
	info := makeStreamInfo("work-queue", func(cfg *jetstream.StreamConfig) {
		cfg.Metadata = BuildMarker(ComponentStream, "")
		cfg.Subjects = []string{"work.>"}
		cfg.Retention = jetstream.WorkQueuePolicy
	})
	state, ok := streamStateFromStreamInfo(info)
	require.True(t, ok)
	require.Equal(t, "workqueue", state.Retention)
}

func TestStreamStateFromStreamInfo_InterestRetention(t *testing.T) {
	t.Parallel()
	info := makeStreamInfo("interest-stream", func(cfg *jetstream.StreamConfig) {
		cfg.Metadata = BuildMarker(ComponentStream, "")
		cfg.Subjects = []string{"events.>"}
		cfg.Retention = jetstream.InterestPolicy
	})
	state, ok := streamStateFromStreamInfo(info)
	require.True(t, ok)
	require.Equal(t, "interest", state.Retention)
}

func TestStreamStateFromStreamInfo_DiscardNew(t *testing.T) {
	t.Parallel()
	info := makeStreamInfo("app-events", func(cfg *jetstream.StreamConfig) {
		cfg.Metadata = BuildMarker(ComponentStream, "")
		cfg.Subjects = []string{"events.>"}
		cfg.Discard = jetstream.DiscardNew
	})
	state, ok := streamStateFromStreamInfo(info)
	require.True(t, ok)
	require.Equal(t, "new", state.Discard)
}

func TestStreamStateFromStreamInfo_MemoryStorage(t *testing.T) {
	t.Parallel()
	info := makeStreamInfo("fast-stream", func(cfg *jetstream.StreamConfig) {
		cfg.Metadata = BuildMarker(ComponentStream, "")
		cfg.Subjects = []string{"fast.>"}
		cfg.Storage = jetstream.MemoryStorage
	})
	state, ok := streamStateFromStreamInfo(info)
	require.True(t, ok)
	require.Equal(t, "memory", state.Storage)
}

func TestStreamStateFromStreamInfo_NoInstance_EmptyInstance(t *testing.T) {
	t.Parallel()
	info := makeStreamInfo("global-stream", func(cfg *jetstream.StreamConfig) {
		cfg.Metadata = BuildMarker(ComponentStream, "")
		cfg.Subjects = []string{"global.>"}
	})
	state, ok := streamStateFromStreamInfo(info)
	require.True(t, ok)
	require.Equal(t, "", state.Instance)
}
