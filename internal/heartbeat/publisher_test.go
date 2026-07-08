package heartbeat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
)

var errSlowKVNotImplemented = errors.New("not implemented by slowPutKV stub")

func requireHeartbeatEntry(t *testing.T, kv jetstream.KeyValue, key string, wait time.Duration) jetstream.KeyValueEntry {
	t.Helper()

	var entry jetstream.KeyValueEntry
	require.Eventually(t, func() bool {
		var err error
		entry, err = kv.Get(t.Context(), key)
		return err == nil && entry != nil && len(entry.Value()) > 0
	}, wait, 25*time.Millisecond, "heartbeat %s should be published", key)

	return entry
}

// TestPublisher_SetOnError_Concurrent exercises the atomic.Pointer hot path:
// the publish goroutine reads onError while SetOnError swaps it from another
// goroutine. Run with -race; no assertion failure here is the pass condition.
func TestPublisher_SetOnError_Concurrent(t *testing.T) {
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "test-hb-onerror-race")

	publisher := New(kv, "worker-hb", "worker-1", 20*time.Millisecond, nil, nil)
	// Install, swap, and clear the callback while the publish loop is
	// running against a valid bucket (so publishes succeed, onError is
	// not invoked, but the hot path still Loads on each tick).
	publisher.SetOnError(func(error) {})
	require.NoError(t, publisher.Start(ctx))
	t.Cleanup(func() { _ = publisher.Stop() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 200 {
			if i%3 == 0 {
				publisher.SetOnError(nil)
			} else {
				publisher.SetOnError(func(error) {})
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SetOnError swap loop did not complete in 5s")
	}
}

func TestPublisher_SetWorkerID(t *testing.T) {
	t.Run("sets worker ID successfully", func(t *testing.T) {
		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-hb-set-id")

		publisher := New(kv, "worker-hb", "worker-1", 2*time.Second, nil, nil)

		require.Equal(t, "worker-1", publisher.WorkerID())
	})
}

func TestPublisher_Start(t *testing.T) {
	t.Run("starts successfully and publishes heartbeat", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-hb-start-1")

		publisher := New(kv, "worker-hb", "worker-1", 100*time.Millisecond, nil, nil)

		err := publisher.Start(ctx)
		require.NoError(t, err)
		require.True(t, publisher.IsStarted())

		// Verify heartbeat was published
		entry, err := kv.Get(ctx, "worker-hb.worker-1")
		require.NoError(t, err)
		require.NotNil(t, entry)

		// Cleanup
		err = publisher.Stop()
		require.NoError(t, err)
	})

	t.Run("returns error if worker ID not set", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-hb-start-2")

		publisher := New(kv, "worker-hb", "", 2*time.Second, nil, nil)

		err := publisher.Start(ctx)
		require.ErrorIs(t, err, ErrNoWorkerID)
		require.False(t, publisher.IsStarted())
	})

	t.Run("returns error if already started", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-hb-start-3")

		publisher := New(kv, "worker-hb", "worker-1", 2*time.Second, nil, nil)

		err := publisher.Start(ctx)
		require.NoError(t, err)

		// Try to start again
		err = publisher.Start(ctx)
		require.ErrorIs(t, err, ErrAlreadyStarted)

		// Cleanup
		err = publisher.Stop()
		require.NoError(t, err)
	})
}

