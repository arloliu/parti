package durable

import (
	"context"
	"testing"
	"text/template"
	"time"

	partitesting "github.com/arloliu/parti/v2/partitest"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestWorkerConsumer_PassesReconcileIntervalToResolver verifies that the
// ReconcileInterval configured on ResolverConfig is plumbed all the way
// through to the auto-created *ClaimBasedResolver via WithReconcileInterval.
func TestWorkerConsumer_PassesReconcileIntervalToResolver(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := WorkerConsumerConfig{
		StreamName:      "RI1",
		ConsumerPrefix:  "wc-ri1",
		SubjectTemplate: "ri1.{{.PartitionID}}",
		ProcessingGate:  &ProcessingGateConfig{Enabled: true},
		Resolver: ResolverConfig{
			HandoffBucketName:   "ri1-handoff",
			HandoffClaimsPrefix: "claims/",
			ReconcileInterval:   1 * time.Second,
		},
	}
	require.NoError(t, cfg.SetDefaults())
	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)

	wc := &WorkerConsumer{
		js:              js,
		config:          cfg,
		logger:          cfg.Logger,
		handler:         messageHandlerFunc(func(context.Context, jetstream.Msg) error { return nil }),
		subjects:        make(map[string]*partitionConsumer),
		iterFactory:     defaultIterFactory,
		subjectTemplate: tmpl,
	}
	require.NoError(t, wc.ensureGateResolver(ctx))
	t.Cleanup(func() { _ = wc.Close(context.Background()) })

	resolver, ok := wc.gateResolver.(*ClaimBasedResolver)
	require.True(t, ok, "auto-created resolver must be *ClaimBasedResolver")
	require.Equal(t, 1*time.Second, resolver.reconcileInterval,
		"ResolverConfig.ReconcileInterval must propagate to the resolver")
}

// TestWorkerConsumer_DefaultReconcileIntervalApplies verifies that when
// ResolverConfig.ReconcileInterval is left at zero, SetDefaults normalises
// it to 30s via the struct tag and the resolver is started with that value.
func TestWorkerConsumer_DefaultReconcileIntervalApplies(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := WorkerConsumerConfig{
		StreamName:      "RI2",
		ConsumerPrefix:  "wc-ri2",
		SubjectTemplate: "ri2.{{.PartitionID}}",
		ProcessingGate:  &ProcessingGateConfig{Enabled: true},
		Resolver: ResolverConfig{
			HandoffBucketName:   "ri2-handoff",
			HandoffClaimsPrefix: "claims/",
			// ReconcileInterval intentionally left at zero.
		},
	}
	require.NoError(t, cfg.SetDefaults())
	require.Equal(t, 30*time.Second, cfg.Resolver.ReconcileInterval,
		"SetDefaults must normalise zero ReconcileInterval to the 30s default")

	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)

	wc := &WorkerConsumer{
		js:              js,
		config:          cfg,
		logger:          cfg.Logger,
		handler:         messageHandlerFunc(func(context.Context, jetstream.Msg) error { return nil }),
		subjects:        make(map[string]*partitionConsumer),
		iterFactory:     defaultIterFactory,
		subjectTemplate: tmpl,
	}
	require.NoError(t, wc.ensureGateResolver(ctx))
	t.Cleanup(func() { _ = wc.Close(context.Background()) })

	resolver, ok := wc.gateResolver.(*ClaimBasedResolver)
	require.True(t, ok)
	require.Equal(t, 30*time.Second, resolver.reconcileInterval,
		"resolver must run with the 30s default when config field is zero")
}
