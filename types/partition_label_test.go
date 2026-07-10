package types_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

func TestPartitionLabel_IdentityBlind(t *testing.T) {
	t.Parallel()

	plain := types.Partition{Keys: []string{"topic", "42"}, Weight: 3}
	vip := types.Partition{Keys: []string{"topic", "42"}, Weight: 3, Label: "vip"}

	require.Equal(t, plain.CanonicalID(), vip.CanonicalID(), "CanonicalID must be label-blind")
	require.Equal(t, plain.HashID(), vip.HashID(), "HashID must be label-blind")
	require.Equal(t, plain.HashIDSeed(7), vip.HashIDSeed(7), "HashIDSeed must be label-blind")
	require.Zero(t, plain.Compare(vip), "Compare must be label-blind")
	require.Equal(t,
		types.PartitionSetDigest([]types.Partition{plain}),
		types.PartitionSetDigest([]types.Partition{vip}),
		"PartitionSetDigest must be label-blind")
}

func TestPartitionLabel_Validate(t *testing.T) {
	t.Parallel()

	valid := func(label string) error {
		return types.Partition{Keys: []string{"k"}, Label: label}.Validate()
	}

	require.NoError(t, valid(""), "empty label = unlabeled, valid")
	require.NoError(t, valid("vip"))
	require.NoError(t, valid("gpu-batch_2"))

	require.Error(t, valid("has space"))
	require.Error(t, valid("has\ttab"))
	require.Error(t, valid("dotted.label"))
	require.Error(t, valid(strings.Repeat("x", 65)), "over 64-byte cap")
	require.NoError(t, valid(strings.Repeat("x", 64)), "exactly 64 bytes ok")
}

func TestValidateLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		label   string
		wantErr bool
	}{
		{"empty is valid", "", false},
		{"simple", "vip", false},
		{"charset ok", "gpu-batch_2", false},
		{"exactly 64 bytes", strings.Repeat("x", 64), false},
		{"over 64 bytes", strings.Repeat("x", 65), true},
		{"multibyte over 64 bytes", strings.Repeat("é", 33), true}, // 66 bytes, 33 runes
		{"dot", "a.b", true},
		{"space", "a b", true},
		{"tab", "a\tb", true},
		{"newline", "a\nb", true},
		{"carriage return", "a\rb", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := types.ValidateLabel(tt.label)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}

	// Partition.Validate delegates its label rules to ValidateLabel: a label
	// that ValidateLabel rejects must also fail Partition.Validate, and the
	// surfaced error must be the shared ValidateLabel error verbatim.
	for _, bad := range []string{"a.b", "a b", strings.Repeat("x", 65)} {
		want := types.ValidateLabel(bad)
		require.Error(t, want)
		got := types.Partition{Keys: []string{"k"}, Label: bad}.Validate()
		require.EqualError(t, got, want.Error(),
			"Partition.Validate must surface the ValidateLabel error for %q", bad)
	}
}

func TestPartitionLabel_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	p := types.Partition{Keys: []string{"a"}, Weight: 2, Label: "vip"}
	b, err := json.Marshal(p)
	require.NoError(t, err)
	require.Contains(t, string(b), `"label":"vip"`)

	var back types.Partition
	require.NoError(t, json.Unmarshal(b, &back))
	require.Equal(t, p, back)

	// omitempty: unlabeled partitions marshal without the field, so the
	// wire bytes of existing label-free lists are unchanged.
	b2, err := json.Marshal(types.Partition{Keys: []string{"a"}})
	require.NoError(t, err)
	require.NotContains(t, string(b2), "label")
}
