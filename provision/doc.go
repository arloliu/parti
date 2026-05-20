// Package provision manages the NATS resources Parti's runtime depends on:
// control-plane KV buckets and the partition-source bucket.
//
// The primary entry points are:
//
//   - Config — desired-state input loaded from YAML or constructed in Go.
//   - Validate(cfg) — pure static validation with bucket-name defaulting.
//   - View(ctx, js, scope) — read-only inventory of Parti-marked resources.
//   - Plan(ctx, js, cfg) — deterministic drift report and proposed actions.
//     Read-only; never mutates NATS.
//   - Apply(ctx, js, cfg) — executes the Plan actions against live NATS.
//
// Plan emits three action kinds depending on the reconcile policy:
//
//   - "create-kv": create a missing KV bucket (all policies except adopt).
//   - "update-kv": reconcile drift-mutable fields in place on a Parti-marked
//     bucket (safe-update policy only).
//   - "stamp-marker": stamp the Parti ownership marker on an unmarked bucket
//     that exists live, preserving all non-Parti metadata (adopt policy only).
//
// Reconcile policies (set via Config.Policy or the -policy CLI flag):
//
//   - "warn" (default): create missing buckets; report drift; never mutate
//     existing resources.
//   - "adopt": stamp the Parti marker on unmarked existing buckets named by
//     config; create nothing; update no non-marker fields.
//   - "safe-update": create missing buckets; reconcile drift-mutable fields
//     (Metadata, TTL, MaxValueSize, Replicas) on Parti-marked buckets. Never
//     mutates unmarked buckets; operators run adopt first.
//   - "force": reserved, not yet supported.
//
// Byte-equivalence invariant: every "create-kv" action's KeyValueConfig is
// built by calling github.com/arloliu/parti/v2/internal/kvbuckets.BuildKeyValueConfig
// and then attaching the Parti ownership marker in Metadata. The same builder
// is used by (*Manager).ensureKVBucket at runtime, so a provision-managed
// bucket is byte-identical (modulo Metadata and an explicit Replicas override)
// to one created by Parti's manager.
//
// ValidateLiveDynamicConsumers is best-effort: stream-info fetch failures
// inside the underlying consumer.CheckWorkQueueRecoveryCompat helper are
// silently ignored, mirroring the runtime's tolerance of transient stream
// unavailability. This means a misconfigured or nonexistent stream name
// produces an "OK" result rather than a "stream not found" error — the check
// answers "is this stream's recovery strategy compatible with WorkQueuePolicy?"
// not "does this stream exist?".
package provision
