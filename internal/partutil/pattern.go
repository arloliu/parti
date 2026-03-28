package partutil

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/nats-io/nats.go/jetstream"
)

const (
	PartitionPlaceholder = "{{partition}}"
	KeyPlaceholder       = "{{key}}"
)

// PatternParts holds the parsed components of a subject pattern.
type PatternParts struct {
	segments       []string
	HasKey         bool
	KeyIndex       int
	PartitionIndex int
}

// ParsePattern parses a subject pattern with {{partition}} and optional {{key}} placeholders.
//
// Parameters:
//   - pattern: Subject pattern string containing {{partition}} placeholder
//
// Returns:
//   - PatternParts: Parsed pattern components
//   - error: ErrInvalidPattern if the pattern is malformed
func ParsePattern(pattern string) (PatternParts, error) {
	parts := PatternParts{
		segments:       nil,
		KeyIndex:       -1,
		PartitionIndex: -1,
	}

	if pattern == "" {
		return parts, ErrInvalidPattern
	}

	cursor := 0
	for {
		idx := strings.Index(pattern[cursor:], "{{")
		if idx < 0 {
			parts.segments = append(parts.segments, pattern[cursor:])
			break
		}
		idx += cursor
		parts.segments = append(parts.segments, pattern[cursor:idx])

		if !isPlaceholderTokenBoundary(pattern, idx) {
			return PatternParts{}, ErrInvalidPattern
		}

		switch {
		case strings.HasPrefix(pattern[idx:], PartitionPlaceholder):
			if !isPlaceholderTokenBoundaryEnd(pattern, idx, len(PartitionPlaceholder)) {
				return PatternParts{}, ErrInvalidPattern
			}
			if parts.PartitionIndex >= 0 {
				return PatternParts{}, ErrInvalidPattern
			}
			parts.PartitionIndex = len(parts.segments) - 1
			cursor = idx + len(PartitionPlaceholder)
		case strings.HasPrefix(pattern[idx:], KeyPlaceholder):
			if !isPlaceholderTokenBoundaryEnd(pattern, idx, len(KeyPlaceholder)) {
				return PatternParts{}, ErrInvalidPattern
			}
			if parts.KeyIndex >= 0 {
				return PatternParts{}, ErrInvalidPattern
			}
			parts.HasKey = true
			parts.KeyIndex = len(parts.segments) - 1
			cursor = idx + len(KeyPlaceholder)
		default:
			return PatternParts{}, ErrInvalidPattern
		}
	}

	if parts.PartitionIndex < 0 {
		return PatternParts{}, ErrInvalidPattern
	}

	return parts, nil
}

func isPlaceholderTokenBoundary(pattern string, start int) bool {
	if start == 0 {
		return true
	}
	return pattern[start-1] == '.'
}

func isPlaceholderTokenBoundaryEnd(pattern string, start int, length int) bool {
	end := start + length
	if end >= len(pattern) {
		return true
	}
	return pattern[end] == '.'
}

// BuildSubject builds a concrete subject for the given key and partition index.
//
// Parameters:
//   - key: Partition key (used only if pattern has {{key}} placeholder)
//   - partition: Partition index
//
// Returns:
//   - string: Fully expanded subject
func (p *PatternParts) BuildSubject(key string, partition int) string {
	if len(p.segments) == 0 {
		return ""
	}
	if !p.HasKey {
		return p.BuildSubjectNoKey(partition)
	}

	partitionStr := strconv.Itoa(partition)
	builder := strings.Builder{}
	builder.Grow(len(key) + len(partitionStr) + len(strings.Join(p.segments, "")))
	builder.WriteString(p.segments[0])
	for i := 0; i < len(p.segments)-1; i++ {
		if i == p.KeyIndex {
			builder.WriteString(key)
		}
		if i == p.PartitionIndex {
			builder.WriteString(partitionStr)
		}
		builder.WriteString(p.segments[i+1])
	}

	return builder.String()
}

// BuildSubjectNoKey builds a concrete subject using only the partition index.
//
// Parameters:
//   - partition: Partition index
//
// Returns:
//   - string: Fully expanded subject ({{key}} replaced with nothing if present)
func (p *PatternParts) BuildSubjectNoKey(partition int) string {
	partitionStr := strconv.Itoa(partition)
	builder := strings.Builder{}
	builder.Grow(len(partitionStr) + len(strings.Join(p.segments, "")))
	builder.WriteString(p.segments[0])
	for i := 0; i < len(p.segments)-1; i++ {
		if i == p.PartitionIndex {
			builder.WriteString(partitionStr)
		}
		builder.WriteString(p.segments[i+1])
	}

	return builder.String()
}

