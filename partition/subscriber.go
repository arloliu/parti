package partition

import (
	"context"
	"errors"
	"sync"

	"github.com/arloliu/parti/v2/internal/partutil"
	"github.com/nats-io/nats.go"
)

// PartitionSubscriber defines the interface for partition-aware core NATS subscriptions.
type PartitionSubscriber interface {
	// Start begins consuming messages in a background callback.
	Start(ctx context.Context) error

	// Stop gracefully stops the subscriber.
	Stop(ctx context.Context) error

	// Partition returns the partition index this subscriber handles.
	Partition() int

	// Subject returns the NATS subject this subscriber subscribes to.
	Subject() string
}

// Subscriber consumes messages from a single static partition using core NATS.
//
// This subscriber is designed for StatefulSet deployments where each pod
// handles a fixed partition based on its ordinal index.
type Subscriber struct {
	nc      *nats.Conn
	config  PartitionConfig
	handler NATSMessageHandler

	partition int
	subject   string

	sub *nats.Subscription

	mu     sync.Mutex
	cancel context.CancelFunc
}

// NewSubscriber creates a core NATS subscriber for a specific partition.
//
// Parameters:
//   - nc: NATS connection
//   - config: Partition configuration
//   - partition: Partition index (0 to N-1)
//   - handler: Message handler
//
// Returns:
//   - *Subscriber: Configured subscriber
//   - error: Validation or initialization error
func NewSubscriber(
	nc *nats.Conn,
	config PartitionConfig,
	partition int,
	handler NATSMessageHandler,
) (*Subscriber, error) {
	if nc == nil {
		return nil, errors.New("nats connection is required")
	}
	if handler == nil {
		return nil, errors.New("message handler is required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if partition < 0 || partition >= config.NumPartitions {
		return nil, ErrPartitionOutOfRange
	}

	parts, err := partutil.ParsePattern(config.SubjectPattern)
	if err != nil {
		return nil, err
	}

	subject := parts.BuildFilterSubject(partition)
	if err := partutil.ValidateSubjectTokens(subject, true); err != nil {
		return nil, err
	}

	return &Subscriber{
		nc:        nc,
		config:    config,
		handler:   handler,
		partition: partition,
		subject:   subject,
	}, nil
}

// Start begins consuming messages in a background callback.
//
// Parameters:
//   - ctx: Context for lifecycle control
//
// Returns:
//   - error: Startup error
func (s *Subscriber) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return errors.New("subscriber already started")
	}

	loopCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	sub, err := s.nc.Subscribe(s.subject, func(msg *nats.Msg) {
		if loopCtx.Err() != nil {
			return
		}
		if err := s.handler.Handle(loopCtx, msg); err != nil {
			s.config.Logger.Warn("partition subscriber handler error", "error", err, "subject", s.subject)
		}
	})
	if err != nil {
		s.cancel = nil

		return err
	}

	s.sub = sub

	return nil
}

// Stop gracefully stops the subscriber and drains pending messages.
//
// Parameters:
//   - ctx: Context with shutdown deadline
//
// Returns:
//   - error: Shutdown error
func (s *Subscriber) Stop(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.cancel
	sub := s.sub
	s.cancel = nil
	s.sub = nil
	s.mu.Unlock()

	if cancel == nil {
		return nil
	}
	cancel()

	if sub == nil {
		return nil
	}

	done := make(chan error, 1)
	go func() {
		done <- sub.Drain()
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Partition returns the partition index this subscriber handles.
//
// Returns:
//   - int: Partition index
func (s *Subscriber) Partition() int {
	return s.partition
}

// Subject returns the NATS subject this subscriber subscribes to.
//
// Returns:
//   - string: Subject filter for this partition
func (s *Subscriber) Subject() string {
	return s.subject
}
