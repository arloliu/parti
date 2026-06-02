package parti

// Degrade reasons — the single registry of every enterDegraded reason string.
//
// These values are an operator-facing contract: they are passed verbatim to the
// OnDegraded hook (see [Hooks.OnDegraded]) and are documented in
// docs/API_REFERENCE.md. The string VALUES are frozen — changing one is an
// operator-visible break — so they are pinned literally by
// TestDegradeReason_LiteralValues in addition to being referenced by name here.
//
// Centralizing them in one block (rather than scattered consts + inline literals
// across manager_degraded.go, manager_assignment.go, manager_setup.go, and
// manager_startup_async.go) keeps the taxonomy in one place and lets the
// recovery-exit gates branch on names instead of magic strings.
const (
	// DegradeReasonKVUnavailable is the connected-but-KV-unavailable condition: a
	// bucket reachable on the live connection but unable to serve ops because its
	// RAFT quorum is lost. Kept distinct from DegradeReasonKVErrorThreshold so the
	// operator surface distinguishes a quorum-loss op stall from a whole-bucket
	// wipe, and so the contract that whole-bucket loss is the ONLY path to the
	// threshold reason is preserved. Recovery-scoped: exit requires a heartbeat Put
	// stamped after the degrade.
	DegradeReasonKVUnavailable = "kv-unavailable"

	// DegradeReasonEnumerationStall is a sustained leader-side worker-enumeration
	// (heartbeat Keys scan) stall that the connectivity / degrading classifiers
	// miss. Kept distinct so the recovery exit can require an enumeration success
	// before exiting. Recovery-scoped (leader-gated).
	DegradeReasonEnumerationStall = "heartbeat-enumeration-stall"

	// DegradeReasonAssignmentWatcherExhausted is emitted by the assignment-watcher
	// retry envelope's permanent-failure callback when the watcher exhausts its
	// attempt budget. Distinct from DegradeReasonKVErrorThreshold so operators can
	// tell "assignment bucket is unrecoverable" from "accumulated transient errors".
	DegradeReasonAssignmentWatcherExhausted = "assignment-watcher-exhausted"

	// DegradeReasonKVErrorThreshold is the whole-bucket-loss reason from
	// recordKVError once the KV-error window crosses the threshold. Whole-bucket
	// loss (connectivity / degrading-JetStream) is the ONLY path to this reason —
	// transient kv-unavailable entries are clearable by a healthy op and degrade
	// with DegradeReasonKVUnavailable instead (the AGENTS.md contract).
	DegradeReasonKVErrorThreshold = "KV error threshold exceeded"

	// DegradeReasonNATSConnectionDown is set by the connection monitor once the
	// NATS connection has been down past the enter threshold.
	DegradeReasonNATSConnectionDown = "NATS connection down"

	// DegradeReasonStreamMissingRecoveryExhausted is the dynamic-consumer
	// stream-missing recovery path: routed through the stream-missing observer
	// rather than the generic KV-error threshold.
	DegradeReasonStreamMissingRecoveryExhausted = "stream-missing-recovery-exhausted"

	// DegradeReasonStartupTimeout is fired by the startup watchdog when the manager
	// is still not Stable after StartupTimeout.
	DegradeReasonStartupTimeout = "startup-timeout"

	// DegradeReasonStartupBackgroundPanic is fired when a background startup
	// goroutine panics.
	DegradeReasonStartupBackgroundPanic = "startup-background-panic"
)

// degradeReasonBucketRecreatedPrefix is the fixed prefix of the dynamic
// bucket-recreated reason; the full reason carries the affected bucket name.
const degradeReasonBucketRecreatedPrefix = "bucket-recreated:"

// degradeReasonBucketRecreated builds the dynamic enterDegraded reason emitted by
// the epoch-fence monitor when a Parti-owned bucket is wiped and recreated. The
// "<prefix><bucket>" shape is an operator-facing contract (the readiness probe and
// simulation oracles match on it), so it is pinned by TestDegradeReason_LiteralValues.
func degradeReasonBucketRecreated(bucket string) string {
	return degradeReasonBucketRecreatedPrefix + bucket
}