func TestPublisher_Stop(t *testing.T) {
	t.Run("stops successfully", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-hb-stop-1")

		publisher := New(kv, "worker-hb", "worker-1", 2*time.Second, nil, nil)

		err := publisher.Start(ctx)
		require.NoError(t, err)

		err = publisher.Stop()
		require.NoError(t, err)
		require.False(t, publisher.IsStarted())
	})

	t.Run("returns error if not started", func(t *testing.T) {
		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-hb-stop-2")

		publisher := New(kv, "worker-hb", "worker-1", 2*time.Second, nil, nil)

		err := publisher.Stop()
		require.ErrorIs(t, err, ErrNotStarted)
	})

	t.Run("restart after stop succeeds without panic", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-hb-stop-restart")

		publisher := New(kv, "worker-hb", "worker-1", 100*time.Millisecond, nil, nil)

		// First start/stop cycle
		require.NoError(t, publisher.Start(ctx))
		require.True(t, publisher.IsStarted())
		require.NoError(t, publisher.Stop())
		require.False(t, publisher.IsStarted())

		// Second start/stop cycle — must not panic
		require.NoError(t, publisher.Start(ctx))
		require.True(t, publisher.IsStarted())

		// Verify heartbeat is actually being published after restart
		key := fmt.Sprintf("worker-hb.%s", "worker-1")
		entry := requireHeartbeatEntry(t, kv, key, 500*time.Millisecond)
		require.NotEmpty(t, entry.Value())

		require.NoError(t, publisher.Stop())
		require.False(t, publisher.IsStarted())
	})
}

func TestPublisher_PeriodicHeartbeats(t *testing.T) {
	t.Run("publishes heartbeats at regular intervals", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-hb-periodic")

		// Use short interval for testing
		publisher := New(kv, "worker-hb", "worker-1", 100*time.Millisecond, nil, nil)

		err := publisher.Start(ctx)
		require.NoError(t, err)
		defer func() { _ = publisher.Stop() }()

		firstEntry := requireHeartbeatEntry(t, kv, "worker-hb.worker-1", 500*time.Millisecond)
		firstHB, err := types.DecodeHeartbeat(firstEntry.Value())
		require.NoError(t, err)
		firstTimestamp := firstHB.Timestamp

		var secondTimestamp time.Time
		require.Eventually(t, func() bool {
			entry := requireHeartbeatEntry(t, kv, "worker-hb.worker-1", 200*time.Millisecond)
			hb, parseErr := types.DecodeHeartbeat(entry.Value())
			if parseErr != nil {
				return false
			}
			if !hb.Timestamp.After(firstTimestamp) {
				return false
			}

			secondTimestamp = hb.Timestamp

			return true
		}, 500*time.Millisecond, 25*time.Millisecond, "heartbeat timestamp should advance")

		// Second timestamp should be after first
		require.True(t, secondTimestamp.After(firstTimestamp))
	})
}

func TestPublisher_TTLExpiry(t *testing.T) {
	t.Run("heartbeat expires after TTL", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)

		// Create KV with short TTL for testing
		js, err := jetstream.New(nc)
		require.NoError(t, err)

		kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:  "test-hb-ttl",
			TTL:     1 * time.Second, // Very short TTL
			Storage: jetstream.MemoryStorage,
		})
		require.NoError(t, err)

		publisher := New(kv, "worker-hb", "worker-1", 2*time.Second, nil, nil) // Longer than TTL

		err = publisher.Start(ctx)
		require.NoError(t, err)

		// Verify initial heartbeat exists
		entry, err := kv.Get(ctx, "worker-hb.worker-1")
		require.NoError(t, err)
		require.NotNil(t, entry)

		// Stop publisher (no more heartbeats)
		err = publisher.Stop()
		require.NoError(t, err)

		// Wait for TTL to expire
		time.Sleep(2 * time.Second)

		// Heartbeat should be gone
		_, err = kv.Get(ctx, "worker-hb.worker-1")
		require.Error(t, err)
		require.ErrorIs(t, err, jetstream.ErrKeyNotFound)
	})
}

func TestPublisher_MultipleWorkers(t *testing.T) {
	t.Run("multiple workers publish independently", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-hb-multiple")

		// Create three publishers
		publishers := make([]*Publisher, 3)
		for i := range publishers {
			workerID := fmt.Sprintf("worker-%d", i+1)
			publishers[i] = New(kv, "worker-hb", workerID, 100*time.Millisecond, nil, nil)

			err := publishers[i].Start(ctx)
			require.NoError(t, err)
		}

		// Verify all heartbeats exist
		for i := range publishers {
			key := fmt.Sprintf("worker-hb.worker-%d", i+1)
			entry := requireHeartbeatEntry(t, kv, key, 500*time.Millisecond)
			require.NotNil(t, entry)
		}

		// Cleanup
		for _, publisher := range publishers {
			err := publisher.Stop()
			require.NoError(t, err)
		}
	})
}

