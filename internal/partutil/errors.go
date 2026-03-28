// Package partutil provides shared partition utilities used by both the public
// partition package and its internal implementation packages.
package partutil

import "errors"

var (
	// ErrInvalidPattern is returned when SubjectPattern is malformed.
	ErrInvalidPattern = errors.New("invalid subject pattern")

	// ErrPatternEmptyToken is returned when pattern would produce empty subject tokens.
	ErrPatternEmptyToken = errors.New("pattern produces empty subject token")

	// ErrPartitionOutOfRange is returned when partition index >= NumPartitions.
	ErrPartitionOutOfRange = errors.New("partition index out of range")

	// ErrDispatchByKeyRequiresKeyPlaceholder is returned when DispatchByKey is enabled
	// but the SubjectPattern does not contain {{key}} placeholder.
	ErrDispatchByKeyRequiresKeyPlaceholder = errors.New("DispatchByKey requires {{key}} placeholder in SubjectPattern")
)
