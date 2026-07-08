package parti

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestConfig_WorkerLabelsNormalization(t *testing.T) {
	t.Parallel()

	got, err := normalizeWorkerLabels([]string{"vip", "batch", "vip"})
	require.NoError(t, err)
	require.Equal(t, []string{"batch", "vip"}, got, "sorted + deduped")

	_, err = normalizeWorkerLabels([]string{"bad label"})
	require.Error(t, err, "whitespace rejected")
	_, err = normalizeWorkerLabels([]string{"dotted.label"})
	require.Error(t, err, "dots rejected")
	_, err = normalizeWorkerLabels([]string{""})
	require.Error(t, err, "empty label rejected")

	seventeen := make([]string, 17)
	for i := range seventeen {
		seventeen[i] = string(rune('a' + i))
	}
	_, err = normalizeWorkerLabels(seventeen)
	require.Error(t, err, "more than 16 labels rejected")
}

func TestConfig_LabelPolicyDefaults(t *testing.T) {
	t.Parallel()

	cfg := Config{}
	require.NoError(t, SetDefaults(&cfg))
	require.Equal(t, "dedicated", cfg.UnlabeledPartitionPolicy)
	require.Equal(t, 60*time.Second, cfg.LabelSpillGrace)

	cfg.UnlabeledPartitionPolicy = "invalid"
	require.Error(t, cfg.Validate())

	cfg.UnlabeledPartitionPolicy = "shared"
	cfg.LabelSpillGrace = -time.Second
	require.Error(t, cfg.Validate())
}
