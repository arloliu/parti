// Package provision provides a read-only SDK for viewing, validating, and
// planning the NATS resources Parti's runtime depends on.
//
// Phase 1 (W1) of the provision-sdk-cli plan delivers:
//
//   - Config — desired-state input loaded from YAML or constructed in Go.
//   - Validate(cfg) — pure static validation with bucket-name defaulting.
//   - View(ctx, js, scope) — read-only inventory of Parti-marked resources.
//   - Plan(ctx, js, cfg) — deterministic drift report and create-kv actions
//     for control-plane KV buckets. No mutation.
//
// Plan emits at most one PlannedAction kind in v1 — "create-kv" — and only
// for control-plane buckets. Partition-source create-kv actions, dynamic-
// consumer alignment, and the Apply mutation path land in later work items
// (W2–W4) of the same plan. See docs/plans/provision-sdk-cli for the full
// design and roadmap.
//
// Byte-equivalence invariant: every "create-kv" action's KeyValueConfig is
// built by calling github.com/arloliu/parti/v2/internal/kvbuckets.BuildKeyValueConfig
// and then attaching the Parti ownership marker in Metadata. The same builder
// is used by (*Manager).ensureKVBucket at runtime, so a provision-managed
// bucket is byte-identical (modulo the Metadata marker) to one created by
// Parti's manager.
//
// ValidateLiveDynamicConsumers is best-effort: stream-info fetch failures
// inside the underlying consumer.CheckWorkQueueRecoveryCompat helper are
// silently ignored, mirroring the runtime's tolerance of transient stream
// unavailability. This means a misconfigured or nonexistent stream name
// produces an "OK" result rather than a "stream not found" error — the check
// answers "is this stream's recovery strategy compatible with WorkQueuePolicy?"
// not "does this stream exist?".
package provision
