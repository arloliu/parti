package partition

import (
	"errors"

	"github.com/arloliu/parti/v2/internal/partutil"
)

var (
	// ErrEmptyKey is returned when Publish is called with an empty key.
	ErrEmptyKey = errors.New("partition key must not be empty")

	// ErrInvalidKey is returned when a partition key contains invalid tokens.
	ErrInvalidKey = errors.New("partition key contains invalid subject tokens")

	// ErrInvalidPattern is returned when SubjectPattern is malformed.
	ErrInvalidPattern = partutil.ErrInvalidPattern

	// ErrPartitionOutOfRange is returned when partition index >= NumPartitions.
	ErrPartitionOutOfRange = partutil.ErrPartitionOutOfRange

	// ErrPatternEmptyToken is returned when pattern would produce empty subject tokens.
	ErrPatternEmptyToken = partutil.ErrPatternEmptyToken

	// ErrDispatchByKeyRequiresKeyPlaceholder is returned when DispatchByKey is enabled
	// but the SubjectPattern does not contain {{key}} placeholder.
	ErrDispatchByKeyRequiresKeyPlaceholder = partutil.ErrDispatchByKeyRequiresKeyPlaceholder
)
