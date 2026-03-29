package durable

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/arloliu/fuda"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// ProcessingGateConfig configures the Processing Gate behavior.
type ProcessingGateConfig struct {
	// Enabled toggles the gate.
	// Default: false (gate disabled).
	Enabled bool

	// AllowedStates defines which handoff states permit message processing.
	// If empty, defaults to [StateStable].
	//
	// Common configurations:
	//  - [StateStable]: Strict consistency (default). Messages only processed when ownership is stable.
	//  - [StateStable, StateHandoff]: Higher availability. Allows processing during handoff.
	AllowedStates []types.HandoffState `validate:"omitempty,min=1"`

	// WarmupDuration, when >0, enables a warm-up phase during which only
	// WarmupAllowedStates are permitted. After the duration elapses, AllowedStates
	// take effect for steady-state processing.
	WarmupDuration time.Duration `validate:"gte=0"`

	// WarmupAllowedStates defines which states are permitted during the warm-up phase.
	// If empty and WarmupDuration>0, defaults to Stable-only.
	WarmupAllowedStates []types.HandoffState

	// NakDelay is the base delay for NAK when the worker is not the owner or
	// the state is disallowed.
	// Default: 100ms.
	NakDelay time.Duration `default:"100ms" validate:"gte=0"`

	// NakJitter is a fractional jitter in [0.0, 1.0] applied to NakDelay.
	// Default: 0.0 (no jitter).
	NakJitter float64 `validate:"gte=0,lte=1"`

	// Debug enables verbose logging for NAK decisions (non-owner, disallowed state,
	// unknown ownership). Only effective when a logger is configured.
	Debug bool

	// Metrics optionally records processing gate metrics (NAK reasons, delays).
	// When nil, no gate metrics are emitted.
	Metrics GateMetrics
}

// applyDefaults sets default values for the configuration.
func (c *ProcessingGateConfig) applyDefaults() error {
	if err := fuda.SetDefaults(c); err != nil {
		return fmt.Errorf("failed to set defaults: %w", err)
	}

	if len(c.AllowedStates) == 0 {
		c.AllowedStates = []types.HandoffState{types.HandoffStateStable}
	}

	// Handle complex defaults that cannot be expressed as tags
	if c.WarmupDuration > 0 && len(c.WarmupAllowedStates) == 0 {
		c.WarmupAllowedStates = []types.HandoffState{types.HandoffStateStable}
	}

	return nil
}

// processingGate wraps a messageHandler and enforces exclusive processing based on ownership.
// Reads are O(1), relying on a resolver backed by a high-performance cache.
type processingGate struct {
	workerID   string
	resolver   types.OwnershipResolver
	cfg        ProcessingGateConfig
	logger     types.Logger
	subjPrefix string
	subjSuffix string
	// warm-up control (evaluated at runtime without locks)
	warmupUntil   time.Time
	allowedWarmup map[types.HandoffState]struct{}
	allowedSteady map[types.HandoffState]struct{}
}

// newProcessingGate constructs a processing gate.
// subjectTemplate must contain "{{.PartitionID}}"; we extract prefix/suffix once for fast parsing.
func newProcessingGate(
	workerID string,
	subjectTemplate string,
	resolver types.OwnershipResolver,
	cfg ProcessingGateConfig,
	logger types.Logger,
) (*processingGate, error) {
	prefix, suffix, ok := parseSubjectTemplateParts(subjectTemplate)
	if !ok {
		return nil, fmt.Errorf("invalid subject template: %q (must contain %s)", subjectTemplate, partitionPlaceholder)
	}

	// Apply defaults via method (copy since cfg passed by value)
	if err := cfg.applyDefaults(); err != nil {
		return nil, fmt.Errorf("apply defaults: %w", err)
	}

	// Build allowed-state maps for O(1) lookups
	allowedWarmup := make(map[types.HandoffState]struct{}, len(cfg.WarmupAllowedStates))
	for _, s := range cfg.WarmupAllowedStates {
		allowedWarmup[s] = struct{}{}
	}
	allowedSteady := make(map[types.HandoffState]struct{}, len(cfg.AllowedStates))
	for _, s := range cfg.AllowedStates {
		allowedSteady[s] = struct{}{}
	}

	g := &processingGate{
		workerID:      workerID,
		resolver:      resolver,
		cfg:           cfg,
		logger:        logger,
		subjPrefix:    prefix,
		subjSuffix:    suffix,
		allowedWarmup: allowedWarmup,
		allowedSteady: allowedSteady,
	}
	if cfg.WarmupDuration > 0 {
		g.warmupUntil = time.Now().Add(cfg.WarmupDuration)
	}

	return g, nil
}

