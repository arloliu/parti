package subscription

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/arloliu/parti/internal/logging"
	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Default configuration values for DurableHelper.
const (
	// DefaultBatchSize is the default number of messages to fetch per pull request.
	DefaultBatchSize = 1

	// DefaultMaxWaiting is the default maximum number of outstanding pull requests.
	DefaultMaxWaiting = 512

	// DefaultFetchTimeout is the default maximum duration to wait for messages.
	DefaultFetchTimeout = 5 * time.Second

	// DefaultMaxRetries is the default maximum number of retry attempts.
	DefaultMaxRetries = 3

	// DefaultRetryBackoff is the default duration between retry attempts.
	DefaultRetryBackoff = 100 * time.Millisecond

	// DefaultAckWait is the default duration to wait for acknowledgment.
	DefaultAckWait = 30 * time.Second

	// DefaultMaxDeliver is the default maximum delivery attempts.
	DefaultMaxDeliver = 3

	// DefaultInactiveThreshold is the default inactive consumer cleanup threshold.
	DefaultInactiveThreshold = 24 * time.Hour
)

// DurableConfig configures the durable consumer helper.
type DurableConfig struct {
	// StreamName is the name of the JetStream stream to consume from.
	// Required: Must be non-empty.
	StreamName string

	// ConsumerPrefix is the consumer naming prefix (e.g., "worker").
	// Required: Must be non-empty.
	// Final consumer name format: <prefix>-<partitionID>
	// The partitionID is composed by joining partition.Keys with "-".
	ConsumerPrefix string

	// SubjectTemplate is the subject template using Go template syntax.
	// Required: Must be non-empty and valid Go template.
	// Template context: .PartitionID (partition.Keys joined with ".")
	// Example: "dc.{{.PartitionID}}.complete"
	SubjectTemplate string

	// AckPolicy is the acknowledgment policy for the consumer.
	// Optional: Defaults to jetstream.AckExplicitPolicy if not set.
	// Common values: AckExplicitPolicy (manual ACK), AckAllPolicy (ACK all up to this message).
	AckPolicy jetstream.AckPolicy

	// AckWait is the duration to wait for an acknowledgment before redelivery.
	// Optional: Defaults to 30 seconds if not set or zero.
	// Recommended: Set based on expected message processing time.
	AckWait time.Duration

	// MaxDeliver is the maximum number of delivery attempts for a message.
	// Optional: Defaults to 3 if not set or zero.
	// After MaxDeliver attempts, message moves to dead letter queue (if configured).
	MaxDeliver int

	// InactiveThreshold is the duration after which inactive consumers are automatically cleaned up by NATS.
	// Optional: Defaults to 24 hours if not set or zero.
	// NATS will delete consumers that have been inactive longer than this threshold.
	InactiveThreshold time.Duration

	// BatchSize is the number of messages to fetch in each pull request.
	// Optional: Defaults to 10 if not set or zero.
	// Larger batch sizes improve throughput but increase memory usage and latency.
	// Recommended range: 1-100 for interactive workloads, 100-500 for batch processing.
	BatchSize int

	// MaxWaiting is the maximum number of outstanding pull requests allowed.
	// Optional: Defaults to 512 if not set or zero.
	// Limits concurrent pull requests to prevent resource exhaustion.
	// Should be >= number of concurrent workers.
	MaxWaiting int

	// FetchTimeout is the maximum duration to wait for messages in a pull request.
	// Optional: Defaults to 5 seconds if not set or zero.
	// Shorter timeouts improve responsiveness during shutdown but may increase overhead.
	FetchTimeout time.Duration

	// MaxRetries is the maximum number of retry attempts for consumer operations.
	// Optional: Defaults to 3 if not set or zero.
	// Applies to consumer creation and retrieval operations.
	MaxRetries int

	// RetryBackoff is the duration to wait between retry attempts.
	// Optional: Defaults to 100 milliseconds if not set or zero.
	// Exponential backoff is not currently implemented.
	RetryBackoff time.Duration

	// Logger for diagnostic messages.
	// Optional: Defaults to NopLogger (no-op logger) if not set.
	// Set to enable debug, info, warn, and error logging.
	Logger types.Logger
}

// MessageHandler processes messages from JetStream consumers.
type MessageHandler interface {
	// Handle processes a message and returns error if processing failed.
	// On error, the message will be NAK'd for retry.
	// On success, the message will be ACK'd.
	Handle(ctx context.Context, msg jetstream.Msg) error
}

// MessageHandlerFunc is a function adapter for MessageHandler.
type MessageHandlerFunc func(ctx context.Context, msg jetstream.Msg) error

