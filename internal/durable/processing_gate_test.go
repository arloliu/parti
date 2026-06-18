package durable

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// fakeMsg implements the subset of jetstream.Msg used by the gate for tests.
type fakeMsg struct {
	subj      string
	nakCount  int
	lastDelay time.Duration
	acked     bool
}

func (m *fakeMsg) Subject() string                           { return m.subj }
func (m *fakeMsg) Data() []byte                              { return nil }
func (m *fakeMsg) Reply() string                             { return "" }
func (m *fakeMsg) Ack() error                                { m.acked = true; return nil }
func (m *fakeMsg) Nak() error                                { m.nakCount++; return nil }
func (m *fakeMsg) Term() error                               { return nil }
func (m *fakeMsg) TermWithReason(_ string) error             { return nil }
func (m *fakeMsg) InProgress() error                         { return nil }
func (m *fakeMsg) DoubleAck(ctx context.Context) error       { return nil }
func (m *fakeMsg) Metadata() (*jetstream.MsgMetadata, error) { return nil, nil } //nolint:nilnil
func (m *fakeMsg) Headers() nats.Header                      { return nil }
func (m *fakeMsg) JetStream() jetstream.JetStream            { return nil }
func (m *fakeMsg) NakWithDelay(d time.Duration) error        { m.nakCount++; m.lastDelay = d; return nil }

// fakeResolver implements OwnershipResolver for tests.
type fakeResolver struct {
	owner string
	state types.HandoffState
	ok    bool
}

func (r fakeResolver) GetOwner(partitionID string) (string, types.HandoffState, int64, bool) { //nolint:revive
	return r.owner, r.state, 1, r.ok
}

// ForceRefreshPartition is a no-op stub; gate tests rely only on GetOwner.
func (r fakeResolver) ForceRefreshPartition(ctx context.Context, partitionID string) error {
	return nil
}

// TestProcessingGate_AllowsOwner tests that the owner in an allowed state processes the message.
func TestProcessingGate_AllowsOwner(t *testing.T) {
	resolver := fakeResolver{owner: "w1", state: types.HandoffStateStable, ok: true}
	g, err := newProcessingGate("w1", "events.{{.PartitionID}}", resolver, ProcessingGateConfig{Enabled: true}, nil)
	require.NoError(t, err)

	processed := false
	base := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		processed = true
		return nil
	})
	wrapped := g.Wrap(base)
	msg := &fakeMsg{subj: "events.partition-7"}
	err = wrapped.Handle(t.Context(), msg)
	require.NoError(t, err)
	require.True(t, processed, "owner with stable state should process")
	require.Equal(t, 0, msg.nakCount)
}

// TestProcessingGate_NaksNonOwner tests that a different owner causes a NAK with delay.
func TestProcessingGate_NaksNonOwner(t *testing.T) {
	resolver := fakeResolver{owner: "w2", state: types.HandoffStateStable, ok: true}
	g, err := newProcessingGate("w1", "events.{{.PartitionID}}", resolver, ProcessingGateConfig{Enabled: true, NakDelay: 150 * time.Millisecond}, nil)
	require.NoError(t, err)
	base := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error { return nil })
	wrapped := g.Wrap(base)
	msg := &fakeMsg{subj: "events.partition-7"}
	_ = wrapped.Handle(t.Context(), msg)
	require.Equal(t, 1, msg.nakCount, "non-owner should be NAKed exactly once")
	require.GreaterOrEqual(t, msg.lastDelay, 100*time.Millisecond)
}

// TestProcessingGate_DisallowedState tests that owner in a disallowed state is NAKed.
func TestProcessingGate_DisallowedState(t *testing.T) {
	resolver := fakeResolver{owner: "w1", state: types.HandoffStatePrepare, ok: true}
	g, err := newProcessingGate("w1", "events.{{.PartitionID}}", resolver, ProcessingGateConfig{Enabled: true}, nil)
	require.NoError(t, err)
	base := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error { return nil })
	wrapped := g.Wrap(base)
	msg := &fakeMsg{subj: "events.partition-7"}
	_ = wrapped.Handle(t.Context(), msg)
	require.Equal(t, 1, msg.nakCount, "owner in prepare state should be NAKed")
}

