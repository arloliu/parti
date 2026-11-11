package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/internal/dynamicbuild"
	"github.com/arloliu/parti/v2/partitest"
)

const (
	delStream   = testConsumerStream
	delPrefix   = testConsumerPrefix
	delTemplate = testConsumerTemplate
	// delSettle keeps the observation window short; the gates do not depend on its length.
	delSettle = "50ms"
)

// newDeleteRig starts an embedded NATS server with the test stream at the given retention.
func newDeleteRig(t *testing.T, retention jetstream.RetentionPolicy) (string, jetstream.JetStream, jetstream.Stream) {
	t.Helper()
	url, js := startTestNATSWithJS(t)
	stream, err := js.CreateStream(t.Context(), jetstream.StreamConfig{
		Name:      delStream,
		Subjects:  []string{"orders.>", "misc.*"},
		Storage:   jetstream.MemoryStorage,
		Retention: retention,
	})
	require.NoError(t, err)

	return url, js, stream
}

// addDurable creates a durable the way a Dynamic consumer names and configures it,
// and returns its name.
func addDurable(
	t *testing.T, stream jetstream.Stream, prefix, template, partition string, edit func(*jetstream.ConsumerConfig),
) (string, jetstream.Consumer) {
	t.Helper()
	pre, suf, ok := dynamicbuild.ParseSubjectTemplateParts(template)
	require.True(t, ok)
	subject := pre + partition + suf
	name := dynamicbuild.PerSubjectDurableName(prefix, subject, pre, suf)

	return name, addNamedDurable(t, stream, name, subject, edit)
}

// addNamedDurable creates a durable with an explicit name.
func addNamedDurable(
	t *testing.T, stream jetstream.Stream, name, subject string, edit func(*jetstream.ConsumerConfig),
) jetstream.Consumer {
	t.Helper()
	cfg := dynamicbuild.ConsumerConfig(name, subject, dynamicbuild.DefaultDynamicDefaults())
	if edit != nil {
		edit(&cfg)
	}
	cons, err := stream.CreateConsumer(t.Context(), cfg)
	require.NoError(t, err)

	return cons
}

func publishN(t *testing.T, js jetstream.JetStream, subject string, n int) {
	t.Helper()
	for range n {
		_, err := js.Publish(t.Context(), subject, []byte("m"))
		require.NoError(t, err)
	}
}

// fetchOne delivers one message from cons and acknowledges it when ack is set.
func fetchOne(t *testing.T, cons jetstream.Consumer, ack bool) {
	t.Helper()
	msg, err := cons.Next(jetstream.FetchMaxWait(5 * time.Second))
	require.NoError(t, err)
	if ack {
		require.NoError(t, msg.DoubleAck(t.Context()))
	}
}

func waitConsumerInfo(t *testing.T, cons jetstream.Consumer, what string, pred func(*jetstream.ConsumerInfo) bool) {
	t.Helper()
	require.Eventually(t, func() bool {
		info, err := cons.Info(t.Context())

		return err == nil && pred(info)
	}, 5*time.Second, 10*time.Millisecond, what)
}

func durableExists(t *testing.T, stream jetstream.Stream, name string) bool {
	t.Helper()
	_, err := stream.Consumer(t.Context(), name)
	if err == nil {
		return true
	}
	require.ErrorIs(t, err, jetstream.ErrConsumerNotFound)

	return false
}

// discoverArgs lists the candidate durables of the test Dynamic consumer.
func discoverArgs(url string, extra ...string) []string {
	return append([]string{
		"consumers", "delete", "-server", url, "-stream", delStream,
		"-prefix", delPrefix, "-subject-template", delTemplate, "-settle", delSettle,
	}, extra...)
}

// applyArgs deletes the explicitly named durables.
func applyArgs(url string, names []string, extra ...string) []string {
	args := make([]string, 0, 9+2*len(names)+len(extra))
	args = append(args, "consumers", "delete", "-server", url, "-stream", delStream, "-settle", delSettle, "-apply")
	for _, name := range names {
		args = append(args, "-name", name)
	}

	return append(args, extra...)
}

