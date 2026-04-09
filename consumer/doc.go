// Package consumer provides unified JetStream consumer types for different consumption patterns.
//
// This package offers a consistent API for four distinct consumer types, each designed
// for specific use cases. All consumer types use the unified [MessageHandler] interface.
//
// # Choosing a Consumer Type
//
//	Type         Use Case                                Coordination  Lifecycle
//	[Queue]      Load-balanced workers                   None          Start → Stop
//	[Static]     StatefulSet fixed partition             None          Start → Stop
//	[Broadcast]  Fan-out to all instances                None          Start → Stop
//	[Dynamic]    Manager-assigned partitions (Parti)     Via Manager   Update → Stop
//
// # Pairing with Publishers
//
// Each consumer type pairs with a publisher from [partition] or JetStream:
//
//	Consumer       Typical Publisher            Notes
//	[Queue]        js.Publish / jsutil          Plain JetStream publish; all workers share load
//	[Static]       partition.JSPublisher        Hashes key → fixed partition subject
//	[Broadcast]    js.Publish                   Single publish reaches all instances
//	[Dynamic]      partition.JSPublisher        Hashes key → partition assigned by Manager
//
// Example: publish to a Static or Dynamic consumer using partition.JSPublisher:
//
//	pub, err := partition.NewJSPublisher(js, partition.NewConfig(
//	    partition.WithNumPartitions(10),
//	    partition.WithSubjectPattern("events.{{partition}}"),
//	))
//	if err != nil { log.Fatal(err) }
//
//	// Key "order-123" is consistently hashed to the same partition.
//	if err := pub.Publish(ctx, "order-123", payload); err != nil { log.Fatal(err) }
//
// # Lifecycle
//
// Each consumer type follows a consistent lifecycle pattern:
//
// Queue and Static (Start/Stop pattern)
//
//	// Create and configure
//	c, err := consumer.NewQueue(js, "stream", "consumer", "subject.>", handler)
//	if err != nil { log.Fatal(err) }
//
//	// Always stop on exit to avoid goroutine leaks
//	defer c.Stop(ctx)
//
//	// Begin consuming (starts background goroutine)
//	if err := c.Start(ctx); err != nil { log.Fatal(err) }
//
//	// ... application runs ...
//
// Broadcast (Start/Stop pattern)
//
//	c, err := consumer.NewBroadcast(js, "stream", "prefix", "events.>", handler)
//	if err != nil { log.Fatal(err) }
//	defer c.Stop(ctx)
//
//	if err := c.Start(ctx); err != nil { log.Fatal(err) }
//
// Dynamic (Update/Stop pattern)
//
//	c, err := consumer.NewDynamic(js, "stream", "prefix", "orders.{{.PartitionID}}", handler)
//	if err != nil { log.Fatal(err) }
//	defer c.Stop(ctx)
//
//	// Consumption starts when Update is called (typically by Parti Manager)
//	if err := c.Update(ctx, "worker-0", partitions); err != nil { log.Fatal(err) }
//
// # Thread Safety
//
// All consumer types are safe for concurrent use. Lifecycle methods (Start, Stop,
// Update) are serialized internally to prevent race conditions.
//
// # Message Handler
//
// All consumer types use the unified [MessageHandler] interface:
//
//	handler := consumer.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
//	    // Process message
//	    return nil  // auto-ack on success
//	})
//
// By default, returning nil automatically acknowledges the message, and returning
// an error triggers a negative acknowledgement (NAK). Set ManualAck=true to control
// acknowledgement explicitly via msg.Ack(), msg.Nak(), or msg.Term().
//
// # Error Handling
//
// Constructor errors are validation failures (nil JetStream, missing required fields).
// These are not exported sentinel errors—use string matching or error wrapping checks.
//
// Runtime errors from lifecycle methods:
//   - [context.DeadlineExceeded]: Stop timed out waiting for graceful shutdown
//   - Worker ID changed unexpectedly (Dynamic consumer only)
//   - Partition count exceeds configured limit (Dynamic consumer only)
//
// # Consumer Types
//
// # Queue
//
// Load-balanced shared consumer.
// Use for classic worker queue patterns where each message goes to exactly one instance.
// Multiple replicas share a single durable consumer name, achieving queue group semantics.
//
//	c, _ := consumer.NewQueue(js, "jobs", "job-workers", "jobs.>", handler)
//	defer c.Stop(ctx)
//	_ = c.Start(ctx)
//
// # Static
//
// Fixed partition assignment (e.g., StatefulSet ordinal).
// Use when each pod processes exactly one predetermined partition.
//
//	c, _ := consumer.NewStatic(js, "events", "processor-0", "events.{{partition}}", 10, 0, handler)
//	defer c.Stop(ctx)
//	_ = c.Start(ctx)
//
// # Dynamic
//
// Manager-assigned partitions (Parti core).
// Use when partitions are dynamically distributed by a coordination layer.
// The consumer manages multiple internal per-partition consumers based on assignments.
//
//	c, _ := consumer.NewDynamic(js, "events", "worker", "orders.{{.PartitionID}}.events", handler)
//	defer c.Stop(ctx)
//	_ = c.Update(ctx, workerID, partitions)
//
// # Broadcast
//
// Fan-out to all instances.
// Use when every instance must receive every message (caching, audit logs, notifications).
// Each instance receives a copy of every message matching the wildcard filter.
//
// IMPORTANT: The stream MUST use LimitsPolicy or InterestPolicy. WorkQueuePolicy
// is incompatible because it delivers each message to exactly one consumer.
//
//	c, _ := consumer.NewBroadcast(js, "events", "cache-updater", "events.>", handler)
//	defer c.Stop(ctx)
//	_ = c.Start(ctx)
//
// # Auto-Recovery
//
// All consumer types can automatically recreate their durable JetStream consumer
// when it is unexpectedly deleted (e.g., after an InactiveThreshold expiry or
// server-side administrative deletion). Use [WithRecoveryStrategy] to enable it:
//
//	c, err := consumer.NewQueue(js, "stream", "workers", "jobs.>", handler,
//	    consumer.WithRecoveryStrategy(consumer.RecoverFromNew),
//	)
//
// Choose a strategy based on your delivery guarantee requirements:
//
//	Strategy                    Behavior on recreation          Risk
//	[RecoveryDisabled]          No recreation (default)         None
//	[RecoverFromNew]            Skip missed messages            Message loss during outage
//	[RecoverFromLastProcessed]  Resume from last acked msg      Works with any ManualAck
//	[RecoverFromBeginning]      Full stream replay from start   Replay storm on large streams
//
// Per-consumer support:
//
//	Consumer     RecoveryDisabled  RecoverFromNew  RecoverFromLastProcessed  RecoverFromBeginning
//	[Queue]      ✓                 ✓               ✗ (shared durable)        ✓
//	[Broadcast]  ✓                 ✓               ✓ (any ManualAck)         ✓
//	[Static]     ✓                 ✓               ✓ (any ManualAck)         ✓
//	[Dynamic]    ✓                 ✓               ✓ (any ManualAck)         ✓
//
// The only invalid combination is [RecoverFromLastProcessed] on a [Queue], which
// returns [ErrInvalidConfig] at construction time.
//
// # Options
//
// All constructors accept functional options for customization:
//
//	c, _ := consumer.NewQueue(js, "stream", "consumer", "subject.>", handler,
//	    consumer.WithLogger(myLogger),
//	    consumer.WithAckWait(60*time.Second),
//	    consumer.WithBatchSize(10),
//	)
//
// Common options ([Option]) work with all consumer types. Type-specific options
// (e.g., [QueueOption], [StaticOption]) are enforced at compile time.
//
// # Field Mapping
//
// The consumer package uses unified field names in constructors:
//
//	consumerName/Prefix  ConsumerName    ConsumerPrefix   ConsumerPrefix   ConsumerName
//	subject pattern      SubjectPattern  SubjectTemplate  FilterSubject    FilterSubject
//
// # Helpers
//
// The package provides helper functions to assist with partition determination in Kubernetes environments:
//
//   - [GetPartitionFromEnv]: Reads partition index from PARTITION_INDEX or HOSTNAME (for StatefulSets).
//   - [ParseStatefulSetOrdinal]: Extracts the ordinal index from a hostname string.
package consumer
