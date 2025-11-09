package subscription

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// WorkerConsumer manages a single JetStream durable pull consumer per worker for partition-based work distribution.
type WorkerConsumer struct {
	conn   *nats.Conn
	js     jetstream.JetStream
	config WorkerConsumerConfig
	logger types.Logger

	// Template for subject generation
	subjectTemplate *template.Template

	// State tracking
	mu sync.RWMutex

	// Single-consumer (per worker) mode state
	workerID       string
	workerConsumer jetstream.Consumer
	workerCancel   context.CancelFunc
	workerSubjects []string // last applied subjects (deduped, sorted)
	handler        MessageHandler
}

// subjectContext is the template context for subject generation.
type subjectContext struct {
	PartitionID string
}

// NewWorkerConsumer creates a new durable consumer manager for a single worker.
//
// This helper implements the single-consumer (per-worker) pattern. Instead of
// creating one consumer per partition, it maintains a single durable pull
// consumer whose FilterSubjects set (plural) is updated whenever the worker's
// assignment changes. This greatly reduces JetStream consumer churn and avoids
// expensive restart storms during rapid scaling events while still allowing
// precise subject-level filtering.
//
// The helper handles:
//   - Durable consumer creation (CreateOrUpdateConsumer)
//   - Subject template expansion per partition (deduped + sorted)
//   - Resilient pull loop with heartbeat tolerance
//   - ACK/NAK semantics driven by the injected MessageHandler
//
// Optional configuration fields are automatically set to sensible defaults if
// not provided. The message handler is required and immutable for the lifetime
// of the helper.
//
// Parameters:
//   - conn: NATS connection (must be non-nil)
//   - cfg: Helper configuration with required fields (StreamName, ConsumerPrefix, SubjectTemplate)
//   - handler: Message handler invoked for each received JetStream message
//
// Returns:
//   - *WorkerConsumer: Initialized helper with defaults applied
//   - error: Configuration, connection, template parsing, or handler error
//
// Example (minimal configuration with defaults):
//
//	helper, err := subscription.NewWorkerConsumer(natsConn, subscription.WorkerConsumerConfig{
//	    StreamName:      "work-stream",
//	    ConsumerPrefix:  "processor",
//	    SubjectTemplate: "metrics.{{.PartitionID}}.collected",
//	}, subscription.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
//	    // process message
//	    return msg.Ack()
//	}))
//
// Example (with custom configuration):
//
//	helper, err := subscription.NewWorkerConsumer(natsConn, subscription.WorkerConsumerConfig{
//	    StreamName:        "work-stream",
//	    ConsumerPrefix:    "processor",
//	    SubjectTemplate:   "metrics.{{.PartitionID}}.collected",
//	    BatchSize:         50,               // Override default (1)
//	    FetchTimeout:      10 * time.Second, // Override default (5s)
//	    AckWait:           45 * time.Second, // Override default (30s)
//	    Logger:            myLogger,         // Optional: omit for no-op logger
//	}, subscription.MessageHandlerFunc(customHandler))
func NewWorkerConsumer(conn *nats.Conn, cfg WorkerConsumerConfig, handler MessageHandler) (*WorkerConsumer, error) {
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

	if handler == nil {
		return nil, errors.New("message handler is required")
	}

	// Create JetStream context and delegate to NewWorkerConsumerJS for construction
	js, err := jetstream.New(conn)
	if err != nil {
		return nil, fmt.Errorf("failed to create JetStream context: %w", err)
	}

	return NewWorkerConsumerJS(js, cfg, handler)
}