func TestConsumersDelete_DiscoveryListsCandidatesAndDeletesNothing(t *testing.T) {
	t.Parallel()
	url, _, stream := newDeleteRig(t, jetstream.WorkQueuePolicy)
	a, _ := addDurable(t, stream, delPrefix, delTemplate, "a", nil)
	b, _ := addDurable(t, stream, delPrefix, delTemplate, "b", nil)
	// Durables that only resemble the selection are not candidates.
	notCandidates := []string{
		// The Broadcast naming scheme under the same prefix.
		addNamedDurable(t, stream, delPrefix+"_broadcast_x", "misc.b", nil).CachedInfo().Name,
		// A subject that fits the template, but not the name parti derives for it.
		addNamedDurable(t, stream, delPrefix+"_z_0000000000000000", "orders.z", nil).CachedInfo().Name,
		// The derived name, but a subject outside the template.
		addNamedDurable(t, stream, dynamicbuild.PerSubjectDurableName(delPrefix, "misc.c", "orders.", ""), "misc.c", nil).
			CachedInfo().Name,
	}
	saved := filepath.Join(t.TempDir(), "durables.txt")

	stdout, stderr, code := runArgs(discoverArgs(url, "-save-names", saved)...)

	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
	require.Contains(t, stdout, a)
	require.Contains(t, stdout, b)
	for _, name := range notCandidates {
		require.NotContains(t, stdout, name)
	}
	require.Contains(t, stdout, "nothing deleted")
	require.True(t, durableExists(t, stream, a))
	require.True(t, durableExists(t, stream, b))

	// The saved list is what a later -apply acts on.
	data, err := os.ReadFile(saved)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{a, b}, strings.Fields(string(data)))

	stdout, stderr, code = runArgs("consumers", "delete", "-server", url, "-stream", delStream,
		"-settle", delSettle, "-names-file", saved, "-apply")
	require.Equal(t, ExitOK, code, "stdout: %s\nstderr: %s", stdout, stderr)
	require.False(t, durableExists(t, stream, a))
	require.False(t, durableExists(t, stream, b))
	for _, name := range notCandidates {
		require.True(t, durableExists(t, stream, name), "%s was deleted", name)
	}
}

// A durable's name does not say where the prefix ends and the partition id begins.
// A consumer with a longer prefix that reads deeper subjects gets the very names
// the selected consumer would use for partition ids with one more token,
// so discovery cannot tell them apart and must never delete on its own.
func TestConsumersDelete_DiscoveryCannotProveOwnership(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// template is the selected consumer's; the other consumer has foreignPrefix and foreignTemplate.
		template        string
		foreignPrefix   string
		foreignTemplate string
	}{
		{
			name: "nested_subject", template: delTemplate,
			foreignPrefix: delPrefix + "_eu", foreignTemplate: "orders.eu.{{.PartitionID}}",
		},
		{
			// A bare placeholder makes the whole subject the partition id.
			name: "bare_placeholder", template: "{{.PartitionID}}",
			foreignPrefix: delPrefix + "_orders", foreignTemplate: "orders.{{.PartitionID}}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			url, _, stream := newDeleteRig(t, jetstream.WorkQueuePolicy)
			foreign, _ := addDurable(t, stream, tt.foreignPrefix, tt.foreignTemplate, "a", nil)
			args := []string{
				"consumers", "delete", "-server", url, "-stream", delStream,
				"-prefix", delPrefix, "-subject-template", tt.template, "-settle", delSettle,
			}

			// The other consumer's durable is a candidate...
			stdout, stderr, code := runArgs(args...)
			require.Equal(t, ExitOK, code, "stderr: %s", stderr)
			require.Contains(t, stdout, foreign)

			// ...so -apply is refused for a discovered selection.
			_, stderr, code = runArgs(append(args, "-apply")...)
			require.Equal(t, ExitValidation, code)
			require.Contains(t, stderr, "-names-file")
			require.True(t, durableExists(t, stream, foreign))
		})
	}
}

func TestConsumersDelete_RetentionGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		retention jetstream.RetentionPolicy
		flags     []string
		wantCode  int
	}{
		{name: "limits_refused", retention: jetstream.LimitsPolicy, wantCode: ExitDrift},
		{name: "limits_allowed", retention: jetstream.LimitsPolicy, flags: []string{"-allow-replay"}, wantCode: ExitOK},
		{name: "limits_wrong_override", retention: jetstream.LimitsPolicy, flags: []string{"-allow-loss"}, wantCode: ExitDrift},
		{name: "interest_refused", retention: jetstream.InterestPolicy, wantCode: ExitDrift},
		{name: "interest_allowed", retention: jetstream.InterestPolicy, flags: []string{"-allow-loss"}, wantCode: ExitOK},
		{name: "interest_wrong_override", retention: jetstream.InterestPolicy, flags: []string{"-allow-replay"}, wantCode: ExitDrift},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			url, _, stream := newDeleteRig(t, tt.retention)
			name, _ := addDurable(t, stream, delPrefix, delTemplate, "a", nil)

			stdout, stderr, code := runArgs(applyArgs(url, []string{name}, tt.flags...)...)

			require.Equal(t, tt.wantCode, code, "stdout: %s\nstderr: %s", stdout, stderr)
			require.Equal(t, tt.wantCode != ExitOK, durableExists(t, stream, name))
		})
	}
}

