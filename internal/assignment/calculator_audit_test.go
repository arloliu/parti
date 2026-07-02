package assignment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// auditRecordingMetrics captures the audit-relevant metric calls so tests
// can assert exact counts and per-worker behavior. Embeds NopMetrics so the
// full MetricsCollector surface is satisfied without listing every method.
type auditRecordingMetrics struct {
	*metrics.NopMetrics

	mu sync.Mutex
	// counts is the last RecordAuditCounts triple.
	counts                 *[3]int
	behindCalls            []behindCall
	escalationSkippedCalls []escalationSkipped
}

type behindCall struct {
	workerID      string
	commitVersion int64
}

type escalationSkipped struct {
	reason   string
	workerID string
}

func newAuditRecordingMetrics() *auditRecordingMetrics {
	return &auditRecordingMetrics{NopMetrics: metrics.NewNop()}
}

func (m *auditRecordingMetrics) RecordAuditCounts(fullyApplied, behind, unverifiable int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts = &[3]int{fullyApplied, behind, unverifiable}
}

func (m *auditRecordingMetrics) RecordWorkerBehind(workerID string, commitVersion int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.behindCalls = append(m.behindCalls, behindCall{workerID, commitVersion})
}

func (m *auditRecordingMetrics) RecordAuditEscalationSkipped(reason, workerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.escalationSkippedCalls = append(m.escalationSkippedCalls, escalationSkipped{reason, workerID})
}

func (m *auditRecordingMetrics) getCounts() (fullyApplied, behind, unverifiable int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.counts == nil {
		return 0, 0, 0
	}
	return m.counts[0], m.counts[1], m.counts[2]
}

func (m *auditRecordingMetrics) getBehindCalls() []behindCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]behindCall, len(m.behindCalls))
	copy(out, m.behindCalls)
	return out
}

func (m *auditRecordingMetrics) getEscalationSkipped() []escalationSkipped {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]escalationSkipped, len(m.escalationSkippedCalls))
	copy(out, m.escalationSkippedCalls)
	return out
}

// auditTestFixture wires a Calculator with a fixed commit + heartbeat map for
// audit testing. It does NOT start the calculator — auditApplied is invoked
// directly so the test is deterministic.
type auditTestFixture struct {
	calc    *Calculator
	commit  *types.AssignmentCommit
	hbs     map[string]types.Heartbeat
	metrics *auditRecordingMetrics
}

// auditPayloadRef returns a synthetic ref with the given SetDigest. The Key
// and PayloadHash are unique-per-worker for log determinism.
func auditPayloadRef(workerID string, setDigest uint64) types.AssignmentPayloadRef {
	return types.AssignmentPayloadRef{
		Key:         "assignment._payload." + workerID,
		PayloadHash: workerID + "-hash",
		SetDigest:   setDigest,
		Revision:    1,
	}
}

