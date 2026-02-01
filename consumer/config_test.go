package consumer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCommonConfig_SetDefaults(t *testing.T) {
	cfg := CommonConfig{}
	require.NoError(t, cfg.SetDefaults())

	// Check defaults are applied
	require.NotNil(t, cfg.Logger)
	require.NotNil(t, cfg.Metrics)
	require.Equal(t, 30*time.Second, cfg.AckWait)
	require.Equal(t, -1, cfg.MaxDeliver)
	require.Equal(t, 1, cfg.BatchSize)
	require.Equal(t, 5*time.Second, cfg.FetchTimeout)
	require.Equal(t, 2, cfg.MaxWaiting)
	require.Equal(t, 24*time.Hour, cfg.InactiveThreshold)
}

func TestCommonConfig_SetDefaults_PreservesExistingValues(t *testing.T) {
	cfg := CommonConfig{
		AckWait:    60 * time.Second,
		MaxDeliver: 5,
		BatchSize:  10,
	}
	require.NoError(t, cfg.SetDefaults())

	// Existing values preserved
	require.Equal(t, 60*time.Second, cfg.AckWait)
	require.Equal(t, 5, cfg.MaxDeliver)
	require.Equal(t, 10, cfg.BatchSize)

	// Unset values get defaults
	require.Equal(t, 5*time.Second, cfg.FetchTimeout)
	require.Equal(t, 2, cfg.MaxWaiting)
	require.Equal(t, 24*time.Hour, cfg.InactiveThreshold)
}

func TestRetryConfig_Defaults(t *testing.T) {
	// RetryConfig doesn't have its own SetDefaults, but we can verify
	// the default tag values through a parent config
	cfg := BroadcastConfig{
		StreamName:     "TEST",
		FilterSubject:  "events.>",
		ConsumerPrefix: "test",
	}
	require.NoError(t, cfg.SetDefaults())

	// Check RetryConfig defaults
	require.Equal(t, 100*time.Millisecond, cfg.Retry.Backoff)
	require.Equal(t, 5*time.Second, cfg.Retry.Max)
	require.Equal(t, 1.6, cfg.Retry.Multiplier)
	require.Equal(t, 200*time.Millisecond, cfg.Retry.Base)
}

func TestDynamicConfig_SetDefaults(t *testing.T) {
	cfg := DynamicConfig{
		StreamName:      "TEST",
		SubjectTemplate: "events.{{.PartitionID}}",
		ConsumerPrefix:  "test",
	}
	require.NoError(t, cfg.SetDefaults())

	// CommonConfig struct tag defaults (Logger/Metrics are set by CommonConfig.SetDefaults, not fuda)
	require.Equal(t, 30*time.Second, cfg.AckWait)
	require.Equal(t, -1, cfg.MaxDeliver)
	require.Equal(t, 1, cfg.BatchSize)
	require.Equal(t, 5*time.Second, cfg.FetchTimeout)
	require.Equal(t, 2, cfg.MaxWaiting)
	require.Equal(t, 24*time.Hour, cfg.InactiveThreshold)

	// DynamicConfig specific defaults
	require.Equal(t, 10*time.Second, cfg.DrainOnRemoveTimeout)
}

func TestDynamicConfig_SetDefaults_PreservesExistingValues(t *testing.T) {
	cfg := DynamicConfig{
		StreamName:           "TEST",
		SubjectTemplate:      "events.{{.PartitionID}}",
		ConsumerPrefix:       "test",
		DrainOnRemoveTimeout: 30 * time.Second,
	}
	cfg.AckWait = 60 * time.Second

	require.NoError(t, cfg.SetDefaults())

	// Existing values preserved
	require.Equal(t, 30*time.Second, cfg.DrainOnRemoveTimeout)
	require.Equal(t, 60*time.Second, cfg.AckWait)

	// Unset values get defaults
	require.Equal(t, -1, cfg.MaxDeliver)
}

func TestStaticConfig_SetDefaults(t *testing.T) {
	cfg := StaticConfig{
		StreamName:     "TEST",
		NumPartitions:  4,
		Partition:      0,
		SubjectPattern: "events.{{partition}}",
		ConsumerName:   "consumer-0",
	}
	require.NoError(t, cfg.SetDefaults())

	// CommonConfig struct tag defaults (Logger/Metrics are set by CommonConfig.SetDefaults, not fuda)
	require.Equal(t, 30*time.Second, cfg.AckWait)
	require.Equal(t, -1, cfg.MaxDeliver)
	require.Equal(t, 1, cfg.BatchSize)
	require.Equal(t, 5*time.Second, cfg.FetchTimeout)
	require.Equal(t, 2, cfg.MaxWaiting)
	require.Equal(t, 24*time.Hour, cfg.InactiveThreshold)
}

func TestBroadcastConfig_SetDefaults(t *testing.T) {
	cfg := BroadcastConfig{
		StreamName:     "TEST",
		FilterSubject:  "events.>",
		ConsumerPrefix: "test",
	}
	require.NoError(t, cfg.SetDefaults())

	// CommonConfig struct tag defaults (Logger/Metrics are set by CommonConfig.SetDefaults, not fuda)
	require.Equal(t, 30*time.Second, cfg.AckWait)
	require.Equal(t, -1, cfg.MaxDeliver)
	require.Equal(t, 1, cfg.BatchSize)
	require.Equal(t, 5*time.Second, cfg.FetchTimeout)
	require.Equal(t, 2, cfg.MaxWaiting)
	require.Equal(t, 24*time.Hour, cfg.InactiveThreshold)

	// BroadcastConfig specific defaults
	require.Equal(t, 60*time.Second, cfg.IteratorEscalationWindow)
	require.Equal(t, 3, cfg.IteratorEscalationThreshold)

	// RetryConfig defaults
	require.Equal(t, 100*time.Millisecond, cfg.Retry.Backoff)
	require.Equal(t, 5*time.Second, cfg.Retry.Max)
	require.Equal(t, 1.6, cfg.Retry.Multiplier)
	require.Equal(t, 200*time.Millisecond, cfg.Retry.Base)
}

func TestBroadcastConfig_SetDefaults_PreservesExistingValues(t *testing.T) {
	cfg := BroadcastConfig{
		StreamName:                  "TEST",
		FilterSubject:               "events.>",
		ConsumerPrefix:              "test",
		IteratorEscalationWindow:    120 * time.Second,
		IteratorEscalationThreshold: 5,
	}
	cfg.AckWait = 60 * time.Second

	require.NoError(t, cfg.SetDefaults())

	// Existing values preserved
	require.Equal(t, 120*time.Second, cfg.IteratorEscalationWindow)
	require.Equal(t, 5, cfg.IteratorEscalationThreshold)
	require.Equal(t, 60*time.Second, cfg.AckWait)

	// Unset values get defaults
	require.Equal(t, -1, cfg.MaxDeliver)
	require.Equal(t, 100*time.Millisecond, cfg.Retry.Backoff)
}
