package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/arloliu/parti/v2/provision"
)

// planFlags holds flags specific to the plan command.
type planFlags struct {
	common      commonFlags
	file        string
	policy      string
	jsonOut     bool
	failOnDrift bool
}

func cmdPlan(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var f planFlags
	fs.StringVar(&f.file, "f", "", "YAML config path (required)")
	fs.StringVar(&f.policy, "policy", "", "reconcile policy: warn, adopt, safe-update, force (default: warn or cfg.policy)")
	bindCommonFlags(fs, &f.common)
	fs.BoolVar(&f.jsonOut, "json", false, "emit JSON output (PlanResult)")
	fs.BoolVar(&f.failOnDrift, "fail-on-drift", false, "exit 2 if non-informational drift is detected")

	if err := fs.Parse(args); err != nil {
		return ExitValidation
	}

	// Reject unsupported --policy values before any I/O.
	if !validatePolicyFlag(f.policy, "plan", stderr) {
		return ExitValidation
	}

	// Validate -timeout before any NATS connect so invalid values exit 3.
	timeoutDur, err := parseTimeout(f.common.timeout)
	if err != nil {
		fmt.Fprintln(stderr, err)

		return ExitValidation
	}

	if f.file == "" {
		fmt.Fprintln(stderr, "partictl plan: -f is required")
		fs.Usage()

		return ExitValidation
	}

	cfg, err := loadConfig(f.file)
	if err != nil {
		fmt.Fprintln(stderr, err)

		return ExitValidation
	}

	// Resolve flag vs YAML policy. policy: "" in YAML is treated as absent.
	if !resolveAndStampPolicy(f.policy, "plan", f.file, &cfg, stderr) {
		return ExitValidation
	}

	// Static validation first (no I/O). provision.Plan also validates, but
	// we want the parse-error → exit 3 short-circuit before any NATS connect.
	if err := provision.Validate(cfg); err != nil {
		fmt.Fprintln(stderr, err)

		return ExitValidation
	}

	conn, code := connectWithTimeout(timeoutDur, f.common, stderr)
	if code != ExitOK {
		return code
	}
	defer conn.close()

	planResult, err := provision.Plan(conn.ctx, conn.js, cfg)
	if err != nil {
		if ctxDead(conn.ctx) {
			return ExitNATS
		}
		fmt.Fprintln(stderr, "partictl plan:", err)

		return classifyError(err)
	}

	emitPlan(stdout, planResult, f.jsonOut)

	if f.failOnDrift && hasDrift(planResult.Drift) {
		return ExitDrift
	}

	return ExitOK
}