// NewWorkerConsumerJS creates a new WorkerConsumer using a pre-initialized JetStream context.
//
// This overload enables looser coupling to the nats client by accepting the
// jetstream.JetStream interface instead of a concrete *nats.Conn. The underlying
// connection is captured via js.Conn() for internal status/logging when needed.
//
// Parameters:
//   - js: Pre-configured JetStream context (must be non-nil)
//   - cfg: Helper configuration with required fields
//   - handler: Message handler invoked for each received JetStream message
//
// Returns:
//   - *WorkerConsumer: Initialized helper
//   - error: Configuration or template parsing error
func NewWorkerConsumerJS(js jetstream.JetStream, cfg WorkerConsumerConfig, handler MessageHandler) (*WorkerConsumer, error) {
	if js == nil {
		return nil, errors.New("JetStream context is required")
	}

	// Validate essential cfg fields and handler (reuse same checks as NewWorkerConsumer)
	if cfg.StreamName == "" {
		return nil, errors.New("stream name is required")
	}
	if cfg.ConsumerPrefix == "" {
		return nil, errors.New("consumer prefix is required")
	}
	if cfg.SubjectTemplate == "" {
		return nil, errors.New("subject template is required")
	}
	if handler == nil {
		return nil, errors.New("message handler is required")
	}

	// Apply defaults and parse template
	cfg.applyDefaults()
	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	if err != nil {
		return nil, fmt.Errorf("invalid subject template: %w", err)
	}

	return &WorkerConsumer{
		conn:            js.Conn(),
		js:              js,
		config:          cfg,
		logger:          cfg.Logger,
		subjectTemplate: tmpl,
		workerSubjects:  nil,
		handler:         handler,
	}, nil
}

// UpdateWorkerConsumer reconciles the worker-level durable consumer with the complete
// set of assigned partitions (single-consumer mode).
//
// Behavior:
//   - Builds subjects from partitions via the SubjectTemplate (deduped + sorted)
//   - Diffs against the previously applied subject list; no-op if unchanged
//   - Uses jetstream.CreateOrUpdateConsumer to atomically apply FilterSubjects
//   - Maintains a single long-lived pull loop (loop is started lazily on first update)
//   - Does NOT restart the pull loop on subsequent updates (hot-reload semantics)
//
// Idempotency: Calling with the same partition set is a no-op and returns nil.
//
// Concurrency: Safe for concurrent calls; internal locking ensures consistent state.
//
// Parameters:
//   - ctx: Context for cancellation and retry backoff timing
//   - workerID: Stable worker identifier (forms durable name <ConsumerPrefix>-<workerID>)
//   - partitions: Complete assignment slice (may be empty for no subjects)
//
// Returns:
//   - error: Non-nil only on unrecoverable configuration or JetStream API failure after retries
//
// Example:
//
//	helper, _ := subscription.NewWorkerConsumer(nc, subscription.WorkerConsumerConfig{
//	    StreamName:      "events",
//	    ConsumerPrefix:  "worker",
//	    SubjectTemplate: "events.{{.PartitionID}}",
//	}, subscription.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
//	    // process
//	    return msg.Ack()
//	}))
//	// initial assignment
//	_ = helper.UpdateWorkerConsumer(ctx, "worker-7", []types.Partition{{Keys: []string{"a","0"}}, {Keys: []string{"b","3"}}})
//	// later, assignment shrinks
//	_ = helper.UpdateWorkerConsumer(ctx, "worker-7", []types.Partition{{Keys: []string{"a","0"}}})
func (dh *WorkerConsumer) UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []types.Partition) error {
	if workerID == "" {
		return errors.New("workerID is required")
	}

	// Build deduped, sorted subject list
	subjects, err := dh.buildSubjects(partitions)
	if err != nil {
		return err
	}

	dh.mu.Lock()
	// Capture prior workerID to allow best-effort cleanup if it changes
	prevWorkerID := dh.workerID
	// Fast no-op if nothing changed (and workerID matches)
	if prevWorkerID == workerID && equalStringSlices(subjects, dh.workerSubjects) {
		dh.mu.Unlock()
		return nil
	}
	dh.mu.Unlock()

	// Prepare consumer config
	durable := dh.sanitizeConsumerName(dh.config.ConsumerPrefix + "-" + workerID)
	cfg := jetstream.ConsumerConfig{
		Name:              durable,
		Durable:           durable,
		FilterSubjects:    subjects,
		AckPolicy:         dh.config.AckPolicy,
		AckWait:           dh.config.AckWait,
		MaxDeliver:        dh.config.MaxDeliver,
		InactiveThreshold: dh.config.InactiveThreshold,
		MaxWaiting:        dh.config.MaxWaiting,
	}

	// Apply with retries using the JS manager to avoid extra stream lookup work
	var cons jetstream.Consumer
	var lastErr error
	for attempt := 0; attempt <= dh.config.MaxRetries; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		cons, lastErr = dh.js.CreateOrUpdateConsumer(ctx, dh.config.StreamName, cfg)
		if lastErr == nil {
			break
		}
		if attempt >= dh.config.MaxRetries {
			return fmt.Errorf("failed to create/update worker consumer %s after %d attempts: %w", durable, dh.config.MaxRetries+1, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(dh.config.RetryBackoff):
		}
	}

	dh.mu.Lock()
	dh.workerID = workerID
	dh.workerConsumer = cons
	dh.workerSubjects = subjects
	// Ensure pull loop is running
	if dh.workerCancel == nil {
		// Start a background pull loop. Use background to decouple from caller ctx; cancellation via Close or future updates is handled internally.
		pullCtx, cancel := context.WithCancel(context.Background())
		dh.workerCancel = cancel
		go dh.runWorkerPullLoop(pullCtx)
	}
	dh.mu.Unlock()

	// Best-effort cleanup: if workerID changed, attempt to delete the old durable consumer.
	if prevWorkerID != "" && prevWorkerID != workerID {
		oldDurable := dh.sanitizeConsumerName(dh.config.ConsumerPrefix + "-" + prevWorkerID)
		go func(streamName, durable string) {
			// Short timeout, detached from caller's ctx
			delCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := dh.js.DeleteConsumer(delCtx, streamName, durable); err != nil {
				// Log and move on; do not fail the update path
				dh.logger.Warn("best-effort delete of old durable failed", "durable", durable, "error", err)
			} else {
				dh.logger.Info("deleted old durable after workerID change", "durable", durable)
			}
		}(dh.config.StreamName, oldDurable)
	}

	return nil
}

