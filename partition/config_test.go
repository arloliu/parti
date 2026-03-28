package partition

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPartitionConfigValidate(t *testing.T) {
	cfg := PartitionConfig{
		NumPartitions:  4,
		SubjectPattern: "events.{{key}}.completed.{{partition}}",
	}

	require.NoError(t, cfg.Validate())
}

func TestPartitionConfigValidate_AllowsWildcardPattern(t *testing.T) {
	cfg := PartitionConfig{
		NumPartitions:  4,
		SubjectPattern: "events.*.{{key}}.{{partition}}",
	}

	require.NoError(t, cfg.Validate())
}

func TestPartitionConfigValidate_InvalidPattern(t *testing.T) {
	cfg := PartitionConfig{
		NumPartitions:  4,
		SubjectPattern: "events..{{partition}}",
	}

	err := cfg.Validate()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrPatternEmptyToken))
}

func TestPartitionConfig_SetDefaults(t *testing.T) {
	cfg := PartitionConfig{
		NumPartitions:  4,
		SubjectPattern: "events.{{partition}}",
	}
	require.NoError(t, cfg.setDefaults())

	// Logger should be set to no-op
	require.NotNil(t, cfg.Logger)
}

func TestPartitionConfig_SetDefaults_PreservesExistingValues(t *testing.T) {
	cfg := PartitionConfig{
		NumPartitions:  4,
		SubjectPattern: "events.{{partition}}",
		HashSeed:       12345,
	}
	require.NoError(t, cfg.setDefaults())

	// Existing values preserved
	require.Equal(t, uint64(12345), cfg.HashSeed)
	require.NotNil(t, cfg.Logger)
}
