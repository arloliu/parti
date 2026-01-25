package partition

import "errors"

var (
	// ErrEmptyKey is returned when Publish is called with an empty key.
	ErrEmptyKey = errors.New("partition key must not be empty")

	// ErrInvalidKey is returned when a partition key contains invalid tokens.
	ErrInvalidKey = errors.New("partition key contains invalid subject tokens")

	// ErrInvalidPattern is returned when SubjectPattern is malformed.
	ErrInvalidPattern = errors.New("invalid subject pattern")

	// ErrPartitionOutOfRange is returned when partition index >= NumPartitions.
	ErrPartitionOutOfRange = errors.New("partition index out of range")

	// ErrPatternEmptyToken is returned when pattern would produce empty subject tokens.
	ErrPatternEmptyToken = errors.New("pattern produces empty subject token")
)
