package main

import (
	"fmt"
	"io"

	"github.com/arloliu/parti/v2/provision"
)

// reconcileParams carries the resolved inputs for an apply-style run. Both
// the apply and adopt commands parse their own flag sets and then delegate
// to runReconcile — adopt is exactly "apply with policyFlag pinned to adopt".
type reconcileParams struct {
	common      commonFlags
	file        string // already checked non-empty by the caller
	policyFlag  string // the --policy value; adopt pins this to "adopt"
	subcmd      string // "apply" or "adopt" — error-message prefix
	dryRun      bool
	jsonOut     bool
	failOnDrift bool // dry-run only; adopt always false
}

// runReconcile loads the config, resolves the policy, validates, connects to
// NATS, and runs either a dry-run plan or a live apply. It is the shared body
// of the apply and adopt commands.
func runReconcile(p reconcileParams, stdout, stderr io.Writer) int {
	timeoutDur, err := parseTimeout(p.common.timeout)
	if err != nil {
		fmt.Fprintln(stderr, err)

		return ExitValidation
	}

	cfg, err := loadConfig(p.file)
	if err != nil {
		fmt.Fprintln(stderr, err)

		return ExitValidation
	}

	if !resolveAndStampPolicy(p.policyFlag, p.subcmd, p.file, &cfg, stderr) {
		return ExitValidation
	}

	// Static validation before any NATS connect so bad config exits 3.
	if err := provision.Validate(cfg); err != nil {
		fmt.Fprintln(stderr, err)

		return ExitValidation
	}

	ctx, stop, exitIfTimeout := makeContext(timeoutDur, stderr)
	defer stop()

	conn, code, err := connectNATS(ctx, p.common)
	if err != nil {
		fmt.Fprintln(stderr, err)
		if exitIfTimeout(ctx) {
			return ExitNATS
		}

		return code
	}
	defer conn.close()

	if p.dryRun {
		planResult, err := provision.Plan(ctx, conn.js, cfg)
		if err != nil {
			if exitIfTimeout(ctx) {
				return ExitNATS
			}
			fmt.Fprintf(stderr, "partictl %s -dry-run: %v\n", p.subcmd, err)

			return classifyError(err)
		}

		emitPlan(stdout, planResult, p.jsonOut)
		if p.failOnDrift && hasDrift(planResult.Drift) {
			return ExitDrift
		}

		return ExitOK
	}

	report, err := provision.Apply(ctx, conn.js, cfg)
	emitReport(stdout, report, p.jsonOut)
	if err != nil {
		if exitIfTimeout(ctx) {
			return ExitNATS
		}

		return classifyError(err)
	}

	return ExitOK
}