// Handle implements MessageHandler interface.
func (f MessageHandlerFunc) Handle(ctx context.Context, msg jetstream.Msg) error {
	return f(ctx, msg)
}

// DurableHelper manages JetStream durable pull consumers for partition-based work distribution.
type DurableHelper struct {
	conn   *nats.Conn
	js     jetstream.JetStream
	config DurableConfig
	logger types.Logger

	// Template for subject generation
	subjectTemplate *template.Template

	// State tracking
	mu          sync.RWMutex
	consumers   map[string]*consumerState
	cancelFuncs map[string]context.CancelFunc
}

// consumerState tracks the state of a consumer for a partition.
type consumerState struct {
	consumer    jetstream.Consumer
	partition   types.Partition
	partitionID string
	subject     string
	createdAt   time.Time
}

// subjectContext is the template context for subject generation.
type subjectContext struct {
	PartitionID string
}

// applyDefaults applies default values to zero-valued configuration fields.
func (cfg *DurableConfig) applyDefaults() {
	if cfg.AckPolicy == 0 {
		cfg.AckPolicy = jetstream.AckExplicitPolicy
	}
	if cfg.AckWait == 0 {
		cfg.AckWait = DefaultAckWait
	}
	if cfg.MaxDeliver == 0 {
		cfg.MaxDeliver = DefaultMaxDeliver
	}
	if cfg.InactiveThreshold == 0 {
		cfg.InactiveThreshold = DefaultInactiveThreshold
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = DefaultBatchSize
	}
	if cfg.MaxWaiting == 0 {
		cfg.MaxWaiting = DefaultMaxWaiting
	}
	if cfg.FetchTimeout == 0 {
		cfg.FetchTimeout = DefaultFetchTimeout
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = DefaultMaxRetries
	}
	if cfg.RetryBackoff == 0 {
		cfg.RetryBackoff = DefaultRetryBackoff
	}
	if cfg.Logger == nil {
		cfg.Logger = logging.NewNop()
	}
}

// NewDurableHelper creates a new durable consumer helper.
//
// The helper manages JetStream durable pull consumers with automatic consumer
// creation, message fetching, and acknowledgment handling. Optional configuration
// fields are automatically set to sensible defaults if not provided.
//
// Parameters:
//   - conn: NATS connection
//   - cfg: Helper configuration with required fields (StreamName, ConsumerPrefix, SubjectTemplate)
//
// Returns:
//   - *DurableHelper: Initialized helper with defaults applied
//   - error: Configuration or connection error
//
// Example (minimal configuration with defaults):
//
//	helper, err := subscription.NewDurableHelper(natsConn, subscription.DurableConfig{
//	    StreamName:      "work-stream",
//	    ConsumerPrefix:  "processor",
//	    SubjectTemplate: "metrics.{{.PartitionID}}.collected",
//	})
//
// Example (with custom configuration):
//
//	helper, err := subscription.NewDurableHelper(natsConn, subscription.DurableConfig{
//	    StreamName:        "work-stream",
//	    ConsumerPrefix:    "processor",
//	    SubjectTemplate:   "metrics.{{.PartitionID}}.collected",
//	    BatchSize:         50,               // Override default (10)
//	    FetchTimeout:      10 * time.Second, // Override default (5s)
//	    Logger:            myLogger,         // Optional: omit for no-op logger
//	})
func NewDurableHelper(conn *nats.Conn, cfg DurableConfig) (*DurableHelper, error) {
	if conn == nil {
		return nil, errors.New("NATS connection is required")
	}

	if cfg.StreamName == "" {
		return nil, errors.New("stream name is required")
	}

	if cfg.ConsumerPrefix == "" {
		return nil, errors.New("consumer prefix is required")
	}

	if cfg.SubjectTemplate == "" {
		return nil, errors.New("subject template is required")
	}

	// Apply default values to zero-valued fields
	cfg.applyDefaults()

	// Parse subject template
	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	if err != nil {
		return nil, fmt.Errorf("invalid subject template: %w", err)
	}

	// Create JetStream context
	js, err := jetstream.New(conn)
	if err != nil {
		return nil, fmt.Errorf("failed to create JetStream context: %w", err)
	}

	return &DurableHelper{
		conn:            conn,
		js:              js,
		config:          cfg,
		logger:          cfg.Logger,
		subjectTemplate: tmpl,
		consumers:       make(map[string]*consumerState),
		cancelFuncs:     make(map[string]context.CancelFunc),
	}, nil
}