// BuildFilterSubject builds a filter subject with {{key}} replaced by "*".
//
// Parameters:
//   - partition: Partition index
//
// Returns:
//   - string: Filter subject for JetStream consumer configuration
func (p *PatternParts) BuildFilterSubject(partition int) string {
	if !p.HasKey {
		return p.BuildSubjectNoKey(partition)
	}

	partitionStr := strconv.Itoa(partition)
	builder := strings.Builder{}
	builder.Grow(len(partitionStr) + len(strings.Join(p.segments, "")) + 1)
	builder.WriteString(p.segments[0])
	for i := 0; i < len(p.segments)-1; i++ {
		if i == p.KeyIndex {
			builder.WriteString("*")
		}
		if i == p.PartitionIndex {
			builder.WriteString(partitionStr)
		}
		builder.WriteString(p.segments[i+1])
	}

	return builder.String()
}

// ExtractKey extracts the routing key from a subject based on the pattern.
//
// Parameters:
//   - subject: The full NATS subject (e.g., "events.0.customer-abc")
//
// Returns:
//   - string: The extracted key, or empty string if extraction fails
func (p *PatternParts) ExtractKey(subject string) string {
	if !p.HasKey {
		return ""
	}

	keyTokenIndex := p.KeyTokenIndex()
	tokens := strings.Split(subject, ".")
	if keyTokenIndex >= len(tokens) {
		return ""
	}

	return tokens[keyTokenIndex]
}

// KeyTokenIndex calculates the token index where {{key}} appears in the subject.
//
// Returns:
//   - int: Token index of the key placeholder, or -1 if no key placeholder
func (p *PatternParts) KeyTokenIndex() int {
	if !p.HasKey {
		return -1
	}

	var prefix strings.Builder
	for i := 0; i < p.KeyIndex; i++ {
		prefix.WriteString(p.segments[i])
		if i == p.PartitionIndex {
			prefix.WriteString("0")
		}
	}
	prefix.WriteString(p.segments[p.KeyIndex])

	return strings.Count(prefix.String(), ".")
}

// KeyExtractorFunc returns a function that extracts the routing key from a message subject.
//
// Returns:
//   - func(jetstream.Msg) string: Key extractor, or nil if pattern has no key placeholder
func (p *PatternParts) KeyExtractorFunc() func(msg jetstream.Msg) string {
	if !p.HasKey {
		return nil
	}

	keyTokenIdx := p.KeyTokenIndex()
	return func(msg jetstream.Msg) string {
		tokens := strings.Split(msg.Subject(), ".")
		if keyTokenIdx >= len(tokens) {
			return ""
		}
		return tokens[keyTokenIdx]
	}
}

// ValidateSubjectTokens validates that a subject string has legal NATS tokens.
//
// Parameters:
//   - subject: Subject string to validate
//   - allowWildcard: Whether to allow * and > wildcards
//
// Returns:
//   - error: ErrInvalidPattern or ErrPatternEmptyToken on invalid tokens
func ValidateSubjectTokens(subject string, allowWildcard bool) error {
	if subject == "" {
		return ErrInvalidPattern
	}

	tokens := strings.Split(subject, ".")
	for i, t := range tokens {
		if t == "" {
			return ErrPatternEmptyToken
		}
		if strings.IndexFunc(t, unicode.IsSpace) >= 0 {
			return ErrInvalidPattern
		}

		if !allowWildcard {
			if t == "*" || t == ">" {
				return ErrInvalidPattern
			}
		} else {
			if t == ">" && i != len(tokens)-1 {
				return ErrInvalidPattern
			}
		}
	}

	return nil
}

// ValidateKeyForPublish validates a partition key for use in publishing.
//
// Parameters:
//   - key: Partition key to validate
//   - errEmptyKey: Error to return when key is empty
//   - errInvalidKey: Error to return when key is invalid
//
// Returns:
//   - error: errEmptyKey, errInvalidKey, or nil
func ValidateKeyForPublish(key string, errEmptyKey, errInvalidKey error) error {
	if key == "" {
		return errEmptyKey
	}
	if strings.ContainsAny(key, "*>") {
		return errInvalidKey
	}
	subject := key
	if err := ValidateSubjectTokens(subject, false); err != nil {
		return errInvalidKey
	}

	return nil
}
