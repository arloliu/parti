package provision_test

// Embedded-NATS integration tests for the planStreams policy orchestration.
// These tests exercise the policy gate (warn / safe-update / adopt) and the
// stream create / update / stamp-marker code paths with a real JetStream server.
// Helper functions (newJS, createStream, safeUpdateCtx) are shared across the
// integration test files in this package.

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/provision"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// streamOnlyCfg returns a Config that declares a single application stream with
// the given policy and no control-plane, partition-source, or other sections.
func streamOnlyCfg(policy provision.ReconcilePolicy, streams ...provision.StreamCfg) provision.Config {
	return provision.Config{
		APIVersion: provision.APIVersionV1,
		Instance:   "prod",
		Policy:     policy,
		Streams:    streams,
	}
}

// defaultStreamCfg returns a simple StreamCfg suitable for most tests.
func defaultStreamCfg() provision.StreamCfg {
	return provision.StreamCfg{
		Name:     "orders",
		Subjects: []string{"orders.>"},
		Storage:  "memory",
	}
}

// markedStream creates an application stream stamped with the Parti marker.
// The mutate variadic functions let a test introduce drift before calling Plan.
func markedStream(name string, mutate ...func(*jetstream.StreamConfig)) jetstream.StreamConfig {
	cfg := jetstream.StreamConfig{
		Name:     name,
		Subjects: []string{name + ".>"},
		Storage:  jetstream.MemoryStorage,
		Metadata: provision.BuildMarker(provision.ComponentStream, "prod"),
	}
	for _, m := range mutate {
		m(&cfg)
	}

	return cfg
}

// actionsOfKind collects all actions with the given Kind from a plan result.
func actionsOfKind(plan provision.PlanResult, kind string) []provision.PlannedAction {
	var out []provision.PlannedAction
	for _, a := range plan.Actions {
		if a.Kind == kind {
			out = append(out, a)
		}
	}

	return out
}

// appStreamDrift returns all drift findings for application-stream resources.
func appStreamDrift(plan provision.PlanResult) []provision.DriftFinding {
	var out []provision.DriftFinding
	for _, d := range plan.Drift {
		if d.Kind == provision.KindApplicationStream {
			out = append(out, d)
		}
	}

	return out
}

// planCtx returns a context bounded for a single Plan call.
func planCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	return ctx
}

// --- Missing-stream cases ---

// TestPlanStreams_MissingStream_Warn_EmitsCreateStream checks that when the
// policy is warn and the stream does not exist, Plan emits a create-stream action.
func TestPlanStreams_MissingStream_Warn_EmitsCreateStream(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := streamOnlyCfg(provision.PolicyWarn, defaultStreamCfg())

	plan, err := provision.Plan(planCtx(t), js, cfg)
	require.NoError(t, err)

	creates := actionsOfKind(plan, provision.ActionCreateStream)
	require.Len(t, creates, 1)
	require.Equal(t, "orders", creates[0].Name)

	// No drift findings: a missing stream under warn doesn't generate drift.
	require.Empty(t, appStreamDrift(plan))
}

// TestPlanStreams_MissingStream_SafeUpdate_EmitsCreateStream checks that
// safe-update also emits create-stream for a missing stream.
func TestPlanStreams_MissingStream_SafeUpdate_EmitsCreateStream(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := streamOnlyCfg(provision.PolicySafeUpdate, defaultStreamCfg())

	plan, err := provision.Plan(planCtx(t), js, cfg)
	require.NoError(t, err)

	creates := actionsOfKind(plan, provision.ActionCreateStream)
	require.Len(t, creates, 1)
	require.Equal(t, "orders", creates[0].Name)

	// The create-stream resource must be a jetstream.StreamConfig.
	_, ok := creates[0].Resource.(jetstream.StreamConfig)
	require.True(t, ok, "create-stream resource must be jetstream.StreamConfig")
}

// TestPlanStreams_MissingStream_Adopt_EmitsInformationalNoDrift checks that
// under adopt, a missing stream generates an informational drift finding and
// no create-stream action.
func TestPlanStreams_MissingStream_Adopt_EmitsInformationalNoDrift(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := streamOnlyCfg(provision.PolicyAdopt, defaultStreamCfg())

	plan, err := provision.Plan(planCtx(t), js, cfg)
	require.NoError(t, err)

	// No create-stream action under adopt.
	require.Empty(t, actionsOfKind(plan, provision.ActionCreateStream))

	// One informational finding for the missing stream.
	findings := appStreamDrift(plan)
	require.Len(t, findings, 1)
	require.Equal(t, provision.SeverityInformational, findings[0].Severity)
	require.Equal(t, "orders", findings[0].Name)
}

// --- Unmarked existing stream (adopt policy) ---

// TestPlanStreams_UnmarkedStream_Adopt_EmitsStampMarker checks that under
// adopt, an existing unmarked stream generates an adopted drift finding and a
// stamp-stream-marker action; no create-stream action is emitted.
func TestPlanStreams_UnmarkedStream_Adopt_EmitsStampMarker(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	// Create an unmarked stream (no Parti metadata).
	createStream(t, js, jetstream.StreamConfig{
		Name:     "orders",
		Subjects: []string{"orders.>"},
		Storage:  jetstream.MemoryStorage,
	})

	cfg := streamOnlyCfg(provision.PolicyAdopt, defaultStreamCfg())

	plan, err := provision.Plan(planCtx(t), js, cfg)
	require.NoError(t, err)

	// No create-stream; stamp-stream-marker must be emitted.
	require.Empty(t, actionsOfKind(plan, provision.ActionCreateStream))

	stamps := actionsOfKind(plan, provision.ActionStampStreamMarker)
	require.Len(t, stamps, 1)
	require.Equal(t, "orders", stamps[0].Name)

	// Drift must carry an adopted finding.
	findings := appStreamDrift(plan)
	var adopted []provision.DriftFinding
	for _, d := range findings {
		if d.Severity == provision.SeverityAdopted {
			adopted = append(adopted, d)
		}
	}
	require.Len(t, adopted, 1)
	require.Equal(t, "orders", adopted[0].Name)
}