func TestConsumersDelete_RefusesWhileAPullRequestWaits(t *testing.T) {
	t.Parallel()
	url, _, stream := newDeleteRig(t, jetstream.WorkQueuePolicy)
	name, cons := addDurable(t, stream, delPrefix, delTemplate, "a", nil)

	// A running consumer instance: a pull request that stays on the server.
	iter, err := cons.Messages(jetstream.PullMaxMessages(1), jetstream.PullExpiry(30*time.Second))
	require.NoError(t, err)
	defer iter.Stop()
	go func() { _, _ = iter.Next() }()
	waitConsumerInfo(t, cons, "pull request never reached the server",
		func(info *jetstream.ConsumerInfo) bool { return info.NumWaiting == 1 })

	// No override lets a delete through while a pull request waits.
	stdout, stderr, code := runArgs(
		applyArgs(url, []string{name}, "-allow-replay", "-allow-loss", "-allow-redelivery")...)

	require.Equal(t, ExitDrift, code, "stdout: %s\nstderr: %s", stdout, stderr)
	require.Contains(t, stdout, "pull request")
	require.True(t, durableExists(t, stream, name))
}

func TestConsumersDelete_RedeliveryGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// maxDeliver 1 makes the server give up on the unacknowledged message once AckWait passes.
		maxDeliver int
		// wantInOutput names the counter that reports the message.
		wantInOutput string
	}{
		{name: "awaiting_ack", maxDeliver: 10, wantInOutput: "ack pending 1"},
		{name: "max_deliver_exhausted", maxDeliver: 1, wantInOutput: "MaxDeliver exhausted 1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			url, js, stream := newDeleteRig(t, jetstream.WorkQueuePolicy)
			name, cons := addDurable(t, stream, delPrefix, delTemplate, "a", func(cfg *jetstream.ConsumerConfig) {
				cfg.MaxDeliver = tt.maxDeliver
				cfg.AckWait = 250 * time.Millisecond
			})
			publishN(t, js, "orders.a", 3)
			fetchOne(t, cons, false)
			if tt.maxDeliver == 1 {
				waitConsumerInfo(t, cons, "the server never gave up on the message",
					func(info *jetstream.ConsumerInfo) bool { return info.NumAckPending == 0 })
			}

			stdout, stderr, code := runArgs(applyArgs(url, []string{name})...)
			require.Equal(t, ExitDrift, code, "stdout: %s\nstderr: %s", stdout, stderr)
			require.Contains(t, stdout, tt.wantInOutput)
			require.True(t, durableExists(t, stream, name))

			stdout, stderr, code = runArgs(applyArgs(url, []string{name}, "-allow-redelivery")...)
			require.Equal(t, ExitOK, code, "stdout: %s\nstderr: %s", stdout, stderr)
			require.False(t, durableExists(t, stream, name))
		})
	}
}

// A backlog that was partly processed and fully acknowledged does not block.
func TestConsumersDelete_AcknowledgedBacklogDoesNotBlock(t *testing.T) {
	t.Parallel()
	url, js, stream := newDeleteRig(t, jetstream.WorkQueuePolicy)
	name, cons := addDurable(t, stream, delPrefix, delTemplate, "a", nil)
	publishN(t, js, "orders.a", 3)
	fetchOne(t, cons, true)

	stdout, stderr, code := runArgs(applyArgs(url, []string{name})...)

	require.Equal(t, ExitOK, code, "stdout: %s\nstderr: %s", stdout, stderr)
	require.False(t, durableExists(t, stream, name))
}

// On limits retention the accepted replay already covers messages that were never acknowledged.
func TestConsumersDelete_LimitsReplayCoversUnacknowledgedMessages(t *testing.T) {
	t.Parallel()
	url, js, stream := newDeleteRig(t, jetstream.LimitsPolicy)
	name, cons := addDurable(t, stream, delPrefix, delTemplate, "a", nil)
	publishN(t, js, "orders.a", 2)
	fetchOne(t, cons, false)

	stdout, stderr, code := runArgs(applyArgs(url, []string{name}, "-allow-replay")...)

	require.Equal(t, ExitOK, code, "stdout: %s\nstderr: %s", stdout, stderr)
	require.False(t, durableExists(t, stream, name))
}