func TestPublisher_KeyFormat(t *testing.T) {
	t.Run("generates correct key format", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-hb-keyformat")

		publisher := New(kv, "worker-hb", "worker-123", 2*time.Second, nil, nil)

		err := publisher.Start(ctx)
		require.NoError(t, err)
		defer func() { _ = publisher.Stop() }()

		// Verify key format
		entry, err := kv.Get(ctx, "worker-hb.worker-123")
		require.NoError(t, err)
		require.NotNil(t, entry)
	})
}

// TestHeartbeat_SetAppliedAssignmentMonotone verifies that SetAppliedAssignment
// rejects updates with a lower AppliedVersion than the current snapshot.
// Equal versions are accepted (idempotent re-application overwrites the snapshot);
// lower versions are silently ignored.
func TestHeartbeat_SetAppliedAssignmentMonotone(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "test-hb-monotone")

	p := New(kv, "worker-hb", "worker-1", 2*time.Second, nil, nil)

	// Apply version 5 with digest 0xAABB.
	p.SetAppliedAssignment(AppliedAssignment{
		AppliedVersion: 5,
		AppliedDigest:  0xAABB,
	})

	// Attempt to regress to version 3 — must be ignored.
	p.SetAppliedAssignment(AppliedAssignment{
		AppliedVersion: 3,
		AppliedDigest:  0xDEAD, // different digest; must NOT replace
	})

	// Re-apply version 5 with a different digest — equal-version must overwrite (idempotent).
	p.SetAppliedAssignment(AppliedAssignment{
		AppliedVersion: 5,
		AppliedDigest:  0xCCDD, // updated digest for same version
	})

	// Read back the internal state by publishing and decoding.
	ctx := t.Context()
	require.NoError(t, p.Start(ctx))
	t.Cleanup(func() { _ = p.Stop() })

	entry, err := kv.Get(ctx, "worker-hb.worker-1")
	require.NoError(t, err)

	hb, err := types.DecodeHeartbeat(entry.Value())
	require.NoError(t, err)
	require.Equal(t, int64(5), hb.AppliedVersion, "version must not regress")
	require.Equal(t, uint64(0xCCDD), hb.AppliedDigest, "digest must reflect equal-version re-application")
}

// TestHeartbeat_NewPublisher_EmitsSchemaVersion1_BeforeFirstApply verifies that
// the v1 publisher emits SchemaVersion=1 in the initial heartbeat even before
// SetAppliedAssignment is ever called. Legacy SchemaVersion=0 must only appear
// when DecodeHeartbeat parses a raw RFC3339 byte payload.
func TestHeartbeat_NewPublisher_EmitsSchemaVersion1_BeforeFirstApply(t *testing.T) {
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	// Long interval so only the initial publish from Start fires.
	kv := partitest.CreateJetStreamKV(t, nc, "test-hb-schema-before-apply")

	p := New(kv, "worker-hb", "worker-1", 10*time.Second, nil, nil)
	require.NoError(t, p.Start(ctx))
	t.Cleanup(func() { _ = p.Stop() })

	// Force an out-of-band publish (AppliedVersion is still 0 — no SetAppliedAssignment).
	require.NoError(t, p.PublishNow(ctx))

	entry, err := kv.Get(ctx, "worker-hb.worker-1")
	require.NoError(t, err)

	raw := entry.Value()
	require.True(t, len(raw) > 0 && raw[0] == '{', "v1 publisher must emit JSON")

	hb, err := types.DecodeHeartbeat(raw)
	require.NoError(t, err)
	require.Equal(t, uint8(1), hb.SchemaVersion, "SchemaVersion must be 1 before first SetAppliedAssignment")
	require.Equal(t, int64(0), hb.AppliedVersion, "AppliedVersion must be 0 before first SetAppliedAssignment")
}