// newAuditFixture builds a Calculator wired with mock heartbeats and a
// hand-crafted commit. The calculator's publisher.lastCommit is set
// directly via the publisher field.
func newAuditFixture(t *testing.T, commit *types.AssignmentCommit, hbs map[string]types.Heartbeat, enableTwoPhase bool) *auditTestFixture {
	t.Helper()
	m := newAuditRecordingMetrics()
	calc := &Calculator{
		Config: Config{
			AssignmentPrefix:         "assignment",
			HeartbeatPrefix:          "heartbeat",
			HeartbeatTTL:             time.Second,
			ApplyGracePeriod:         time.Microsecond,       // past gate 1 by default
			ExtendedApplyGracePeriod: 100 * time.Microsecond, // past gate 2 by default
			AuditInterval:            time.Second,
			EnableTwoPhaseHandoff:    enableTwoPhase,
			Metrics:                  m,
			Logger:                   logging.NewNop(),
		},
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	// publisher holds the commit; monitor must implement GetHeartbeats. For
	// audit tests we use a stub monitor that returns hbs directly.
	//
	// The audit's grace windows are now gated by lastCommitObservedAtMono
	// (PR-2 / ISSUE-007). Existing tests express their grace-elapsed /
	// grace-pending intent via commit.PublishedAt; mirror that onto the
	// monotonic observation field so the prior verdicts are preserved.
	publisher := &AssignmentPublisher{
		lastCommit: commit,
		logger:     logging.NewNop(),
	}
	if commit != nil {
		publisher.lastCommitObservedAtMono = commit.PublishedAt
	}
	calc.publisher = publisher
	calc.monitor = buildAuditMonitorFromKV(t, hbs)
	// State machine present so escalation paths can call EnterScaling without
	// nil deref. The mock rebalance callback is a no-op so the test does not
	// require any post-rebalance state.
	calc.stateMach = NewStateMachine(
		logging.NewNop(),
		m,
		func(_ context.Context, _ string) error { return nil },
		calc.stopCh,
	)

	return &auditTestFixture{calc: calc, commit: commit, hbs: hbs, metrics: m}
}

// buildAuditMonitorFromKV writes hbs into the given KV and constructs a
// WorkerMonitor that reads back via GetHeartbeats.
func buildAuditMonitorFromKV(t *testing.T, hbs map[string]types.Heartbeat) *WorkerMonitor {
	t.Helper()
	_, nc := partitest.StartEmbeddedNATS(t)
	hbKV := partitest.CreateJetStreamKV(t, nc, "audit-fixture-hb")
	ctx := context.Background()
	for w, hb := range hbs {
		if hb.SchemaVersion == 0 {
			// Encode as legacy timestamp string so GetHeartbeats decodes via
			// the legacy path.
			_, err := hbKV.Put(ctx, "heartbeat."+w, []byte(hb.Timestamp.UTC().Format(time.RFC3339Nano)))
			require.NoError(t, err)
			continue
		}
		// v1 JSON heartbeat.
		hb.WorkerID = w
		data, err := json.Marshal(hb)
		require.NoError(t, err)
		_, err = hbKV.Put(ctx, "heartbeat."+w, data)
		require.NoError(t, err)
	}
	mon := &WorkerMonitor{
		heartbeatKV: hbKV,
		hbPrefix:    "heartbeat",
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
		logger:      logging.NewNop(),
	}

	return mon
}

// ============================================================================
// Tests
// ============================================================================

func TestCalculatorAudit_FullyApplied_AllInSync(t *testing.T) {
	t.Parallel()
	digestA, digestB, digestC := uint64(0xAA), uint64(0xBB), uint64(0xCC)
	commit := &types.AssignmentCommit{
		Version:             10,
		LeaderRevision:      20,
		SourceRevision:      5,
		SourceRevisionKnown: true,
		PublishedAt:         time.Now().Add(-time.Hour),
		Workers:             []string{"worker-A", "worker-B", "worker-C"},
		Payloads: map[string]types.AssignmentPayloadRef{
			"worker-A": auditPayloadRef("worker-A", digestA),
			"worker-B": auditPayloadRef("worker-B", digestB),
			"worker-C": auditPayloadRef("worker-C", digestC),
		},
	}
	hbs := map[string]types.Heartbeat{
		"worker-A": fullCapsAck(20, 10, 5, true, digestA),
		"worker-B": fullCapsAck(20, 10, 5, true, digestB),
		"worker-C": fullCapsAck(20, 10, 5, true, digestC),
	}
	f := newAuditFixture(t, commit, hbs, true)

	f.calc.auditApplied(context.Background())

	fa, b, u := f.metrics.getCounts()
	require.Equal(t, 3, fa)
	require.Equal(t, 0, b)
	require.Equal(t, 0, u)
	require.Empty(t, f.metrics.getBehindCalls())
	require.Empty(t, f.metrics.getEscalationSkipped())
}

func TestCalculatorAudit_Behind_VersionMismatch(t *testing.T) {
	t.Parallel()
	digestA, digestB := uint64(0xAA), uint64(0xBB)
	commit := &types.AssignmentCommit{
		Version:             10,
		LeaderRevision:      20,
		SourceRevision:      5,
		SourceRevisionKnown: true,
		PublishedAt:         time.Now().Add(-time.Hour),
		Workers:             []string{"worker-A", "worker-B"},
		Payloads: map[string]types.AssignmentPayloadRef{
			"worker-A": auditPayloadRef("worker-A", digestA),
			"worker-B": auditPayloadRef("worker-B", digestB),
		},
	}
	hbs := map[string]types.Heartbeat{
		"worker-A": fullCapsAck(20, 9, 5, true, digestA), // version=9, behind
		"worker-B": fullCapsAck(20, 10, 5, true, digestB),
	}
	// Direct-mode so we never escalate; we only assert classification + metric.
	f := newAuditFixture(t, commit, hbs, false)

	f.calc.auditApplied(context.Background())

	fa, b, u := f.metrics.getCounts()
	require.Equal(t, 1, fa)
	require.Equal(t, 1, b)
	require.Equal(t, 0, u)
	behindCalls := f.metrics.getBehindCalls()
	require.Len(t, behindCalls, 1)
	require.Equal(t, "worker-A", behindCalls[0].workerID)
	require.Equal(t, int64(10), behindCalls[0].commitVersion)
}

func TestCalculatorAudit_Behind_DigestMismatch(t *testing.T) {
	t.Parallel()
	digestA, digestB := uint64(0xAA), uint64(0xBB)
	commit := &types.AssignmentCommit{
		Version:        10,
		LeaderRevision: 20,
		PublishedAt:    time.Now().Add(-time.Hour),
		Workers:        []string{"worker-A", "worker-B"},
		Payloads: map[string]types.AssignmentPayloadRef{
			"worker-A": auditPayloadRef("worker-A", digestA),
			"worker-B": auditPayloadRef("worker-B", digestB),
		},
	}
	hbs := map[string]types.Heartbeat{
		"worker-A": fullCapsAck(20, 10, 0, false, digestA),
		// worker-B reports the wrong digest.
		"worker-B": fullCapsAck(20, 10, 0, false, 0xFFFF),
	}
	f := newAuditFixture(t, commit, hbs, false)

	f.calc.auditApplied(context.Background())

	fa, b, u := f.metrics.getCounts()
	require.Equal(t, 1, fa)
	require.Equal(t, 1, b)
	require.Equal(t, 0, u)
	behindCalls := f.metrics.getBehindCalls()
	require.Len(t, behindCalls, 1)
	require.Equal(t, "worker-B", behindCalls[0].workerID)
}

func TestCalculatorAudit_Unverifiable_LegacyWorker(t *testing.T) {
	t.Parallel()
	digestA, digestC := uint64(0xAA), uint64(0xCC)
	commit := &types.AssignmentCommit{
		Version:        10,
		LeaderRevision: 20,
		PublishedAt:    time.Now().Add(-time.Hour),
		Workers:        []string{"worker-A", "worker-C"},
		Payloads: map[string]types.AssignmentPayloadRef{
			"worker-A": auditPayloadRef("worker-A", digestA),
			"worker-C": auditPayloadRef("worker-C", digestC),
		},
	}
	hbs := map[string]types.Heartbeat{
		"worker-A": fullCapsAck(20, 10, 0, false, digestA),
		// worker-C is a legacy worker — SchemaVersion=0, Capabilities=0.
		"worker-C": {SchemaVersion: 0, Capabilities: 0, Timestamp: time.Now()},
	}
	f := newAuditFixture(t, commit, hbs, true)

	f.calc.auditApplied(context.Background())

	fa, b, u := f.metrics.getCounts()
	require.Equal(t, 1, fa)
	require.Equal(t, 0, b)
	require.Equal(t, 1, u)
	// Legacy worker MUST NOT trigger escalation even though
	// ExtendedApplyGracePeriod has elapsed.
	for _, e := range f.metrics.getEscalationSkipped() {
		require.NotEqual(t, "cap_missing_behind", e.reason, "legacy worker is unverifiable, not behind")
	}
}

func TestCalculatorAudit_StrictSrcRevMatch_KnownCommitButUnknownWorker_Behind(t *testing.T) {
	t.Parallel()
	digestD := uint64(0xDD)
	commit := &types.AssignmentCommit{
		Version:             10,
		LeaderRevision:      20,
		SourceRevision:      5,
		SourceRevisionKnown: true,
		PublishedAt:         time.Now().Add(-time.Hour),
		Workers:             []string{"worker-D"},
		Payloads: map[string]types.AssignmentPayloadRef{
			"worker-D": auditPayloadRef("worker-D", digestD),
		},
	}
	hbs := map[string]types.Heartbeat{
		// AppliedSourceRevKnown=false despite commit declaring SrcRev known.
		"worker-D": fullCapsAck(20, 10, 0, false, digestD),
	}
	f := newAuditFixture(t, commit, hbs, false)

	f.calc.auditApplied(context.Background())

	fa, b, u := f.metrics.getCounts()
	require.Equal(t, 0, fa)
	require.Equal(t, 1, b, "strict source-revision check classifies as behind, not unverifiable")
	require.Equal(t, 0, u)
}

func TestCalculatorAudit_NonStrictWhenCommitUnknown(t *testing.T) {
	t.Parallel()
	digestE := uint64(0xEE)
	commit := &types.AssignmentCommit{
		Version:             10,
		LeaderRevision:      20,
		SourceRevisionKnown: false, // commit declares source revision unknown
		PublishedAt:         time.Now().Add(-time.Hour),
		Workers:             []string{"worker-E"},
		Payloads: map[string]types.AssignmentPayloadRef{
			"worker-E": auditPayloadRef("worker-E", digestE),
		},
	}
	hbs := map[string]types.Heartbeat{
		"worker-E": fullCapsAck(20, 10, 0, false, digestE),
	}
	f := newAuditFixture(t, commit, hbs, false)

	f.calc.auditApplied(context.Background())

	fa, b, u := f.metrics.getCounts()
	require.Equal(t, 1, fa, "audit skips source-rev check when commit declares unknown")
	require.Equal(t, 0, b)
	require.Equal(t, 0, u)
}

func TestCalculatorAudit_BehindWorkerMissingCaps_EscalationSkipped(t *testing.T) {
	t.Parallel()
	digestA := uint64(0xAA)
	commit := &types.AssignmentCommit{
		Version:        10,
		LeaderRevision: 20,
		PublishedAt:    time.Now().Add(-time.Hour),
		Workers:        []string{"worker-A", "worker-B"},
		Payloads: map[string]types.AssignmentPayloadRef{
			"worker-A": auditPayloadRef("worker-A", digestA),
			"worker-B": auditPayloadRef("worker-B", uint64(0xBB)),
		},
	}
	hbs := map[string]types.Heartbeat{
		// worker-A is behind but only has CapAckV1 — no two-phase, no gate.
		"worker-A": {
			SchemaVersion:  1,
			Capabilities:   types.CapAckV1,
			LeaderRevision: 20,
			AppliedVersion: 9, // behind
			AppliedDigest:  digestA,
			Timestamp:      time.Now(),
		},
		"worker-B": fullCapsAck(20, 10, 0, false, uint64(0xBB)),
	}
	f := newAuditFixture(t, commit, hbs, true)

	f.calc.auditApplied(context.Background())

	skipped := f.metrics.getEscalationSkipped()
	hasCapMissingBehind := false
	for _, s := range skipped {
		if s.reason == "cap_missing_behind" && s.workerID == "worker-A" {
			hasCapMissingBehind = true
		}
		require.NotEqual(t, "direct_mode", s.reason)
	}
	require.True(t, hasCapMissingBehind, "behind worker missing caps must emit cap_missing_behind")
}

func TestCalculatorAudit_TargetsMissingCaps_EscalationSkipped(t *testing.T) {
	t.Parallel()
	digestA, digestB := uint64(0xAA), uint64(0xBB)
	commit := &types.AssignmentCommit{
		Version:        10,
		LeaderRevision: 20,
		PublishedAt:    time.Now().Add(-time.Hour),
		Workers:        []string{"worker-A", "worker-B"},
		Payloads: map[string]types.AssignmentPayloadRef{
			"worker-A": auditPayloadRef("worker-A", digestA),
			"worker-B": auditPayloadRef("worker-B", digestB),
		},
	}
	hbs := map[string]types.Heartbeat{
		// Behind with full caps.
		"worker-A": {
			SchemaVersion:  1,
			Capabilities:   requiredAuditCaps,
			LeaderRevision: 20,
			AppliedVersion: 9,
			AppliedDigest:  digestA,
			Timestamp:      time.Now(),
		},
		// Fully applied but missing CapProcessingGate.
		"worker-B": {
			SchemaVersion:  1,
			Capabilities:   types.CapAckV1 | types.CapTwoPhaseHandoff,
			LeaderRevision: 20,
			AppliedVersion: 10,
			AppliedDigest:  digestB,
			Timestamp:      time.Now(),
		},
	}
	f := newAuditFixture(t, commit, hbs, true)

	f.calc.auditApplied(context.Background())

	skipped := f.metrics.getEscalationSkipped()
	hasTargets := 0
	for _, s := range skipped {
		if s.reason == "cap_missing_targets" {
			hasTargets++
			require.Equal(t, "", s.workerID)
		}
	}
	require.Equal(t, 1, hasTargets, "cap_missing_targets must be emitted exactly once when no targets carry full caps")
}

func TestCalculatorAudit_DirectMode_SkipsEscalation(t *testing.T) {
	t.Parallel()
	digestA, digestB := uint64(0xAA), uint64(0xBB)
	commit := &types.AssignmentCommit{
		Version:        10,
		LeaderRevision: 20,
		PublishedAt:    time.Now().Add(-time.Hour),
		Workers:        []string{"worker-A", "worker-B"},
		Payloads: map[string]types.AssignmentPayloadRef{
			"worker-A": auditPayloadRef("worker-A", digestA),
			"worker-B": auditPayloadRef("worker-B", digestB),
		},
	}
	hbs := map[string]types.Heartbeat{
		"worker-A": {
			SchemaVersion:  1,
			Capabilities:   requiredAuditCaps,
			LeaderRevision: 20,
			AppliedVersion: 9, // behind
			AppliedDigest:  digestA,
			Timestamp:      time.Now(),
		},
		"worker-B": fullCapsAck(20, 10, 0, false, digestB),
	}
	// EnableTwoPhaseHandoff=false.
	f := newAuditFixture(t, commit, hbs, false)

	f.calc.auditApplied(context.Background())

	skipped := f.metrics.getEscalationSkipped()
	hasDirect := 0
	for _, s := range skipped {
		if s.reason == "direct_mode" {
			hasDirect++
		}
	}
	require.Equal(t, 1, hasDirect, "direct_mode must be emitted once when EnableTwoPhaseHandoff=false")
}

// TestCalculatorAudit_DirectMode_WarnsBehindWorker proves the direct-mode skip
// also emits a per-worker Warn so a behind worker that cannot be auto-repaired
// is visible in logs, not only in the worker-behind metric.
func TestCalculatorAudit_DirectMode_WarnsBehindWorker(t *testing.T) {
	t.Parallel()
	digestA, digestB := uint64(0xAA), uint64(0xBB)
	commit := &types.AssignmentCommit{
		Version:        10,
		LeaderRevision: 20,
		PublishedAt:    time.Now().Add(-time.Hour),
		Workers:        []string{"worker-A", "worker-B"},
		Payloads: map[string]types.AssignmentPayloadRef{
			"worker-A": auditPayloadRef("worker-A", digestA),
			"worker-B": auditPayloadRef("worker-B", digestB),
		},
	}
	hbs := map[string]types.Heartbeat{
		"worker-A": {
			SchemaVersion:  1,
			Capabilities:   requiredAuditCaps,
			LeaderRevision: 19, // behind on leader revision
			AppliedVersion: 9,  // behind on version
			AppliedDigest:  digestA,
			Timestamp:      time.Now(),
		},
		"worker-B": fullCapsAck(20, 10, 0, false, digestB),
	}
	f := newAuditFixture(t, commit, hbs, false)

	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	f.calc.Logger = logging.NewSlog(slog.New(handler))

	f.calc.auditApplied(context.Background())

	out := buf.String()
	require.Contains(t, out, "audit: worker behind in direct mode")
	require.Contains(t, out, `"worker":"worker-A"`)
	require.Contains(t, out, `"expected_version":10`)
	require.Contains(t, out, `"expected_leader_revision":20`)
	require.Contains(t, out, `"acked_version":9`)
	require.Contains(t, out, `"acked_leader_revision":19`)
	// worker-B is fully applied; it must NOT appear in a behind Warn.
	require.NotContains(t, out, `"worker":"worker-B"`)
}

// TestCalculatorAudit_DirectMode_WarnsBehindWorker_NoCaps proves the Warn fires
// for a behind worker that carries NO capabilities — a homogeneous direct-mode
// fleet that never sets CapTwoPhaseHandoff or CapProcessingGate. This is the
// target audience: such workers are filtered out of behindReassignable by the
// requiredAuditCaps gate and were therefore unreachable by the Warn before the
// fix moved it before the filters.
func TestCalculatorAudit_DirectMode_WarnsBehindWorker_NoCaps(t *testing.T) {
	t.Parallel()
	digestA, digestB := uint64(0xAA), uint64(0xBB)
	commit := &types.AssignmentCommit{
		Version:        10,
		LeaderRevision: 20,
		PublishedAt:    time.Now().Add(-time.Hour),
		Workers:        []string{"worker-A", "worker-B"},
		Payloads: map[string]types.AssignmentPayloadRef{
			"worker-A": auditPayloadRef("worker-A", digestA),
			"worker-B": auditPayloadRef("worker-B", digestB),
		},
	}
	hbs := map[string]types.Heartbeat{
		// worker-A is behind and carries only CapAckV1 — a direct-mode worker
		// that never set CapTwoPhaseHandoff or CapProcessingGate. It is
		// classified as "behind" (CapAckV1 is set, SchemaVersion>0) but is
		// filtered out of behindReassignable by the requiredAuditCaps gate
		// before the Warn could fire in the pre-fix code path.
		"worker-A": {
			SchemaVersion:  1,
			Capabilities:   types.CapAckV1,
			LeaderRevision: 19,
			AppliedVersion: 9,
			AppliedDigest:  digestA,
			Timestamp:      time.Now(),
		},
		// worker-B is fully applied (also only CapAckV1 — pure direct-mode fleet).
		"worker-B": {
			SchemaVersion:  1,
			Capabilities:   types.CapAckV1,
			LeaderRevision: 20,
			AppliedVersion: 10,
			AppliedDigest:  digestB,
			Timestamp:      time.Now(),
		},
	}
	// EnableTwoPhaseHandoff=false — direct-mode calculator.
	f := newAuditFixture(t, commit, hbs, false)

	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	f.calc.Logger = logging.NewSlog(slog.New(handler))

	f.calc.auditApplied(context.Background())

	out := buf.String()
	// The Warn must fire for worker-A even though it carries no caps.
	require.Contains(t, out, "audit: worker behind in direct mode",
		"Warn must fire for a no-caps behind worker in a direct-mode fleet")
	require.Contains(t, out, `"worker":"worker-A"`)
	require.Contains(t, out, `"expected_version":10`)
	require.Contains(t, out, `"expected_leader_revision":20`)
	require.Contains(t, out, `"acked_version":9`)
	require.Contains(t, out, `"acked_leader_revision":19`)
	// worker-B is fully applied; it must NOT appear in a behind Warn.
	require.NotContains(t, out, `"worker":"worker-B"`)
}

func TestCalculatorAudit_BeforeGrace_NoMetricsForBehind(t *testing.T) {
	t.Parallel()
	digestA := uint64(0xAA)
	commit := &types.AssignmentCommit{
		Version:        10,
		LeaderRevision: 20,
		PublishedAt:    time.Now(), // within ApplyGracePeriod
		Workers:        []string{"worker-A"},
		Payloads: map[string]types.AssignmentPayloadRef{
			"worker-A": auditPayloadRef("worker-A", digestA),
		},
	}
	hbs := map[string]types.Heartbeat{
		"worker-A": fullCapsAck(20, 9, 0, false, digestA), // behind
	}
	f := newAuditFixture(t, commit, hbs, false)
	// Force a generous grace period so PublishedAt=now is within it.
	f.calc.ApplyGracePeriod = time.Hour
	f.calc.ExtendedApplyGracePeriod = time.Hour

	f.calc.auditApplied(context.Background())

	fa, b, u := f.metrics.getCounts()
	require.Equal(t, 0, fa)
	require.Equal(t, 1, b, "RecordAuditCounts always fires")
	require.Equal(t, 0, u)
	require.Empty(t, f.metrics.getBehindCalls(), "RecordWorkerBehind must NOT fire before ApplyGracePeriod elapses")
}

func TestCalculatorAudit_PreCommitBootstrap_NoOp(t *testing.T) {
	t.Parallel()
	// commit=nil — bootstrap path before any commit has landed.
	f := newAuditFixture(t, nil, map[string]types.Heartbeat{}, true)

	f.calc.auditApplied(context.Background())

	require.Nil(t, f.metrics.counts, "no metrics when publisher.LastCommit is nil")
}

// TestCalculatorAudit_GraceFromLocalObservedAt verifies ISSUE-007 (PR-2):
// the audit's grace windows depend on THIS leader's monotonic observation
// time, not on the wire-clock commit.PublishedAt. A stale PublishedAt
// from a former leader (or a wall-clock-skewed peer) must NOT bypass
// the grace windows when the current leader has only just observed the
// commit.
//
// Pre-fix: time.Since(commit.PublishedAt) ≈ 1h on a 1h ApplyGracePeriod,
// so the audit emits behind/escalation metrics for what looks like an
// elapsed grace. Post-fix: lastCommitObservedAtMono = time.Now(), so
// the grace is fresh and no metrics fire.
func TestCalculatorAudit_GraceFromLocalObservedAt(t *testing.T) {
	t.Parallel()
	digestA := uint64(0xAA)
	commit := &types.AssignmentCommit{
		Version:        10,
		LeaderRevision: 20,
		// Wire timestamp is an hour old (simulates a former leader's clock
		// being ahead of ours OR a stale persisted commit).
		PublishedAt: time.Now().Add(-time.Hour),
		Workers:     []string{"worker-A"},
		Payloads: map[string]types.AssignmentPayloadRef{
			"worker-A": auditPayloadRef("worker-A", digestA),
		},
	}
	hbs := map[string]types.Heartbeat{
		"worker-A": fullCapsAck(20, 9, 0, false, digestA), // behind by version
	}
	f := newAuditFixture(t, commit, hbs, true)

	// Grace windows large enough that pre-fix code (which uses PublishedAt)
	// would see them as elapsed only because PublishedAt is an hour ago;
	// post-fix, we override the observation instant to "just now" so the
	// monotonic-clock gate keeps both grace windows live.
	f.calc.ApplyGracePeriod = time.Hour
	f.calc.ExtendedApplyGracePeriod = time.Hour
	f.calc.publisher.lastCommitObservedAtMono = time.Now()

	f.calc.auditApplied(context.Background())

	require.Empty(t, f.metrics.getBehindCalls(),
		"RecordWorkerBehind must NOT fire while local-observed ApplyGracePeriod has not elapsed")
	require.Empty(t, f.metrics.getEscalationSkipped(),
		"escalation-skip metrics must NOT fire while local-observed ExtendedApplyGracePeriod has not elapsed")
}

// ============================================================================
// helpers
// ============================================================================

// fullCapsAck builds an ack-capable Heartbeat with the full safety
// capability chain. All current test cases pass leaderRev=20 (the fixture's
// commit.LeaderRevision); the parameter is retained so future tests can
// vary it without touching the helper.
//
//nolint:unparam // leaderRev parameterized for future fixtures.
func fullCapsAck(leaderRev uint64, appliedVer int64, srcRev uint64, srcKnown bool, digest uint64) types.Heartbeat {
	return types.Heartbeat{
		SchemaVersion:         1,
		Capabilities:          requiredAuditCaps,
		LeaderRevision:        leaderRev,
		AppliedVersion:        appliedVer,
		AppliedDigest:         digest,
		AppliedSourceRevision: srcRev,
		AppliedSourceRevKnown: srcKnown,
		AppliedAt:             time.Now(),
		Timestamp:             time.Now(),
	}
}

// --- merged from calculator_enumeration_stall_test.go ---

// keysFaultKV wraps a real jetstream.KeyValue and, when armed, makes Keys()
// return the configured err while every other operation — notably the
// single-key Put/Get a worker uses for its own heartbeat — passes through to
// the real bucket unchanged. Configuring err as context.DeadlineExceeded
// models NP-10's defining asymmetry: the stream-wide heartbeat-enumeration
// scan times out, single-key ops do not. Configuring err as a connectivity
// error (e.g. nats.ErrConnectionClosed) models the §5.4 connectivity row.
//
// The wrapper returns err directly rather than blocking on ctx so the
// calculator-level proof is deterministic and timing-free; the deadline
// variant's error value is identical to what a real Keys scan returns when
// WorkerMonitor's bounded op-context (hbTTL/2) expires mid-scan.
type keysFaultKV struct {
	jetstream.KeyValue
	armed atomic.Bool
	err   error
}

func (k *keysFaultKV) Keys(ctx context.Context, opts ...jetstream.WatchOpt) ([]string, error) {
	if k.armed.Load() {
		return nil, k.err
	}

	return k.KeyValue.Keys(ctx, opts...)
}

// TestNP10_EnumerationDeadline_EvadesClassifiers is the empirical linchpin of
// NP-10. It confirms — by running the real classifiers, not by reading them —
// that a heartbeat Keys-scan context.DeadlineExceeded is classified as NEITHER
// a connectivity error NOR a degrading-JetStream error, both bare AND wrapped
// (worker_monitor wraps it as "failed to list heartbeat keys: %w"). That dual
// miss is precisely why getActiveWorkers returns the deadline raw and no caller
// routes it to the manager's degraded circuit.
//
// PASSES on the parent: it characterizes the current classification gap.
func TestNP10_EnumerationDeadline_EvadesClassifiers(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
	}{
		{"bare", context.DeadlineExceeded},
		{"wrapped", fmt.Errorf("failed to list heartbeat keys: %w", context.DeadlineExceeded)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.False(t, natsutil.IsConnectivityError(tc.err),
				"%s context.DeadlineExceeded must NOT be a connectivity error; if it were, getActiveWorkers would take the cache-fallback/ErrDegraded branch and NP-10 would not exist", tc.name)
			require.False(t, natsutil.IsDegradingJetStreamError(tc.err),
				"%s context.DeadlineExceeded must NOT be a degrading-JetStream error", tc.name)
		})
	}

	// The fix will need to classify the WRAPPED deadline, so confirm the
	// errors.Is chain that a fix would key on is intact today.
	require.ErrorIs(t, cases[1].err, context.DeadlineExceeded)
}

