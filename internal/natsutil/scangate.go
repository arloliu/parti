package natsutil

import (
	"time"

	"github.com/arloliu/parti/v2/types"
)

// Scan-gate tuning shared by the durable reconcile gate and the handoff
// sweep gate. Both gates skip their full KV walk when two stream-position
// probes prove the bucket byte-identical to what the last clean full pass
// observed.
const (
	// ScanGateMaxSkippedPasses bounds consecutive gated skips: a full
	// pass runs at least every ScanGateMaxSkippedPasses+1 ticks
	// regardless of what the probes report — the unknown-unknown backstop
	// (10 min at the default 30s interval).
	ScanGateMaxSkippedPasses = 19

	// ScanGateDefaultConfirmGap is the double-probe confirmation wait. It
	// spans ~2 raft heartbeat intervals so a deposed-but-not-yet-stepped-
	// down stream leader cannot easily answer both probes of one tick with
	// stale state. Tests shorten each gate's own copy of this value.
	ScanGateDefaultConfirmGap = 2 * time.Second
)

// ScanGateConfigGuard drives the unsafe-config edge machine shared by the
// reconcile and sweep scan gates: it emits exactly one Warn on entry into
// an unsafe bucket config, a Debug while the config stays unsafe, and one
// Info when the config is restored. The zero value is ready to use. It
// must be accessed under the same single-goroutine discipline as the gate
// state it guards.
type ScanGateConfigGuard struct {
	unsafeSeen bool
}

// Check reports whether pos's live bucket config is safe for scan gating.
// On the transition into an unsafe config it Warns once (with the
// offending config fields); while the config stays unsafe it Debugs; on
// the transition back to a safe config it Infos once. It returns true iff
// the config is safe. A nil logger suppresses all logging. Check never
// touches the caller's latch/cache state — the caller invalidates that on
// a false return.
func (g *ScanGateConfigGuard) Check(pos KVStreamPos, logger types.Logger) bool {
	if pos.UnsafeConfig() {
		if !g.unsafeSeen {
			g.unsafeSeen = true
			if logger != nil {
				logger.Warn("handoff bucket config violates the scan-gate contract; gate disabled",
					"max_age", pos.MaxAge,
					"allow_msg_ttl", pos.AllowMsgTTL,
					"subject_delete_marker_ttl", pos.SubjectDeleteMarkerTTL,
				)
			}
		} else if logger != nil {
			logger.Debug("handoff bucket config still violates the scan-gate contract")
		}

		return false
	}
	if g.unsafeSeen {
		g.unsafeSeen = false
		if logger != nil {
			logger.Info("handoff bucket config restored; scan gate re-enabled")
		}
	}

	return true
}