// runWorkerPullLoop runs the single-consumer pull loop using the configured handler.
func (dh *WorkerConsumer) runWorkerPullLoop(ctx context.Context) {
	// Snapshot workerID under read lock to avoid races with Close()
	dh.mu.RLock()
	durableName := dh.sanitizeConsumerName(dh.config.ConsumerPrefix + "-" + dh.workerID)
	dh.mu.RUnlock()
	dh.logger.Debug("starting worker pull loop", "durable", durableName)

	for {
		dh.mu.RLock()
		cons := dh.workerConsumer
		handler := dh.handler
		batch := dh.config.BatchSize
		expiry := dh.config.FetchTimeout
		dh.mu.RUnlock()

		if cons == nil {
			// Wait briefly and retry until consumer appears; this can happen briefly during startup/update races
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
				continue
			}
		}

		iter, err := cons.Messages(
			jetstream.PullMaxMessages(batch),
			jetstream.PullExpiry(expiry),
			jetstream.PullHeartbeat(expiry/2),
		)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			dh.logger.Error("failed to create worker message iterator", "error", err)
			// Backoff and retry creating iterator
			select {
			case <-ctx.Done():
				return
			case <-time.After(dh.config.RetryBackoff):
				continue
			}
		}

		// Iterate until error or context cancel
		for {
			select {
			case <-ctx.Done():
				iter.Stop()
				return
			default:
			}

			msg, err := iter.Next()
			if err != nil {
				iter.Stop()
				// Treat missing heartbeat as a signal to recreate iterator
				if errors.Is(err, jetstream.ErrMsgIteratorClosed) {
					return
				}
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return
				}
				if errors.Is(err, jetstream.ErrNoHeartbeat) {
					dh.logger.Error("worker pull loop: no heartbeat", "error", err)
					// recreate iterator by breaking the inner loop
					break
				}
				dh.logger.Warn("worker pull loop: iterator error, retrying", "error", err)
				// recreate iterator by breaking the inner loop after backoff
				select {
				case <-ctx.Done():
					return
				case <-time.After(dh.config.RetryBackoff):
				}

				break
			}

			if handler == nil {
				// No handler configured yet; NAK to retry later and avoid message loss
				_ = msg.Nak()
				continue
			}

			if err := handler.Handle(ctx, msg); err != nil {
				_ = msg.Nak()
			} else {
				_ = msg.Ack()
			}
		}
		// loop to recreate iterator
	}
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
func (dh *WorkerConsumer) Close(ctx context.Context) error {
	dh.mu.Lock()
	defer dh.mu.Unlock()

	dh.logger.Info("closing durable helper")

	// Cancel worker single-consumer pull loop if running
	if dh.workerCancel != nil {
		dh.logger.Debug("cancelling worker pull loop", "workerID", dh.workerID)
		dh.workerCancel()
		dh.workerCancel = nil
	}

	dh.workerConsumer = nil
	dh.workerSubjects = nil
	dh.workerID = ""

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

// WorkerConsumerInfo returns the JetStream ConsumerInfo for the worker-level durable consumer
// created/managed by UpdateWorkerConsumer.
//
// Behavior:
//   - Returns an error if UpdateWorkerConsumer has not been called yet (consumer uninitialized)
//   - Delegates to the Consumer.Info(ctx) call with the provided context
//
// Parameters:
//   - ctx: Context for cancellation and timeout
//
// Returns:
//   - *jetstream.ConsumerInfo: Consumer metadata and current FilterSubjects
//   - error: Non-nil if consumer is not initialized or Info() call fails
func (dh *WorkerConsumer) WorkerConsumerInfo(ctx context.Context) (*jetstream.ConsumerInfo, error) {
	dh.mu.RLock()
	cons := dh.workerConsumer
	dh.mu.RUnlock()
	if cons == nil {
		return nil, errors.New("worker consumer not initialized")
	}
	info, err := cons.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get worker consumer info: %w", err)
	}

	return info, nil
}

