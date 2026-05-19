package dynamicbuild

import (
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/types"
)

func TestSanitizeConsumerName_ReplacesInvalidRunes(t *testing.T) {
	in := "prefix s/$ub ject!*"
	out := SanitizeConsumerName(in)
	require.NotContains(t, out, " ")
	require.NotContains(t, out, "/")
	require.NotContains(t, out, "$")
	require.NotContains(t, out, "!")
	for _, r := range out {
		require.True(t, IsAllowedConsumerRune(r), "rune %q not allowed", r)
	}
}

func TestIsAllowedConsumerRune(t *testing.T) {
	for _, r := range "abcXYZ089-_" {
		require.True(t, IsAllowedConsumerRune(r), "%q must be allowed", r)
	}
	for _, r := range " .!@#/\\$" {
		require.False(t, IsAllowedConsumerRune(r), "%q must NOT be allowed", r)
	}
}

func TestPerSubjectDurableName_StableAndDistinct(t *testing.T) {
	prefix := "dur"
	pre, suf := "work.", ""
	d1 := PerSubjectDurableName(prefix, "work.a.b", pre, suf)
	d2 := PerSubjectDurableName(prefix, "work.a.b", pre, suf)
	require.Equal(t, d1, d2)
	d3 := PerSubjectDurableName(prefix, "work.a.c", pre, suf)
	require.NotEqual(t, d1, d3)
}

func TestPerSubjectDurableName_FallbackToSubjectWhenFramingMissing(t *testing.T) {
	// No template framing → partID falls back to the full subject; format is
	// prefix_<sanitizedSubject>_<hash>.
	d := PerSubjectDurableName("dur", "work.a.b", "", "")
	require.True(t, strings.HasPrefix(d, "dur_work_a_b_"), "got %q", d)
}

func TestPerSubjectDurableName_TruncatesLongPartID(t *testing.T) {
	// 60-char partition id; framing is "p." prefix only.
	pid := strings.Repeat("x", 60)
	subject := "p." + pid
	d := PerSubjectDurableName("dur", subject, "p.", "")
	// Expect: dur_<50 x's>_<16hex>
	parts := strings.Split(d, "_")
	require.Len(t, parts, 3)
	require.Equal(t, "dur", parts[0])
	require.Len(t, parts[1], 50, "sanitizedPartID portion must be truncated to 50")
	require.Len(t, parts[2], 16, "hash portion must be 16 hex chars")
}

func TestPerSubjectDurableName_UnicodeSanitized(t *testing.T) {
	// Non-ASCII runes get replaced with underscore.
	d := PerSubjectDurableName("dur", "p.日本語", "p.", "")
	// The partition id "日本語" → sanitizes to "___" (3 underscores from 3 multi-byte runes).
	require.Contains(t, d, "dur____") // dur + _ separator + 3 underscores
}

func TestBuildSubjects_DedupeAndSort(t *testing.T) {
	parts := []types.Partition{
		{Keys: []string{"b", "2"}},
		{Keys: []string{"a", "1"}},
		{Keys: []string{"a", "1"}}, // duplicate
	}
	subs, err := BuildSubjects("events.{{.PartitionID}}", parts)
	require.NoError(t, err)
	require.Equal(t, []string{"events.a.1", "events.b.2"}, subs)
}

func TestBuildSubjects_EmptyPartitionsReturnsEmptySlice(t *testing.T) {
	subs, err := BuildSubjects("events.{{.PartitionID}}", nil)
	require.NoError(t, err)
	require.Empty(t, subs)
}