// --- Marked stream with mutable drift (safe-update policy) ---

// TestPlanStreams_MarkedStream_MutableDrift_SafeUpdate_EmitsUpdateStream
// verifies that a Parti-marked stream with mutable drift (MaxAge change)
// under safe-update produces an update-stream action plus a drift-mutable
// finding, and that the Before/After resources are populated correctly.
func TestPlanStreams_MarkedStream_MutableDrift_SafeUpdate_EmitsUpdateStream(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	// Live stream: MaxAge = 1 hour (drifted from config's 2 hours).
	createStream(t, js, markedStream("orders", func(cfg *jetstream.StreamConfig) {
		cfg.Subjects = []string{"orders.>"}
		cfg.MaxAge = 1 * time.Hour
	}))

	// Config desires MaxAge = 2 hours.
	streamCfg := provision.StreamCfg{
		Name:     "orders",
		Subjects: []string{"orders.>"},
		Storage:  "memory",
		MaxAge:   2 * time.Hour,
	}
	cfg := streamOnlyCfg(provision.PolicySafeUpdate, streamCfg)

	plan, err := provision.Plan(planCtx(t), js, cfg)
	require.NoError(t, err)

	// update-stream action must be emitted.
	updates := actionsOfKind(plan, provision.ActionUpdateStream)
	require.Len(t, updates, 1)
	require.Equal(t, "orders", updates[0].Name)

	res, ok := updates[0].Resource.(*provision.UpdateStreamResource)
	require.True(t, ok, "update-stream resource must be *UpdateStreamResource")
	require.Equal(t, 1*time.Hour, res.Before.MaxAge)
	require.Equal(t, 2*time.Hour, res.After.MaxAge)

	// drift-mutable finding must also appear.
	findings := appStreamDrift(plan)
	var mutable []provision.DriftFinding
	for _, d := range findings {
		if d.Severity == provision.SeverityDriftMutable {
			mutable = append(mutable, d)
		}
	}
	require.Len(t, mutable, 1)
	require.Contains(t, mutable[0].Detail, "maxAge")
}

// --- Marked stream with immutable drift (safe-update policy) ---

// TestPlanStreams_MarkedStream_ImmutableDrift_SafeUpdate_NoUpdateStream
// proves that buildStreamUpdateTarget inherits Storage from live: when the
// only divergence is the Storage field (immutable), the update target equals
// live config and no update-stream action is emitted. The drift-immutable
// finding is still reported.
func TestPlanStreams_MarkedStream_ImmutableDrift_SafeUpdate_NoUpdateStream(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	// Live stream: MemoryStorage (marked).
	createStream(t, js, markedStream("orders", func(cfg *jetstream.StreamConfig) {
		cfg.Subjects = []string{"orders.>"}
	}))

	// Config desires FileStorage — diverges on an immutable field only.
	streamCfg := provision.StreamCfg{
		Name:     "orders",
		Subjects: []string{"orders.>"},
		Storage:  "file", // differs from live MemoryStorage — immutable
	}
	cfg := streamOnlyCfg(provision.PolicySafeUpdate, streamCfg)

	plan, err := provision.Plan(planCtx(t), js, cfg)
	require.NoError(t, err)

	// No update-stream: buildStreamUpdateTarget inherits Storage from live, so
	// the target is identical to live and no write is needed.
	require.Empty(t, actionsOfKind(plan, provision.ActionUpdateStream),
		"update-stream must NOT be emitted for immutable-only drift")

	// drift-immutable finding must appear.
	findings := appStreamDrift(plan)
	var immutable []provision.DriftFinding
	for _, d := range findings {
		if d.Severity == provision.SeverityDriftImmutable {
			immutable = append(immutable, d)
		}
	}
	require.Len(t, immutable, 1)
	require.Contains(t, immutable[0].Detail, "storage")
}

// --- Converged marked stream ---

// TestPlanStreams_MarkedStream_Converged_InformationalNoAction checks that a
// marked stream with no drift produces an informational finding and no actions.
func TestPlanStreams_MarkedStream_Converged_InformationalNoAction(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	createStream(t, js, markedStream("orders", func(cfg *jetstream.StreamConfig) {
		cfg.Subjects = []string{"orders.>"}
	}))

	streamCfg := provision.StreamCfg{
		Name:     "orders",
		Subjects: []string{"orders.>"},
		Storage:  "memory",
	}
	cfg := streamOnlyCfg(provision.PolicySafeUpdate, streamCfg)

	plan, err := provision.Plan(planCtx(t), js, cfg)
	require.NoError(t, err)

	require.Empty(t, actionsOfKind(plan, provision.ActionCreateStream))
	require.Empty(t, actionsOfKind(plan, provision.ActionUpdateStream))

	findings := appStreamDrift(plan)
	require.Len(t, findings, 1)
	require.Equal(t, provision.SeverityInformational, findings[0].Severity)
}