// Wrap returns a messageHandler that applies gate policy before calling the base handler.
func (g *processingGate) Wrap(base messageHandler) messageHandler {
	if base == nil || g == nil || g.resolver == nil || !g.cfg.Enabled {
		return base
	}

	return messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		// Select allowed states based on warm-up status (no locking required)
		allowed := g.allowedSteady
		if !g.warmupUntil.IsZero() && time.Now().Before(g.warmupUntil) && len(g.allowedWarmup) > 0 {
			allowed = g.allowedWarmup
		}
		// Fast path: extract partitionID from subject via prefix/suffix slice ops.
		subj := msg.Subject()
		pid, ok := g.extractPartitionID(subj)
		if !ok {
			// Defensive: if parsing fails, allow processing rather than risk drop/loop.
			return base.Handle(ctx, msg)
		}

		owner, state, _, ok := g.resolver.GetOwner(pid)
		if ok {
			if owner == g.workerID {
				if _, ok := allowed[state]; ok {
					return base.Handle(ctx, msg)
				}
				// owner but disallowed state (e.g., prepare) -> NAK with delay
				return g.nakWithDelay(msg, pid, owner, state, "owner_disallowed_state")
			}

			// not owner -> NAK with delay
			return g.nakWithDelay(msg, pid, owner, state, "non_owner")
		}

		// Unknown ownership: attempt a fast single refresh to reduce timing gaps
		// during handoff commit propagation. If still unknown, NAK.
		_ = g.resolver.ForceRefreshPartition(ctx, pid) // best-effort; ignore error
		if owner2, state2, _, ok2 := g.resolver.GetOwner(pid); ok2 {
			if owner2 == g.workerID {
				if _, allowed2 := allowed[state2]; allowed2 {
					return base.Handle(ctx, msg)
				}
				return g.nakWithDelay(msg, pid, owner2, state2, "owner_disallowed_state")
			}

			return g.nakWithDelay(msg, pid, owner2, state2, "non_owner")
		}

		return g.nakWithDelay(msg, pid, "", types.HandoffStateUnknown, "unknown_ownership")
	})
}

func (g *processingGate) extractPartitionID(subject string) (string, bool) {
	if !strings.HasPrefix(subject, g.subjPrefix) || !strings.HasSuffix(subject, g.subjSuffix) {
		return "", false
	}

	start := len(g.subjPrefix)
	end := len(subject) - len(g.subjSuffix)
	if start > end || start < 0 || end > len(subject) {
		return "", false
	}

	return subject[start:end], true
}

func (g *processingGate) nakWithDelay(msg jetstream.Msg, pid string, owner string, state types.HandoffState, reason string) error {
	// Decorrelated jitter around NakDelay.
	d := g.cfg.NakDelay
	if g.cfg.NakJitter > 0 {
		// random factor in [1-jitter, 1+jitter]
		f := rand.Float64() //nolint:gosec // Jitter does not require cryptographic randomness
		low := 1 - g.cfg.NakJitter
		high := 1 + g.cfg.NakJitter
		d = time.Duration(float64(d) * (low + f*(high-low)))
	}
	if g.cfg.Debug && g.logger != nil {
		g.logger.Debug("processing gate NAK",
			"worker", g.workerID,
			"partition", pid,
			"owner", owner,
			"state", state,
			"delay", d.String(),
			"reason", reason,
		)
	}

	// Emit optional gate metrics
	if g.cfg.Metrics != nil {
		g.cfg.Metrics.IncGateNAK(reason)
		g.cfg.Metrics.ObserveGateNakDelay(d)
	}

	return msg.NakWithDelay(d)
}
