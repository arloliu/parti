package recovery

import (
	"sync/atomic"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// Checkpoint tracks the highest known safe resume sequence for recovery.
// It is seeded from consumer info and advanced on successful helper-owned ack.
//
// All methods are safe for concurrent use.
type Checkpoint struct {
	maxAckedStreamSeq atomic.Uint64
	logger            types.Logger
}

// newCheckpoint creates a Checkpoint with the given logger.
// If logger is nil, a no-op logger is used.
func newCheckpoint(logger types.Logger) Checkpoint {
	if logger == nil {
		logger = logging.NewNop()
	}

	return Checkpoint{logger: logger}
}

// Seed monotonically updates the checkpoint to streamAckFloor if it is higher
// than the current value. Called after binding to a consumer to capture the
// server-side ack floor.
func (cp *Checkpoint) Seed(streamAckFloor uint64) {
	for {
		old := cp.maxAckedStreamSeq.Load()
		if streamAckFloor <= old || cp.maxAckedStreamSeq.CompareAndSwap(old, streamAckFloor) {
			return
		}
	}
}

// Advance monotonically updates the checkpoint to the message's stream sequence.
// Called after a successful helper-owned msg.Ack() when ManualAck is false.
//
// msg.Metadata() parses the reply subject string — no network call, nanosecond overhead.
func (cp *Checkpoint) Advance(msg jetstream.Msg) {
	md, err := msg.Metadata()
	if err != nil {
		cp.logger.Debug("checkpoint: failed to parse message metadata, skipping advance", "error", err)
		return
	}
	seq := md.Sequence.Stream
	for {
		old := cp.maxAckedStreamSeq.Load()
		if seq <= old || cp.maxAckedStreamSeq.CompareAndSwap(old, seq) {
			return
		}
	}
}

// Value returns the current checkpoint sequence.
func (cp *Checkpoint) Value() uint64 {
	return cp.maxAckedStreamSeq.Load()
}