// NATS has no conditional delete, so the durable is read once more just before it is deleted.
func TestDeleteDurables_StopsAtADurableThatMoved(t *testing.T) {
	t.Parallel()
	_, js, stream := newDeleteRig(t, jetstream.WorkQueuePolicy)
	// Names sort in this order, which is the order they are deleted in.
	first := addNamedDurable(t, stream, "d1", "orders.a", nil).CachedInfo().Name
	movedCons := addNamedDurable(t, stream, "d2", "orders.b", nil)
	last := addNamedDurable(t, stream, "d3", "orders.c", nil).CachedInfo().Name

	snap, err := takeSnapshot(t.Context(), stream, durableSelector{stream: delStream, names: []string{"d1", "d2", "d3"}}, false)
	require.NoError(t, err)
	require.Len(t, snap.Durables, 3)

	// A consumer delivers from the second durable after the gates looked at it.
	publishN(t, js, "orders.b", 1)
	fetchOne(t, movedCons, true)

	deleted, err := deleteDurables(t.Context(), stream, snap.Durables)

	require.ErrorIs(t, err, errConsumerActive)
	require.Equal(t, []string{first}, deleted)
	require.False(t, durableExists(t, stream, first))
	require.True(t, durableExists(t, stream, "d2"))
	require.True(t, durableExists(t, stream, last), "the run must stop at the durable that moved")
}

func TestConsumersDelete_MissingNameIsNotAnError(t *testing.T) {
	t.Parallel()
	url, _, stream := newDeleteRig(t, jetstream.WorkQueuePolicy)
	addNamedDurable(t, stream, "queue-one", "orders.q", nil)
	addNamedDurable(t, stream, "queue-other", "misc.q", nil)

	// A re-run after a partial delete names durables that are already gone.
	stdout, stderr, code := runArgs(applyArgs(url, []string{"queue-one", "queue-gone"})...)

	require.Equal(t, ExitOK, code, "stdout: %s\nstderr: %s", stdout, stderr)
	require.Contains(t, stdout, "queue-gone")
	require.False(t, durableExists(t, stream, "queue-one"))
	require.True(t, durableExists(t, stream, "queue-other"))
}

// A run that could not finish its checks must not read as a clean report.
func TestRunDelete_InterruptedDuringObservationIsIncomplete(t *testing.T) {
	t.Parallel()
	_, _, stream := newDeleteRig(t, jetstream.WorkQueuePolicy)
	name, _ := addDurable(t, stream, delPrefix, delTemplate, "a", nil)

	// The deadline falls inside the observation window, after the first reading.
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	report, err := runDelete(ctx, stream, deleteParams{
		sel: durableSelector{stream: delStream, names: []string{name}}, settle: time.Hour, apply: true,
	})
	report.conclude(err)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Len(t, report.Durables, 1, "the first reading must have completed")
	require.Equal(t, outcomeIncomplete, report.Outcome)
	require.NotEmpty(t, report.Error)
	var text strings.Builder
	emitDeleteReport(&text, report, false, false)
	require.Contains(t, text.String(), "did not complete")
	require.NotContains(t, text.String(), "no check blocked")
	require.True(t, durableExists(t, stream, name))
}

