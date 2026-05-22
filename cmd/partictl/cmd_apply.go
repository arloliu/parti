package main

import (
	"flag"
	"fmt"
	"io"
)

// applyFlags holds flags specific to the apply command.
type applyFlags struct {
	common      commonFlags
	file        string
	policy      string
	dryRun      bool
	jsonOut     bool
	failOnDrift bool
}

func cmdApply(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var f applyFlags
	fs.StringVar(&f.file, "f", "", "YAML config path (required)")
	fs.StringVar(&f.policy, "policy", "", "reconcile policy: warn, adopt, safe-update, force (default: warn or cfg.policy)")
	bindCommonFlags(fs, &f.common)
	fs.BoolVar(&f.dryRun, "dry-run", false, "plan only — emit the same output as partictl plan")
	fs.BoolVar(&f.jsonOut, "json", false, "emit JSON output (PlanResult for -dry-run, Report otherwise)")
	fs.BoolVar(&f.failOnDrift, "fail-on-drift", false, "exit 2 if non-informational drift is detected (dry-run only)")

	if err := fs.Parse(args); err != nil {
		return ExitValidation
	}

	// Reject unsupported --policy values before any I/O.
	if !validatePolicyFlag(f.policy, "apply", stderr) {
		return ExitValidation
	}

	if f.file == "" {
		fmt.Fprintln(stderr, "partictl apply: -f is required")
		fs.Usage()

		return ExitValidation
	}

	return runReconcile(reconcileParams{
		common:      f.common,
		file:        f.file,
		policyFlag:  f.policy,
		subcmd:      "apply",
		dryRun:      f.dryRun,
		jsonOut:     f.jsonOut,
		failOnDrift: f.failOnDrift,
	}, stdout, stderr)
}
