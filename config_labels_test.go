package parti

import (
	"strings"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// TestWorkerAndPartitionLabelRulesAgree guards against the charset/length
// rules drifting between Partition.Validate and normalizeWorkerLabels now that
// both delegate to types.ValidateLabel. Every non-empty label that one path
// rejects, the other must reject too (the empty string is excluded: it is
// valid for a partition but forbidden for a worker label).
func TestWorkerAndPartitionLabelRulesAgree(t *testing.T) {
	t.Parallel()

	cases := []string{
		"vip",                   // valid on both
		"gpu-batch_2",           // valid on both
		"a.b",                   // dot
		"a b",                   // space
		"a\tb",                  // tab
		strings.Repeat("x", 64), // exactly at cap, valid
		strings.Repeat("x", 65), // over cap
	}
	for _, label := range cases {
		partErr := types.Partition{Keys: []string{"k"}, Label: label}.Validate()
		_, workerErr := normalizeWorkerLabels([]string{label})
		require.Equal(t, partErr != nil, workerErr != nil,
			"partition and worker label validity must agree for %q (partErr=%v workerErr=%v)",
			label, partErr, workerErr)
	}
}

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