// TestHeartbeat_PublishNow_LeaderObservesWithinOneRoundTrip verifies that after
// SetAppliedAssignment + PublishNow, a reader (simulating the leader) can
// immediately fetch the updated heartbeat from KV and see the new AppliedVersion.
func TestHeartbeat_PublishNow_LeaderObservesWithinOneRoundTrip(t *testing.T) {
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	// Use a long heartbeat interval so the periodic tick does not interfere.
	kv := partitest.CreateJetStreamKV(t, nc, "test-hb-publishnow")

	p := New(kv, "worker-hb", "worker-1", 10*time.Second, nil, nil)
	require.NoError(t, p.Start(ctx))
	t.Cleanup(func() { _ = p.Stop() })

	appliedAt := time.Now().UTC()
	p.SetAppliedAssignment(AppliedAssignment{
		AppliedVersion:        99,
		AppliedDigest:         0xCAFE,
		LeaderRevision:        7,
		AppliedSourceRevision: 3,
		AppliedSourceRevKnown: true,
		AppliedAt:             appliedAt,
	})

	// Force immediate publish — must complete well within HeartbeatInterval.
	start := time.Now()
	require.NoError(t, p.PublishNow(ctx))
	require.Less(t, time.Since(start), 5*time.Second, "PublishNow must complete quickly")

	entry, err := kv.Get(ctx, "worker-hb.worker-1")
	require.NoError(t, err)

	hb, err := types.DecodeHeartbeat(entry.Value())
	require.NoError(t, err)
	require.Equal(t, int64(99), hb.AppliedVersion)
	require.Equal(t, uint64(0xCAFE), hb.AppliedDigest)
	require.Equal(t, uint64(7), hb.LeaderRevision)
	require.Equal(t, uint64(3), hb.AppliedSourceRevision)
	require.True(t, hb.AppliedSourceRevKnown)
	require.Equal(t, uint8(1), hb.SchemaVersion)
}

// TestHeartbeat_CapabilitiesReflectWiredComponents verifies that the published
// heartbeat's Capabilities field ORs the value returned by the capabilities
// function registered via SetCapabilitiesFn with the intrinsic CapAckV1 bit.
// CapAckV1 is always present because this publisher is by definition ack-capable.
func TestHeartbeat_CapabilitiesReflectWiredComponents(t *testing.T) {
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "test-hb-caps")

	var capsValue uint32 // starts at zero
	p := New(kv, "worker-hb", "worker-1", 10*time.Second, nil, nil)
	p.SetCapabilitiesFn(func() uint32 { return capsValue })

	require.NoError(t, p.Start(ctx))
	t.Cleanup(func() { _ = p.Stop() })

	// Initial publish: capsFn returns 0, but CapAckV1 is always ORed in.
	entry, err := kv.Get(ctx, "worker-hb.worker-1")
	require.NoError(t, err)
	hb, err := types.DecodeHeartbeat(entry.Value())
	require.NoError(t, err)
	require.Equal(t, types.CapAckV1, hb.Capabilities)

	// Wire all three capabilities and force a publish.
	capsValue = types.CapAckV1 | types.CapTwoPhaseHandoff | types.CapProcessingGate
	require.NoError(t, p.PublishNow(ctx))

	entry, err = kv.Get(ctx, "worker-hb.worker-1")
	require.NoError(t, err)
	hb, err = types.DecodeHeartbeat(entry.Value())
	require.NoError(t, err)
	require.Equal(t, types.CapAckV1|types.CapTwoPhaseHandoff|types.CapProcessingGate, hb.Capabilities)

	// Clear external capabilities; CapAckV1 must remain because it is intrinsic.
	capsValue = 0
	require.NoError(t, p.PublishNow(ctx))

	entry, err = kv.Get(ctx, "worker-hb.worker-1")
	require.NoError(t, err)
	hb, err = types.DecodeHeartbeat(entry.Value())
	require.NoError(t, err)
	require.Equal(t, types.CapAckV1, hb.Capabilities)
}

