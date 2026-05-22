package main

import (
	"context"
	"fmt"
	"io"
	"os/signal"
	"syscall"
	"time"
)

// run is the testable entrypoint for partictl. args should be the command-line
// arguments (excluding the program name), stdout and stderr are the output
// writers. Returns an exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return ExitValidation
	}

	subCmd := args[0]
	rest := args[1:]

	switch subCmd {
	case "view":
		return cmdView(rest, stdout, stderr)
	case "validate":
		return cmdValidate(rest, stdout, stderr)
	case "plan":
		return cmdPlan(rest, stdout, stderr)
	case "apply":
		return cmdApply(rest, stdout, stderr)
	case "adopt":
		return cmdAdopt(rest, stdout, stderr)
	case "partitions":
		return cmdPartitions(rest, stdout, stderr)
	case "consumers":
		return cmdConsumers(rest, stdout, stderr)
	case "stream":
		return cmdStream(rest, stdout, stderr)
	case "help", "-help", "--help", "-h":
		printUsage(stdout)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "partictl: unknown command %q\n\n", subCmd)
		printUsage(stderr)
		return ExitValidation
	}
}

// printUsage writes the top-level usage text.
func printUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `partictl — Parti environment provisioning CLI

Usage:
  partictl <command> [flags]

Commands:
  view        Show live Parti-managed resources (Snapshot)
  validate    Validate a config file (static; -live adds NATS preflight)
  plan        Compute desired-vs-live drift and proposed actions (PlanResult)
  apply       Create missing resources; -dry-run emits the same output as plan
  adopt       Stamp Parti ownership marker on unmarked resources (shorthand for apply --policy=adopt)
  partitions  Plan/apply the partition-source key contents (partitions plan|apply)
  consumers   Precreate per-partition durable consumers (consumers plan|apply)
  stream      Manage application JetStream streams (stream view|plan|apply)

Common flags (accepted by all commands):
  -server   <url>       NATS server URL (default: $NATS_URL or nats://127.0.0.1:4222)
  -creds    <path>      Path to NATS credentials file
  -nkey     <path>      Path to NATS nkey seed file
  -token    <string>    NATS token
  -timeout  <duration>  Operation timeout (default: 30s)
  -f        <path>      YAML config file (required for validate/plan/apply/adopt/partitions/consumers; optional for view)
  -json                 Emit machine-readable JSON output
  -instance <name>      Filter by parti.io/instance (view without -f; validate)
  -policy   <policy>    Reconcile policy for plan/apply/adopt/stream/consumers: warn, adopt, safe-update, force (default: warn).
                        force is the destructive policy — it can delete/recreate resources.
                        partitions does not accept -policy; consumers accepts only warn or force.

Deferred commands (not yet supported):
  init, emit

Exit codes:
  0  success
  1  runtime / generic error
  2  drift detected (-fail-on-drift)
  3  config parse or validation error
  4  NATS connect / auth / timeout failure

Run 'partictl <command> -help' for per-command flags.
`)
}

// parseTimeout parses a duration string supplied via -timeout. Returns an
// error if the string is empty, syntactically invalid, or non-positive.
// Callers should treat any error as a flag-parse error (ExitValidation).
func parseTimeout(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("partictl: invalid -timeout value %q: %w", s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("partictl: -timeout must be positive, got %q", s)
	}

	return d, nil
}

// makeContext creates a context with the signal-cancel wrapper on top of a
// timeout. The returned stop function must be deferred by the caller.
func makeContext(d time.Duration) (context.Context, context.CancelFunc) {
	// Signal-cancellable root context.
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	ctx, cancel := context.WithTimeout(sigCtx, d)

	combinedStop := func() {
		cancel()
		stop()
	}

	return ctx, combinedStop
}

// ctxDead reports whether ctx has expired due to a timeout or a received
// signal. Commands use it to map a downstream error to ExitNATS.
func ctxDead(ctx context.Context) bool {
	return ctx.Err() != nil
}
