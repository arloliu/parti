package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/arloliu/parti/v2/internal/dynamicbuild"
)

// maxListedBlockers bounds how many blockers the text output spells out.
const maxListedBlockers = 10

// Outcomes of a delete run, as reported in deleteReport.Outcome.
const (
	// outcomeClear: every check completed, none blocked, and nothing was deleted (no -apply).
	outcomeClear = "clear"
	// outcomeBlocked: a check blocked the delete; nothing was deleted.
	outcomeBlocked = "blocked"
	// outcomeDeleted: every selected durable was deleted or was already gone.
	outcomeDeleted = "deleted"
	// outcomeIncomplete: the checks did not complete; nothing was deleted.
	outcomeIncomplete = "incomplete"
	// outcomePartial: the delete stopped part-way; Deleted lists what is gone.
	outcomePartial = "partial"
)

// errConsumerActive reports a durable that showed activity after the gates passed.
var errConsumerActive = errors.New("consumer became active")

// durableSelector names the durable consumers a command works on.
//
// Explicit names are exact.
// A prefix with a subject template discovers the durables that parti would name that way
// for some partition id.
// Discovery cannot prove ownership: a durable name does not say where the prefix ends,
// so a consumer with a longer prefix that reads deeper subjects produces the same names.
// A discovered selection may therefore be listed and verified, but never deleted.
type durableSelector struct {
	stream    string
	prefix    string
	template  string
	names     []string
	namesFile string
}

// deleteOverrides are the consequences the operator accepted on the command line.
type deleteOverrides struct {
	replay     bool
	loss       bool
	redelivery bool
}

type deleteParams struct {
	common    commonFlags
	sel       durableSelector
	overrides deleteOverrides
	settle    time.Duration
	saveNames string
	apply     bool
	jsonOut   bool
}

// streamState is what the delete gates need to know about the stream.
type streamState struct {
	Name      string                    `json:"name"`
	Retention jetstream.RetentionPolicy `json:"retention"`
	MaxAge    time.Duration             `json:"maxAge"`
	Messages  uint64                    `json:"messages"`
	LastSeq   uint64                    `json:"lastSeq"`
}

// durableState is one observation of a durable consumer.
type durableState struct {
	Name          string `json:"name"`
	FilterSubject string `json:"filterSubject,omitempty"`
	MaxWaiting    int    `json:"maxWaiting"`
	NumWaiting    int    `json:"numWaiting"`
	NumAckPending int    `json:"numAckPending"`
	NumPending    uint64 `json:"numPending"`
	DeliveredSeq  uint64 `json:"deliveredSeq"`
	AckFloorSeq   uint64 `json:"ackFloorSeq"`
	// Exhausted counts messages still in a WorkQueue stream that the durable
	// neither holds for delivery nor waits on an acknowledgement for:
	// it delivered them MaxDeliver times and gave up.
	Exhausted uint64 `json:"maxDeliverExhausted"`
}

// snapshot is one observation of the stream and the selected durables.
type snapshot struct {
	Stream   streamState    `json:"stream"`
	Durables []durableState `json:"durables"`
	// Missing lists explicitly named durables that do not exist.
	Missing []string `json:"missing,omitempty"`
}

// blocker is a reason not to delete.
// Override names the flag that accepts it; empty means nothing does.
type blocker struct {
	Reason   string `json:"reason"`
	Override string `json:"override,omitempty"`
}

type deleteReport struct {
	snapshot
	// Outcome is one of the outcome constants.
	Outcome  string    `json:"outcome"`
	Error    string    `json:"error,omitempty"`
	Blockers []blocker `json:"blockers,omitempty"`
	Warnings []string  `json:"warnings,omitempty"`
	Applied  bool      `json:"applied"`
	Deleted  []string  `json:"deleted,omitempty"`
}

type verifyReport struct {
	Stream     string         `json:"stream"`
	MaxWaiting int            `json:"expectedMaxWaiting"`
	Durables   []durableState `json:"durables"`
	Missing    []string       `json:"missing,omitempty"`
	Mismatched []string       `json:"mismatched,omitempty"`
}

