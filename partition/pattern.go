package partition

import (
	"strconv"
	"strings"
	"unicode"
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

		switch {
		case strings.HasPrefix(pattern[idx:], partitionPlaceholder):
			if parts.partitionIndex >= 0 {
				return patternParts{}, ErrInvalidPattern
			}
			parts.partitionIndex = len(parts.segments) - 1
			cursor = idx + len(partitionPlaceholder)
		case strings.HasPrefix(pattern[idx:], keyPlaceholder):
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
