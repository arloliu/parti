package partition

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewConfigOptions(t *testing.T) {
	cfg := NewConfig(
		WithNumPartitions(8),
		WithSubjectPattern("events.{{partition}}"),
		WithHashSeed(42),
	)

	require.Equal(t, 8, cfg.NumPartitions)
	require.Equal(t, "events.{{partition}}", cfg.SubjectPattern)
	require.Equal(t, uint64(42), cfg.HashSeed)
}