func bindSelectorFlags(fs *flag.FlagSet, sel *durableSelector) {
	fs.StringVar(&sel.stream, "stream", "", "stream the durable consumers belong to (required)")
	fs.StringVar(&sel.prefix, "prefix", "",
		"discover the durables named like those of a Dynamic consumer with this ConsumerPrefix; requires -subject-template")
	fs.StringVar(&sel.template, "subject-template", "", "SubjectTemplate of that Dynamic consumer")
	fs.Func("name", "durable consumer name (repeatable)", func(v string) error {
		sel.names = append(sel.names, v)

		return nil
	})
	fs.StringVar(&sel.namesFile, "names-file", "", "file with one durable consumer name per line; blank lines are skipped")
}

func cmdConsumersDelete(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("consumers delete", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		p         deleteParams
		settleStr string
	)
	bindSelectorFlags(fs, &p.sel)
	bindCommonFlags(fs, &p.common)
	fs.BoolVar(&p.apply, "apply", false,
		"delete the durables; without it the command only reports. Needs -name or -names-file")
	fs.StringVar(&p.saveNames, "save-names", "", "write the names of the selected durables to this file")
	fs.StringVar(&settleStr, "settle", "5s", "how long to watch the durables for activity before deciding")
	fs.BoolVar(&p.overrides.replay, "allow-replay", false,
		"limits retention: accept that recreated durables replay every retained message")
	fs.BoolVar(&p.overrides.loss, "allow-loss", false,
		"interest retention: accept that messages no other consumer holds are removed")
	fs.BoolVar(&p.overrides.redelivery, "allow-redelivery", false,
		"workqueue retention: accept that messages delivered without an acknowledgement are delivered again")
	fs.BoolVar(&p.jsonOut, "json", false, "emit JSON output")

	if err := fs.Parse(args); err != nil {
		return ExitValidation
	}
	var err error
	p.settle, err = time.ParseDuration(settleStr)
	if err == nil && p.settle < 0 {
		err = errors.New("must not be negative")
	}
	if err != nil {
		fmt.Fprintf(stderr, "partictl consumers delete: invalid -settle %q: %v\n", settleStr, err)

		return ExitValidation
	}
	err = p.sel.resolve()
	if err == nil && p.apply && p.sel.prefix != "" {
		err = errors.New("-apply needs an explicit list of durables: discovery by -prefix cannot prove " +
			"that a durable belongs to this consumer. Save the candidates with -save-names <file>, " +
			"review them, and delete with -names-file <file> -apply")
	}
	if err == nil && p.apply && p.saveNames != "" {
		err = errors.New("-save-names belongs to a report run; it cannot be combined with -apply")
	}
	if err != nil {
		fmt.Fprintf(stderr, "partictl consumers delete: %v\n", err)

		return ExitValidation
	}

	conn, stream, code := openStream(p.common, p.sel.stream, "consumers delete", stderr)
	if code != ExitOK {
		return code
	}
	defer conn.close()

	report, err := runDelete(conn.ctx, stream, p)
	if err == nil && p.saveNames != "" {
		err = saveDurableNames(p.saveNames, report.Durables)
	}
	report.conclude(err)
	emitDeleteReport(stdout, report, p.jsonOut, p.sel.prefix != "")
	if err != nil {
		fmt.Fprintf(stderr, "partictl consumers delete: %v\n", err)
	}

	switch {
	case errors.Is(err, errConsumerActive):
		return ExitDrift
	case err != nil:
		if ctxDead(conn.ctx) {
			return ExitNATS
		}

		return classifyError(err)
	case len(report.Blockers) > 0:
		return ExitDrift
	}

	return ExitOK
}

func cmdConsumersVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("consumers verify", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		common     commonFlags
		sel        durableSelector
		maxWaiting int
		jsonOut    bool
	)
	bindSelectorFlags(fs, &sel)
	bindCommonFlags(fs, &common)
	fs.IntVar(&maxWaiting, "max-waiting", 0, "MaxWaiting every selected durable must have (required)")
	fs.BoolVar(&jsonOut, "json", false, "emit JSON output")

	if err := fs.Parse(args); err != nil {
		return ExitValidation
	}
	err := sel.resolve()
	if err == nil && maxWaiting <= 0 {
		err = errors.New("-max-waiting is required and must be positive")
	}
	if err != nil {
		fmt.Fprintf(stderr, "partictl consumers verify: %v\n", err)

		return ExitValidation
	}

	conn, stream, code := openStream(common, sel.stream, "consumers verify", stderr)
	if code != ExitOK {
		return code
	}
	defer conn.close()

	snap, err := takeSnapshot(conn.ctx, stream, sel, false)
	if err != nil {
		fmt.Fprintf(stderr, "partictl consumers verify: %v\n", err)
		if ctxDead(conn.ctx) {
			return ExitNATS
		}

		return classifyError(err)
	}

	report := verifyReport{Stream: sel.stream, MaxWaiting: maxWaiting, Durables: snap.Durables, Missing: snap.Missing}
	for _, d := range snap.Durables {
		if d.MaxWaiting != maxWaiting {
			report.Mismatched = append(report.Mismatched, d.Name)
		}
	}
	emitVerifyReport(stdout, report, jsonOut)

	// A durable that does not exist is not a failure on its own:
	// a Dynamic consumer creates one only while its partition is assigned.
	if len(report.Durables) == 0 || len(report.Mismatched) > 0 {
		return ExitDrift
	}

	return ExitOK
}

// openStream connects and looks the stream up.
// A missing stream is a validation error.
func openStream(cf commonFlags, name, subcmd string, stderr io.Writer) (*natsConn, jetstream.Stream, int) {
	timeout, err := parseTimeout(cf.timeout)
	if err != nil {
		fmt.Fprintln(stderr, err)

		return nil, nil, ExitValidation
	}

	conn, code := connectWithTimeout(timeout, cf, stderr)
	if code != ExitOK {
		return nil, nil, code
	}

	stream, err := conn.js.Stream(conn.ctx, name)
	if err != nil {
		fmt.Fprintf(stderr, "partictl %s: stream %q: %v\n", subcmd, name, err)
		conn.close()
		switch {
		case errors.Is(err, jetstream.ErrStreamNotFound):
			return nil, nil, ExitValidation
		case ctxDead(conn.ctx):
			return nil, nil, ExitNATS
		}

		return nil, nil, classifyError(err)
	}

	return conn, stream, ExitOK
}

// runDelete observes the durables twice, settle apart, evaluates the gates,
// and deletes when nothing blocks and apply is set.
func runDelete(ctx context.Context, stream jetstream.Stream, p deleteParams) (deleteReport, error) {
	before, err := takeSnapshot(ctx, stream, p.sel, false)
	if err != nil {
		return deleteReport{}, err
	}

	select {
	case <-ctx.Done():
		return deleteReport{snapshot: before}, ctx.Err()
	case <-time.After(p.settle):
	}

	after, err := takeSnapshot(ctx, stream, p.sel, true)
	if err != nil {
		return deleteReport{snapshot: before}, err
	}

	report := deleteReport{snapshot: after}
	report.Blockers, report.Warnings = evaluateDelete(before, after, p.overrides)
	if !p.apply || len(report.Blockers) > 0 {
		return report, nil
	}

	report.Applied = true
	report.Deleted, err = deleteDurables(ctx, stream, after.Durables)

	return report, err
}