// A clustered consumer whose only server is down is listed without its name.
// It must not be taken for a durable that does not exist:
// a delete would then report success and leave its old configuration behind.
func TestDurableSelector_OfflineDurable(t *testing.T) {
	t.Parallel()
	servers, nc := partitest.StartEmbeddedNATSCluster(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	// Limits retention lets a consumer have fewer replicas than the stream.
	stream, err := js.CreateStream(t.Context(), jetstream.StreamConfig{
		Name: delStream, Subjects: []string{"orders.>", "misc.*"}, Storage: jetstream.MemoryStorage, Replicas: 3,
	})
	require.NoError(t, err)
	healthy, _ := addDurable(t, stream, delPrefix, delTemplate, "a", nil)
	const offlinePrefix, offlineTemplate = "audit", "misc.{{.PartitionID}}"
	offline, offlineCons := addDurable(t, stream, offlinePrefix, offlineTemplate, "x",
		func(cfg *jetstream.ConsumerConfig) { cfg.Replicas = 1 })

	// Stop the one server that hosts the single-replica durable, and look from a server that survives.
	info, err := offlineCons.Info(t.Context())
	require.NoError(t, err)
	host := info.Cluster.Leader
	require.NotEmpty(t, host)
	var survivorURL string
	for _, srv := range servers {
		if srv.Name() != host {
			survivorURL = srv.ClientURL()
		}
	}
	survivorNC, err := nats.Connect(survivorURL)
	require.NoError(t, err)
	defer survivorNC.Close()
	survivorJS, err := jetstream.New(survivorNC)
	require.NoError(t, err)
	for _, srv := range servers {
		if srv.Name() == host {
			srv.Shutdown()
			srv.WaitForShutdown()
		}
	}

	find := func(sel durableSelector) ([]*jetstream.ConsumerInfo, []string, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		survivorStream, streamErr := survivorJS.Stream(ctx, delStream)
		if streamErr != nil {
			return nil, nil, streamErr
		}

		return sel.find(ctx, survivorStream)
	}

	// Discovery of another consumer's durables is not blocked by the offline one.
	// It also shows that the cluster answers again after losing a server.
	require.Eventually(t, func() bool {
		infos, _, findErr := find(durableSelector{stream: delStream, prefix: delPrefix, template: delTemplate})

		return findErr == nil && len(infos) == 1 && infos[0].Name == healthy
	}, 30*time.Second, 200*time.Millisecond, "discovery of the healthy candidate did not succeed")

	// Named explicitly, the offline durable is an error, not a missing durable.
	infos, missing, err := find(durableSelector{stream: delStream, names: []string{healthy, offline}})
	require.Error(t, err, "found %d, missing %v", len(infos), missing)
	require.NotErrorIs(t, err, jetstream.ErrConsumerNotFound)
	t.Logf("offline durable, named: %v", err)

	// As a discovery candidate it cannot be observed, so discovery fails too.
	_, _, err = find(durableSelector{stream: delStream, prefix: offlinePrefix, template: offlineTemplate})
	require.Error(t, err)
	t.Logf("offline durable, discovered: %v", err)
}

// A names file keeps every name it is given, whatever characters the name has.
func TestDurableSelector_NamesFileRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "durables.txt")
	require.NoError(t, saveDurableNames(path, []durableState{{Name: "#queue"}, {Name: "plain"}}))

	sel := durableSelector{stream: delStream, namesFile: path}
	require.NoError(t, sel.resolve())
	require.Equal(t, []string{"#queue", "plain"}, sel.names)
}

func TestConsumersDelete_FlagValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
	}{
		{name: "no_stream", args: []string{"-prefix", delPrefix, "-subject-template", delTemplate}},
		{name: "no_selection", args: []string{"-stream", delStream}},
		{name: "prefix_without_template", args: []string{"-stream", delStream, "-prefix", delPrefix}},
		{name: "template_without_placeholder", args: []string{"-stream", delStream, "-prefix", delPrefix, "-subject-template", "orders.x"}},
		{
			// The runtime names such durables by the whole subject; discovery would silently find none.
			name: "template_with_two_placeholders",
			args: []string{"-stream", delStream, "-prefix", delPrefix, "-subject-template", "orders.{{.PartitionID}}.{{.PartitionID}}"},
		},
		{
			// A spelling the placeholder parser does not recognise as the placeholder.
			name: "template_with_two_actions",
			args: []string{"-stream", delStream, "-prefix", delPrefix, "-subject-template", "{{ .PartitionID }}.{{.PartitionID}}"},
		},
		{name: "prefix_and_name", args: []string{"-stream", delStream, "-prefix", delPrefix, "-subject-template", delTemplate, "-name", "x"}},
		{name: "bad_settle", args: []string{"-stream", delStream, "-name", "x", "-settle", "soon"}},
		{name: "negative_settle", args: []string{"-stream", delStream, "-name", "x", "-settle", "-1s"}},
		{name: "names_file_missing", args: []string{"-stream", delStream, "-names-file", "/nonexistent/durables.txt"}},
		{name: "save_names_with_apply", args: []string{"-stream", delStream, "-name", "x", "-apply", "-save-names", "out.txt"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, code := runArgs(append([]string{"consumers", "delete"}, tt.args...)...)
			require.Equal(t, ExitValidation, code)
		})
	}
}

func TestConsumersDelete_MissingStream(t *testing.T) {
	t.Parallel()
	url := startTestNATS(t)

	_, stderr, code := runArgs("consumers", "delete", "-server", url, "-stream", "absent", "-name", "x")

	require.Equal(t, ExitValidation, code)
	require.Contains(t, stderr, "absent")
}

