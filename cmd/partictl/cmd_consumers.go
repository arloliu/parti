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
	failOnDrift bool   // plan, or apply -dry-run
	policy      string // reconcile policy flag value; force enables consumer recreate
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
  -policy <p>     Reconcile policy: warn or force (default: warn or cfg.policy).
                  Precreation of missing consumers is policy-independent; -policy force
                  additionally enables delete/recreate repair of consumers with immutable drift.
  -json           Emit machine-readable JSON output
  -fail-on-drift  plan / apply -dry-run: exit 2 if non-informational drift is detected
  -dry-run        apply: compute the plan only — write nothing
  -server, -creds, -nkey, -token, -timeout   NATS connection flags

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
// sub-subcommands. force is the only policy that changes consumer behavior
// (it enables delete/recreate repair of consumers with immutable drift).
func bindConsumersCommonFlags(fs *flag.FlagSet, f *consumerParams) {
	fs.StringVar(&f.file, "f", "", "YAML config path (required)")
	fs.StringVar(&f.policy, "policy", "", "reconcile policy: force enables consumer recreate (default: warn or cfg.policy)")
	bindCommonFlags(fs, &f.common)
	fs.BoolVar(&f.jsonOut, "json", false, "emit JSON output")
}

// runConsumers loads the config, validates it, connects to NATS, and runs
// either a consumer plan or a consumer apply. It is the shared body of the
// consumers plan and apply sub-subcommands.
func runConsumers(p consumerParams, stdout, stderr io.Writer) int {
	// Validate the -policy flag before any I/O (mirrors how cmdPlan rejects a
	// bad -policy before any network I/O).
	if !validatePolicyFlag(p.policy, p.subcmd, stderr) {
		return ExitValidation
	}

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

	// Resolve the -policy flag against any YAML policy: field and stamp the
	// effective value into cfg.Policy before Validate (which defaults it).
	if !resolveAndStampPolicy(p.policy, p.subcmd, p.file, &cfg, stderr) {
		return ExitValidation
	}

	// consumers accepts only warn or force. adopt and safe-update are
	// meaningless for consumer precreation; reject them with a clear message.
	switch cfg.Policy {
	case provision.PolicyWarn, provision.PolicyForce:
		// accepted
	case provision.PolicyAdopt, provision.PolicySafeUpdate:
		fmt.Fprintf(stderr, "partictl %s: consumers accepts only warn or force for -policy; got %q\n",
			p.subcmd, cfg.Policy)

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

	conn, code := connectWithTimeout(timeoutDur, p.common, stderr)
	if code != ExitOK {
		return code
	}
	defer conn.close()

	planResult, err := provision.PlanConsumers(conn.ctx, conn.js, cfg)
	if err != nil {
		if ctxDead(conn.ctx) {
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

	report, err := provision.ApplyConsumers(conn.ctx, conn.js, planResult)
	emitReport(stdout, report, p.jsonOut)
	if err != nil {
		if ctxDead(conn.ctx) {
			return ExitNATS
		}

		return classifyError(err)
	}

	return ExitOK
}
