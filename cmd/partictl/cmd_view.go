package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/arloliu/parti/v2/provision"
)

// viewFlags holds flags specific to the view command.
type viewFlags struct {
	common   commonFlags
	file     string
	jsonOut  bool
	instance string
}

func cmdView(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("view", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var f viewFlags
	fs.StringVar(&f.file, "f", "", "YAML config path (optional; scopes view to config resources)")
	bindCommonFlags(fs, &f.common)
	fs.BoolVar(&f.jsonOut, "json", false, "emit JSON output (Snapshot)")
	fs.StringVar(&f.instance, "instance", "", "restrict results to resources with matching parti.io/instance")

	if err := fs.Parse(args); err != nil {
		return ExitValidation
	}

	// Validate -timeout before any NATS connect so invalid values exit 3.
	timeoutDur, err := parseTimeout(f.common.timeout)
	if err != nil {
		fmt.Fprintln(stderr, err)

		return ExitValidation
	}

	// Create the operation context before scope derivation so a config load
	// and static validation count against -timeout. connectNATS needs no
	// scope, but the context must precede the load, so this command opens the
	// connection itself rather than fusing it with makeContext.
	ctx, stop := makeContext(timeoutDur)
	defer stop()

	// Determine scope: config-scoped (with -f) or inventory (without -f).
	var scope provision.Scope
	if f.file != "" {
		// Warn when -instance is also set: -f scopes resources from the config
		// and the -instance flag is silently ignored.
		if f.instance != "" {
			fmt.Fprintln(stderr, "partictl: -instance ignored because -f scopes resources from the config file")
		}
		cfg, err := loadConfig(f.file)
		if err != nil {
			fmt.Fprintln(stderr, err)

			return ExitValidation
		}
		// Static validate to catch obviously bad configs early; view still
		// functions with a partial config, but we want the user to fix it.
		if err := provision.Validate(cfg); err != nil {
			fmt.Fprintln(stderr, err)

			return ExitValidation
		}
		scope = provision.ScopeFromConfig(cfg)
		// When -f is set the config's Instance wins; -instance flag is ignored.
	} else {
		scope = provision.ScopeAll()
		scope.Instance = f.instance
	}

	conn, code, err := connectNATS(ctx, f.common)
	if err != nil {
		fmt.Fprintln(stderr, err)
		if ctxDead(ctx) {
			return ExitNATS
		}

		return code
	}
	defer conn.close()

	snap, err := provision.View(ctx, conn.js, scope)
	if err != nil {
		if ctxDead(ctx) {
			return ExitNATS
		}
		fmt.Fprintln(stderr, "partictl view:", err)

		return classifyError(err)
	}

	if f.jsonOut {
		_ = jsonOutput(stdout, snap)
	} else {
		renderSnapshotText(stdout, snap)
	}

	return ExitOK
}
