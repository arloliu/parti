package consumer

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseStatefulSetOrdinal(t *testing.T) {
	tests := []struct {
		name     string
		hostname string
		want     int
		wantErr  bool
	}{
		{"valid", "worker-3", 3, false},
		{"valid-multi-digit", "worker-123", 123, false},
		{"invalid-no-dash", "worker", 0, true},
		{"invalid-not-int", "worker-abc", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseStatefulSetOrdinal(tt.hostname)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.want, got)
			}
		})
	}
}

func TestGetPartitionFromEnv(t *testing.T) {
	// Save/Restore env
	originalIndex := os.Getenv("PARTITION_INDEX")
	originalHostname := os.Getenv("HOSTNAME")
	defer func() {
		if originalIndex != "" {
			_ = os.Setenv("PARTITION_INDEX", originalIndex)
		} else {
			_ = os.Unsetenv("PARTITION_INDEX")
		}
		if originalHostname != "" {
			_ = os.Setenv("HOSTNAME", originalHostname)
		} else {
			_ = os.Unsetenv("HOSTNAME")
		}
	}()

	t.Run("from PARTITION_INDEX", func(t *testing.T) {
		require.NoError(t, os.Setenv("PARTITION_INDEX", "5"))
		require.NoError(t, os.Unsetenv("HOSTNAME"))
		got, err := GetPartitionFromEnv()
		require.NoError(t, err)
		require.Equal(t, 5, got)
	})

	t.Run("from HOSTNAME", func(t *testing.T) {
		require.NoError(t, os.Unsetenv("PARTITION_INDEX"))
		require.NoError(t, os.Setenv("HOSTNAME", "app-7"))
		got, err := GetPartitionFromEnv()
		require.NoError(t, err)
		require.Equal(t, 7, got)
	})

	t.Run("error", func(t *testing.T) {
		require.NoError(t, os.Unsetenv("PARTITION_INDEX"))
		require.NoError(t, os.Setenv("HOSTNAME", "invalid"))
		_, err := GetPartitionFromEnv()
		require.Error(t, err)
	})
}