// TestHeartbeat_InitialStartPublish_IncludesCapAckV1 is a regression test for
// the startup-ordering bug where the very first heartbeat (published synchronously
// inside Start before the manager called SetCapability) decoded with CapAckV1==0.
//
// The fix: build() ORs CapAckV1 directly, independent of the capsFn callback.
// This test proves it by wiring a capsFn that explicitly returns zero and
// asserting that the initial heartbeat still carries CapAckV1.
func TestHeartbeat_InitialStartPublish_IncludesCapAckV1(t *testing.T) {
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	// Long interval so only the synchronous initial publish from Start fires.
	kv := partitest.CreateJetStreamKV(t, nc, "test-hb-initial-cap-ackv1")

	p := New(kv, "worker-hb", "worker-1", 10*time.Second, nil, nil)
	// Explicitly return 0 from the external callback to prove the publisher
	// self-asserts CapAckV1 without relying on an external caller setting it first.
	p.SetCapabilitiesFn(func() uint32 { return 0 })

	require.NoError(t, p.Start(ctx))
	t.Cleanup(func() { _ = p.Stop() })

	// Read the heartbeat KV key directly — this is the first (synchronous) publish.
	entry, err := kv.Get(ctx, "worker-hb.worker-1")
	require.NoError(t, err)

	decoded, err := types.DecodeHeartbeat(entry.Value())
	require.NoError(t, err)
	require.Equal(t, uint8(1), decoded.SchemaVersion, "initial heartbeat must be SchemaVersion=1")
	require.NotZero(t, decoded.Capabilities&types.CapAckV1, "CapAckV1 must be set on the initial heartbeat even when capsFn returns 0")
}

// Verify that the publisher's JSON output round-trips cleanly through
// encoding/json. This catches any field-tag mistakes at compile time.
func TestPublisher_JSONOutputRoundTrip(t *testing.T) {
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "test-hb-json-rt")

	p := New(kv, "worker-hb", "worker-1", 10*time.Second, nil, nil)
	require.NoError(t, p.Start(ctx))
	t.Cleanup(func() { _ = p.Stop() })

	entry, err := kv.Get(ctx, "worker-hb.worker-1")
	require.NoError(t, err)

	var hb types.Heartbeat
	require.NoError(t, json.Unmarshal(entry.Value(), &hb))
	require.Equal(t, "worker-1", hb.WorkerID)
	require.False(t, hb.Timestamp.IsZero())
}

// TestPublisher_EmitsLabels verifies SetLabels is reflected in every published
// heartbeat, and that an unlabeled publisher emits payload bytes with no "labels"
// field (byte-compat for unlabeled fleets and pre-label readers).
func TestPublisher_EmitsLabels(t *testing.T) {
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)

	t.Run("labels present after SetLabels", func(t *testing.T) {
		kv := partitest.CreateJetStreamKV(t, nc, "test-hb-labels-present")
		p := New(kv, "worker-hb", "worker-1", 10*time.Second, nil, nil)
		p.SetLabels([]string{"vip"})
		require.NoError(t, p.Start(ctx))
		t.Cleanup(func() { _ = p.Stop() })

		entry, err := kv.Get(ctx, "worker-hb.worker-1")
		require.NoError(t, err)
		require.Contains(t, string(entry.Value()), `"labels":["vip"]`)

		hb, err := types.DecodeHeartbeat(entry.Value())
		require.NoError(t, err)
		require.Equal(t, []string{"vip"}, hb.Labels)
	})

	t.Run("labels field absent without SetLabels", func(t *testing.T) {
		kv := partitest.CreateJetStreamKV(t, nc, "test-hb-labels-absent")
		p := New(kv, "worker-hb", "worker-2", 10*time.Second, nil, nil)
		require.NoError(t, p.Start(ctx))
		t.Cleanup(func() { _ = p.Stop() })

		entry, err := kv.Get(ctx, "worker-hb.worker-2")
		require.NoError(t, err)
		require.NotContains(t, string(entry.Value()), "labels",
			"unlabeled fleets must keep byte-compatible payloads (omitempty)")

		hb, err := types.DecodeHeartbeat(entry.Value())
		require.NoError(t, err)
		require.Nil(t, hb.Labels)
	})
}

