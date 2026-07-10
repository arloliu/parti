package parti_test

import (
	"testing"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// labelParkCollector is the observability-without-a-pipeline pattern from
// docs/LABELS.md: embed types.NopMetrics for a complete no-op base and override
// only the LabelMetrics method(s) of interest.
type labelParkCollector struct {
	types.NopMetrics
	parked map[string]int
}

func (c *labelParkCollector) RecordParkedPartitions(label string, count int) {
	c.parked[label] = count
}

// Compile-time proof that embedding NopMetrics is enough to satisfy the full
// collector plus the optional label extension with a single overridden method.
var (
	_ types.MetricsCollector = (*labelParkCollector)(nil)
	_ types.LabelMetrics     = (*labelParkCollector)(nil)
)

func TestNopMetrics_EmbedAndWire(t *testing.T) {
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	t.Cleanup(cleanup)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	coll := &labelParkCollector{parked: map[string]int{}}

	cfg := testutil.IntegrationTestConfig()
	src := source.NewStatic(testutil.CreateTestPartitions(3))
	mgr, err := parti.NewManager(&cfg, js, src, strategy.NewConsistentHash(), parti.WithMetrics(coll))
	require.NoError(t, err, "a NopMetrics-embedding collector must wire into WithMetrics")
	require.NotNil(t, mgr)

	// The overridden method records; the inherited no-ops are safe to call.
	coll.RecordParkedPartitions("vip", 4)
	coll.RecordLabelPoolSize("vip", 0) // inherited no-op
	coll.IncrementLabelSpill("vip")    // inherited no-op
	coll.RecordActiveWorkers(2)        // inherited no-op from a non-label sub-interface
	require.Equal(t, map[string]int{"vip": 4}, coll.parked)
}
