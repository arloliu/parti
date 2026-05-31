package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/test/simulation/internal/config"
	"github.com/arloliu/parti/v2/test/simulation/internal/coordinator"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestSimKVFaultController_KVUnavailableFaultsSelectedBucket(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	fc := newSimKVFaultController()
	wrapped := newSimKVFaultJetStream(js, fc)

	kv, err := wrapped.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "parti-heartbeat"})
	require.NoError(t, err)
	_, err = kv.Put(ctx, "heartbeat.worker-0", []byte("ok"))
	require.NoError(t, err)

	fc.armKVUnavailable([]string{"parti-heartbeat"})
	_, err = kv.Get(ctx, "heartbeat.worker-0")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	_, err = kv.Put(ctx, "heartbeat.worker-0", []byte("faulted"))
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, int64(2), fc.kvUnavailableInjected.Load())

	other, err := wrapped.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "parti-assignment"})
	require.NoError(t, err)
	_, err = other.Put(ctx, "assignment.worker-0", []byte("ok"))
	require.NoError(t, err)
}

func TestSimKVFaultController_HandoffClaimWriteFaultOnlyClaimsWrites(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	fc := newSimKVFaultController()
	wrapped := newSimKVFaultJetStream(js, fc)
	kv, err := wrapped.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "parti-handoff"})
	require.NoError(t, err)

	fc.armHandoffClaimWrite()
	_, err = kv.Create(ctx, "claims/p0", []byte("claim"))
	require.ErrorIs(t, err, context.DeadlineExceeded)
	_, err = kv.Put(ctx, "metadata/p0", []byte("metadata"))
	require.NoError(t, err)

	_, err = kv.Get(ctx, "metadata/p0")
	require.NoError(t, err)
	require.Equal(t, int64(1), fc.handoffClaimWriteInjected.Load())
}

func TestSimKVFaultController_HandoffClaimWriteDoesNotFaultClaimCleanup(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	fc := newSimKVFaultController()
	wrapped := newSimKVFaultJetStream(js, fc)
	kv, err := wrapped.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "parti-handoff"})
	require.NoError(t, err)

	_, err = kv.Put(ctx, "claims/delete", []byte("claim"))
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/purge", []byte("claim"))
	require.NoError(t, err)

	fc.armHandoffClaimWrite()
	require.NoError(t, kv.Delete(ctx, "claims/delete"))
	require.NoError(t, kv.Purge(ctx, "claims/purge"))
	require.Zero(t, fc.handoffClaimWriteInjected.Load(), "cleanup ops must not count as claim write faults")
}

func TestSimKVFaultController_DisarmRestoresKV(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	fc := newSimKVFaultController()
	wrapped := newSimKVFaultJetStream(js, fc)
	kv, err := wrapped.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "parti-heartbeat"})
	require.NoError(t, err)

	fc.armKVUnavailable([]string{"parti-heartbeat"})
	_, err = kv.Put(ctx, "heartbeat.worker-0", []byte("faulted"))
	require.ErrorIs(t, err, context.DeadlineExceeded)

	fc.disarm()
	rev, err := kv.Put(ctx, "heartbeat.worker-0", []byte("ok"))
	require.NoError(t, err)
	require.Positive(t, rev)

	_, err = kv.Get(ctx, "missing")
	require.True(t, errors.Is(err, jetstream.ErrKeyNotFound), "non-fault errors should pass through after disarm")
}

func TestHandleKVUnavailableFaultArmsAndAutoDisarms(t *testing.T) {
	fc := newSimKVFaultController()
	params := map[string]any{
		"buckets":  []any{"parti-heartbeat"},
		"duration": "20ms",
	}

	handleKVUnavailableFault(context.Background(), fc, nil, params)
	require.True(t, fc.isKVUnavailableArmed("parti-heartbeat"))

	require.Eventually(t, func() bool {
		return !fc.isKVUnavailableArmed("parti-heartbeat")
	}, time.Second, 10*time.Millisecond)
}

