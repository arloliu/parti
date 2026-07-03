package natsutil

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Sentinel errors returned by ProbeKVStreamPos. Callers treat ANY error
// as "cannot prove the bucket is idle" and run their full scan (fail
// open); the sentinels exist so tests can pin the classification.
var (
	// ErrNotJetStreamBucket reports that kv.Status returned something
	// other than a NATS-backed *jetstream.KeyValueBucketStatus (e.g. a
	// test fake), so no stream position is available.
	ErrNotJetStreamBucket = errors.New("kv status is not a JetStream-backed bucket status")
	// ErrNoStreamInfo reports a NATS-backed status that carries no
	// stream info (defensive; not observed in practice).
	ErrNoStreamInfo = errors.New("kv bucket status carries no stream info")
)

// KVStreamPos is a point-in-time position + contract snapshot of the
// stream backing a KV bucket, obtained from one read-only
// $JS.API.STREAM.INFO request (no consumer, no Raft proposal, no write).
//
// The position triple (Created, LastSeq, Msgs) proves bucket idleness:
// every KV mutation appends a sequence-consuming message (advancing
// LastSeq), and every message removal (stream purge, MaxAge expiry,
// per-message TTL) decrements Msgs without changing LastSeq. An
// unchanged triple within one stream generation (Created unchanged)
// therefore means the bucket's message set is byte-identical to what a
// prior observer saw.
type KVStreamPos struct {
	Created time.Time // stream generation (changes on delete+recreate)
	LastSeq uint64    // highest sequence ever assigned
	Msgs    uint64    // current message count (removals decrement this)

	// Contract-validation fields: config that would permit message
	// removals invisible to LastSeq. See UnsafeConfig.
	MaxAge                 time.Duration
	AllowMsgTTL            bool
	SubjectDeleteMarkerTTL time.Duration
}

// Same reports whether two probes prove an unchanged message set within
// one stream generation. It is equality on Created (time.Time.Equal),
// LastSeq, AND Msgs — never an ordering comparison: a regressed LastSeq
// (e.g. file-store truncation after an unclean shutdown) must unlatch a
// scan gate, not satisfy it. Config fields do not participate.
func (p KVStreamPos) Same(o KVStreamPos) bool {
	return p.Created.Equal(o.Created) && p.LastSeq == o.LastSeq && p.Msgs == o.Msgs
}

// UnsafeConfig reports whether the stream's live config permits
// invisible removals (MaxAge / per-message-TTL machinery). Gates must
// treat an unsafe config as "cannot latch": run the full pass and stay
// disabled until the config is clean again. Servers predating the
// message-TTL feature decode these fields as zero values, so the check
// passes there.
func (p KVStreamPos) UnsafeConfig() bool {
	return p.MaxAge != 0 || p.AllowMsgTTL || p.SubjectDeleteMarkerTTL != 0
}

// ProbeKVStreamPos fetches the bucket's stream position via kv.Status —
// one read-only STREAM.INFO request. It returns an error when the
// Status call fails (bucket deleted, timeout, no quorum), when the
// status is not backed by NATS JetStream (test fakes), or when the
// returned status carries no stream info. Callers MUST treat any error
// as "cannot prove idle" and run their full pass (fail open).
//
// Handle-ownership contract: kv MUST be a handle dedicated to the
// probing goroutine. kv.Status resolves through stream.Info, which
// mutates the handle's cached *stream state and races concurrent
// Get/Put/Watch/Keys issued on the same handle (the race class fixed
// for the manager's epoch monitor by a dedicated probe handle; see
// test/integration/manager/manager_epoch_monitor_concurrency_test.go).
// Open a separate js.KeyValue handle for probing — never pass a handle
// production paths also use.
func ProbeKVStreamPos(ctx context.Context, kv jetstream.KeyValue) (KVStreamPos, error) {
	status, err := kv.Status(ctx)
	if err != nil {
		return KVStreamPos{}, fmt.Errorf("kv status: %w", err)
	}
	bucket, ok := status.(*jetstream.KeyValueBucketStatus)
	if !ok {
		return KVStreamPos{}, ErrNotJetStreamBucket
	}
	info := bucket.StreamInfo()
	if info == nil {
		return KVStreamPos{}, ErrNoStreamInfo
	}

	return KVStreamPos{
		Created:                info.Created,
		LastSeq:                info.State.LastSeq,
		Msgs:                   info.State.Msgs,
		MaxAge:                 info.Config.MaxAge,
		AllowMsgTTL:            info.Config.AllowMsgTTL,
		SubjectDeleteMarkerTTL: info.Config.SubjectDeleteMarkerTTL,
	}, nil
}
