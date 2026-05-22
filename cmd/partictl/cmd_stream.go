package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/arloliu/parti/v2/provision"
)

// streamViewParams carries the resolved inputs for a `stream view` run.
type streamViewParams struct {
	common   commonFlags
	file     string
	instance string
	jsonOut  bool
}

// streamPlanApplyParams carries the resolved inputs for a `stream plan` or
// `stream apply` run.
type streamPlanApplyParams struct {
	common      commonFlags
	file        string
	policy      string
	subcmd      string // "stream plan" or "stream apply" — for error messages
	apply       bool   // false = plan, true = apply
	dryRun      bool   // apply only
	jsonOut     bool
	failOnDrift bool // plan only
}

// cmdStream dispatches the `stream` sub-subcommands.
func cmdStream(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "partictl stream: a subcommand is required (view, plan, or apply)")
		printStreamUsage(stderr)

		return ExitValidation
	}

	rest := args[1:]
	switch args[0] {
	case "view":
		return cmdStreamView(rest, stdout, stderr)
	case "plan":
		return cmdStreamPlan(rest, stdout, stderr)
	case "apply":
		return cmdStreamApply(rest, stdout, stderr)
	case "help", "-help", "--help", "-h":
		printStreamUsage(stdout)

		return ExitOK
	default:
		fmt.Fprintf(stderr, "partictl stream: unknown subcommand %q\n\n", args[0])
		printStreamUsage(stderr)

		return ExitValidation
	}
}

// printStreamUsage writes the `stream` command help text.
func printStreamUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `partictl stream — manage application JetStream streams

Usage:
  partictl stream view  [-f <config>] [flags]
  partictl stream plan   -f <config>  [flags]
  partictl stream apply  -f <config>  [flags]

Subcommands:
  view   Show live Parti-marked application streams (instance-scoped inventory).
  plan   Compute desired-vs-live drift and proposed stream actions (PlanResult).
  apply  Create or update declared streams; -dry-run emits the same output as plan.

Flags:
  -f <path>       YAML config file (required for plan/apply; optional for view)
  -json           Emit machine-readable JSON output
  -instance <n>   Filter by parti.io/instance (view without -f only)
  -fail-on-drift  plan: exit 2 if non-informational drift is detected
  -dry-run        apply: compute the plan only — write nothing
  -policy <p>     Reconcile policy: warn, adopt, safe-update (default: warn or cfg.policy)
  -server, -creds, -nkey, -token, -timeout   NATS connection flags
`)
}

func cmdStreamView(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stream view", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var p streamViewParams
	fs.StringVar(&p.file, "f", "", "YAML config path (optional; scopes view to config instance)")
	fs.StringVar(&p.common.server, "server", defaultServer(), "NATS server URL")
	fs.StringVar(&p.common.creds, "creds", "", "path to NATS credentials file")
	fs.StringVar(&p.common.nkey, "nkey", "", "path to NATS nkey seed file")
	fs.StringVar(&p.common.token, "token", "", "NATS token")
	fs.StringVar(&p.common.timeout, "timeout", "30s", "operation timeout (e.g. 30s, 1m)")
	fs.BoolVar(&p.jsonOut, "json", false, "emit JSON output (Snapshot)")
	fs.StringVar(&p.instance, "instance", "", "restrict results to resources with matching parti.io/instance")

	if err := fs.Parse(args); err != nil {
		return ExitValidation
	}

	return runStreamView(p, stdout, stderr)
}

func cmdStreamPlan(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stream plan", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var p streamPlanApplyParams
	p.subcmd = "stream plan"
	bindStreamPlanApplyFlags(fs, &p)
	fs.BoolVar(&p.failOnDrift, "fail-on-drift", false, "exit 2 if non-informational drift is detected")

	if err := fs.Parse(args); err != nil {
		return ExitValidation
	}

	if !validatePolicyFlag(p.policy, "stream plan", stderr) {
		return ExitValidation
	}

	if p.file == "" {
		fmt.Fprintln(stderr, "partictl stream plan: -f is required")
		fs.Usage()

		return ExitValidation
	}

	return runStreamPlanApply(p, stdout, stderr)
}

func cmdStreamApply(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stream apply", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var p streamPlanApplyParams
	p.subcmd = "stream apply"
	p.apply = true
	bindStreamPlanApplyFlags(fs, &p)
	fs.BoolVar(&p.dryRun, "dry-run", false, "compute the plan only — write nothing")

	if err := fs.Parse(args); err != nil {
		return ExitValidation
	}

	if !validatePolicyFlag(p.policy, "stream apply", stderr) {
		return ExitValidation
	}

	if p.file == "" {
		fmt.Fprintln(stderr, "partictl stream apply: -f is required")
		fs.Usage()

		return ExitValidation
	}

	return runStreamPlanApply(p, stdout, stderr)
}

// bindStreamPlanApplyFlags registers the flags shared by both stream
// plan and stream apply sub-subcommands.
func bindStreamPlanApplyFlags(fs *flag.FlagSet, p *streamPlanApplyParams) {
	fs.StringVar(&p.file, "f", "", "YAML config path (required)")
	fs.StringVar(&p.policy, "policy", "", "reconcile policy: warn, adopt, safe-update (default: warn or cfg.policy)")
	fs.StringVar(&p.common.server, "server", defaultServer(), "NATS server URL")
	fs.StringVar(&p.common.creds, "creds", "", "path to NATS credentials file")
	fs.StringVar(&p.common.nkey, "nkey", "", "path to NATS nkey seed file")
	fs.StringVar(&p.common.token, "token", "", "NATS token")
	fs.StringVar(&p.common.timeout, "timeout", "30s", "operation timeout (e.g. 30s, 1m)")
	fs.BoolVar(&p.jsonOut, "json", false, "emit JSON output")
}

