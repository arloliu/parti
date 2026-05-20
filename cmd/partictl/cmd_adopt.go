package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/arloliu/parti/v2/provision"
)

// adoptFlags holds flags specific to the adopt command.
type adoptFlags struct {
	common  commonFlags
	file    string
	dryRun  bool
	jsonOut bool
}

// cmdAdopt is shorthand for "apply --policy=adopt [-dry-run]". With -dry-run it
// emits the same PlanResult output as "plan --policy=adopt"; without it the
// command performs a live Apply under PolicyAdopt.
//
// If cfg.policy in the YAML is non-empty and differs from "adopt" the conflict
// check fires (exit 3). policy: "" or absent in YAML proceeds normally.
func cmdAdopt(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("adopt", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var f adoptFlags
	fs.StringVar(&f.file, "f", "", "YAML config path (required)")
	fs.StringVar(&f.common.server, "server", defaultServer(), "NATS server URL")
	fs.StringVar(&f.common.creds, "creds", "", "path to NATS credentials file")
	fs.StringVar(&f.common.nkey, "nkey", "", "path to NATS nkey seed file")
	fs.StringVar(&f.common.token, "token", "", "NATS token")
	fs.StringVar(&f.common.timeout, "timeout", "30s", "operation timeout (e.g. 30s, 1m)")
	fs.BoolVar(&f.dryRun, "dry-run", false, "plan only — emit the same output as plan --policy=adopt")
	fs.BoolVar(&f.jsonOut, "json", false, "emit JSON output (PlanResult for -dry-run, Report otherwise)")

	if err := fs.Parse(args); err != nil {
		return ExitValidation
	}

	if f.file == "" {
		fmt.Fprintln(stderr, "partictl adopt: -f is required")
		fs.Usage()

		return ExitValidation
	}

	return runReconcile(reconcileParams{
		common:     f.common,
		file:       f.file,
		policyFlag: string(provision.PolicyAdopt),
		subcmd:     "adopt",
		dryRun:     f.dryRun,
		jsonOut:    f.jsonOut,
	}, stdout, stderr)
}