// WorkerSubjects returns a copy of the last applied FilterSubjects list for the worker-level
// durable consumer managed by UpdateWorkerConsumer.
//
// Returns:
//   - []string: Copy of subject list (nil if consumer not yet initialized)
//
// Example:
//
//	subjects := helper.WorkerSubjects()
//	for _, s := range subjects { log.Println("subject", s) }
func (dh *WorkerConsumer) WorkerSubjects() []string {
	dh.mu.RLock()
	defer dh.mu.RUnlock()
	if dh.workerSubjects == nil {
		return nil
	}

	out := make([]string, len(dh.workerSubjects))
	copy(out, dh.workerSubjects)

	return out
}

// sanitizeConsumerName replaces invalid characters from consumer name to underscore (_).
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
func (dh *WorkerConsumer) sanitizeConsumerName(name string) string {
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

// generateSubject generates a subject from the template.
//
// Template context contains PartitionID (keys joined with ".").
// Example: ["source", "region", "us"] → "source.region.us"
func (dh *WorkerConsumer) generateSubject(partition types.Partition) (string, error) {
	if len(partition.Keys) == 0 {
		return "", errors.New("partition has no keys")
	}

	ctx := subjectContext{PartitionID: partition.SubjectKey()}

	// Execute template
	var buf strings.Builder
	if err := dh.subjectTemplate.Execute(&buf, ctx); err != nil {
		return "", fmt.Errorf("failed to execute subject template: %w", err)
	}

	return buf.String(), nil
}

// buildSubjects generates a sorted, deduplicated list of subjects from partitions.
func (dh *WorkerConsumer) buildSubjects(partitions []types.Partition) ([]string, error) {
	if len(partitions) == 0 {
		return []string{}, nil
	}
	// Deduplicate via map
	m := make(map[string]struct{}, len(partitions))
	for _, p := range partitions {
		subj, err := dh.generateSubject(p)
		if err != nil {
			return nil, err
		}
		m[subj] = struct{}{}
	}
	subjects := make([]string, 0, len(m))
	for s := range m {
		subjects = append(subjects, s)
	}
	// Sort for deterministic ordering
	slices.Sort(subjects)

	return subjects, nil
}

// equalStringSlices compares two string slices for equality assuming both are sorted.
func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