// evaluateDelete decides whether the durables seen in two observations may be deleted.
//
// A durable that shows any sign of a running consumer blocks without override:
// deleting it would discard messages in flight,
// and the consumer would create the durable again with its own configuration.
// The other blockers are consequences of the stream's retention; each has an override.
func evaluateDelete(before, after snapshot, overrides deleteOverrides) ([]blocker, []string) {
	blockers := activityBlockers(before.Durables, after.Durables)

	switch after.Stream.Retention {
	case jetstream.LimitsPolicy:
		if !overrides.replay {
			blockers = append(blockers, blocker{
				Reason:   "stream retention is limits: a recreated durable replays every message retained on its subject",
				Override: "-allow-replay",
			})
		}
	case jetstream.InterestPolicy:
		if !overrides.loss {
			blockers = append(blockers, blocker{
				Reason: "stream retention is interest: messages that no other consumer holds are removed, " +
					"including those published before the durables exist again",
				Override: "-allow-loss",
			})
		}
	case jetstream.WorkQueuePolicy:
		// Only here does a recreated durable receive exactly the unacknowledged messages.
		var ackPending, exhausted uint64
		for _, d := range after.Durables {
			ackPending += uint64(max(d.NumAckPending, 0))
			exhausted += d.Exhausted
		}
		if ackPending+exhausted > 0 && !overrides.redelivery {
			blockers = append(blockers, blocker{
				Reason: fmt.Sprintf("%d message(s) were delivered without an acknowledgement and will be delivered again "+
					"(ack pending %d, MaxDeliver exhausted %d)", ackPending+exhausted, ackPending, exhausted),
				Override: "-allow-redelivery",
			})
		}
	}

	var warnings []string
	if after.Stream.LastSeq != before.Stream.LastSeq {
		warnings = append(warnings,
			"the stream received messages during the observation window: publishers are running, "+
				"and the MaxDeliver exhausted count is an upper bound")
	}
	if after.Stream.MaxAge > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"the stream removes messages older than %s, processed or not: keep the consumers stopped for less",
			after.Stream.MaxAge))
	}

	return blockers, warnings
}

// activityBlockers reports every durable that a consumer instance still uses,
// judged from two observations of the same selection.
func activityBlockers(before, after []durableState) []blocker {
	var blockers []blocker
	firstObserved := make(map[string]durableState, len(before))
	for _, d := range before {
		firstObserved[d.Name] = d
	}

	for _, d := range after {
		first, seen := firstObserved[d.Name]
		delete(firstObserved, d.Name)

		switch {
		case !seen:
			blockers = append(blockers, blocker{Reason: d.Name + " appeared during the observation window"})
		case first.NumWaiting > 0 || d.NumWaiting > 0:
			blockers = append(blockers, blocker{
				Reason: d.Name + " has a pull request waiting: a consumer instance is still running",
			})
		case first.moved(d):
			blockers = append(blockers, blocker{
				Reason: d.Name + " delivered or acknowledged messages during the observation window",
			})
		}
	}
	for _, name := range slices.Sorted(maps.Keys(firstObserved)) {
		blockers = append(blockers, blocker{Reason: name + " disappeared during the observation window"})
	}

	return blockers
}

// deleteDurables deletes the observed durables one by one.
//
// NATS has no conditional delete, so each durable is read once more just before:
// a durable that moved since the observation stops the run.
// A durable that is already gone is skipped, which makes a re-run safe.
//
// shortcut: two round trips per durable, one after the other;
// raise -timeout for thousands of durables, or delete in parallel if that gets too slow.
func deleteDurables(ctx context.Context, stream jetstream.Stream, observed []durableState) ([]string, error) {
	deleted := make([]string, 0, len(observed))
	for _, want := range observed {
		cons, err := stream.Consumer(ctx, want.Name)
		if errors.Is(err, jetstream.ErrConsumerNotFound) {
			continue
		}
		if err != nil {
			return deleted, fmt.Errorf("read %s before delete: %w", want.Name, err)
		}
		if now := newDurableState(cons.CachedInfo()); now.NumWaiting > 0 || want.moved(now) {
			return deleted, fmt.Errorf("%w: %s changed after it was observed; nothing more was deleted",
				errConsumerActive, want.Name)
		}

		err = stream.DeleteConsumer(ctx, want.Name)
		if err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
			return deleted, fmt.Errorf("delete %s: %w", want.Name, err)
		}
		deleted = append(deleted, want.Name)
	}

	return deleted, nil
}