// TestNP10_GetActiveWorkers_EnumerationDeadline_IsSwallowed proves the NP-10
// gap at the calculator level: a sustained heartbeat Keys-scan deadline is
// surfaced RAW by getActiveWorkers — it does NOT take the connectivity
// cache-fallback branch and is NOT wrapped in types.ErrDegraded, so nothing in
// the assignment layer routes it to a degrade signal. The worker's own
// single-key heartbeat Put is untouched (the wrapper faults Keys only), modeling
// "own heartbeat healthy, enumeration blind".
//
// PASSES on the parent: it characterizes the swallow. It does NOT — and cannot —
// assert the manager stays falsely StateStable; that is a different state
// machine and is proven end-to-end in the integration proof
// (test/integration/failure/np10_enumeration_stall_test.go).
func TestNP10_GetActiveWorkers_EnumerationDeadline_IsSwallowed(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "np10-calc-assign")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "np10-calc-hb")

	// Seed a live worker heartbeat so a non-faulted scan succeeds and sees it.
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	fault := &keysFaultKV{KeyValue: heartbeatKV, err: context.DeadlineExceeded}
	calc, err := NewCalculator(&Config{
		AssignmentKV:     assignmentKV,
		HeartbeatKV:      fault,
		AssignmentPrefix: "assignment",
		Source:           &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}}},
		Strategy:         &mockStrategy{},
		HeartbeatPrefix:  "worker-hb",
		HeartbeatTTL:     6 * time.Second,
	})
	require.NoError(t, err)

	// Baseline (fault disarmed): enumeration succeeds and observes the worker.
	workers, fresh, err := calc.getActiveWorkers(ctx)
	require.NoError(t, err)
	require.True(t, fresh)
	require.Contains(t, workers, "worker-1")

	// Arm the Keys-only stall. The scan now times out; single-key Puts (the
	// worker's own heartbeat) would still succeed against the real bucket.
	fault.armed.Store(true)

	workers, fresh, err = calc.getActiveWorkers(ctx)

	// THE GAP: the deadline surfaces raw. Not a connectivity error (so no cache
	// fallback and no types.ErrDegraded wrap), not classified as degrading —
	// nothing routes it to the degraded circuit.
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotErrorIs(t, err, types.ErrDegraded,
		"parent gap: a sustained enumeration deadline is swallowed (returned raw), NOT surfaced as ErrDegraded")
	require.False(t, fresh)
	require.Nil(t, workers)
}