func TestConsumersVerify(t *testing.T) {
	t.Parallel()
	url, _, stream := newDeleteRig(t, jetstream.WorkQueuePolicy)
	a, _ := addDurable(t, stream, delPrefix, delTemplate, "a", func(cfg *jetstream.ConsumerConfig) { cfg.MaxWaiting = 16 })
	addDurable(t, stream, delPrefix, delTemplate, "b", func(cfg *jetstream.ConsumerConfig) { cfg.MaxWaiting = 16 })

	verify := func(maxWaiting int, selection ...string) int {
		args := append([]string{"consumers", "verify", "-server", url, "-stream", delStream}, selection...)
		_, _, code := runArgs(append(args, "-max-waiting", strconv.Itoa(maxWaiting))...)

		return code
	}
	dynamic := []string{"-prefix", delPrefix, "-subject-template", delTemplate}

	require.Equal(t, ExitOK, verify(16, dynamic...))
	require.Equal(t, ExitDrift, verify(2, dynamic...), "a durable with another value must fail the check")
	require.Equal(t, ExitDrift, verify(16, "-prefix", "nobody", "-subject-template", delTemplate),
		"no durable found must fail the check")
	require.Equal(t, ExitDrift, verify(16, "-name", "queue-gone"), "no durable found must fail the check")
	// A Dynamic durable exists only while its partition is assigned,
	// so a listed name that was not created again does not fail the check on its own.
	require.Equal(t, ExitOK, verify(16, "-name", a, "-name", "unassigned-partition"))
}

// TestEvaluateDeleteGates covers the gates that need two observations of the same durable.
func TestEvaluateDeleteGates(t *testing.T) {
	t.Parallel()

	idle := durableState{Name: "d", DeliveredSeq: 5, AckFloorSeq: 5}
	stream := streamState{Retention: jetstream.WorkQueuePolicy, LastSeq: 9}

	tests := []struct {
		name          string
		before, after []durableState
		streamAfter   streamState
		wantBlocked   bool
		wantWarning   bool
	}{
		{name: "idle", before: []durableState{idle}, after: []durableState{idle}, streamAfter: stream},
		{
			name:   "delivered_during_window",
			before: []durableState{idle}, after: []durableState{{Name: "d", DeliveredSeq: 6, AckFloorSeq: 5}},
			streamAfter: stream, wantBlocked: true,
		},
		{
			name:   "acknowledged_during_window",
			before: []durableState{{Name: "d", DeliveredSeq: 5, AckFloorSeq: 4}}, after: []durableState{idle},
			streamAfter: stream, wantBlocked: true,
		},
		{
			// An out-of-order acknowledgement leaves both sequences where they were.
			name:        "ack_pending_changed_during_window",
			before:      []durableState{{Name: "d", DeliveredSeq: 5, AckFloorSeq: 3, NumAckPending: 2}},
			after:       []durableState{{Name: "d", DeliveredSeq: 5, AckFloorSeq: 3, NumAckPending: 1}},
			streamAfter: stream, wantBlocked: true,
		},
		{
			name:   "pull_waiting_at_first_read_only",
			before: []durableState{{Name: "d", DeliveredSeq: 5, AckFloorSeq: 5, NumWaiting: 1}}, after: []durableState{idle},
			streamAfter: stream, wantBlocked: true,
		},
		{name: "appeared_during_window", before: nil, after: []durableState{idle}, streamAfter: stream, wantBlocked: true},
		{name: "disappeared_during_window", before: []durableState{idle}, after: nil, streamAfter: stream, wantBlocked: true},
		{
			// Publishing alone is harmless on a WorkQueue stream; it only makes the counts approximate.
			name:   "stream_received_messages",
			before: []durableState{idle}, after: []durableState{idle},
			streamAfter: streamState{Retention: stream.Retention, LastSeq: 12}, wantWarning: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Every override is set: none of them applies to a durable that shows activity.
			overrides := deleteOverrides{replay: true, loss: true, redelivery: true}
			blockers, warnings := evaluateDelete(
				snapshot{Stream: stream, Durables: tt.before},
				snapshot{Stream: tt.streamAfter, Durables: tt.after},
				overrides,
			)
			require.Equal(t, tt.wantBlocked, len(blockers) > 0, "blockers: %v", blockers)
			require.Equal(t, tt.wantWarning, len(warnings) > 0, "warnings: %v", warnings)
		})
	}
}
