package partition

import (
	"context"
	"errors"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// JSPublisher is a JetStream variant for durable publishing.
// Implements PartitionPublisher semantics with JetStream acknowledgments.
type JSPublisher struct {
	js   jetstream.JetStream
	core *partitionCore
}

// NewJSPublisher creates a JetStream-based publisher.
//
// Parameters:
//   - js: JetStream context used for publishing
//   - config: Partition configuration
//
// Returns:
//   - *JSPublisher: Configured JetStream publisher
//   - error: Validation or initialization error
func NewJSPublisher(js jetstream.JetStream, config PartitionConfig) (*JSPublisher, error) {
	if js == nil {
		return nil, errors.New("jetstream context is required")
	}
	core, err := newPartitionCore(config)
	if err != nil {
		return nil, err
	}

	return &JSPublisher{
		js:   js,
		core: core,
	}, nil
}

// Publish publishes with JetStream acknowledgment.
//
// Parameters:
//   - ctx: Context for cancellation and tracing
//   - key: Partition key used for hashing
//   - data: Message payload
//
// Returns:
//   - *jetstream.PubAck: Publish acknowledgement
//   - error: ErrEmptyKey/ErrInvalidKey for key issues, or JetStream publish error
func (p *JSPublisher) Publish(ctx context.Context, key string, data []byte) (*jetstream.PubAck, error) {
	if err := validateKeyForPublish(key); err != nil {
		return nil, err
	}
	partition := p.core.getPartition(key)
	subject := p.core.subjectForKey(key, partition)
	return p.js.Publish(ctx, subject, data)
}

// PublishAsync publishes asynchronously.
//
// Parameters:
//   - key: Partition key used for hashing
//   - data: Message payload
//
// Returns:
//   - jetstream.PubAckFuture: Future for publish acknowledgement
//   - error: ErrEmptyKey/ErrInvalidKey for key issues, or JetStream publish error
func (p *JSPublisher) PublishAsync(key string, data []byte) (jetstream.PubAckFuture, error) {
	if err := validateKeyForPublish(key); err != nil {
		return nil, err
	}
	partition := p.core.getPartition(key)
	subject := p.core.subjectForKey(key, partition)
	return p.js.PublishAsync(subject, data)
}

// PublishMsg publishes a JetStream message with acknowledgment.
//
// Parameters:
//   - ctx: Context for cancellation and tracing
//   - key: Partition key used for hashing
//   - msg: NATS message (Subject will be overwritten)
//
// Returns:
//   - *jetstream.PubAck: Publish acknowledgement
//   - error: ErrEmptyKey/ErrInvalidKey for key issues, or JetStream publish error
func (p *JSPublisher) PublishMsg(ctx context.Context, key string, msg *nats.Msg) (*jetstream.PubAck, error) {
	if msg == nil {
		return nil, errors.New("message is required")
	}
	if err := validateKeyForPublish(key); err != nil {
		return nil, err
	}
	partition := p.core.getPartition(key)
	msg.Subject = p.core.subjectForKey(key, partition)

	return p.js.PublishMsg(ctx, msg)
}

// GetPartition returns the partition index (0 to N-1) for a given key.
//
// Parameters:
//   - key: Partition key used for hashing
//
// Returns:
//   - int: Partition index (0 to N-1) or -1 if key is empty/invalid
func (p *JSPublisher) GetPartition(key string) int {
	return p.core.getPartition(key)
}

// GetSubject returns the fully expanded subject for a given partition key.
//
// Parameters:
//   - key: Partition key used for hashing
//
// Returns:
//   - string: Fully expanded subject (empty if key is invalid)
func (p *JSPublisher) GetSubject(key string) string {
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
func (p *JSPublisher) GetSubjectForPartition(partition int) (string, error) {
	return p.core.getSubjectForPartition(partition)
}

// NumPartitions returns the configured partition count.
//
// Returns:
//   - int: Number of partitions
func (p *JSPublisher) NumPartitions() int {
	return p.core.numPartitions()
}