// capturingLogger captures Warn messages for test assertions.
type capturingLogger struct {
	mu    sync.Mutex
	warns []string
}

func (l *capturingLogger) Debug(_ string, _ ...any) {}
func (l *capturingLogger) Info(_ string, _ ...any)  {}
func (l *capturingLogger) Error(_ string, _ ...any) {}
func (l *capturingLogger) Fatal(_ string, _ ...any) {}
func (l *capturingLogger) Warn(msg string, _ ...any) {
	l.mu.Lock()
	l.warns = append(l.warns, msg)
	l.mu.Unlock()
}
func (l *capturingLogger) hasWarn(substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, w := range l.warns {
		if strings.Contains(w, substr) {
			return true
		}
	}

	return false
}

// warnCount returns the number of Warn messages containing substr.
func (l *capturingLogger) warnCount(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, w := range l.warns {
		if strings.Contains(w, substr) {
			n++
		}
	}

	return n
}

// slowPutKV is a test-only jetstream.KeyValue fake whose Put method ignores
// ctx.Done() and blocks until releaseCh is closed. This ensures the in-flight
// gate stays set across tick boundaries regardless of the per-goroutine timeout,
// allowing TestPublisher_OverlappingPublishSkipsTick to observe the skip path.
// The first put call (from Start's initial publish) is let through immediately
// by a buffered channel; subsequent calls block until releaseCh is closed.
type slowPutKV struct {
	mu        sync.Mutex
	firstDone bool
	releaseCh chan struct{}
	putCount  atomic.Int32
}

func (k *slowPutKV) Put(_ context.Context, _ string, _ []byte) (uint64, error) {
	k.mu.Lock()
	first := !k.firstDone
	if first {
		k.firstDone = true
	}
	k.mu.Unlock()

	if !first {
		<-k.releaseCh // intentionally ignores ctx.Done()
	}
	k.putCount.Add(1)

	return 0, nil
}

// Stub implementations for unused KeyValue interface methods.
func (k *slowPutKV) PutString(_ context.Context, _ string, _ string) (uint64, error) {
	return 0, nil
}
func (k *slowPutKV) Create(_ context.Context, _ string, _ []byte, _ ...jetstream.KVCreateOpt) (uint64, error) {
	return 0, nil
}
func (k *slowPutKV) Update(_ context.Context, _ string, _ []byte, _ uint64) (uint64, error) {
	return 0, nil
}
func (k *slowPutKV) Delete(_ context.Context, _ string, _ ...jetstream.KVDeleteOpt) error {
	return nil
}
func (k *slowPutKV) Purge(_ context.Context, _ string, _ ...jetstream.KVDeleteOpt) error {
	return nil
}
func (k *slowPutKV) Get(_ context.Context, _ string) (jetstream.KeyValueEntry, error) {
	return nil, jetstream.ErrKeyNotFound
}
func (k *slowPutKV) GetRevision(_ context.Context, _ string, _ uint64) (jetstream.KeyValueEntry, error) {
	return nil, jetstream.ErrKeyNotFound
}
func (k *slowPutKV) History(_ context.Context, _ string, _ ...jetstream.WatchOpt) ([]jetstream.KeyValueEntry, error) {
	return nil, errSlowKVNotImplemented
}
func (k *slowPutKV) Watch(_ context.Context, _ string, _ ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	return nil, errSlowKVNotImplemented
}
func (k *slowPutKV) WatchAll(_ context.Context, _ ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	return nil, errSlowKVNotImplemented
}
func (k *slowPutKV) WatchFiltered(_ context.Context, _ []string, _ ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	return nil, errSlowKVNotImplemented
}
func (k *slowPutKV) Keys(_ context.Context, _ ...jetstream.WatchOpt) ([]string, error) {
	return nil, nil
}
func (k *slowPutKV) ListKeys(_ context.Context, _ ...jetstream.WatchOpt) (jetstream.KeyLister, error) {
	return nil, errSlowKVNotImplemented
}
func (k *slowPutKV) ListKeysFiltered(_ context.Context, _ ...string) (jetstream.KeyLister, error) {
	return nil, errSlowKVNotImplemented
}
func (k *slowPutKV) Bucket() string { return "fake" }
func (k *slowPutKV) Status(_ context.Context) (jetstream.KeyValueStatus, error) {
	return nil, errSlowKVNotImplemented
}
func (k *slowPutKV) PurgeDeletes(_ context.Context, _ ...jetstream.KVPurgeOpt) error { return nil }

