package partition

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/nats-io/nats.go/jetstream"
)

const (
	partitionPlaceholder = "{{partition}}"
	keyPlaceholder       = "{{key}}"
)

type patternParts struct {
	segments       []string
	hasKey         bool
	keyIndex       int
	partitionIndex int
}

func parsePattern(pattern string) (patternParts, error) {
	parts := patternParts{
		segments:       nil,
		keyIndex:       -1,
		partitionIndex: -1,
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
			return patternParts{}, ErrInvalidPattern
		}

		switch {
		case strings.HasPrefix(pattern[idx:], partitionPlaceholder):
			if !isPlaceholderTokenBoundaryEnd(pattern, idx, len(partitionPlaceholder)) {
				return patternParts{}, ErrInvalidPattern
			}
			if parts.partitionIndex >= 0 {
				return patternParts{}, ErrInvalidPattern
			}
			parts.partitionIndex = len(parts.segments) - 1
			cursor = idx + len(partitionPlaceholder)
		case strings.HasPrefix(pattern[idx:], keyPlaceholder):
			if !isPlaceholderTokenBoundaryEnd(pattern, idx, len(keyPlaceholder)) {
				return patternParts{}, ErrInvalidPattern
			}
			if parts.keyIndex >= 0 {
				return patternParts{}, ErrInvalidPattern
			}
			parts.hasKey = true
			parts.keyIndex = len(parts.segments) - 1
			cursor = idx + len(keyPlaceholder)
		default:
			return patternParts{}, ErrInvalidPattern
		}
	}

	if parts.partitionIndex < 0 {
		return patternParts{}, ErrInvalidPattern
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

func (p *patternParts) buildSubject(key string, partition int) string {
	if len(p.segments) == 0 {
		return ""
	}
	if !p.hasKey {
		return p.buildSubjectNoKey(partition)
	}

	partitionStr := strconv.Itoa(partition)
	builder := strings.Builder{}
	builder.Grow(len(key) + len(partitionStr) + len(strings.Join(p.segments, "")))
	builder.WriteString(p.segments[0])
	for i := 0; i < len(p.segments)-1; i++ {
		if i == p.keyIndex {
			builder.WriteString(key)
		}
		if i == p.partitionIndex {
			builder.WriteString(partitionStr)
		}
		builder.WriteString(p.segments[i+1])
	}

	return builder.String()
}

func (p *patternParts) buildSubjectNoKey(partition int) string {
	partitionStr := strconv.Itoa(partition)
	builder := strings.Builder{}
	builder.Grow(len(partitionStr) + len(strings.Join(p.segments, "")))
	builder.WriteString(p.segments[0])
	for i := 0; i < len(p.segments)-1; i++ {
		if i == p.partitionIndex {
			builder.WriteString(partitionStr)
		}
		builder.WriteString(p.segments[i+1])
	}

	return builder.String()
}

func (p *patternParts) buildFilterSubject(partition int) string {
	if !p.hasKey {
		return p.buildSubjectNoKey(partition)
	}

	partitionStr := strconv.Itoa(partition)
	builder := strings.Builder{}
	builder.Grow(len(partitionStr) + len(strings.Join(p.segments, "")) + 1)
	builder.WriteString(p.segments[0])
	for i := 0; i < len(p.segments)-1; i++ {
		if i == p.keyIndex {
			builder.WriteString("*")
		}
		if i == p.partitionIndex {
			builder.WriteString(partitionStr)
		}
		builder.WriteString(p.segments[i+1])
	}

	return builder.String()
}

// extractKey extracts the key from a subject based on the pattern.
//
// The pattern defines where {{key}} is located. This method parses the subject
// and returns the token at the key position.
//
// Parameters:
//   - subject: The full NATS subject (e.g., "events.0.customer-abc")
//
// Returns:
//   - string: The extracted key, or empty string if extraction fails
//
// Example:
//
//	pattern: "events.{{partition}}.{{key}}"
//	subject: "events.0.customer-abc"
//	→ returns "customer-abc"
func (p *patternParts) extractKey(subject string) string {
	if !p.hasKey {
		return ""
	}

	keyTokenIndex := p.keyTokenIndex()
	tokens := strings.Split(subject, ".")
	if keyTokenIndex >= len(tokens) {
		return ""
	}

	return tokens[keyTokenIndex]
}

// keyTokenIndex calculates the token index where {{key}} appears in the subject.
//
// For pattern "events.{{partition}}.{{key}}", the key is at token index 2.
// For pattern "events.{{key}}.{{partition}}", the key is at token index 1.
func (p *patternParts) keyTokenIndex() int {
	if !p.hasKey {
		return -1
	}

	// Build the prefix string up to the key placeholder and count dots
	// Each dot represents a token boundary
	var prefix strings.Builder
	for i := 0; i < p.keyIndex; i++ {
		prefix.WriteString(p.segments[i])
		// Add a placeholder character for each placeholder before keyIndex
		if i == p.partitionIndex {
			prefix.WriteString("0") // partition placeholder
		}
	}
	// Add the segment right before the key placeholder
	prefix.WriteString(p.segments[p.keyIndex])

	// Count dots to determine token index
	return strings.Count(prefix.String(), ".")
}

// keyExtractorFunc returns a KeyExtractorFunc that uses this pattern's structure
// to extract the key from message subjects.
func (p *patternParts) keyExtractorFunc() KeyExtractorFunc {
	if !p.hasKey {
		return nil
	}

	keyTokenIdx := p.keyTokenIndex()
	return func(msg jetstream.Msg) string {
		tokens := strings.Split(msg.Subject(), ".")
		if keyTokenIdx >= len(tokens) {
			return ""
		}
		return tokens[keyTokenIdx]
	}
}

func validateSubjectTokens(subject string, allowWildcard bool) error {
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

func validateKeyForPublish(key string) error {
	if key == "" {
		return ErrEmptyKey
	}
	if strings.ContainsAny(key, "*>") {
		return ErrInvalidKey
	}
	subject := key
	if err := validateSubjectTokens(subject, false); err != nil {
		return ErrInvalidKey
	}

	return nil
}
