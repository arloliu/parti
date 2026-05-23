package types

import "errors"

// ErrStreamMissing is the typed-error umbrella for the operator-driven
// stream-missing recovery flow. The library wraps every failure that
// occurs during the post-StreamMissingHook recovery episode with this
// sentinel, regardless of the underlying cause (still-missing-stream,
// incompatible-restored-consumer-config, etc.), so the manager
// observer route fires consistently and enterDegraded surfaces a
// single named reason ("stream-missing-recovery-exhausted") instead
// of the generic "KV error threshold exceeded".
//
// Callers identify the umbrella via errors.Is(err, ErrStreamMissing);
// the underlying cause remains in the error chain for diagnostics.
var ErrStreamMissing = errors.New("stream missing (operator-driven recovery)")

// StreamMissingHook fires when a dynamic consumer cannot create a
// consumer because the underlying JetStream stream is absent.
//
// Contract:
//   - The hook is the escalation path; the library does not recreate
//     streams. Operators recreate the stream (e.g. via parti.Provision)
//     and return nil so the library's recovery loop can rebuild the
//     consumer against the freshly-restored stream.
//   - Returning a nil error indicates the caller has re-created the
//     stream. The library will then call Controller.HandleStreamRecreated
//     and RebuildAfterStreamRecreated to bind a new consumer.
//   - Returning a non-nil error or omitting the hook entirely surfaces
//     the loss via the F2 envelope's exhaustion path — manager
//     Hooks.OnError + enterDegraded — so the readiness probe can
//     rotate the pod.
//
// Consumer-state restore is OPTIONAL but is bound to two rules:
//
//   - SAME-DURABLE-NAME. If the caller preserves the durable consumer
//     WITH THE SAME NAME Parti was using and with non-zero AckFloor,
//     Controller.HandleStreamRecreated picks up the AckFloor via the
//     existing consumer handle. The next consumer build produces
//     DeliverByStartSequencePolicy(AckFloor+1).
//
//   - COMPATIBLE CONFIG. If the caller preserves a same-named consumer
//     but with INCOMPATIBLE config (different DeliverPolicy, AckPolicy,
//     InactiveThreshold), the post-recreate consumer build invokes
//     js.CreateOrUpdateConsumer with the Parti-derived config; NATS
//     responds with a consumer-config-mismatch error. This surfaces as
//     a wrapped ErrStreamMissing and the F2 envelope retries until
//     exhaustion. The operator must either reconcile the restored
//     consumer config OR delete the consumer so Parti can recreate it
//     fresh on the restored stream.
//
//   - If the caller recreates the stream with NO consumer (or a
//     consumer with a DIFFERENT durable name), Parti treats it as "no
//     preserved consumer state". The checkpoint stays at 0 and the
//     next consumer build produces DeliverAllPolicy (replay from
//     sequence 1).
//
// The hook MUST be safe to call from a recovery goroutine and SHOULD
// return promptly. A long-running hook delays the consumer rebuild
// and keeps the F2 envelope's attempt budget ticking.
//
// REQUIRED RecoveryStrategy: configuring StreamMissingHook requires
// either RecoverFromLastProcessed (at-least-once semantics) or
// RecoverFromBeginning (replay-all, intentional duplicate processing).
// RecoveryDisabled (the default) and RecoverFromNew are rejected at
// construction time because they would either disable the recovery
// controller entirely or silently skip messages published after a
// fresh-stream recreate.
type StreamMissingHook func(streamName string) error
