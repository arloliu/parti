package partition

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParsePattern(t *testing.T) {
	parts, err := parsePattern("events.{{key}}.completed.{{partition}}")
	require.NoError(t, err)
	require.True(t, parts.hasKey)
	require.Equal(t, 0, parts.keyIndex)
	require.Equal(t, 1, parts.partitionIndex)
}

func TestParsePattern_Invalid(t *testing.T) {
	tests := []string{
		"",
		"events.{{key}}.completed",
		"events.{{partition}}.{{partition}}",
		"events.{{key}}.{{key}}.{{partition}}",
		"events.{{foo}}.{{partition}}",
	}

	for _, pattern := range tests {
		t.Run(pattern, func(t *testing.T) {
			_, err := parsePattern(pattern)
			require.Error(t, err)
			require.True(t, errors.Is(err, ErrInvalidPattern))
		})
	}
}

func TestBuildSubject(t *testing.T) {
	parts, err := parsePattern("events.{{key}}.completed.{{partition}}")
	require.NoError(t, err)

	subject := parts.buildSubject("tool-1", 3)
	require.Equal(t, "events.tool-1.completed.3", subject)

	filter := parts.buildFilterSubject(3)
	require.Equal(t, "events.*.completed.3", filter)
}

func TestValidateKeyForPublish(t *testing.T) {
	require.ErrorIs(t, validateKeyForPublish(""), ErrEmptyKey)
	require.ErrorIs(t, validateKeyForPublish("a.*.b"), ErrInvalidKey)
	require.ErrorIs(t, validateKeyForPublish("a..b"), ErrInvalidKey)
	require.NoError(t, validateKeyForPublish("a.b"))
}

func TestValidateSubjectTokens(t *testing.T) {
	require.NoError(t, validateSubjectTokens("a.b.c", false))
	require.ErrorIs(t, validateSubjectTokens("a..c", false), ErrPatternEmptyToken)
	require.ErrorIs(t, validateSubjectTokens("a.*.c", false), ErrInvalidPattern)
	require.NoError(t, validateSubjectTokens("a.*.c", true))
	require.ErrorIs(t, validateSubjectTokens("a.>.c", true), ErrInvalidPattern)
}

func TestExtractKey(t *testing.T) {
	tests := []struct {
		name     string
		pattern  string
		subject  string
		expected string
	}{
		{
			name:     "key at end",
			pattern:  "events.{{partition}}.{{key}}",
			subject:  "events.0.customer-abc",
			expected: "customer-abc",
		},
		{
			name:     "key in middle",
			pattern:  "events.{{key}}.{{partition}}",
			subject:  "events.customer-abc.0",
			expected: "customer-abc",
		},
		{
			name:     "key at start",
			pattern:  "{{key}}.events.{{partition}}",
			subject:  "customer-abc.events.0",
			expected: "customer-abc",
		},
		{
			name:     "complex pattern key at end",
			pattern:  "orders.{{partition}}.region.{{key}}.created",
			subject:  "orders.2.region.us-west.created",
			expected: "us-west",
		},
		{
			name:     "complex pattern key before partition",
			pattern:  "orders.{{key}}.region.{{partition}}.created",
			subject:  "orders.us-west.region.2.created",
			expected: "us-west",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parts, err := parsePattern(tt.pattern)
			require.NoError(t, err)
			require.True(t, parts.hasKey)

			key := parts.extractKey(tt.subject)
			require.Equal(t, tt.expected, key)
		})
	}
}

func TestExtractKey_NoKeyPlaceholder(t *testing.T) {
	parts, err := parsePattern("events.{{partition}}.data")
	require.NoError(t, err)
	require.False(t, parts.hasKey)

	key := parts.extractKey("events.0.data")
	require.Empty(t, key)
}

func TestKeyTokenIndex(t *testing.T) {
	tests := []struct {
		name     string
		pattern  string
		expected int
	}{
		{
			name:     "key at position 2",
			pattern:  "events.{{partition}}.{{key}}",
			expected: 2,
		},
		{
			name:     "key at position 1",
			pattern:  "events.{{key}}.{{partition}}",
			expected: 1,
		},
		{
			name:     "key at position 0",
			pattern:  "{{key}}.events.{{partition}}",
			expected: 0,
		},
		{
			name:     "key at position 3",
			pattern:  "orders.{{partition}}.region.{{key}}.created",
			expected: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parts, err := parsePattern(tt.pattern)
			require.NoError(t, err)

			idx := parts.keyTokenIndex()
			require.Equal(t, tt.expected, idx)
		})
	}
}
