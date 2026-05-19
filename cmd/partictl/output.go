package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/arloliu/parti/v2/provision"
)

// jsonOutput marshals v to JSON and writes it to w, followed by a newline.
// Returns an error only when the marshal fails (should never happen for
// well-formed provision types).
func jsonOutput(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// --- Snapshot renderer ---

// renderSnapshotText writes a human-readable table of the snapshot to w.
// Wall-clock timestamps are excluded per the plan's output rules.
func renderSnapshotText(w io.Writer, snap provision.Snapshot) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	defer tw.Flush()

	allEmpty := len(snap.ControlPlane) == 0 &&
		len(snap.PartitionSource) == 0 &&
		len(snap.DynamicConsumers) == 0
	if allEmpty {
		fmt.Fprintln(w, "no Parti-managed resources found")
		return
	}

	if len(snap.ControlPlane) > 0 {
		fmt.Fprintln(tw, "CONTROL PLANE")
		fmt.Fprintln(tw, "  BUCKET\tCOMPONENT\tSTORAGE\tHISTORY\tTTL\tINSTANCE")
		for _, b := range snap.ControlPlane {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%d\t%s\t%s\n",
				b.Bucket, b.Component, b.Storage, b.History,
				fmtTTL(b.TTL), b.Instance)
		}
		fmt.Fprintln(tw)
	}

	if len(snap.PartitionSource) > 0 {
		fmt.Fprintln(tw, "PARTITION SOURCE")
		fmt.Fprintln(tw, "  BUCKET\tCOMPONENT\tSTORAGE\tHISTORY\tTTL\tINSTANCE")
		for _, b := range snap.PartitionSource {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%d\t%s\t%s\n",
				b.Bucket, b.Component, b.Storage, b.History,
				fmtTTL(b.TTL), b.Instance)
		}
		fmt.Fprintln(tw)
	}

	if len(snap.DynamicConsumers) > 0 {
		fmt.Fprintln(tw, "DYNAMIC CONSUMERS")
		fmt.Fprintln(tw, "  STREAM\tDURABLE\tCOMPONENT\tINSTANCE")
		for _, c := range snap.DynamicConsumers {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
				c.StreamName, c.Durable, c.Component, c.Instance)
		}
		fmt.Fprintln(tw)
	}
}

func fmtTTL(d time.Duration) string {
	if d == 0 {
		return "none"
	}
	return d.String()
}

// --- PlanResult renderer ---

// renderPlanText writes a human-readable plan output to w.
func renderPlanText(w io.Writer, plan provision.PlanResult) {
	if len(plan.Actions) == 0 && len(plan.Drift) == 0 {
		fmt.Fprintln(w, "plan: no changes required")
		return
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	defer tw.Flush()

	if len(plan.Actions) > 0 {
		fmt.Fprintln(tw, "ACTIONS")
		fmt.Fprintln(tw, "  KIND\tNAME")
		for _, a := range plan.Actions {
			fmt.Fprintf(tw, "  %s\t%s\n", a.Kind, a.Name)
		}
		fmt.Fprintln(tw)
	}

	if len(plan.Drift) > 0 {
		fmt.Fprintln(tw, "DRIFT")
		fmt.Fprintln(tw, "  SEVERITY\tKIND\tNAME\tDETAIL")
		for _, d := range plan.Drift {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
				d.Severity, d.Kind, d.Name, fmtDriftDetail(d.Detail))
		}
		fmt.Fprintln(tw)
	}
}

func fmtDriftDetail(detail map[string]any) string {
	if len(detail) == 0 {
		return ""
	}
	parts := make([]string, 0, len(detail))
	for k, v := range detail {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	// Sort for deterministic output.
	sortStrings(parts)

	return strings.Join(parts, " ")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// --- Report renderer ---

// renderReportText writes a human-readable apply report to w.
func renderReportText(w io.Writer, report provision.Report) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	defer tw.Flush()

	if report.Aborted {
		fmt.Fprintln(w, "apply: aborted (context cancelled)")
	}

	if len(report.Executed) == 0 && len(report.Skipped) == 0 && len(report.Errors) == 0 {
		if !report.Aborted {
			fmt.Fprintln(w, "apply: no changes applied")
		}
		return
	}

	if len(report.Executed) > 0 {
		fmt.Fprintln(tw, "EXECUTED")
		fmt.Fprintln(tw, "  KIND\tNAME\tRACED")
		for _, a := range report.Executed {
			raced := ""
			if a.Raced {
				raced = "yes"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", a.Kind, a.Name, raced)
		}
		fmt.Fprintln(tw)
	}

	if len(report.Skipped) > 0 {
		fmt.Fprintln(tw, "SKIPPED")
		fmt.Fprintln(tw, "  KIND\tNAME\tREASON")
		for _, a := range report.Skipped {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", a.Kind, a.Name, a.Reason)
		}
		fmt.Fprintln(tw)
	}

	if len(report.Errors) > 0 {
		fmt.Fprintln(tw, "ERRORS")
		fmt.Fprintln(tw, "  KIND\tNAME\tERROR")
		for _, e := range report.Errors {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", e.Kind, e.Name, e.Error)
		}
		fmt.Fprintln(tw)
	}
}

// renderValidateText writes a human-readable validate result to w.
// ok is true when validation passed; errs contains the validation error strings.
func renderValidateText(w io.Writer, ok bool, errs []string) {
	if ok {
		fmt.Fprintln(w, "ok")
		return
	}
	for _, e := range errs {
		fmt.Fprintln(w, "validation error:", e)
	}
}

// validateResultJSON is the JSON envelope for the validate command.
// We reuse the Report shape as directed by the plan ("Prefer reusing Report").
// On success, Executed/Skipped/Errors are all nil/empty.
// On failure, Errors contains one entry per validation error.
func validateResultJSON(ok bool, errs []string) provision.Report {
	r := provision.Report{
		APIVersion: provision.APIVersionProvisionV1,
		Kind:       provision.KindReport,
		Executed:   []provision.ExecutedAction{},
		Skipped:    []provision.SkippedAction{},
		Errors:     []provision.ResourceError{},
	}
	if !ok {
		for _, e := range errs {
			r.Errors = append(r.Errors, provision.ResourceError{
				Kind:  "config",
				Name:  "validate",
				Error: e,
			})
		}
	}

	return r
}