// TestPublisher_OverlappingPublishSkipsTick verifies that when a Put call
// blocks beyond one tick interval, subsequent ticks are skipped (Warn log via
// recordSkippedPublish) rather than blocking the main publishLoop goroutine.
// After the blocked Put is released, the gate clears and the publisher recovers.
func TestPublisher_OverlappingPublishSkipsTick(t *testing.T) {
	t.Parallel()

	releaseCh := make(chan struct{})
	kv := &slowPutKV{releaseCh: releaseCh}
	logger := &capturingLogger{}

	interval := 50 * time.Millisecond
	p := New(kv, "worker-hb", "worker-1", interval, nil, logger)

	var onErrorCount atomic.Int32
	p.SetOnError(func(error) { onErrorCount.Add(1) })

	ctx := t.Context()
	// Start succeeds immediately: the initial synchronous publish is the first
	// put and is let through by slowPutKV's first-done logic.
	require.NoError(t, p.Start(ctx))

	// Phase 1: the first periodic tick spawns a publish goroutine whose Put
	// blocks (it's not the first put). Let at least two more ticks fire while
	// the goroutine is blocked — each should log a skip warning.
	time.Sleep(3 * interval)

	require.True(t, logger.hasWarn("heartbeat publish in flight, skipping tick"),
		"expected skip warning to be logged")

	// onError must NOT have been called — skipping is not a KV error.
	require.Zero(t, onErrorCount.Load(), "onError must not be called on skipped tick")

	// Phase 2: release the blocked Put so the in-flight goroutine completes
	// and the gate clears.
	close(releaseCh)

	// Wait for at least one successful publish AFTER the gate clears:
	// putCount=1 (initial), putCount=2 (blocked put returns), putCount=3 (new tick).
	// putCount >= 3 proves a post-release tick reached Put, not just the
	// unblocking of the previously blocked goroutine.
	require.Eventually(t, func() bool {
		return kv.putCount.Load() >= 3
	}, 500*time.Millisecond, 5*time.Millisecond, "publish should succeed after release")

	// Once putCount >= 3 is confirmed the gate has cleared and a full new tick
	// succeeded. Snapshot the skip count now and verify it does not grow further
	// over the next two tick intervals — the publisher has recovered.
	skipsAfterRecovery := logger.warnCount("heartbeat publish in flight, skipping tick")
	time.Sleep(2 * interval)
	require.Equal(t, skipsAfterRecovery, logger.warnCount("heartbeat publish in flight, skipping tick"),
		"no new skip warnings expected after recovery")

	// Stop must drain the in-flight WaitGroup and return within 500ms.
	stopDone := make(chan error, 1)
	go func() { stopDone <- p.Stop() }()
	select {
	case err := <-stopDone:
		require.NoError(t, err)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Stop hung — in-flight WaitGroup not draining")
	}
}