func TestHandleKVUnavailableFaultOverlappingTimersKeepNewestFaultArmed(t *testing.T) {
	fc := newSimKVFaultController()

	handleKVUnavailableFault(context.Background(), fc, nil, map[string]any{
		"buckets":  []any{"parti-heartbeat"},
		"duration": "30ms",
	})
	time.Sleep(10 * time.Millisecond)
	handleKVUnavailableFault(context.Background(), fc, nil, map[string]any{
		"buckets":  []any{"parti-stableid"},
		"duration": "120ms",
	})

	time.Sleep(50 * time.Millisecond)
	require.False(t, fc.isKVUnavailableArmed("parti-heartbeat"))
	require.True(t, fc.isKVUnavailableArmed("parti-stableid"))

	require.Eventually(t, func() bool {
		return !fc.isKVUnavailableArmed("parti-stableid")
	}, time.Second, 10*time.Millisecond)
}

func TestHandleHandoffClaimWriteFaultOverlappingTimersKeepNewestFaultArmed(t *testing.T) {
	fc := newSimKVFaultController()

	handleHandoffClaimWriteFault(context.Background(), fc, map[string]any{"duration": "30ms"})
	time.Sleep(10 * time.Millisecond)
	handleHandoffClaimWriteFault(context.Background(), fc, map[string]any{"duration": "120ms"})

	time.Sleep(50 * time.Millisecond)
	require.True(t, fc.isHandoffClaimWriteArmed())

	require.Eventually(t, func() bool {
		return !fc.isHandoffClaimWriteArmed()
	}, time.Second, 10*time.Millisecond)
}

func TestScenarioHasChaosEvent(t *testing.T) {
	cfg := &config.Config{
		Chaos: config.ChaosConfig{
			Enabled: true,
			Events:  []string{string(coordinator.HandoffClaimWriteFaultEvent)},
		},
	}
	require.True(t, scenarioHasChaosEvent(cfg, coordinator.HandoffClaimWriteFaultEvent))
	require.False(t, scenarioHasChaosEvent(cfg, coordinator.KVUnavailableEvent))

	cfg = &config.Config{
		Chaos: config.ChaosConfig{
			ScheduledEvents: []config.ScheduledChaosEvent{
				{At: time.Second, Event: string(coordinator.KVUnavailableEvent)},
			},
		},
	}
	require.True(t, scenarioHasChaosEvent(cfg, coordinator.KVUnavailableEvent))
}

func TestScenarioHasHandoffClaimWriteFault(t *testing.T) {
	cfg := &config.Config{
		Chaos: config.ChaosConfig{
			ScheduledEvents: []config.ScheduledChaosEvent{
				{At: time.Second, Event: string(coordinator.HandoffClaimWriteFaultEvent)},
			},
		},
	}
	require.True(t, scenarioHasHandoffClaimWriteFault(cfg))

	cfg = &config.Config{
		Chaos: config.ChaosConfig{
			Faults: config.FaultsConfig{
				HandoffClaimWriteOnStart: true,
			},
		},
	}
	require.True(t, scenarioHasHandoffClaimWriteFault(cfg))
}

func TestInstallStartupKVFaultsArmsHandoffClaimWrite(t *testing.T) {
	fc := newSimKVFaultController()
	cfg := &config.Config{
		Chaos: config.ChaosConfig{
			Faults: config.FaultsConfig{
				HandoffClaimWriteOnStart:  true,
				HandoffClaimWriteDuration: 20 * time.Millisecond,
			},
		},
	}

	installStartupKVFaults(context.Background(), fc, cfg)
	require.True(t, fc.isHandoffClaimWriteArmed())

	require.Eventually(t, func() bool {
		return !fc.isHandoffClaimWriteArmed()
	}, time.Second, 10*time.Millisecond)
}