func TestBuildSubjects_BadTemplate(t *testing.T) {
	_, err := BuildSubjects("events.{{.MissingClose", []types.Partition{{Keys: []string{"a"}}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse subject template")
}

func TestBuildSubjects_PartitionWithNoKeys(t *testing.T) {
	_, err := BuildSubjects("events.{{.PartitionID}}", []types.Partition{{Keys: nil}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "partition has no keys")
}

func TestGenerateSubject(t *testing.T) {
	s, err := GenerateSubject("events.{{.PartitionID}}.>", types.Partition{Keys: []string{"a", "1"}})
	require.NoError(t, err)
	require.Equal(t, "events.a.1.>", s)
}

func TestParseSubjectTemplateParts(t *testing.T) {
	p, s, ok := ParseSubjectTemplateParts("a.{{.PartitionID}}.b")
	require.True(t, ok)
	require.Equal(t, "a.", p)
	require.Equal(t, ".b", s)

	_, _, ok = ParseSubjectTemplateParts("a.no-placeholder.b")
	require.False(t, ok)
}

func TestValidateSubjectTemplate(t *testing.T) {
	require.NoError(t, ValidateSubjectTemplate("events.{{.PartitionID}}", true))
	require.NoError(t, ValidateSubjectTemplate("events.{{.PartitionID}}.>", true))
	require.Error(t, ValidateSubjectTemplate("events.>", true), "missing placeholder")
	require.Error(t, ValidateSubjectTemplate("events.{{.PartitionID}}.*", false), "wildcard disallowed")
}

func TestExtractPartitionIDFromSubject(t *testing.T) {
	pid, ok := ExtractPartitionIDFromSubject("a.123.b", "a.", ".b")
	require.True(t, ok)
	require.Equal(t, "123", pid)

	_, ok = ExtractPartitionIDFromSubject("anything", "", "")
	require.False(t, ok)
}

// TestConsumerConfig_ProducesExpectedLiteral pins every field on
// jetstream.ConsumerConfig that ConsumerConfig sets. This is the seam that
// catches drift between the runtime literal and the extracted helper.
func TestConsumerConfig_ProducesExpectedLiteral(t *testing.T) {
	def := Defaults{
		AckPolicy:             jetstream.AckExplicitPolicy,
		AckWait:               30 * time.Second,
		MaxDeliver:            -1,
		InactiveThreshold:     24 * time.Hour,
		MaxWaiting:            2,
		MaxAckPending:         0,
		ConsumerMemoryStorage: false,
		ConsumerReplicas:      0,
	}
	got := ConsumerConfig("dur_p1_aaaa", "orders.p1", def)

	require.Equal(t, "dur_p1_aaaa", got.Name)
	require.Equal(t, "dur_p1_aaaa", got.Durable)
	require.Equal(t, "orders.p1", got.FilterSubject)
	require.Equal(t, jetstream.AckExplicitPolicy, got.AckPolicy)
	require.Equal(t, jetstream.DeliverAllPolicy, got.DeliverPolicy)
	require.Equal(t, 30*time.Second, got.AckWait)
	require.Equal(t, -1, got.MaxDeliver)
	require.Equal(t, 24*time.Hour, got.InactiveThreshold)
	require.Equal(t, 2, got.MaxWaiting)
	require.Equal(t, 0, got.MaxAckPending)
	require.False(t, got.MemoryStorage)
	require.Equal(t, 0, got.Replicas)
}

func TestConsumerConfig_TunablesPassThrough(t *testing.T) {
	def := Defaults{
		AckPolicy:             jetstream.AckExplicitPolicy,
		AckWait:               5 * time.Second,
		MaxDeliver:            7,
		InactiveThreshold:     time.Hour,
		MaxWaiting:            8,
		MaxAckPending:         9,
		ConsumerMemoryStorage: true,
		ConsumerReplicas:      3,
	}
	got := ConsumerConfig("dur", "subj", def)
	require.Equal(t, 5*time.Second, got.AckWait)
	require.Equal(t, 7, got.MaxDeliver)
	require.Equal(t, time.Hour, got.InactiveThreshold)
	require.Equal(t, 8, got.MaxWaiting)
	require.Equal(t, 9, got.MaxAckPending)
	require.True(t, got.MemoryStorage)
	require.Equal(t, 3, got.Replicas)
}

// TestConsumerConfig_NoStrayFieldsSet asserts that ConsumerConfig does not
// touch any field beyond the 12 it documents. Adding a new field to the
// helper without updating callers is a silent bug; this test makes that
// surface as a failure.
func TestConsumerConfig_NoStrayFieldsSet(t *testing.T) {
	got := ConsumerConfig("d", "s", Defaults{AckPolicy: jetstream.AckExplicitPolicy})
	// Fields the runtime does NOT touch — assert zero value.
	require.Empty(t, got.Description)
	require.Empty(t, got.DeliverSubject)
	require.Empty(t, got.DeliverGroup)
	require.Zero(t, got.OptStartSeq)
	require.Nil(t, got.OptStartTime)
	require.Zero(t, got.RateLimit)
	require.Empty(t, got.SampleFrequency)
	require.False(t, got.HeadersOnly)
	require.Zero(t, got.MaxRequestBatch)
	require.Zero(t, got.MaxRequestExpires)
	require.Zero(t, got.MaxRequestMaxBytes)
	require.Nil(t, got.BackOff)
	require.Nil(t, got.Metadata)
	require.Empty(t, got.FilterSubjects)
}