// takeSnapshot reads the stream and the selected durables.
// withExhausted also counts, on a WorkQueue stream, the messages each durable gave up on.
func takeSnapshot(ctx context.Context, stream jetstream.Stream, sel durableSelector, withExhausted bool) (snapshot, error) {
	info, err := stream.Info(ctx)
	if err != nil {
		return snapshot{}, fmt.Errorf("stream info: %w", err)
	}
	snap := snapshot{Stream: streamState{
		Name:      info.Config.Name,
		Retention: info.Config.Retention,
		MaxAge:    info.Config.MaxAge,
		Messages:  info.State.Msgs,
		LastSeq:   info.State.LastSeq,
	}}

	infos, missing, err := sel.find(ctx, stream)
	if err != nil {
		return snapshot{}, err
	}
	snap.Missing = missing

	snap.Durables = make([]durableState, 0, len(infos))
	for _, ci := range infos {
		d := newDurableState(ci)
		if withExhausted && info.Config.Retention == jetstream.WorkQueuePolicy {
			backlog, backlogErr := subjectBacklog(ctx, stream, ci.Config)
			if backlogErr != nil {
				return snapshot{}, backlogErr
			}
			// Everything the durable has not given up on is either undelivered or awaiting an ack.
			if tracked := d.NumPending + uint64(max(d.NumAckPending, 0)); backlog > tracked {
				d.Exhausted = backlog - tracked
			}
		}
		snap.Durables = append(snap.Durables, d)
	}

	return snap, nil
}

// subjectBacklog counts the messages in the stream on the subjects a consumer reads.
//
// It must be read after the consumer it is compared with:
// a message published in between then counts as one the consumer gave up on,
// which blocks the delete instead of hiding such a message.
//
// shortcut: one stream info per filter subject;
// read all subject counts in one call if runs over thousands of durables get too slow.
func subjectBacklog(ctx context.Context, stream jetstream.Stream, cfg jetstream.ConsumerConfig) (uint64, error) {
	filters := filterSubjects(cfg)
	if len(filters) == 0 {
		info, err := stream.Info(ctx)
		if err != nil {
			return 0, fmt.Errorf("stream info: %w", err)
		}

		return info.State.Msgs, nil
	}

	var total uint64
	for _, filter := range filters {
		info, err := stream.Info(ctx, jetstream.WithSubjectFilter(filter))
		if err != nil {
			return 0, fmt.Errorf("stream info for subject %q: %w", filter, err)
		}
		for _, n := range info.State.Subjects {
			total += n
		}
	}

	return total, nil
}

func newDurableState(ci *jetstream.ConsumerInfo) durableState {
	return durableState{
		Name:          ci.Name,
		FilterSubject: strings.Join(filterSubjects(ci.Config), ","),
		MaxWaiting:    ci.Config.MaxWaiting,
		NumWaiting:    ci.NumWaiting,
		NumAckPending: ci.NumAckPending,
		NumPending:    ci.NumPending,
		DeliveredSeq:  ci.Delivered.Consumer,
		AckFloorSeq:   ci.AckFloor.Consumer,
	}
}

// saveDurableNames writes one durable name per line, the format -names-file reads.
func saveDurableNames(path string, durables []durableState) error {
	var b strings.Builder
	for _, d := range durables {
		b.WriteString(d.Name)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("-save-names: %w", err)
	}

	return nil
}

func filterSubjects(cfg jetstream.ConsumerConfig) []string {
	if cfg.FilterSubject != "" {
		return []string{cfg.FilterSubject}
	}

	return cfg.FilterSubjects
}

