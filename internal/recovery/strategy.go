package recovery

// Strategy defines how a recreated consumer decides where to resume
// after an unexpected deletion. The zero value means recovery is disabled.
type Strategy int

const (
	// Disabled is the zero value. No strategy-aware auto-recovery is performed.
	// Iterator errors will trigger backoff and retry without recreating the
	// consumer using a recovery-adjusted DeliverPolicy.
	//
	// Note: Dynamic consumers retain their pre-existing escalation-based
	// remediation (rebind to the same durable with the original config) even
	// when recovery is Disabled. This is intentional — Disabled controls
	// strategy-aware recreation, not the legacy remediation path.
	Disabled Strategy = iota

	// FromNew recreates the consumer to only receive newly published messages.
	// Maps to: DeliverPolicy = DeliverNewPolicy.
	// Pros: Zero replay storm. Safe default for Queue consumers.
	// Cons: Unacknowledged messages since deletion are skipped.
	FromNew

	// FromLastProcessed recreates the consumer starting at
	// (highest_acked_stream_sequence + 1).
	// Maps to: DeliverPolicy = DeliverByStartSequencePolicy, OptStartSeq = checkpoint + 1.
	//
	// Supported with both ManualAck modes. With ManualAck=false, the framework advances
	// the checkpoint automatically. With ManualAck=true, the message is wrapped so that
	// msg.Ack() / msg.DoubleAck() advance the checkpoint before forwarding.
	// Not supported for Queue consumers: shared durables make cross-instance resume
	// nondeterministic.
	FromLastProcessed

	// FromBeginning recreates the consumer to deliver all messages in the stream.
	// Maps to: DeliverPolicy = DeliverAllPolicy.
	// WARNING: Causes a complete backlog replay storm. Use only for small/bounded streams.
	FromBeginning
)
