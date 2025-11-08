// Package subscription provides durable JetStream consumer management utilities.
//
// # Overview
//
// Historically this package exposed a per-partition subscription reconciler that
// created/removed one durable pull consumer per partition. That model caused
// excessive JetStream control-plane churn during scaling events (hundreds of
// rapid consumer create/delete operations) and increased restart storms when
// multiple workers rebalanced simultaneously.
//
// The modern design eliminates per-partition consumers in favor of a single
// durable pull consumer per worker whose FilterSubjects list is hot-reloaded
// whenever the worker's assignment changes. This drastically reduces churn
// while preserving precise subject filtering for partition isolation.
//
// Key Types
//   - WorkerConsumer: Manages the single durable consumer lifecycle, calculates
//     deterministic subject sets from partitions (template expansion, dedupe,
//     sort), applies changes atomically via CreateOrUpdateConsumer, and runs a
//     resilient pull loop with heartbeat tolerance.
//   - MessageHandler / MessageHandlerFunc: Minimal interface for user message
//     processing, enabling dependency injection and straightforward testing.
//
// WorkerConsumer Highlights
//   - Hot-reload semantics: Pull loop is started once and not restarted on
//     subject changes, avoiding message processing gaps.
//   - Deterministic subject ordering: Subjects are deduped and sorted to prevent
//     unnecessary consumer updates due to ordering differences.
//   - Best-effort old durable cleanup: When workerID changes (e.g., stable ID
//     reallocation) the previous durable is deleted asynchronously to prevent
//     orphaned consumers accumulating.
//   - Batched pulls: Uses Messages() iterator with configurable batch size and
//     heartbeat interval for efficient streaming without busy polling.
//   - Minimal defaults: Sensible configuration defaults applied automatically.
//
// Removed / Deprecated
//   - UpdateSubscriptions (per-partition API) and related helper methods
//     (getOrCreateConsumer, tryGetOrCreateConsumer, startPullLoop, generatePartitionID,
//     consumerState tracking) have been removed. Tests referencing these have
//     been migrated to worker-level semantics or excluded via build tags.
//
// Usage Pattern
//
//	helper, err := subscription.NewWorkerConsumer(nc, subscription.WorkerConsumerConfig{
//	    StreamName:      "events-stream",
//	    ConsumerPrefix:  "worker",
//	    SubjectTemplate: "events.{{.PartitionID}}",
//	}, subscription.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
//	    // process message
//	    return msg.Ack()
//	}))
//	if err != nil { log.Fatal(err) }
//	defer helper.Close(context.Background())
//
//	// Apply initial assignment
//	_ = helper.UpdateWorkerConsumer(ctx, "worker-7", partitions)
//
//	// Later, after a rebalance shrinks assignment
//	_ = helper.UpdateWorkerConsumer(ctx, "worker-7", smallerPartitions)
//
// Interface Assertions
// Internal compile-time assertions live in internal packages; public packages
// avoid importing root to prevent cycles. WorkerConsumer deliberately omits an
// interface assertion; users can add one in their own test code if desired.
//
// Concurrency & Safety
// All exported WorkerConsumer methods are safe for concurrent use. UpdateWorkerConsumer
// employs fine-grained locking and performs diff checks before mutating state to
// minimize unnecessary JetStream calls.
//
// See worker_consumer_extended_test.go for integration scenarios covering
// lifecycle, expansion, workerID switches, external deletion, and concurrency.
package subscription
