package natsutil

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func basePos() KVStreamPos {
	return KVStreamPos{
		Created: time.Unix(1000, 0),
		LastSeq: 42,
		Msgs:    7,
	}
}

func TestKVStreamPos_Same(t *testing.T) {
	t.Parallel()

	p := basePos()
	require.True(t, p.Same(basePos()))

	seq := basePos()
	seq.LastSeq++
	require.False(t, p.Same(seq), "LastSeq change must break equality")

	msgs := basePos()
	msgs.Msgs--
	require.False(t, p.Same(msgs), "Msgs change must break equality")

	created := basePos()
	created.Created = created.Created.Add(time.Second)
	require.False(t, p.Same(created), "Created change must break equality")

	// Same must be pure equality, never an ordering comparison: a
	// REGRESSED LastSeq (file-store truncation) must also mismatch.
	regressed := basePos()
	regressed.LastSeq--
	require.False(t, p.Same(regressed))

	// Config fields do not participate in position equality (they are
	// validated separately via UnsafeConfig).
	cfg := basePos()
	cfg.MaxAge = time.Minute
	require.True(t, p.Same(cfg))

	// Created comparison uses time.Time.Equal semantics, so a
	// wall-clock-identical instant in a different location still matches.
	loc := basePos()
	loc.Created = loc.Created.In(time.FixedZone("X", 3600))
	require.True(t, p.Same(loc))
}

func TestKVStreamPos_UnsafeConfig(t *testing.T) {
	t.Parallel()

	require.False(t, basePos().UnsafeConfig())

	maxAge := basePos()
	maxAge.MaxAge = time.Hour
	require.True(t, maxAge.UnsafeConfig())

	ttl := basePos()
	ttl.AllowMsgTTL = true
	require.True(t, ttl.UnsafeConfig())

	marker := basePos()
	marker.SubjectDeleteMarkerTTL = time.Minute
	require.True(t, marker.UnsafeConfig())
}

// fakeStatusKV lets us drive the two statically-testable failure paths of
// ProbeKVStreamPos. The success path requires a *jetstream.
// KeyValueBucketStatus, which has unexported fields and cannot be
// fabricated outside nats.go — it is exercised against real NATS by the
// integration suite (handoff scan-gate tests).
type fakeStatusKV struct {
	jetstream.KeyValue
	status jetstream.KeyValueStatus
	err    error
}

func (f *fakeStatusKV) Status(ctx context.Context) (jetstream.KeyValueStatus, error) {
	return f.status, f.err
}

// fakeStatus is a non-NATS KeyValueStatus (what a test double would
// return); the probe must reject it with ErrNotJetStreamBucket.
type fakeStatus struct{ jetstream.KeyValueStatus }

func TestProbeKVStreamPos_StatusError(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	_, err := ProbeKVStreamPos(context.Background(), &fakeStatusKV{err: boom})
	require.Error(t, err)
	require.ErrorIs(t, err, boom)
}

func TestProbeKVStreamPos_NotJetStreamBucket(t *testing.T) {
	t.Parallel()

	_, err := ProbeKVStreamPos(context.Background(), &fakeStatusKV{status: &fakeStatus{}})
	require.ErrorIs(t, err, ErrNotJetStreamBucket)
}
