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
