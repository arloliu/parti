package provision_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/provision"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func validControlPlane() *provision.ControlPlaneConfig {
	return &provision.ControlPlaneConfig{
		StableIDBucket:   "parti-stableid",
		ElectionBucket:   "parti-election",
		HeartbeatBucket:  "parti-heartbeat",
		AssignmentBucket: "parti-assignment",
		HandoffBucket:    "parti-handoff",
		WorkerIDTTL:      75 * time.Second,
		ElectionTimeout:  10 * time.Second,
		HeartbeatTTL:     15 * time.Second,
		AssignmentTTL:    0,
		HandoffTTL:       2 * time.Minute,
	}
}

func TestValidate_TableDriven(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(c *provision.Config)
		wantErr bool
		// errSubstr is checked against err.Error() to ensure the test
		// fails for the right reason.
		errSubstr string
	}{
		{
			name:    "valid_minimal_control_plane",
			mutate:  func(*provision.Config) {},
			wantErr: false,
		},
		{
			name: "apiversion_mismatch_rejected",
			mutate: func(c *provision.Config) {
				c.APIVersion = "parti.io/v2"
			},
			wantErr:   true,
			errSubstr: "apiVersion",
		},
		{
			name: "apiversion_empty_rejected",
			mutate: func(c *provision.Config) {
				c.APIVersion = ""
			},
			wantErr:   true,
			errSubstr: "apiVersion",
		},
		{
			name: "policy_safe_update_rejected",
			mutate: func(c *provision.Config) {
				c.Policy = "safe-update"
			},
			wantErr:   true,
			errSubstr: "safe-update",
		},
		{
			name: "policy_force_rejected",
			mutate: func(c *provision.Config) {
				c.Policy = "force"
			},
			wantErr:   true,
			errSubstr: "force",
		},
		{
			name: "policy_unknown_rejected",
			mutate: func(c *provision.Config) {
				c.Policy = "yolo"
			},
			wantErr:   true,
			errSubstr: "policy",
		},
		{
			name: "policy_warn_explicit_accepted",
			mutate: func(c *provision.Config) {
				c.Policy = provision.PolicyWarn
			},
			wantErr: false,
		},
		{
			name: "zero_workerid_ttl_rejected",
			mutate: func(c *provision.Config) {
				c.ControlPlane.WorkerIDTTL = 0
			},
			wantErr:   true,
			errSubstr: "workerIdTtl",
		},
		{
			name: "zero_election_timeout_rejected",
			mutate: func(c *provision.Config) {
				c.ControlPlane.ElectionTimeout = 0
			},
			wantErr:   true,
			errSubstr: "electionTimeout",
		},
		{
			name: "zero_heartbeat_ttl_rejected",
			mutate: func(c *provision.Config) {
				c.ControlPlane.HeartbeatTTL = 0
			},
			wantErr:   true,
			errSubstr: "heartbeatTtl",
		},
		{
			name: "zero_assignment_ttl_accepted",
			mutate: func(c *provision.Config) {
				c.ControlPlane.AssignmentTTL = 0
			},
			wantErr: false,
		},
		{
			name: "negative_assignment_ttl_rejected",
			mutate: func(c *provision.Config) {
				c.ControlPlane.AssignmentTTL = -1
			},
			wantErr:   true,
			errSubstr: "assignmentTtl",
		},
		{
			name: "handoff_enabled_without_ttl_rejected",
			mutate: func(c *provision.Config) {
				c.ControlPlane.EnableTwoPhaseHandoff = true
				c.ControlPlane.HandoffTTL = 0
			},
			wantErr:   true,
			errSubstr: "handoffTtl",
		},
		{
			name: "handoff_enabled_with_ttl_accepted",
			mutate: func(c *provision.Config) {
				c.ControlPlane.EnableTwoPhaseHandoff = true
				c.ControlPlane.HandoffTTL = 2 * time.Minute
			},
			wantErr: false,
		},
		{
			name: "empty_bucket_names_filled_by_defaults",
			mutate: func(c *provision.Config) {
				c.ControlPlane.StableIDBucket = ""
				c.ControlPlane.ElectionBucket = ""
				c.ControlPlane.HeartbeatBucket = ""
				c.ControlPlane.AssignmentBucket = ""
				c.ControlPlane.HandoffBucket = ""
			},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := provision.Config{
				APIVersion:   provision.APIVersionV1,
				ControlPlane: validControlPlane(),
			}
			tc.mutate(&cfg)

			err := provision.Validate(cfg)
			if tc.wantErr {
				require.Error(t, err)
				require.ErrorIs(t, err, provision.ErrInvalidConfig)
				if tc.errSubstr != "" {
					require.Contains(t, err.Error(), tc.errSubstr)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidate_DoesNotMutateCaller(t *testing.T) {
	t.Parallel()
	cfg := provision.Config{
		APIVersion: provision.APIVersionV1,
		ControlPlane: &provision.ControlPlaneConfig{
			WorkerIDTTL:     75 * time.Second,
			ElectionTimeout: 10 * time.Second,
			HeartbeatTTL:    15 * time.Second,
			// Bucket names intentionally empty so defaults apply.
		},
	}
	err := provision.Validate(cfg)
	require.NoError(t, err)
	// Caller's pointer target must remain empty.
	require.Equal(t, "", cfg.ControlPlane.StableIDBucket)
	require.Equal(t, "", cfg.ControlPlane.ElectionBucket)
	require.Equal(t, "", cfg.ControlPlane.HeartbeatBucket)
	require.Equal(t, "", cfg.ControlPlane.AssignmentBucket)
	require.Equal(t, "", cfg.ControlPlane.HandoffBucket)
	require.Equal(t, provision.ReconcilePolicy(""), cfg.Policy)
}

func TestApplyDefaults_FillsBucketsAndPolicy(t *testing.T) {
	t.Parallel()
	cfg := provision.Config{
		APIVersion: provision.APIVersionV1,
		ControlPlane: &provision.ControlPlaneConfig{
			WorkerIDTTL:     75 * time.Second,
			ElectionTimeout: 10 * time.Second,
			HeartbeatTTL:    15 * time.Second,
		},
	}
	got, err := provision.ApplyDefaults(cfg)
	require.NoError(t, err)
	require.Equal(t, provision.PolicyWarn, got.Policy)
	require.Equal(t, "parti-stableid", got.ControlPlane.StableIDBucket)
	require.Equal(t, "parti-election", got.ControlPlane.ElectionBucket)
	require.Equal(t, "parti-heartbeat", got.ControlPlane.HeartbeatBucket)
	require.Equal(t, "parti-assignment", got.ControlPlane.AssignmentBucket)
	require.Equal(t, "parti-handoff", got.ControlPlane.HandoffBucket)
}

func TestApplyDefaults_RejectsAPIVersionMismatch(t *testing.T) {
	t.Parallel()
	_, err := provision.ApplyDefaults(provision.Config{APIVersion: "bogus"})
	require.Error(t, err)
	require.ErrorIs(t, err, provision.ErrInvalidConfig)
}

func TestErrInvalidConfig_Sentinel(t *testing.T) {
	t.Parallel()
	err := provision.Validate(provision.Config{APIVersion: ""})
	require.True(t, errors.Is(err, provision.ErrInvalidConfig))
}

// TestValidate_FullYAMLFixture verifies the canonical "Full" YAML example
// from the plan unmarshals into Config and passes Validate. It also
// double-checks every nested field is populated via the declared YAML tag
// (the test fixture uses distinct values per field so any tag mistake
// surfaces as a zero-value comparison).
func TestValidate_FullYAMLFixture(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "full.yaml"))
	require.NoError(t, err)

	var cfg provision.Config
	require.NoError(t, yaml.Unmarshal(raw, &cfg))

	require.NoError(t, provision.Validate(cfg))

	// Spot-check every field is populated via its declared YAML tag.
	require.Equal(t, provision.APIVersionV1, cfg.APIVersion)
	require.Equal(t, "prod-us-east", cfg.Instance)
	require.Equal(t, provision.PolicyWarn, cfg.Policy)

	require.NotNil(t, cfg.ControlPlane)
	require.Equal(t, "parti-stableid", cfg.ControlPlane.StableIDBucket)
	require.Equal(t, "parti-election", cfg.ControlPlane.ElectionBucket)
	require.Equal(t, "parti-heartbeat", cfg.ControlPlane.HeartbeatBucket)
	require.Equal(t, "parti-assignment", cfg.ControlPlane.AssignmentBucket)
	require.Equal(t, "parti-handoff", cfg.ControlPlane.HandoffBucket)
	require.Equal(t, 75*time.Second, cfg.ControlPlane.WorkerIDTTL)
	require.Equal(t, 10*time.Second, cfg.ControlPlane.ElectionTimeout)
	require.Equal(t, 15*time.Second, cfg.ControlPlane.HeartbeatTTL)
	require.Equal(t, 90*time.Second, cfg.ControlPlane.AssignmentTTL)
	require.Equal(t, 2*time.Minute, cfg.ControlPlane.HandoffTTL)
	require.True(t, cfg.ControlPlane.EnableTwoPhaseHandoff)

	require.NotNil(t, cfg.PartitionSource)
	require.Equal(t, "parti-partitions", cfg.PartitionSource.Bucket)
	require.Equal(t, "partitions/v1", cfg.PartitionSource.Key)
	require.Equal(t, 3, cfg.PartitionSource.Replicas)
	require.Equal(t, "file", cfg.PartitionSource.Storage)
	require.Equal(t, uint8(1), cfg.PartitionSource.History)
	require.Equal(t, int32(1048576), cfg.PartitionSource.MaxValueSize)
	require.Equal(t, 7*time.Minute, cfg.PartitionSource.TTL)

	require.Len(t, cfg.DynamicConsumers, 1)
	dc := cfg.DynamicConsumers[0]
	require.Equal(t, "orders", dc.StreamName)
	require.Equal(t, "ord-worker", dc.ConsumerPrefix)
	require.Equal(t, "orders.{partition_id}.>", dc.SubjectTemplate)
	require.Equal(t, "./partitions.yaml", dc.PartitionsRef)
}

// TestValidate_MinimalYAMLFixture covers the minimal example from the plan
// (control-plane block with only the three required TTLs; bucket names
// defaulted) — must pass validation after defaulting.
func TestValidate_MinimalYAMLFixture(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "minimal.yaml"))
	require.NoError(t, err)

	var cfg provision.Config
	require.NoError(t, yaml.Unmarshal(raw, &cfg))
	require.NoError(t, provision.Validate(cfg))

	// Bucket names should still be the empty strings as written
	// (Validate does not mutate the caller); defaults appear only on the
	// internal copy.
	require.Equal(t, "", cfg.ControlPlane.StableIDBucket)
}