// UpdateSubscriptions reconciles consumers based on partition changes.
//
// Creates durable pull consumers for added partitions and stops consumers for
// removed partitions. Consumers are NOT deleted on removal - NATS will
// automatically clean them up based on InactiveThreshold setting.
//
// Parameters:
//   - ctx: Context for cancellation
//   - added: Partitions newly assigned to this worker
//   - removed: Partitions no longer assigned to this worker
//   - handler: Message handler function
//
// Returns:
//   - error: Consumer creation or management error
//
// Example:
//
//	err := helper.UpdateSubscriptions(ctx, added, removed, handler)
func (dh *DurableHelper) UpdateSubscriptions(
	ctx context.Context,
	added, removed []types.Partition,
	handler MessageHandler,
) error {
	dh.mu.Lock()
	defer dh.mu.Unlock()

	// Stop consumers for removed partitions
	for _, partition := range removed {
		partitionID := dh.generatePartitionID(partition)

		dh.logger.Debug("stopping consumer for removed partition",
			"partitionID", partitionID,
			"partition", partition.Keys)

		// Cancel pull loop if running
		if cancel, exists := dh.cancelFuncs[partitionID]; exists {
			cancel()
			delete(dh.cancelFuncs, partitionID)
		}

		// Remove from tracking (consumer remains on NATS server)
		delete(dh.consumers, partitionID)
	}

	// Create consumers for added partitions
	for _, partition := range added {
		partitionID := dh.generatePartitionID(partition)

		// Skip if already consuming
		if _, exists := dh.consumers[partitionID]; exists {
			dh.logger.Debug("consumer already exists, skipping",
				"partitionID", partitionID,
				"partition", partition.Keys)

			continue
		}

		dh.logger.Info("creating consumer for added partition",
			"partitionID", partitionID,
			"partition", partition.Keys)

		// Get or create consumer
		consumer, subject, err := dh.getOrCreateConsumer(ctx, partition)
		if err != nil {
			return fmt.Errorf("failed to get/create consumer for partition %s: %w", partitionID, err)
		}

		dh.logger.Info("consumer created successfully",
			"partitionID", partitionID,
			"subject", subject,
			"consumerName", dh.sanitizeConsumerName(dh.config.ConsumerPrefix+"-"+partitionID))

		// Track consumer state
		dh.consumers[partitionID] = &consumerState{
			consumer:    consumer,
			partition:   partition,
			partitionID: partitionID,
			subject:     subject,
			createdAt:   time.Now(),
		}

		// Start pull loop
		pullCtx, cancel := context.WithCancel(context.Background())
		dh.cancelFuncs[partitionID] = cancel

		go func(partID string) {
			dh.logger.Debug("starting pull loop", "partitionID", partID)

			if err := dh.startPullLoop(pullCtx, consumer, handler); err != nil {
				dh.logger.Error("pull loop exited with error",
					"partitionID", partID,
					"error", err)
			} else {
				dh.logger.Debug("pull loop exited normally", "partitionID", partID)
			}

			// Clean up on exit
			dh.mu.Lock()
			delete(dh.cancelFuncs, partID)
			dh.mu.Unlock()
		}(partitionID)
	}

	return nil
}

// getOrCreateConsumer gets an existing consumer or creates a new one using an optimistic approach.
//
// This method first tries to get the existing consumer (fast path), and only creates it if
// it doesn't exist. This preserves existing consumer configuration and handles race conditions
// gracefully when multiple workers try to create the same consumer.
func (dh *DurableHelper) getOrCreateConsumer(
	ctx context.Context,
	partition types.Partition,
) (jetstream.Consumer, string, error) {
	partitionID := dh.generatePartitionID(partition)
	consumerName := dh.sanitizeConsumerName(dh.config.ConsumerPrefix + "-" + partitionID)

	// Generate subject from template
	subject, err := dh.generateSubject(partition)
	if err != nil {
		return nil, "", fmt.Errorf("failed to generate subject: %w", err)
	}

	// Get stream
	stream, err := dh.js.Stream(ctx, dh.config.StreamName)
	if err != nil {
		return nil, "", fmt.Errorf("failed to get stream %s: %w", dh.config.StreamName, err)
	}

	// Try to get or create consumer with retries
	for attempt := 0; attempt <= dh.config.MaxRetries; attempt++ {
		// Check context cancellation
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}

		// Try to get or create the consumer
		consumer, err := dh.tryGetOrCreateConsumer(ctx, stream, consumerName, subject)
		if err == nil {
			return consumer, subject, nil
		}

		// If this was the last attempt, return error
		if attempt >= dh.config.MaxRetries {
			return nil, "", fmt.Errorf("failed to get/create consumer %s after %d attempts: %w",
				consumerName, dh.config.MaxRetries+1, err)
		}

		// Retry with backoff
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-time.After(dh.config.RetryBackoff):
			continue
		}
	}

	// Should never reach here, but satisfy compiler
	return nil, "", fmt.Errorf("failed to get/create consumer %s", consumerName)
}

