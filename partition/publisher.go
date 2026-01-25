package partition

import (
	"context"
	"errors"

	"github.com/nats-io/nats.go"
)

// PartitionPublisher defines the interface for partition-aware publishing.
type PartitionPublisher interface {
	// Publish sends a message to the partition determined by key.
	Publish(ctx context.Context, key string, data []byte) error

	// PublishMsg sends a message to the partition determined by key.
	PublishMsg(ctx context.Context, key string, msg *nats.Msg) error

	// GetPartition returns the partition index (0 to N-1) for a key.
	GetPartition(key string) int

	// GetSubject returns the fully expanded subject for a key.
	GetSubject(key string) string

	// NumPartitions returns the configured partition count.
	NumPartitions() int
}

// Publisher publishes messages to statically partitioned subjects.
//
// Messages are routed to partition subjects using consistent hashing on a partition key.
// This ensures that messages with the same key always go to the same partition.
type Publisher struct {
	nc   *nats.Conn
	core *partitionCore
}

// NewPublisher creates a new static partition publisher.
//
// Parameters:
//   - nc: NATS connection used for publishing
//   - config: Partition configuration
//
// Returns:
//   - *Publisher: Configured publisher
//   - error: Validation or initialization error
func NewPublisher(nc *nats.Conn, config PartitionConfig) (*Publisher, error) {
	if nc == nil {
		return nil, errors.New("nats connection is required")
	}
	core, err := newPartitionCore(config)
	if err != nil {
		return nil, err
	}

	return &Publisher{
		nc:   nc,
		core: core,
	}, nil
}

// Publish sends a message to the appropriate partition based on the partition key.
//
// The underlying nats.Publish is fire-and-forget (non-blocking), but this method
// checks ctx.Err() before publishing to respect context cancellation.
//
// Parameters:
//   - ctx: Context for cancellation (checked before publish)
//   - key: Partition key used for hashing
//   - data: Message payload
//
// Returns:
//   - error: context.Canceled/DeadlineExceeded, ErrEmptyKey/ErrInvalidKey, or NATS error
func (p *Publisher) Publish(ctx context.Context, key string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateKeyForPublish(key); err != nil {
		return err
	}
	partition := p.core.getPartition(key)
	subject := p.core.subjectForKey(key, partition)
	if subject == "" {
		return ErrInvalidPattern
	}

	return p.nc.Publish(subject, data)
}

// PublishMsg sends a NATS message to the appropriate partition.
// The message subject is computed based on the partition key.
//
// The underlying nats.PublishMsg is fire-and-forget (non-blocking), but this method
// checks ctx.Err() before publishing to respect context cancellation.
//
// Parameters:
//   - ctx: Context for cancellation (checked before publish)
//   - key: Partition key used for hashing
//   - msg: NATS message (Subject will be overwritten)
//
// Returns:
//   - error: context.Canceled/DeadlineExceeded, ErrEmptyKey/ErrInvalidKey, or NATS error
func (p *Publisher) PublishMsg(ctx context.Context, key string, msg *nats.Msg) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if msg == nil {
		return errors.New("message is required")
	}
	if err := validateKeyForPublish(key); err != nil {
		return err
	}
	partition := p.core.getPartition(key)
	msg.Subject = p.core.subjectForKey(key, partition)

	return p.nc.PublishMsg(msg)
}

// GetPartition returns the partition index (0 to N-1) for a given key.
//
// Parameters:
//   - key: Partition key used for hashing
//
// Returns:
//   - int: Partition index (0 to N-1) or -1 if key is empty/invalid
func (p *Publisher) GetPartition(key string) int {
	return p.core.getPartition(key)
}

// GetSubject returns the fully expanded subject for a given partition key.
//
// Parameters:
//   - key: Partition key used for hashing
//
// Returns:
//   - string: Fully expanded subject (empty if key is invalid)
func (p *Publisher) GetSubject(key string) string {
	return p.core.getSubject(key)
}

// GetSubjectForPartition returns the subject for a specific partition index.
// Only {{partition}} is substituted; {{key}} becomes "*" wildcard.
//
// Parameters:
//   - partition: Partition index (0 to N-1)
//
// Returns:
//   - string: Subject for the partition
//   - error: ErrPartitionOutOfRange if partition is invalid
func (p *Publisher) GetSubjectForPartition(partition int) (string, error) {
	return p.core.getSubjectForPartition(partition)
}

// NumPartitions returns the configured partition count.
//
// Returns:
//   - int: Number of partitions
func (p *Publisher) NumPartitions() int {
	return p.core.numPartitions()
}
