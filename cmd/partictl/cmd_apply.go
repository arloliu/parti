package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/arloliu/parti/v2/provision"
)

// applyFlags holds flags specific to the apply command.
type applyFlags struct {
	common      commonFlags
	file        string
	dryRun      bool
	jsonOut     bool
	failOnDrift bool
}

func cmdApply(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var f applyFlags
	fs.StringVar(&f.file, "f", "", "YAML config path (required)")
	fs.StringVar(&f.common.server, "server", defaultServer(), "NATS server URL")
	fs.StringVar(&f.common.creds, "creds", "", "path to NATS credentials file")
	fs.StringVar(&f.common.nkey, "nkey", "", "path to NATS nkey seed file")
	fs.StringVar(&f.common.token, "token", "", "NATS token")
	fs.StringVar(&f.common.timeout, "timeout", "30s", "operation timeout (e.g. 30s, 1m)")
	fs.BoolVar(&f.dryRun, "dry-run", false, "plan only — emit the same output as partictl plan")
	fs.BoolVar(&f.jsonOut, "json", false, "emit JSON output (PlanResult for -dry-run, Report otherwise)")
	fs.BoolVar(&f.failOnDrift, "fail-on-drift", false, "exit 2 if non-informational drift is detected (dry-run only)")

	if err := fs.Parse(args); err != nil {
		return ExitValidation
	}

	// Validate -timeout before any NATS connect so invalid values exit 3.
	timeoutDur, err := parseTimeout(f.common.timeout)
	if err != nil {
		fmt.Fprintln(stderr, err)

		return ExitValidation
	}

	if f.file == "" {
		fmt.Fprintln(stderr, "partictl apply: -f is required")
		fs.Usage()

		return ExitValidation
	}

	cfg, err := loadConfig(f.file)
	if err != nil {
		fmt.Fprintln(stderr, err)

		return ExitValidation
	}

	// Static validation before any NATS connect.
	if err := provision.Validate(cfg); err != nil {
		fmt.Fprintln(stderr, err)

		return ExitValidation
	}

	ctx, stop, exitIfTimeout := makeContext(timeoutDur, stderr)
	defer stop()

	conn, code, err := connectNATS(ctx, f.common)
	if err != nil {
		fmt.Fprintln(stderr, err)
		if exitIfTimeout(ctx) {
			return ExitNATS
		}

		return code
	}
	defer conn.close()

	// -dry-run: runs the same planning path as cmdPlan and emits identical output.
	if f.dryRun {
		planResult, err := provision.Plan(ctx, conn.js, cfg)
		if err != nil {
			if exitIfTimeout(ctx) {
				return ExitNATS
			}
			fmt.Fprintln(stderr, "partictl apply -dry-run:", err)

			return classifyError(err)
		}

		if f.jsonOut {
			_ = jsonOutput(stdout, planResult)
		} else {
			renderPlanText(stdout, planResult)
		}

		if f.failOnDrift && hasDrift(planResult.Drift) {
			return ExitDrift
		}

		return ExitOK
	}

	// Live apply.
	report, err := provision.Apply(ctx, conn.js, cfg)
	if err != nil {
		if exitIfTimeout(ctx) {
			if f.jsonOut {
				_ = jsonOutput(stdout, report)
			} else {
				renderReportText(stdout, report)
			}

			return ExitNATS
		}
		if f.jsonOut {
			_ = jsonOutput(stdout, report)
		} else {
			renderReportText(stdout, report)
		}

		return classifyError(err)
	}

	if f.jsonOut {
		_ = jsonOutput(stdout, report)
	} else {
		renderReportText(stdout, report)
	}

	return ExitOK
}