// tryGetOrCreateConsumer attempts to get existing consumer or create a new one.
// Returns consumer on success, error on failure.
func (dh *DurableHelper) tryGetOrCreateConsumer(
	ctx context.Context,
	stream jetstream.Stream,
	consumerName string,
	subject string,
) (jetstream.Consumer, error) {
	// Optimistic approach: try to get existing consumer first (fast path)
	consumer, err := stream.Consumer(ctx, consumerName)
	if err == nil {
		dh.logger.Debug("using existing consumer", "consumerName", consumerName)

		return consumer, nil
	}

	// If consumer doesn't exist, create it
	if !errors.Is(err, jetstream.ErrConsumerNotFound) {
		dh.logger.Error("failed to access consumer",
			"consumerName", consumerName,
			"error", err)

		return nil, fmt.Errorf("failed to access consumer: %w", err)
	}

	dh.logger.Debug("consumer not found, creating new one",
		"consumerName", consumerName,
		"subject", subject)

	// Create consumer configuration
	consumerCfg := jetstream.ConsumerConfig{
		Name:              consumerName,
		Durable:           consumerName,
		FilterSubject:     subject,
		AckPolicy:         dh.config.AckPolicy,
		AckWait:           dh.config.AckWait,
		MaxDeliver:        dh.config.MaxDeliver,
		InactiveThreshold: dh.config.InactiveThreshold,
		MaxWaiting:        dh.config.MaxWaiting,
	}

	consumer, err = stream.CreateConsumer(ctx, consumerCfg)
	if err == nil {
		dh.logger.Info("consumer created successfully", "consumerName", consumerName)

		return consumer, nil
	}

	// Handle race condition: consumer was created by another worker
	if !errors.Is(err, jetstream.ErrConsumerNameAlreadyInUse) {
		dh.logger.Error("failed to create consumer",
			"consumerName", consumerName,
			"error", err)

		return nil, fmt.Errorf("failed to create consumer: %w", err)
	}

	dh.logger.Debug("consumer created by another worker, fetching it",
		"consumerName", consumerName)

	// Get the consumer that was just created by another worker
	consumer, err = stream.Consumer(ctx, consumerName)
	if err != nil {
		dh.logger.Error("failed to get consumer after race condition",
			"consumerName", consumerName,
			"error", err)

		return nil, fmt.Errorf("failed to get consumer after race: %w", err)
	}

	return consumer, nil
}

// sanitizeConsumerName removes invalid characters from consumer name.
//
// NATS consumer name restrictions:
// - Cannot contain whitespace
// - Cannot contain . (dot)
// - Cannot contain * (asterisk)
// - Cannot contain > (greater than)
// - Cannot contain path separators (/ or \)
// - Cannot contain non-printable characters
//
// We replace invalid characters with underscore (_).
func (dh *DurableHelper) sanitizeConsumerName(name string) string {
	var result strings.Builder
	result.Grow(len(name))

	for _, r := range name {
		// Check for invalid characters
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || // whitespace
			r == '.' || r == '*' || r == '>' || // special chars
			r == '/' || r == '\\' || // path separators
			r < 32 || r == 127 { // non-printable
			result.WriteRune('_')
		} else {
			result.WriteRune(r)
		}
	}

	return result.String()
}