// TestProcessingGate_Warmup tests behavior during the warm-up phase.
func TestProcessingGate_Warmup(t *testing.T) {
	// Warmup: Allow Prepare, Disallow Stable (inverse of default)
	// Duration: 1h (effectively forever for this test)
	cfg := ProcessingGateConfig{
		Enabled:             true,
		WarmupDuration:      time.Hour,
		WarmupAllowedStates: []types.HandoffState{types.HandoffStatePrepare},
		AllowedStates:       []types.HandoffState{types.HandoffStateStable},
	}

	// Case 1: Owner in Prepare state (Allowed during warmup)
	resolver := fakeResolver{owner: "w1", state: types.HandoffStatePrepare, ok: true}
	g, err := newProcessingGate("w1", "events.{{.PartitionID}}", resolver, cfg, nil)
	require.NoError(t, err)

	processed := false
	base := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		processed = true
		return nil
	})
	wrapped := g.Wrap(base)
	msg := &fakeMsg{subj: "events.partition-7"}
	err = wrapped.Handle(t.Context(), msg)
	require.NoError(t, err)
	require.True(t, processed, "should process Prepare state during warmup")

	// Case 2: Owner in Stable state (Disallowed during warmup)
	resolver2 := fakeResolver{owner: "w1", state: types.HandoffStateStable, ok: true}
	g2, err := newProcessingGate("w1", "events.{{.PartitionID}}", resolver2, cfg, nil)
	require.NoError(t, err)
	processed = false
	wrapped2 := g2.Wrap(base)
	msg2 := &fakeMsg{subj: "events.partition-7"}
	_ = wrapped2.Handle(t.Context(), msg2)
	require.False(t, processed, "should NOT process Stable state during warmup")
	require.Equal(t, 1, msg2.nakCount)
}

// TestProcessingGate_SubjectParsingFailure tests fail-open behavior when subject doesn't match template.
func TestProcessingGate_SubjectParsingFailure(t *testing.T) {
	resolver := fakeResolver{owner: "w1", state: types.HandoffStateStable, ok: true}
	g, err := newProcessingGate("w1", "events.{{.PartitionID}}", resolver, ProcessingGateConfig{Enabled: true}, nil)
	require.NoError(t, err)

	processed := false
	base := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		processed = true
		return nil
	})
	wrapped := g.Wrap(base)

	// Subject mismatch
	msg := &fakeMsg{subj: "other.topic"}
	err = wrapped.Handle(t.Context(), msg)
	require.NoError(t, err)
	require.True(t, processed, "should fail open (process) when subject parsing fails")
}

// refreshResolver simulates a resolver that finds the owner after a forced refresh.
type refreshResolver struct {
	refreshed bool
}

func (r *refreshResolver) GetOwner(partitionID string) (string, types.HandoffState, int64, bool) { //nolint:revive
	if r.refreshed {
		return "w1", types.HandoffStateStable, 1, true
	}
	return "", types.HandoffStateUnknown, 0, false
}

func (r *refreshResolver) ForceRefreshPartition(ctx context.Context, partitionID string) error {
	r.refreshed = true
	return nil
}

// TestProcessingGate_UnknownOwnership_Refresh tests the force refresh logic.
func TestProcessingGate_UnknownOwnership_Refresh(t *testing.T) {
	r := &refreshResolver{}
	g, err := newProcessingGate("w1", "events.{{.PartitionID}}", r, ProcessingGateConfig{Enabled: true}, nil)
	require.NoError(t, err)

	processed := false
	base := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		processed = true
		return nil
	})
	wrapped := g.Wrap(base)

	msg := &fakeMsg{subj: "events.partition-7"}
	err = wrapped.Handle(t.Context(), msg)
	require.NoError(t, err)
	require.True(t, processed, "should process after refresh finds owner")
	require.True(t, r.refreshed, "ForceRefreshPartition should have been called")
}