// emitDeleteReport prints the report.
// discovered says the selection came from -prefix, which changes the advice on a clear outcome.
func emitDeleteReport(w io.Writer, report deleteReport, jsonOut, discovered bool) {
	if jsonOut {
		_ = jsonOutput(w, report)

		return
	}

	fmt.Fprintf(w, "Stream %s: retention %s, %d message(s), max age %s\n\n",
		report.Stream.Name, report.Stream.Retention, report.Stream.Messages, fmtTTL(report.Stream.MaxAge))

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DURABLE\tFILTER\tMAX_WAITING\tWAITING\tACK_PENDING\tPENDING\tEXHAUSTED")
	for _, d := range report.Durables {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%d\n",
			d.Name, d.FilterSubject, d.MaxWaiting, d.NumWaiting, d.NumAckPending, d.NumPending, d.Exhausted)
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "\n%d durable(s) selected.\n", len(report.Durables))

	for _, name := range report.Missing {
		fmt.Fprintf(w, "Not found: %s\n", name)
	}
	for _, warning := range report.Warnings {
		fmt.Fprintf(w, "Warning: %s.\n", warning)
	}
	for i, b := range report.Blockers {
		if i == maxListedBlockers {
			fmt.Fprintf(w, "Blocked: ... and %d more.\n", len(report.Blockers)-i)

			break
		}
		if b.Override != "" {
			fmt.Fprintf(w, "Blocked: %s (accept with %s).\n", b.Reason, b.Override)

			continue
		}
		fmt.Fprintf(w, "Blocked: %s.\n", b.Reason)
	}

	switch report.Outcome {
	case outcomeDeleted:
		fmt.Fprintf(w, "Result: deleted %d durable(s).\n", len(report.Deleted))
	case outcomePartial:
		fmt.Fprintf(w, "Result: stopped after deleting %d durable(s): %s.\n", len(report.Deleted), report.Error)
	case outcomeIncomplete:
		fmt.Fprintf(w, "Result: nothing deleted, the checks did not complete: %s.\n", report.Error)
	case outcomeBlocked:
		fmt.Fprintln(w, "Result: nothing deleted, the delete is blocked.")
	case outcomeClear:
		if discovered {
			fmt.Fprintln(w, "Result: nothing deleted, no check blocked. These are candidates: "+
				"save them with -save-names, review the list, and delete with -names-file <file> -apply.")

			break
		}
		fmt.Fprintln(w, "Result: nothing deleted, no check blocked. Run again with -apply to delete.")
	}
}

