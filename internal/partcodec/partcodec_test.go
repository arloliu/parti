package partcodec

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"testing"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// goldenPartitions is the fixed input used for wire-format golden tests.
var goldenPartitions = []types.Partition{
	{Keys: []string{"topic-a", "0"}, Weight: 3},
	{Keys: []string{"topic-b", "1"}, Weight: 0},
}

// goldenJSON is the expected gunzipped JSON for goldenPartitions.
// Field order is struct-defined (keys then weight) and Go-version-stable.
const goldenJSON = `[{"keys":["topic-a","0"],"weight":3},{"keys":["topic-b","1"],"weight":0}]`

// TestEncode_GoldenWireFormat verifies that Encode produces gzip output whose
// gunzipped content is byte-identical to the expected JSON. This is the wire-
// format tripwire: any accidental field-order or struct change breaks it.
func TestEncode_GoldenWireFormat(t *testing.T) {
	encoded, err := Encode(goldenPartitions)
	require.NoError(t, err)

	// Output must begin with gzip magic bytes.
	require.GreaterOrEqual(t, len(encoded), 2)
	require.Equal(t, byte(0x1f), encoded[0], "expected gzip magic byte 0")
	require.Equal(t, byte(0x8b), encoded[1], "expected gzip magic byte 1")

	// Gunzip and compare byte-for-byte against the golden JSON string.
	gr, err := gzip.NewReader(bytes.NewReader(encoded))
	require.NoError(t, err)
	defer gr.Close()

	gunzipped, err := io.ReadAll(gr)
	require.NoError(t, err)
	require.Equal(t, goldenJSON, string(gunzipped))
}

// TestRoundTrip verifies that Decode(Encode(x)) deep-equals x for various inputs.
func TestRoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		input []types.Partition
	}{
		{
			name:  "empty slice",
			input: []types.Partition{},
		},
		{
			name: "single partition",
			input: []types.Partition{
				{Keys: []string{"k1"}, Weight: 10},
			},
		},
		{
			name:  "multi-record with multi-key and zero weight",
			input: goldenPartitions,
		},
		{
			name: "three records",
			input: []types.Partition{
				{Keys: []string{"a", "b"}, Weight: 1},
				{Keys: []string{"c"}, Weight: 5},
				{Keys: []string{"d", "e", "f"}, Weight: 0},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := Encode(tc.input)
			require.NoError(t, err)

			decoded, err := Decode(encoded)
			require.NoError(t, err)

			if len(tc.input) == 0 {
				require.Empty(t, decoded)
			} else {
				require.Equal(t, tc.input, decoded)
			}
		})
	}
}

// TestDecode_DualFormat verifies that Decode accepts both plain-JSON and
// gzip-compressed payloads, returning equal results for both.
func TestDecode_DualFormat(t *testing.T) {
	want := goldenPartitions

	// Build plain-JSON payload.
	plainJSON, err := json.Marshal(want)
	require.NoError(t, err)

	// Build gzip-compressed payload via Encode.
	gzipped, err := Encode(want)
	require.NoError(t, err)

	fromPlain, err := Decode(plainJSON)
	require.NoError(t, err)

	fromGzip, err := Decode(gzipped)
	require.NoError(t, err)

	require.Equal(t, fromPlain, fromGzip)
	require.Equal(t, want, fromPlain)
}

// TestDecode_DuplicateCanonicalID verifies that Decode rejects a payload
// containing two records with identical Keys (duplicate CanonicalID).
func TestDecode_DuplicateCanonicalID(t *testing.T) {
	dup := []types.Partition{
		{Keys: []string{"a", "b"}, Weight: 1},
		{Keys: []string{"a", "b"}, Weight: 2},
	}

	// Encode without validation to get a gzip payload with duplicates.
	data, err := json.Marshal(dup)
	require.NoError(t, err)

	_, err = Decode(data)
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate partition")
}

// TestDecode_InvalidRecord verifies that Decode rejects a payload containing
// a record that fails Partition.Validate — an empty key or a key with a dot.
func TestDecode_InvalidRecord(t *testing.T) {
	cases := []struct {
		name        string
		partitions  []types.Partition
		errContains string
	}{
		{
			name:        "empty key",
			partitions:  []types.Partition{{Keys: []string{""}}},
			errContains: "invalid partition",
		},
		{
			name:        "key with dot",
			partitions:  []types.Partition{{Keys: []string{"a.b"}}},
			errContains: "invalid partition",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.partitions)
			require.NoError(t, err)

			_, err = Decode(data)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.errContains)
		})
	}
}

// TestDecode_EmptyInput verifies that Decode returns a non-nil empty slice
// for nil and zero-length inputs.
func TestDecode_EmptyInput(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Decode(tc.data)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Empty(t, result)
		})
	}
}