// TestProcessingGate_Disabled tests that the gate is bypassed when disabled.
func TestProcessingGate_Disabled(t *testing.T) {
	// Resolver says "not owner", but gate is disabled.
	resolver := fakeResolver{owner: "w2", state: types.HandoffStateStable, ok: true}
	g, err := newProcessingGate("w1", "events.{{.PartitionID}}", resolver, ProcessingGateConfig{Enabled: false}, nil)
	require.NoError(t, err)

	processed := false
	base := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		processed = true
		return nil
	})
	wrapped := g.Wrap(base)

	msg := &fakeMsg{subj: "events.partition-7"}
	err = wrapped.Handle(t.Context(), msg)
	require.NoError(t, err)
	require.True(t, processed, "should process when gate is disabled")
	require.Equal(t, 0, msg.nakCount)
}

type mockMetrics struct {
	naks   map[string]int
	delays []time.Duration
}

func (m *mockMetrics) IncGateNAK(reason string) {
	if m.naks == nil {
		m.naks = make(map[string]int)
	}
	m.naks[reason]++
}

func (m *mockMetrics) ObserveGateNakDelay(d time.Duration) {
	m.delays = append(m.delays, d)
}

// TestProcessingGate_Metrics tests that metrics are recorded.
func TestProcessingGate_Metrics(t *testing.T) {
	resolver := fakeResolver{owner: "w2", state: types.HandoffStateStable, ok: true}
	metrics := &mockMetrics{}
	g, err := newProcessingGate("w1", "events.{{.PartitionID}}", resolver, ProcessingGateConfig{
		Enabled: true,
		Metrics: metrics,
	}, nil)
	require.NoError(t, err)

	base := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error { return nil })
	wrapped := g.Wrap(base)

	msg := &fakeMsg{subj: "events.partition-7"}
	_ = wrapped.Handle(t.Context(), msg)

	require.Equal(t, 1, metrics.naks["non_owner"])
	require.Len(t, metrics.delays, 1)
}

func TestProcessingGateConfig_Defaults(t *testing.T) {
	tests := []struct {
		name  string
		input ProcessingGateConfig
		check func(*testing.T, *ProcessingGateConfig)
	}{
		{
			// was TestProcessingGateConfig_SetDefaults
			name: "set_defaults",
			input: ProcessingGateConfig{
				Enabled: true,
			},
			check: func(t *testing.T, cfg *ProcessingGateConfig) {
				// Check defaults are applied
				require.Equal(t, 100*time.Millisecond, cfg.NakDelay)
				require.Equal(t, float64(0), cfg.NakJitter)
				require.Equal(t, []types.HandoffState{types.HandoffStateStable}, cfg.AllowedStates)
			},
		},
		{
			// was TestProcessingGateConfig_SetDefaults_PreservesExistingValues
			name: "preserves_existing_values",
			input: ProcessingGateConfig{
				Enabled:       true,
				NakDelay:      200 * time.Millisecond,
				NakJitter:     0.1,
				AllowedStates: []types.HandoffState{types.HandoffStateStable, types.HandoffStatePrepare},
			},
			check: func(t *testing.T, cfg *ProcessingGateConfig) {
				// Existing values preserved
				require.Equal(t, 200*time.Millisecond, cfg.NakDelay)
				require.Equal(t, 0.1, cfg.NakJitter)
				require.Equal(t, []types.HandoffState{types.HandoffStateStable, types.HandoffStatePrepare}, cfg.AllowedStates)
			},
		},
		{
			// was TestProcessingGateConfig_WarmupDefaults
			name: "warmup_defaults",
			input: ProcessingGateConfig{
				Enabled:        true,
				WarmupDuration: 5 * time.Second, // triggers WarmupAllowedStates default
			},
			check: func(t *testing.T, cfg *ProcessingGateConfig) {
				// WarmupAllowedStates should default to Stable-only when WarmupDuration > 0
				require.Equal(t, []types.HandoffState{types.HandoffStateStable}, cfg.WarmupAllowedStates)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.input
			require.NoError(t, cfg.applyDefaults())
			tt.check(t, &cfg)
		})
	}
}
