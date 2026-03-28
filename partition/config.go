package partition

import (
	"errors"
	"fmt"

	"github.com/arloliu/fuda"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/partutil"
	"github.com/arloliu/parti/v2/types"
)

// PartitionConfig configures static partition-based publishing and consuming.
type PartitionConfig struct {
	// NumPartitions is the total number of partitions (N).
	// Must be > 0. Partition indices range from 0 to NumPartitions-1.
	// WARNING: Changing NumPartitions changes the hash mapping.
	NumPartitions int `validate:"required,gt=0"`

	// SubjectPattern is the subject template with placeholders.
	//
	// Placeholders:
	//   - {{partition}} - Replaced with partition index (0 to N-1). Required.
	//   - {{key}}       - Replaced with the partition key. Optional.
	//
	// Placeholders must occupy a full token between dots. Embedded placeholders
	// like "orders.{{partition}}-v1" are invalid.
	//
	// Examples:
	//   - "events.completed.{{partition}}"       → "events.completed.0"
	//   - "events.{{key}}.{{partition}}"         → "events.tool-abc.3"
	//   - "orders.{{partition}}.{{key}}.created" → "orders.2.customer-xyz.created"
	//   - "events.*.{{key}}.{{partition}}"       → "events.*.tool-abc.3" (consumer filter)
	//   - "events.{{key}}.{{partition}}.>"       → "events.tool-abc.3.>" (consumer filter)
	//
	// Validation:
	//   - Must contain {{partition}} placeholder
	//   - Must not produce empty NATS subject tokens (e.g., "events..{{partition}}" is invalid)
	//   - Wildcards are allowed for consumers/subscribers:
	//     - "*" can appear in any token
	//     - ">" can only appear as the last token
	//   - Publishers require concrete subjects; wildcard patterns are rejected for publishing
	SubjectPattern string `validate:"required,contains={{partition}}"`

	// HashSeed is an optional seed for the consistent hash function.
	// Use 0 for default behavior, non-zero for deterministic hashing.
	HashSeed uint64

	// Logger provides structured logging. Defaults to no-op logger when nil.
	Logger types.Logger
}

func (cfg *PartitionConfig) setDefaults() error {
	if err := fuda.SetDefaults(cfg); err != nil {
		return fmt.Errorf("failed to set defaults: %w", err)
	}
	if cfg.Logger == nil {
		cfg.Logger = logging.NewNop()
	}

	return nil
}

// Validate validates the partition configuration.
//
// Returns:
//   - error: Validation or initialization error
func (cfg *PartitionConfig) Validate() error {
	if cfg == nil {
		return errors.New("partition config is required")
	}
	if err := cfg.setDefaults(); err != nil {
		return err
	}
	if cfg.NumPartitions <= 0 {
		return errors.New("num partitions must be > 0")
	}
	if cfg.SubjectPattern == "" {
		return errors.New("subject pattern is required")
	}
	parts, err := partutil.ParsePattern(cfg.SubjectPattern)
	if err != nil {
		return err
	}

	// Validate filter subject (wildcards allowed in patterns).
	filter := parts.BuildFilterSubject(0)

	return partutil.ValidateSubjectTokens(filter, true)
}
