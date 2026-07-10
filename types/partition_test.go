package types

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/xxh3"
)

func TestPartitionSubjectKey(t *testing.T) {
	t.Parallel()

	// single-case direct test
	p := Partition{Keys: []string{"topic", "p", "42"}}
	require.Equal(t, "topic.p.42", p.SubjectKey())

	// empty keys
	p2 := Partition{}
	require.Equal(t, "", p2.SubjectKey())
}

func TestPartitionValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		partition Partition
		wantErr   bool
	}{
		{
			name:      "Valid",
			partition: Partition{Keys: []string{"topic", "p", "42"}},
			wantErr:   false,
		},
		{
			name:      "InvalidDot",
			partition: Partition{Keys: []string{"topic.name", "p"}},
			wantErr:   true,
		},
		{
			name:      "InvalidSpace",
			partition: Partition{Keys: []string{"topic", "p "}},
			wantErr:   true,
		},
		{
			name:      "InvalidTab",
			partition: Partition{Keys: []string{"topic\t", "p"}},
			wantErr:   true,
		},
		{
			name:      "InvalidEmptyKey",
			partition: Partition{Keys: []string{"topic", ""}},
			wantErr:   true,
		},
		{
			name:      "EmptyPartition",
			partition: Partition{},
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.partition.Validate()
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestPartitionID(t *testing.T) {
	t.Parallel()

	p := Partition{Keys: []string{"topic", "p", "42"}}
	require.Equal(t, "topic-p-42", p.ID())

	p2 := Partition{}
	require.Equal(t, "", p2.ID())
}

func TestPartitionCompare(t *testing.T) {
	t.Parallel()

	tests := []struct {
		a    Partition
		b    Partition
		want int
	}{
		{Partition{Keys: []string{"a"}}, Partition{Keys: []string{"a"}}, 0},
		{Partition{Keys: []string{"a"}}, Partition{Keys: []string{"b"}}, -1},
		{Partition{Keys: []string{"b"}}, Partition{Keys: []string{"a"}}, 1},
		{Partition{Keys: []string{"a"}}, Partition{Keys: []string{"a", "x"}}, -1},
		{Partition{Keys: []string{"a", "x"}}, Partition{Keys: []string{"a"}}, 1},
		{Partition{Keys: []string{"a", "b"}}, Partition{Keys: []string{"a", "c"}}, -1},
		{Partition{Keys: []string{"a", "d"}}, Partition{Keys: []string{"a", "c"}}, 1},
	}

	for _, tt := range tests {
		got := tt.a.Compare(tt.b)
		switch tt.want {
		case 0:
			require.Equal(t, 0, got)
		case -1:
			require.Less(t, got, 0)
		case 1:
			require.Greater(t, got, 0)
		default:
			t.Fatalf("invalid test case want: %d", tt.want)
		}
	}
}

func TestPartitionHashID(t *testing.T) {
	t.Parallel()

	// Deterministic and equal for identical keys order
	p1 := Partition{Keys: []string{"topic", "p", "42"}}
	p2 := Partition{Keys: []string{"topic", "p", "42"}}
	require.Equal(t, p1.HashID(), p2.HashID())

	// Different for different boundaries (no ambiguity)
	p3 := Partition{Keys: []string{"ab", "c"}}
	p4 := Partition{Keys: []string{"a", "bc"}}
	require.NotEqual(t, p3.HashID(), p4.HashID())

	// Empty keys returns 0
	pEmpty := Partition{}
	require.EqualValues(t, 0, pEmpty.HashID())

	// Seeded vs unseeded behavior: seed=0 equals HashID; non-zero seed alters hash.
	base := p1.HashID()
	seeded := p1.HashIDSeed(12345)
	require.Equal(t, base, p1.HashIDSeed(0))
	require.NotEqual(t, base, seeded)
}

func TestPartitionCanonicalID_NoTupleCollision(t *testing.T) {
	t.Parallel()

	// These two partitions produce the same ID() ("a-b-c") but must have
	// distinct CanonicalIDs.
	p1 := Partition{Keys: []string{"a-b", "c"}}
	p2 := Partition{Keys: []string{"a", "b-c"}}

	// ID() collision pair — both produce "a-b-c"
	require.Equal(t, "a-b-c", p1.ID())
	require.Equal(t, "a-b-c", p2.ID())

	// CanonicalID() must NOT collide
	cid1 := p1.CanonicalID()
	cid2 := p2.CanonicalID()
	require.NotEqual(t, cid1, cid2, "CanonicalID must distinguish tuple boundaries")

	// Exact expected encodings from the spec
	require.Equal(t, "3:a-b/1:c", cid1)
	require.Equal(t, "1:a/3:b-c", cid2)

	// Empty keys → empty string (symmetric with ID())
	pEmpty := Partition{}
	require.Equal(t, "", pEmpty.CanonicalID())

	// Single key
	pSingle := Partition{Keys: []string{"abc"}}
	require.Equal(t, "3:abc", pSingle.CanonicalID())

	// §3.4 "anywhere correctness depends on tuple identity" check:
	// Two partitions with colliding ID() must produce distinct results when
	// their CanonicalIDs are used as set-equality keys or joined/hashed.
	require.NotEqual(t, xxh3.HashString(cid1), xxh3.HashString(cid2),
		"xxh3(CanonicalID) must be distinct for tuple-boundary-distinct partitions")

	// Joined with newline separator (as in multi-partition set digests): also distinct.
	joined1 := cid1 + "\n" + cid2
	joined2 := cid2 + "\n" + cid1
	require.NotEqual(t, xxh3.HashString(joined1), xxh3.HashString(joined2),
		"join order of CanonicalIDs must produce distinct hashes (set-ordering matters)")
}

// Benchmarks

// BenchmarkPartitionHashID measures chained hashing over keys.
func BenchmarkPartitionHashID(b *testing.B) {
	parts := []Partition{
		{Keys: []string{"topic", "p", "0"}},
		{Keys: []string{"topic", "p", "1"}},
		{Keys: []string{"topic", "p", "2"}},
		{Keys: []string{"topic", "p", "3"}},
		{Keys: []string{"topic", "p", "4"}},
	}

	var sink uint64
	b.ResetTimer()
	for b.Loop() {
		for _, p := range parts {
			sink ^= p.HashID()
		}
	}

	_ = sink
}

// BenchmarkPartitionIDJoinHash measures strings.Join then HashString for comparison.
func TestValidatePartitions(t *testing.T) {
	t.Parallel()

	t.Run("valid set", func(t *testing.T) {
		t.Parallel()
		ps := []Partition{
			{Keys: []string{"a"}},
			{Keys: []string{"b"}, Label: "vip"},
			{Keys: []string{"c", "d"}},
		}
		require.NoError(t, ValidatePartitions(ps))
	})

	t.Run("empty and nil", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, ValidatePartitions(nil))
		require.NoError(t, ValidatePartitions([]Partition{}))
	})

	t.Run("invalid partition names index", func(t *testing.T) {
		t.Parallel()
		ps := []Partition{
			{Keys: []string{"ok"}},
			{Keys: []string{"bad.key"}},
		}
		err := ValidatePartitions(ps)
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid partition at index 1")
	})

	t.Run("empty-key partition", func(t *testing.T) {
		t.Parallel()
		err := ValidatePartitions([]Partition{{Keys: nil}})
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid partition at index 0")
	})

	t.Run("bad label delegates to ValidateLabel", func(t *testing.T) {
		t.Parallel()
		err := ValidatePartitions([]Partition{{Keys: []string{"a"}, Label: "has space"}})
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid partition at index 0")
		require.Contains(t, err.Error(), "label contains whitespace")
	})

	t.Run("duplicate CanonicalID names the id", func(t *testing.T) {
		t.Parallel()
		ps := []Partition{
			{Keys: []string{"a"}},
			{Keys: []string{"a"}}, // same CanonicalID
		}
		err := ValidatePartitions(ps)
		require.Error(t, err)
		require.Contains(t, err.Error(), "duplicate partition at index 1")
		require.Contains(t, err.Error(), ps[0].CanonicalID())
	})

	t.Run("dedupe keys on CanonicalID not ID", func(t *testing.T) {
		t.Parallel()
		// The ID()-collision pair ("a-b-c" both) has distinct CanonicalIDs, so
		// it is NOT a duplicate: ValidatePartitions must accept it, matching the
		// write path's CanonicalID dedupe.
		ps := []Partition{
			{Keys: []string{"a-b", "c"}},
			{Keys: []string{"a", "b-c"}},
		}
		require.Equal(t, ps[0].ID(), ps[1].ID())
		require.NoError(t, ValidatePartitions(ps))
	})

	t.Run("mixed failure: earlier duplicate outranks later invalid", func(t *testing.T) {
		t.Parallel()
		// index 0 valid, index 1 duplicates it, index 2 is invalid. The single
		// left-to-right pass must return the duplicate at index 1 and never
		// reach the invalid at index 2. A two-pass (validate-all-then-detect)
		// shape would flip this to the index-2 invalid error.
		ps := []Partition{
			{Keys: []string{"a"}},
			{Keys: []string{"a"}},       // duplicate at index 1
			{Keys: []string{"bad.key"}}, // invalid at index 2
		}
		err := ValidatePartitions(ps)
		require.Error(t, err)
		require.Contains(t, err.Error(), "duplicate partition at index 1")
		require.NotContains(t, err.Error(), "index 2")
	})

	t.Run("same index: Validate runs before the duplicate check", func(t *testing.T) {
		t.Parallel()
		// index 1 has the same CanonicalID as index 0 (labels are ID-blind) AND
		// an invalid label. The single pass must run Validate() first at index 1
		// and report the invalid label, not the duplicate — pinning per-index
		// ordering, not just cross-index ordering.
		ps := []Partition{
			{Keys: []string{"a"}},
			{Keys: []string{"a"}, Label: "bad label"}, // dup CanonicalID + invalid label
		}
		err := ValidatePartitions(ps)
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid partition at index 1")
		require.Contains(t, err.Error(), "label contains whitespace")
		require.NotContains(t, err.Error(), "duplicate")
	})
}

func BenchmarkPartitionIDJoinHash(b *testing.B) {
	parts := []Partition{
		{Keys: []string{"topic", "p", "0"}},
		{Keys: []string{"topic", "p", "1"}},
		{Keys: []string{"topic", "p", "2"}},
		{Keys: []string{"topic", "p", "3"}},
		{Keys: []string{"topic", "p", "4"}},
	}

	var sink uint64
	b.ResetTimer()
	for b.Loop() {
		for _, p := range parts {
			joined := strings.Join(p.Keys, "-")
			sink ^= xxh3.HashString(joined)
		}
	}

	_ = sink
}