// runStreamView handles the `stream view` command. When -f is absent it uses
// inventory mode (Scope{Streams: true} + instance filter from -instance).
// When -f is given it loads the config, validates the full config, and derives
// the instance filter from cfg.Instance (-instance is ignored with a warning).
func runStreamView(p streamViewParams, stdout, stderr io.Writer) int {
	timeoutDur, err := parseTimeout(p.common.timeout)
	if err != nil {
		fmt.Fprintln(stderr, err)

		return ExitValidation
	}

	ctx, stop, exitIfTimeout := makeContext(timeoutDur, stderr)
	defer stop()

	// Build the stream-only scope: always Streams: true.
	var scope provision.Scope
	if p.file != "" {
		// Step 1: load the full config.
		cfg, err := loadConfig(p.file)
		if err != nil {
			fmt.Fprintln(stderr, err)

			return ExitValidation
		}
		// Warn when -instance is also set: the config's Instance wins.
		if p.instance != "" {
			fmt.Fprintln(stderr, "partictl: -instance ignored because -f scopes resources from the config file")
		}
		// Step 3: validate the full config (not the stream-only view).
		if err := provision.Validate(cfg); err != nil {
			fmt.Fprintln(stderr, err)

			return ExitValidation
		}
		scope = provision.Scope{Streams: true, Instance: cfg.Instance}
	} else {
		// Inventory mode: no config, instance filter from -instance flag.
		scope = provision.Scope{Streams: true, Instance: p.instance}
	}

	conn, code, err := connectNATS(ctx, p.common)
	if err != nil {
		fmt.Fprintln(stderr, err)
		if exitIfTimeout(ctx) {
			return ExitNATS
		}

		return code
	}
	defer conn.close()

	snap, err := provision.View(ctx, conn.js, scope)
	if err != nil {
		if exitIfTimeout(ctx) {
			return ExitNATS
		}
		fmt.Fprintln(stderr, "partictl stream view:", err)

		return classifyError(err)
	}

	if p.jsonOut {
		_ = jsonOutput(stdout, snap)
	} else {
		renderSnapshotText(stdout, snap)
	}

	return ExitOK
}

// runStreamPlanApply implements the 5-step command sequence for `stream plan`
// and `stream apply`:
//  1. Load the full config from -f.
//  2. Apply the -policy override (plan/apply only).
//  3. Validate the FULL loaded config so a malformed non-stream section exits 3.
//  4. Derive the stream-only config view (nil ControlPlane/PartitionSource,
//     cleared DynamicConsumers).
//  5. Connect to NATS and run provision.Plan or provision.Apply against the
//     derived view.
func runStreamPlanApply(p streamPlanApplyParams, stdout, stderr io.Writer) int {
	timeoutDur, err := parseTimeout(p.common.timeout)
	if err != nil {
		fmt.Fprintln(stderr, err)

		return ExitValidation
	}

	// Step 1: load the full config.
	cfg, err := loadConfig(p.file)
	if err != nil {
		fmt.Fprintln(stderr, err)

		return ExitValidation
	}

	// Step 2: apply the -policy flag override.
	if !resolveAndStampPolicy(p.policy, p.subcmd, p.file, &cfg, stderr) {
		return ExitValidation
	}

	// Step 3: validate the full config — a malformed controlPlane or
	// partitionSource section must exit 3 even under `stream` commands.
	if err := provision.Validate(cfg); err != nil {
		fmt.Fprintln(stderr, err)

		return ExitValidation
	}

	// Step 4: derive the stream-only config view.
	streamOnly := cfg
	streamOnly.ControlPlane = nil
	streamOnly.PartitionSource = nil
	streamOnly.DynamicConsumers = nil

	ctx, stop, exitIfTimeout := makeContext(timeoutDur, stderr)
	defer stop()

	// Step 5: connect to NATS and run provision.Plan or provision.Apply.
	conn, code, err := connectNATS(ctx, p.common)
	if err != nil {
		fmt.Fprintln(stderr, err)
		if exitIfTimeout(ctx) {
			return ExitNATS
		}

		return code
	}
	defer conn.close()

	// plan, or apply -dry-run: emit the plan and write nothing.
	if !p.apply || p.dryRun {
		planResult, err := provision.Plan(ctx, conn.js, streamOnly)
		if err != nil {
			if exitIfTimeout(ctx) {
				return ExitNATS
			}
			fmt.Fprintf(stderr, "partictl %s: %v\n", p.subcmd, err)

			return classifyError(err)
		}

		emitPlan(stdout, planResult, p.jsonOut)
		if p.failOnDrift && hasDrift(planResult.Drift) {
			return ExitDrift
		}

		return ExitOK
	}

	report, err := provision.Apply(ctx, conn.js, streamOnly)
	emitReport(stdout, report, p.jsonOut)
	if err != nil {
		if exitIfTimeout(ctx) {
			return ExitNATS
		}

		return classifyError(err)
	}

	return ExitOK
}
