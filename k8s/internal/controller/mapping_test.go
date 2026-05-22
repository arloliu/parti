package controller

import (
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/arloliu/parti/v2/k8s/api/v1alpha1"
	"github.com/arloliu/parti/v2/provision"
	"github.com/stretchr/testify/require"
)

// mappingAllowlist defines exported fields in provision.Config and its nested
// structs that are deliberately NOT mapped by toProvisionConfig. These are
// documented Non-Goals for the operator. Any field listed here that starts
// returning a non-zero value without an explicit assignment would indicate a
// regression — keep this list minimal and commented.
var mappingAllowlist = map[string]struct{}{
	// provision.Config.DynamicConsumers — operator never manages consumers;
	// they are runtime-owned (Phase 5 model).
	"Config.DynamicConsumers": {},

	// provision.PartitionSourceConfig.Partitions — partition-record contents
	// are managed by partictl, not the operator (Non-Goal per plan W2).
	"PartitionSourceConfig.Partitions": {},
}

// allSetSpec builds a ProvisionedPartiEnvSpec with every field set to a
// distinctive non-zero value. The goal is for toProvisionConfig to produce a
// provision.Config where every non-allowlisted exported field is non-zero,
// proving that the mapping covers all fields.
//
// Strings use their JSON field name; ints use small primes; durations use
// distinct multiples of a second so they are individually identifiable.
func allSetSpec() v1alpha1.ProvisionedPartiEnvSpec {
	return v1alpha1.ProvisionedPartiEnvSpec{
		NATS: v1alpha1.NATSConnection{
			Server: "nats://test:4222",
			CredentialsSecret: &v1alpha1.NATSAuthSecret{
				Name:           "test-secret",
				CredentialsKey: "creds",
			},
		},
		Instance: "test-instance",
		// Use "force" so Policy is non-empty; the "empty stays empty" check is
		// a separate per-field test.
		Policy: "force",
		ControlPlane: &v1alpha1.ControlPlaneSpec{
			StableIDBucket:        "stableIdBucket",
			ElectionBucket:        "electionBucket",
			HeartbeatBucket:       "heartbeatBucket",
			AssignmentBucket:      "assignmentBucket",
			HandoffBucket:         "handoffBucket",
			WorkerIDTTL:           metav1.Duration{Duration: 11 * time.Second},
			ElectionTimeout:       metav1.Duration{Duration: 13 * time.Second},
			HeartbeatTTL:          metav1.Duration{Duration: 17 * time.Second},
			AssignmentTTL:         metav1.Duration{Duration: 19 * time.Second},
			HandoffTTL:            metav1.Duration{Duration: 23 * time.Second},
			EnableTwoPhaseHandoff: true,
			Replicas:              3,
			AllowDeleteRecreate:   true,
		},
		PartitionSource: &v1alpha1.PartitionSourceSpec{
			Bucket:              "partition-bucket",
			Key:                 "partition-key",
			Replicas:            2,
			Storage:             "file",
			History:             5,
			MaxValueSize:        1024,
			TTL:                 metav1.Duration{Duration: 29 * time.Second},
			AllowDeleteRecreate: true,
		},
		Streams: []v1alpha1.StreamSpec{
			{
				Name:                "stream-name",
				Subjects:            []string{"subject.>"},
				Retention:           "limits",
				Storage:             "file",
				Discard:             "old",
				Replicas:            3,
				MaxAge:              metav1.Duration{Duration: 31 * time.Second},
				MaxBytes:            1000000,
				MaxMsgs:             10000,
				Description:         "test stream",
				AllowDeleteRecreate: true,
			},
		},
	}
}

// TestMappingCompleteness is the load-bearing schema-drift guard.
//
// It constructs a fully-populated ProvisionedPartiEnvSpec, maps it to a
// provision.Config, then reflection-walks every exported field of the result
// and asserts each is non-zero — except the explicit allowlist of documented
// Non-Goals. A new field added to provision.Config or a nested type that the
// mapper does not carry will stay zero and fail this test, forcing a conscious
// decision: map it or add it to the allowlist with a reason.
func TestMappingCompleteness(t *testing.T) {
	t.Parallel()

	spec := allSetSpec()
	cfg, err := toProvisionConfig(spec)
	require.NoError(t, err)

	var zeroFields []string
	checkNonZero(reflect.ValueOf(cfg), "Config", mappingAllowlist, &zeroFields)

	if len(zeroFields) > 0 {
		t.Errorf("toProvisionConfig left %d exported field(s) at their zero value — either map them or add them to the allowlist:\n", len(zeroFields))
		for _, f := range zeroFields {
			t.Errorf("  zero field: %s", f)
		}
	}
}

