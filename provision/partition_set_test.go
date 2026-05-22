package provision_test

import (
	"testing"

	"github.com/arloliu/parti/v2/provision"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestValidatePartitionSet_Valid(t *testing.T) {
	set := []types.Partition{
		{Keys: []string{"topic-a", "0"}, Weight: 100},
		{Keys: []string{"topic-a", "1"}},
		{Keys: []string{"topic-b"}, Weight: 5},
	}
	require.NoError(t, provision.ValidatePartitionSet(set))
}

func TestValidatePartitionSet_Empty(t *testing.T) {
	cases := []struct {
		name string
		set  []types.Partition
	}{
		{"nil", nil},
		{"empty", []types.Partition{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := provision.ValidatePartitionSet(tc.set)
			require.Error(t, err)
			require.ErrorIs(t, err, provision.ErrInvalidConfig)
			require.Contains(t, err.Error(), "no partitions declared")
		})
	}
}

func TestValidatePartitionSet_InvalidRecord(t *testing.T) {
	cases := []struct {
		name string
		set  []types.Partition
	}{
		{"empty key", []types.Partition{{Keys: []string{""}}}},
		{"dotted key", []types.Partition{{Keys: []string{"a.b"}}}},
		{"whitespace key", []types.Partition{{Keys: []string{"a b"}}}},
		{"no keys", []types.Partition{{Keys: nil}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := provision.ValidatePartitionSet(tc.set)
			require.Error(t, err)
			require.ErrorIs(t, err, provision.ErrInvalidConfig)
		})
	}
}

func TestValidatePartitionSet_DuplicateCanonicalID(t *testing.T) {
	set := []types.Partition{
		{Keys: []string{"topic-a", "0"}, Weight: 1},
		{Keys: []string{"topic-b"}},
		{Keys: []string{"topic-a", "0"}, Weight: 9}, // same CanonicalID as [0]
	}
	err := provision.ValidatePartitionSet(set)
	require.Error(t, err)
	require.ErrorIs(t, err, provision.ErrInvalidConfig)
	require.Contains(t, err.Error(), "duplicates")
}

// TestValidate_PartitionSourceWithoutPartitions_Accepted proves the new
// Partitions field does not break the bucket-provisioning commands: an env
// config that declares a partition-source bucket but no partitions still
// passes the inherited static Validate.
func TestValidate_PartitionSourceWithoutPartitions_Accepted(t *testing.T) {
	cfg := provision.Config{
		APIVersion: provision.APIVersionV1,
		PartitionSource: &provision.PartitionSourceConfig{
			Bucket: "parti-partitions",
			Key:    "partitions/v1",
		},
	}
	require.NoError(t, provision.Validate(cfg))
}

// TestPartitionSourceConfig_YAMLRoundTrip confirms partitions parse from YAML
// into types.Partition (keys / weight populate Keys / Weight).
func TestPartitionSourceConfig_YAMLRoundTrip(t *testing.T) {
	const doc = `
bucket: parti-partitions
key: partitions/v1
partitions:
  - keys: ["topic-a", "0"]
    weight: 100
  - keys: ["topic-b"]
`
	var ps provision.PartitionSourceConfig
	require.NoError(t, yaml.Unmarshal([]byte(doc), &ps))

	require.Equal(t, "parti-partitions", ps.Bucket)
	require.Equal(t, "partitions/v1", ps.Key)
	require.Len(t, ps.Partitions, 2)
	require.Equal(t, []string{"topic-a", "0"}, ps.Partitions[0].Keys)
	require.Equal(t, int64(100), ps.Partitions[0].Weight)
	require.Equal(t, []string{"topic-b"}, ps.Partitions[1].Keys)
	require.Equal(t, int64(0), ps.Partitions[1].Weight)

	require.NoError(t, provision.ValidatePartitionSet(ps.Partitions))
}