// TestNP10_GetActiveWorkers_SustainedEnumerationDeadline_FiresCallback drives the
// fix: a SINGLE enumeration deadline must NOT fire OnEnumerationError (debounce —
// a transient blip is not a sustained stall), EnumerationFailureThreshold
// consecutive deadlines MUST fire it, and a subsequent healthy scan must fire
// OnEnumerationSuccess and reset the counter.
func TestNP10_GetActiveWorkers_SustainedEnumerationDeadline_FiresCallback(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "np10-fire-assign")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "np10-fire-hb")

	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	fault := &keysFaultKV{KeyValue: heartbeatKV, err: context.DeadlineExceeded}
	var enumErr, enumOK atomic.Int64
	calc, err := NewCalculator(&Config{
		AssignmentKV:                assignmentKV,
		HeartbeatKV:                 fault,
		AssignmentPrefix:            "assignment",
		Source:                      &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}}},
		Strategy:                    &mockStrategy{},
		HeartbeatPrefix:             "worker-hb",
		HeartbeatTTL:                6 * time.Second,
		EnumerationFailureThreshold: 3,
		OnEnumerationError:          func(error) { enumErr.Add(1) },
		OnEnumerationSuccess:        func() { enumOK.Add(1) },
	})
	require.NoError(t, err)

	// NEGATIVE SPACE: a single failure must NOT fire (below the threshold). This
	// distinguishes a correct consecutive-counter from a broken one that fires on
	// the first failure.
	fault.armed.Store(true)
	_, _, err = calc.getActiveWorkers(ctx)
	require.Error(t, err)
	require.Zero(t, enumErr.Load(), "one enumeration failure must not fire OnEnumerationError (below threshold)")

	// Sustained: two more consecutive failures cross the threshold of 3.
	for range 2 {
		_, _, _ = calc.getActiveWorkers(ctx)
	}
	require.GreaterOrEqual(t, enumErr.Load(), int64(1),
		"a sustained enumeration deadline must fire OnEnumerationError once the threshold is crossed")

	// Recovery: a healthy scan resets the counter and signals success.
	fault.armed.Store(false)
	_, _, err = calc.getActiveWorkers(ctx)
	require.NoError(t, err)
	require.Positive(t, enumOK.Load(), "a healthy enumeration must signal recovery via OnEnumerationSuccess")
}