// checkNonZero recursively walks v, which must be a struct (or pointer to one,
// or slice of them). For each exported struct field it checks whether the value
// is zero. Fields named in the allowlist by "TypeName.FieldName" are skipped.
func checkNonZero(v reflect.Value, typeName string, allowlist map[string]struct{}, zeroFields *[]string) {
	// Dereference pointers.
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			*zeroFields = append(*zeroFields, typeName+" (nil pointer)")
			return
		}
		v = v.Elem()
	}

	if v.Kind() != reflect.Struct {
		return
	}

	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}

		fv := v.Field(i)
		key := typeName + "." + f.Name

		if _, skip := allowlist[key]; skip {
			continue
		}

		switch {
		case fv.Kind() == reflect.Ptr:
			if fv.IsNil() {
				*zeroFields = append(*zeroFields, key+" (nil pointer)")
			} else {
				nestedTypeName := fv.Elem().Type().Name()
				checkNonZero(fv, nestedTypeName, allowlist, zeroFields)
			}

		case fv.Kind() == reflect.Slice:
			if fv.IsNil() || fv.Len() == 0 {
				*zeroFields = append(*zeroFields, key+" (nil/empty slice)")
			} else {
				// Walk first element if it is a struct.
				elem := fv.Index(0)
				if elem.Kind() == reflect.Struct {
					elemTypeName := elem.Type().Name()
					checkNonZero(elem, elemTypeName, allowlist, zeroFields)
				}
			}

		case fv.Kind() == reflect.Struct:
			// Recurse into nested structs (e.g. time.Duration is an int64
			// underneath; the reflect package sees it as a struct wrapper).
			// For simple structs, use IsZero to detect the zero value.
			if fv.IsZero() {
				*zeroFields = append(*zeroFields, key+" (zero struct)")
			} else {
				nestedTypeName := fv.Type().Name()
				checkNonZero(fv, nestedTypeName, allowlist, zeroFields)
			}

		default:
			if fv.IsZero() {
				*zeroFields = append(*zeroFields, key)
			}
		}
	}
}

// TestMapping_MetaDuration verifies that metav1.Duration fields are converted
// to time.Duration via .Duration (not zero-valued).
func TestMapping_MetaDuration(t *testing.T) {
	t.Parallel()

	d := 42 * time.Second
	spec := v1alpha1.ProvisionedPartiEnvSpec{
		ControlPlane: &v1alpha1.ControlPlaneSpec{
			WorkerIDTTL: metav1.Duration{Duration: d},
		},
		PartitionSource: &v1alpha1.PartitionSourceSpec{
			TTL: metav1.Duration{Duration: d},
		},
		Streams: []v1alpha1.StreamSpec{
			{
				Name:     "s",
				Subjects: []string{"foo"},
				MaxAge:   metav1.Duration{Duration: d},
			},
		},
	}

	cfg, err := toProvisionConfig(spec)
	require.NoError(t, err)
	require.Equal(t, d, cfg.ControlPlane.WorkerIDTTL)
	require.Equal(t, d, cfg.PartitionSource.TTL)
	require.Equal(t, d, cfg.Streams[0].MaxAge)
}

// TestMapping_NilPointers verifies that nil ControlPlane and PartitionSource
// specs produce nil config pointers (not zero-valued structs).
func TestMapping_NilPointers(t *testing.T) {
	t.Parallel()

	spec := v1alpha1.ProvisionedPartiEnvSpec{
		ControlPlane:    nil,
		PartitionSource: nil,
	}

	cfg, err := toProvisionConfig(spec)
	require.NoError(t, err)
	require.Nil(t, cfg.ControlPlane)
	require.Nil(t, cfg.PartitionSource)
}

// TestMapping_EmptyPolicyStaysEmpty verifies that an empty Policy string maps
// to an empty ReconcilePolicy (the SDK defaults it to PolicyWarn on Validate).
func TestMapping_EmptyPolicyStaysEmpty(t *testing.T) {
	t.Parallel()

	spec := v1alpha1.ProvisionedPartiEnvSpec{
		Policy: "",
	}

	cfg, err := toProvisionConfig(spec)
	require.NoError(t, err)
	require.Equal(t, provision.ReconcilePolicy(""), cfg.Policy)
}

// TestMapping_APIVersion verifies that APIVersion is stamped to APIVersionV1
// ("parti.io/v1") regardless of what the CRD spec says.
func TestMapping_APIVersion(t *testing.T) {
	t.Parallel()

	spec := v1alpha1.ProvisionedPartiEnvSpec{}
	cfg, err := toProvisionConfig(spec)
	require.NoError(t, err)
	require.Equal(t, provision.APIVersionV1, cfg.APIVersion)
}

// TestMapping_HistoryNarrowing covers the boundary conditions for the
// int32 → uint8 narrowing guard on PartitionSourceSpec.History.
func TestMapping_HistoryNarrowing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		history   int32
		wantErr   bool
		wantUint8 uint8
	}{
		{
			name:      "zero_is_valid",
			history:   0,
			wantErr:   false,
			wantUint8: 0,
		},
		{
			name:      "max_uint8_255_maps_cleanly",
			history:   255,
			wantErr:   false,
			wantUint8: 255,
		},
		{
			name:    "256_returns_error_never_wraps",
			history: 256,
			wantErr: true,
		},
		{
			name:    "negative_returns_error",
			history: -1,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spec := v1alpha1.ProvisionedPartiEnvSpec{
				PartitionSource: &v1alpha1.PartitionSourceSpec{
					History: tc.history,
				},
			}

			cfg, err := toProvisionConfig(spec)
			if tc.wantErr {
				require.Error(t, err)
				// Ensure History=256 does NOT silently wrap to uint8(0).
				if tc.history == 256 {
					if err == nil {
						// This would be a bug: uint8(256) == 0, indistinguishable
						// from "no history set".
						t.Fatal("expected error for History=256, got nil — narrowing wrap not caught")
					}
				}
			} else {
				require.NoError(t, err)
				require.NotNil(t, cfg.PartitionSource)
				require.Equal(t, tc.wantUint8, cfg.PartitionSource.History)
			}
		})
	}
}
