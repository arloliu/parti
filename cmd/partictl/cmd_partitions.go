package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/arloliu/parti/v2/provision"
)

// partitionParams carries the resolved inputs for a `partitions` run. The plan
// and apply sub-subcommands parse their own flag sets and delegate to
// runPartitions.
type partitionParams struct {
	common      commonFlags
	file        string
	subcmd      string // "partitions plan" / "partitions apply" — message prefix
	apply       bool   // false = plan, true = apply
	prune       bool   // apply only
	dryRun      bool   // apply only
	jsonOut     bool
	failOnDrift bool // plan, or apply -dry-run
}

// cmdPartitions dispatches the `partitions` sub-subcommands. The partitions
// command manages the contents of the partition-source key (the partition
// table) — distinct from the bucket-provisioning commands, which manage the
// bucket config.
func cmdPartitions(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "partictl partitions: a subcommand is required (plan or apply)")
		printPartitionsUsage(stderr)

		return ExitValidation
	}

	rest := args[1:]
	switch args[0] {
	case "plan":
		return cmdPartitionsPlan(rest, stdout, stderr)
	case "apply":
		return cmdPartitionsApply(rest, stdout, stderr)
	case "help", "-help", "--help", "-h":
		printPartitionsUsage(stdout)

		return ExitOK
	default:
		fmt.Fprintf(stderr, "partictl partitions: unknown subcommand %q\n\n", args[0])
		printPartitionsUsage(stderr)

		return ExitValidation
	}
}

// printPartitionsUsage writes the `partitions` command help text.
func printPartitionsUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `partictl partitions — manage partition-source key contents

Usage:
  partictl partitions plan  -f <config> [flags]
  partictl partitions apply -f <config> [flags]

Subcommands:
  plan   Compute the record-level diff between partitionSource.partitions
         and the live partition-source key (PlanResult).
  apply  Write the declared partition table to the partition-source key.

Flags:
  -f <path>       YAML config file (required)
  -json           Emit machine-readable JSON output
  -fail-on-drift  plan / apply -dry-run: exit 2 if a record-level diff exists
  -prune          apply: allow removing records absent from the declared set
  -dry-run        apply: compute the plan only — write nothing
  -server, -creds, -nkey, -token, -timeout   NATS connection flags

The reconcile policy (warn / adopt / safe-update) does not govern partitions;
record-removal safety is the -prune flag alone.
`)
}

func cmdPartitionsPlan(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("partitions plan", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var f partitionParams
	f.subcmd = "partitions plan"
	bindPartitionCommonFlags(fs, &f)
	fs.BoolVar(&f.failOnDrift, "fail-on-drift", false, "exit 2 if a record-level diff is detected")

	if err := fs.Parse(args); err != nil {
		return ExitValidation
	}
	if f.file == "" {
		fmt.Fprintln(stderr, "partictl partitions plan: -f is required")
		fs.Usage()

		return ExitValidation
	}

	return runPartitions(f, stdout, stderr)
}

func cmdPartitionsApply(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("partitions apply", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var f partitionParams
	f.subcmd = "partitions apply"
	f.apply = true
	bindPartitionCommonFlags(fs, &f)
	fs.BoolVar(&f.prune, "prune", false, "allow removing records absent from the declared set")
	fs.BoolVar(&f.dryRun, "dry-run", false, "compute the plan only — write nothing")
	fs.BoolVar(&f.failOnDrift, "fail-on-drift", false, "exit 2 if a record-level diff is detected (dry-run only)")

	if err := fs.Parse(args); err != nil {
		return ExitValidation
	}
	if f.file == "" {
		fmt.Fprintln(stderr, "partictl partitions apply: -f is required")
		fs.Usage()

		return ExitValidation
	}

	return runPartitions(f, stdout, stderr)
}

// bindPartitionCommonFlags registers the flags shared by both partitions
// sub-subcommands. Note -policy is intentionally absent: the reconcile policy
// does not govern record-level reconciliation.
func bindPartitionCommonFlags(fs *flag.FlagSet, f *partitionParams) {
	fs.StringVar(&f.file, "f", "", "YAML config path (required)")
	bindCommonFlags(fs, &f.common)
	fs.BoolVar(&f.jsonOut, "json", false, "emit JSON output")
}

// runPartitions loads the config, validates it, connects to NATS, and runs
// either a record-level plan or a record-level apply. It is the shared body of
// the partitions plan and apply sub-subcommands.
func runPartitions(p partitionParams, stdout, stderr io.Writer) int {
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
	// 3 — even when NATS is also unreachable. PlanPartitions re-runs this same
	// boundary (Validate plus the partition-set checks); duplicating it here
	// short-circuits before a connection is opened, mirroring how cmd_plan.go
	// pre-validates ahead of connectNATS.
	if err := provision.Validate(cfg); err != nil {
		fmt.Fprintln(stderr, err)

		return ExitValidation
	}
	if cfg.PartitionSource == nil {
		fmt.Fprintf(stderr, "partictl %s: config declares no partitionSource\n", p.subcmd)

		return ExitValidation
	}
	if err := provision.ValidatePartitionSet(cfg.PartitionSource.Partitions); err != nil {
		fmt.Fprintln(stderr, err)

		return ExitValidation
	}

	conn, code := connectWithTimeout(timeoutDur, p.common, stderr)
	if code != ExitOK {
		return code
	}
	defer conn.close()

	planResult, err := provision.PlanPartitions(conn.ctx, conn.js, cfg)
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

	report, err := provision.ApplyPartitions(conn.ctx, conn.js, planResult, p.prune)
	emitReport(stdout, report, p.jsonOut)
	if err != nil {
		if ctxDead(conn.ctx) {
			return ExitNATS
		}

		return classifyError(err)
	}

	return ExitOK
}