func emitVerifyReport(w io.Writer, report verifyReport, jsonOut bool) {
	if jsonOut {
		_ = jsonOutput(w, report)

		return
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DURABLE\tMAX_WAITING\tWAITING\tPENDING")
	for _, d := range report.Durables {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\n", d.Name, d.MaxWaiting, d.NumWaiting, d.NumPending)
	}
	_ = tw.Flush()

	for _, name := range report.Missing {
		fmt.Fprintf(w, "Not found: %s\n", name)
	}
	for _, name := range report.Mismatched {
		fmt.Fprintf(w, "Mismatch: %s does not have MaxWaiting %d\n", name, report.MaxWaiting)
	}

	switch {
	case len(report.Durables) == 0:
		fmt.Fprintln(w, "Result: no durable found.")
	case len(report.Mismatched) > 0:
		fmt.Fprintf(w, "Result: not every durable has MaxWaiting %d.\n", report.MaxWaiting)
	default:
		fmt.Fprintf(w, "Result: all %d durable(s) have MaxWaiting %d.\n", len(report.Durables), report.MaxWaiting)
	}
}

// moved reports whether a later observation shows deliveries or acknowledgements since d.
func (d durableState) moved(later durableState) bool {
	return d.DeliveredSeq != later.DeliveredSeq || d.AckFloorSeq != later.AckFloorSeq ||
		d.NumAckPending != later.NumAckPending
}

// conclude records how the run ended.
func (r *deleteReport) conclude(err error) {
	switch {
	case err != nil && r.Applied:
		r.Outcome, r.Error = outcomePartial, err.Error()
	case err != nil:
		r.Outcome, r.Error = outcomeIncomplete, err.Error()
	case len(r.Blockers) > 0:
		r.Outcome = outcomeBlocked
	case r.Applied:
		r.Outcome = outcomeDeleted
	default:
		r.Outcome = outcomeClear
	}
}

// resolve loads the names file and checks that the selection is usable.
func (s *durableSelector) resolve() error {
	if s.namesFile != "" {
		data, err := os.ReadFile(s.namesFile)
		if err != nil {
			return fmt.Errorf("-names-file: %w", err)
		}
		for line := range strings.Lines(string(data)) {
			// No comment syntax: every character a durable name may contain must round-trip.
			if name := strings.TrimSpace(line); name != "" {
				s.names = append(s.names, name)
			}
		}
		if len(s.names) == 0 {
			return fmt.Errorf("-names-file %s names no durable", s.namesFile)
		}
	}

	switch {
	case s.stream == "":
		return errors.New("-stream is required")
	case s.prefix != "" && len(s.names) > 0:
		return errors.New("use either -prefix with -subject-template, or -name / -names-file, not both")
	case s.prefix == "" && len(s.names) == 0:
		return errors.New("select durables with -name or -names-file, or discover them with -prefix and -subject-template")
	case s.prefix == "" && s.template != "":
		return errors.New("-subject-template requires -prefix")
	case s.prefix != "" && s.template == "":
		return errors.New("-prefix requires -subject-template")
	}
	if s.prefix == "" {
		return nil
	}

	if err := dynamicbuild.ValidateSubjectTemplate(s.template, true); err != nil {
		return fmt.Errorf("-subject-template: %w", err)
	}
	// With a second template action the runtime names durables by the whole subject,
	// which discovery cannot tell from the subject of any other consumer.
	if strings.Count(s.template, "{{") != 1 {
		return errors.New("-subject-template with more than one placeholder cannot be discovered; use -name or -names-file")
	}

	return nil
}

// find returns the selected durables sorted by name,
// and the explicitly named ones that do not exist.
//
// It lists the stream's consumers once, however many names are selected.
// The listing alone does not establish absence:
// a clustered consumer whose servers are all offline is listed without its name.
// Every name the listing did not show is therefore looked up on its own,
// and only "consumer not found" counts as absent.
func (s durableSelector) find(ctx context.Context, stream jetstream.Stream) ([]*jetstream.ConsumerInfo, []string, error) {
	wanted := make(map[string]struct{}, len(s.names))
	for _, name := range s.names {
		wanted[name] = struct{}{}
	}

	var infos []*jetstream.ConsumerInfo
	lister := stream.ListConsumers(ctx)
	for ci := range lister.Info() {
		if ci.Name == "" {
			// An offline consumer: only its configuration is listed.
			// A named one is caught by the lookup below; a candidate cannot be observed, so discovery fails.
			if s.prefix != "" && s.ownsDurable(ci.Config.Durable, ci.Config.FilterSubject) {
				return nil, nil, fmt.Errorf("candidate %s is offline and cannot be observed", ci.Config.Durable)
			}

			continue
		}
		if _, named := wanted[ci.Name]; named || (s.prefix != "" && s.ownsDurable(ci.Name, ci.Config.FilterSubject)) {
			infos = append(infos, ci)
			delete(wanted, ci.Name)
		}
	}
	if err := lister.Err(); err != nil {
		return nil, nil, fmt.Errorf("list consumers: %w", err)
	}

	var missing []string
	for _, name := range slices.Sorted(maps.Keys(wanted)) {
		cons, err := stream.Consumer(ctx, name)
		switch {
		case errors.Is(err, jetstream.ErrConsumerNotFound):
			missing = append(missing, name)
		case err != nil:
			return nil, nil, fmt.Errorf("consumer %s: %w", name, err)
		default:
			infos = append(infos, cons.CachedInfo())
		}
	}

	slices.SortFunc(infos, func(a, b *jetstream.ConsumerInfo) int { return strings.Compare(a.Name, b.Name) })

	return infos, missing, nil
}

// ownsDurable reports whether name is what the selected Dynamic consumer
// would call the durable for subject, and subject fits the template.
//
// This makes the durable a candidate, not a proven member: see durableSelector.
func (s durableSelector) ownsDurable(name, subject string) bool {
	if name == "" || subject == "" {
		return false
	}
	pre, suf, _ := dynamicbuild.ParseSubjectTemplateParts(s.template)
	if pre != "" || suf != "" {
		if _, ok := dynamicbuild.ExtractPartitionIDFromSubject(subject, pre, suf); !ok {
			return false
		}
	}

	return name == dynamicbuild.PerSubjectDurableName(s.prefix, subject, pre, suf)
}