// validPartitionSource returns a fully-specified PartitionSourceConfig that
// passes Validate. Tests mutate copies of this.
func validPartitionSource() *provision.PartitionSourceConfig {
	return &provision.PartitionSourceConfig{
		Bucket:       "parti-partitions",
		Key:          "partitions/v1",
		Storage:      "file",
		History:      1,
		Replicas:     1,
		MaxValueSize: 1048576,
		TTL:          0,
	}
}

func TestValidate_PartitionSource_TableDriven(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(*provision.PartitionSourceConfig)
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "valid_full_config",
			mutate:  func(*provision.PartitionSourceConfig) {},
			wantErr: false,
		},
		{
			name: "empty_bucket_rejected",
			mutate: func(ps *provision.PartitionSourceConfig) {
				ps.Bucket = ""
			},
			wantErr:   true,
			errSubstr: "partitionSource.bucket",
		},
		{
			name: "empty_key_rejected",
			mutate: func(ps *provision.PartitionSourceConfig) {
				ps.Key = ""
			},
			wantErr:   true,
			errSubstr: "partitionSource.key",
		},
		{
			name: "invalid_storage_rejected",
			mutate: func(ps *provision.PartitionSourceConfig) {
				ps.Storage = "disk"
			},
			wantErr:   true,
			errSubstr: "partitionSource.storage",
		},
		{
			name: "storage_file_accepted",
			mutate: func(ps *provision.PartitionSourceConfig) {
				ps.Storage = "file"
			},
			wantErr: false,
		},
		{
			name: "storage_memory_accepted",
			mutate: func(ps *provision.PartitionSourceConfig) {
				ps.Storage = "memory"
			},
			wantErr: false,
		},
		{
			name: "negative_replicas_rejected",
			mutate: func(ps *provision.PartitionSourceConfig) {
				ps.Replicas = -1
			},
			wantErr:   true,
			errSubstr: "partitionSource.replicas",
		},
		{
			name: "zero_replicas_accepted",
			mutate: func(ps *provision.PartitionSourceConfig) {
				ps.Replicas = 0
			},
			wantErr: false,
		},
		{
			name: "negative_max_value_size_rejected",
			mutate: func(ps *provision.PartitionSourceConfig) {
				ps.MaxValueSize = -1
			},
			wantErr:   true,
			errSubstr: "partitionSource.maxValueSize",
		},
		{
			name: "zero_max_value_size_accepted",
			mutate: func(ps *provision.PartitionSourceConfig) {
				ps.MaxValueSize = 0
			},
			wantErr: false,
		},
		{
			name: "negative_ttl_rejected",
			mutate: func(ps *provision.PartitionSourceConfig) {
				ps.TTL = -1
			},
			wantErr:   true,
			errSubstr: "partitionSource.ttl",
		},
		{
			name: "zero_ttl_accepted",
			mutate: func(ps *provision.PartitionSourceConfig) {
				ps.TTL = 0
			},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ps := validPartitionSource()
			tc.mutate(ps)
			cfg := provision.Config{
				APIVersion:      provision.APIVersionV1,
				PartitionSource: ps,
			}
			err := provision.Validate(cfg)
			if tc.wantErr {
				require.Error(t, err)
				require.ErrorIs(t, err, provision.ErrInvalidConfig)
				if tc.errSubstr != "" {
					require.Contains(t, err.Error(), tc.errSubstr)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestApplyDefaults_PartitionSource verifies that normalize() applies the
// storage and history defaults and that the caller's struct is not mutated.
func TestApplyDefaults_PartitionSource(t *testing.T) {
	t.Parallel()

	ps := &provision.PartitionSourceConfig{
		Bucket: "parti-partitions",
		Key:    "partitions/v1",
		// Storage and History intentionally zero to trigger defaults.
	}
	cfg := provision.Config{
		APIVersion:      provision.APIVersionV1,
		PartitionSource: ps,
	}

	got, err := provision.ApplyDefaults(cfg)
	require.NoError(t, err)

	// Defaults applied on the copy.
	require.Equal(t, "file", got.PartitionSource.Storage)
	require.Equal(t, uint8(1), got.PartitionSource.History)

	// Caller's struct must not be mutated.
	require.Equal(t, "", ps.Storage, "caller struct must not be mutated")
	require.Equal(t, uint8(0), ps.History, "caller struct must not be mutated")
}
