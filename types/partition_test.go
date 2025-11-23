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