// startPullLoop runs the pull consumer loop for a partition using Messages() iterator.
// This provides better performance than Fetch() as it maintains a persistent subscription
// with automatic buffering and refilling.
func (dh *DurableHelper) startPullLoop(
	ctx context.Context,
	consumer jetstream.Consumer,
	handler MessageHandler,
) error {
	// Create Messages iterator with optimized settings
	// PullMaxMessages sets buffer size, PullExpiry sets timeout per pull request
	// PullHeartbeat enables heartbeat monitoring for stuck consumers
	iter, err := consumer.Messages(
		jetstream.PullMaxMessages(dh.config.BatchSize),
		jetstream.PullExpiry(dh.config.FetchTimeout),
		jetstream.PullHeartbeat(dh.config.FetchTimeout/2), // Heartbeat at half of expiry
	)
	if err != nil {
		dh.logger.Error("failed to create message iterator", "error", err)

		return fmt.Errorf("failed to create message iterator: %w", err)
	}
	defer iter.Stop()

	dh.logger.Debug("message iterator created successfully",
		"batchSize", dh.config.BatchSize,
		"fetchTimeout", dh.config.FetchTimeout)

	for {
		// Check context cancellation before fetching next message
		select {
		case <-ctx.Done():
			dh.logger.Debug("context cancelled, stopping pull loop")

			return ctx.Err()
		default:
		}

		// Get next message from iterator
		// Next() blocks until message is available or context is cancelled
		msg, err := iter.Next()
		if err != nil {
			// Check for specific error types
			if errors.Is(err, jetstream.ErrMsgIteratorClosed) {
				dh.logger.Debug("message iterator closed")

				return nil
			}

			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}

			if errors.Is(err, jetstream.ErrNoHeartbeat) {
				dh.logger.Error("no heartbeat received from server", "error", err)

				return err
			}

			// Other errors - log and continue
			dh.logger.Warn("error fetching next message, retrying", "error", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(dh.config.RetryBackoff):
				continue
			}
		}

		// Handle message
		err = handler.Handle(ctx, msg)
		if err != nil {
			dh.logger.Warn("message handler returned error, sending NAK",
				"subject", msg.Subject(),
				"error", err)
			// NAK on error for retry
			_ = msg.Nak()
		} else {
			// ACK on success
			_ = msg.Ack()
		}
	}
}

// generatePartitionID creates a partition ID for consumer naming.
//
// Joins partition keys with "-" separator.
// Example: ["source", "region", "us"] → "source-region-us"
func (dh *DurableHelper) generatePartitionID(partition types.Partition) string {
	if len(partition.Keys) == 0 {
		return ""
	}

	return strings.Join(partition.Keys, "-")
}

// generateSubject generates a subject from the template.
//
// Template context contains PartitionID (keys joined with ".").
// Example: ["source", "region", "us"] → "source.region.us"
func (dh *DurableHelper) generateSubject(partition types.Partition) (string, error) {
	if len(partition.Keys) == 0 {
		return "", errors.New("partition has no keys")
	}

	// Create context with PartitionID (keys joined with ".")
	ctx := subjectContext{
		PartitionID: strings.Join(partition.Keys, "."),
	}

	// Execute template
	var buf strings.Builder
	if err := dh.subjectTemplate.Execute(&buf, ctx); err != nil {
		return "", fmt.Errorf("failed to execute subject template: %w", err)
	}

	return buf.String(), nil
}

// ActivePartitions returns the list of currently active partition IDs.
//
// Returns:
//   - []string: List of active partition IDs
func (dh *DurableHelper) ActivePartitions() []string {
	dh.mu.RLock()
	defer dh.mu.RUnlock()

	partitions := make([]string, 0, len(dh.consumers))
	for partID := range dh.consumers {
		partitions = append(partitions, partID)
	}

	return partitions
}

// ConsumerInfo returns information about a consumer for a partition.
//
// Parameters:
//   - partitionID: Partition ID (keys joined with "-")
//
// Returns:
//   - *jetstream.ConsumerInfo: Consumer information
//   - error: Error if consumer not found or info fetch failed
func (dh *DurableHelper) ConsumerInfo(partitionID string) (*jetstream.ConsumerInfo, error) {
	dh.mu.RLock()
	state, exists := dh.consumers[partitionID]
	dh.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("consumer for partition %s not found", partitionID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	info, err := state.consumer.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get consumer info: %w", err)
	}

	return info, nil
}

// Close stops all pull loops and cleans up resources.
//
// Consumers are NOT deleted from NATS - they will be automatically cleaned up
// by NATS based on InactiveThreshold setting.
//
// Parameters:
//   - ctx: Context for graceful shutdown timeout
//
// Returns:
//   - error: Cleanup error
//
// Example:
//
//	defer helper.Close(context.Background())
func (dh *DurableHelper) Close(ctx context.Context) error {
	dh.mu.Lock()
	defer dh.mu.Unlock()

	dh.logger.Info("closing durable helper", "activeConsumers", len(dh.consumers))

	// Cancel all pull loops
	for partID, cancel := range dh.cancelFuncs {
		dh.logger.Debug("cancelling pull loop", "partitionID", partID)
		cancel()
		delete(dh.cancelFuncs, partID)
	}

	// Clear consumer tracking
	dh.consumers = make(map[string]*consumerState)

	// Wait a bit for goroutines to exit gracefully
	select {
	case <-ctx.Done():
		dh.logger.Warn("close context cancelled before graceful shutdown completed")

		return ctx.Err()
	case <-time.After(100 * time.Millisecond):
		// Continue
	}

	dh.logger.Info("durable helper closed successfully")

	return nil
}