// TestGetActiveWorkers_ConnectivityErrorBypassesStallCounter pins the
// §5.4 connectivity row: a connectivity-classified enumeration failure
// takes the cache-fallback path and must NOT feed the enumeration-stall
// counter (that class stays owned by the heartbeat-Put circuit).
func TestGetActiveWorkers_ConnectivityErrorBypassesStallCounter(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "connfb-assign")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "connfb-hb")

	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	fault := &keysFaultKV{KeyValue: heartbeatKV, err: nats.ErrConnectionClosed}
	var enumErr atomic.Int64
	calc, err := NewCalculator(&Config{
		AssignmentKV:                assignmentKV,
		HeartbeatKV:                 fault,
		AssignmentPrefix:            "assignment",
		Source:                      &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}}},
		Strategy:                    &mockStrategy{},
		HeartbeatPrefix:             "worker-hb",
		HeartbeatTTL:                6 * time.Second,
		EnumerationFailureThreshold: 1, // even threshold=1 must not fire on connectivity errors
		OnEnumerationError:          func(error) { enumErr.Add(1) },
	})
	require.NoError(t, err)

	fault.armed.Store(true)
	for range 3 {
		_, _, _ = calc.getActiveWorkers(ctx)
	}
	require.Zero(t, enumErr.Load(),
		"connectivity-classified enumeration failures must bypass the stall counter (cache-fallback path)")
}
