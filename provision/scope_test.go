package provision_test

import (
	"testing"

	"github.com/arloliu/parti/v2/provision"
	"github.com/stretchr/testify/require"
)

func TestScopeAll_IncludesEveryKind(t *testing.T) {
	t.Parallel()
	s := provision.ScopeAll()
	require.True(t, s.ControlPlane)
	require.True(t, s.PartitionSource)
	require.True(t, s.DynamicConsumers)
	require.True(t, s.Streams)
	require.Equal(t, "", s.Instance)
}

func TestScopeFromConfig_ControlPlaneOnly(t *testing.T) {
	t.Parallel()
	cfg := provision.Config{
		ControlPlane: &provision.ControlPlaneConfig{},
	}
	s := provision.ScopeFromConfig(cfg)
	require.True(t, s.ControlPlane)
	require.False(t, s.PartitionSource)
	require.False(t, s.DynamicConsumers)
	require.Equal(t, "", s.Instance)
}

func TestScopeFromConfig_AllSet(t *testing.T) {
	t.Parallel()
	cfg := provision.Config{
		Instance:        "prod",
		ControlPlane:    &provision.ControlPlaneConfig{},
		PartitionSource: &provision.PartitionSourceConfig{},
		DynamicConsumers: []provision.DynamicConsumerCfg{
			{StreamName: "orders"},
		},
		Streams: []provision.StreamCfg{
			{Name: "orders", Subjects: []string{"orders.>"}},
		},
	}
	s := provision.ScopeFromConfig(cfg)
	require.True(t, s.ControlPlane)
	require.True(t, s.PartitionSource)
	require.True(t, s.DynamicConsumers)
	require.True(t, s.Streams)
	require.Equal(t, "prod", s.Instance)
}

func TestScopeFromConfig_EmptyDynamicConsumersDoesNotEnable(t *testing.T) {
	t.Parallel()
	cfg := provision.Config{
		DynamicConsumers: []provision.DynamicConsumerCfg{},
	}
	s := provision.ScopeFromConfig(cfg)
	require.False(t, s.DynamicConsumers)
}

func TestScopeFromConfig_EmptyStreamsDoesNotEnable(t *testing.T) {
	t.Parallel()
	cfg := provision.Config{
		Streams: []provision.StreamCfg{},
	}
	s := provision.ScopeFromConfig(cfg)
	require.False(t, s.Streams)
}
