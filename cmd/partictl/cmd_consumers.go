package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/arloliu/parti/v2/provision"
)

// consumerParams carries the resolved inputs for a `consumers` run. The plan
// and apply sub-subcommands parse their own flag sets and delegate to
// runConsumers.
type consumerParams struct {
	common      commonFlags
	file        string
	subcmd      string // "consumers plan" / "consumers apply" — message prefix
	apply       bool   // false = plan, true = apply
	dryRun      bool   // apply only
	jsonOut     bool
	failOnDrift bool // plan, or apply -dry-run
}

// cmdConsumers dispatches the `consumers` sub-subcommands. The consumers
// command manages per-partition durable consumers for opted-in
// dynamicConsumers targets — distinct from the alignment-check path, which is
// handled by the top-level plan/apply commands.
func cmdConsumers(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "partictl consumers: a subcommand is required (plan or apply)")
		printConsumersUsage(stderr)

		return ExitValidation
	}

	rest := args[1:]
	switch args[0] {
	case "plan":
		return cmdConsumersPlan(rest, stdout, stderr)
	case "apply":
		return cmdConsumersApply(rest, stdout, stderr)
	case "help", "-help", "--help", "-h":
		printConsumersUsage(stdout)

		return ExitOK
	default:
		fmt.Fprintf(stderr, "partictl consumers: unknown subcommand %q\n\n", args[0])
		printConsumersUsage(stderr)

		return ExitValidation
	}
}

// printConsumersUsage writes the `consumers` command help text.
func printConsumersUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `partictl consumers — precreate per-partition durable consumers

Usage:
  partictl consumers plan  -f <config> [flags]
  partictl consumers apply -f <config> [flags]

Subcommands:
  plan   Compute which per-partition durable consumers are missing (PlanResult).
  apply  Create missing per-partition durable consumers.

Flags:
  -f <path>       YAML config file (required)
  -json           Emit machine-readable JSON output
  -fail-on-drift  plan / apply -dry-run: exit 2 if non-informational drift is detected
  -dry-run        apply: compute the plan only — write nothing
  -server, -creds, -nkey, -token, -timeout   NATS connection flags

The reconcile policy does not govern consumers; precreation is policy-independent.
A dynamicConsumers target must set partitionsRef to opt into precreation.
`)
}

func cmdConsumersPlan(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("consumers plan", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var f consumerParams
	f.subcmd = "consumers plan"
	bindConsumersCommonFlags(fs, &f)
	fs.BoolVar(&f.failOnDrift, "fail-on-drift", false, "exit 2 if non-informational drift is detected")

	if err := fs.Parse(args); err != nil {
		return ExitValidation
	}
	if f.file == "" {
		fmt.Fprintln(stderr, "partictl consumers plan: -f is required")
		fs.Usage()

		return ExitValidation
	}

	return runConsumers(f, stdout, stderr)
}

func cmdConsumersApply(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("consumers apply", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var f consumerParams
	f.subcmd = "consumers apply"
	f.apply = true
	bindConsumersCommonFlags(fs, &f)
	fs.BoolVar(&f.dryRun, "dry-run", false, "compute the plan only — write nothing")
	fs.BoolVar(&f.failOnDrift, "fail-on-drift", false, "exit 2 if non-informational drift is detected (dry-run only)")

	if err := fs.Parse(args); err != nil {
		return ExitValidation
	}
	if f.file == "" {
		fmt.Fprintln(stderr, "partictl consumers apply: -f is required")
		fs.Usage()

		return ExitValidation
	}

	return runConsumers(f, stdout, stderr)
}

// bindConsumersCommonFlags registers the flags shared by both consumers
// sub-subcommands. Note -policy is intentionally absent: precreation is
// policy-independent (a consumer either exists or is created; there is no
// warn/safe-update/adopt distinction for it).
func bindConsumersCommonFlags(fs *flag.FlagSet, f *consumerParams) {
	fs.StringVar(&f.file, "f", "", "YAML config path (required)")
	fs.StringVar(&f.common.server, "server", defaultServer(), "NATS server URL")
	fs.StringVar(&f.common.creds, "creds", "", "path to NATS credentials file")
	fs.StringVar(&f.common.nkey, "nkey", "", "path to NATS nkey seed file")
	fs.StringVar(&f.common.token, "token", "", "NATS token")
	fs.StringVar(&f.common.timeout, "timeout", "30s", "operation timeout (e.g. 30s, 1m)")
	fs.BoolVar(&f.jsonOut, "json", false, "emit JSON output")
}

// runConsumers loads the config, validates it, connects to NATS, and runs
// either a consumer plan or a consumer apply. It is the shared body of the
// consumers plan and apply sub-subcommands.
func runConsumers(p consumerParams, stdout, stderr io.Writer) int {
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

	// Static validation before any NATS connect so a bad config always exits
	// 3 — even when NATS is also unreachable. PlanConsumers re-runs this same
	// boundary (Validate plus the consumer-set checks); duplicating it here
	// short-circuits before a connection is opened, mirroring how cmd_plan.go
	// pre-validates ahead of connectNATS.
	if err := provision.Validate(cfg); err != nil {
		fmt.Fprintln(stderr, err)

		return ExitValidation
	}
	if err := provision.ValidateConsumerSet(cfg); err != nil {
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

	planResult, err := provision.PlanConsumers(ctx, conn.js, cfg)
	if err != nil {
		if exitIfTimeout(ctx) {
			return ExitNATS
		}
		fmt.Fprintf(stderr, "partictl %s: %v\n", p.subcmd, err)

		return classifyError(err)
	}

	// plan, or apply -dry-run: emit the plan and write nothing.
	if !p.apply || p.dryRun {
		emitPlan(stdout, planResult, p.jsonOut)
		if p.failOnDrift && hasDrift(planResult.Drift) {
			return ExitDrift
		}

		return ExitOK
	}

	report, err := provision.ApplyConsumers(ctx, conn.js, planResult)
	emitReport(stdout, report, p.jsonOut)
	if err != nil {
		if exitIfTimeout(ctx) {
			return ExitNATS
		}

		return classifyError(err)
	}

	return ExitOK
}
