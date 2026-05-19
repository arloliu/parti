package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/arloliu/parti/v2/provision"
)

// validateFlags holds flags specific to the validate command.
type validateFlags struct {
	common  commonFlags
	file    string
	live    bool
	jsonOut bool
	// instance is accepted for CLI flag consistency with view but not used:
	// validate is config-scoped, not live-resource-scoped.
	instance string
}

func cmdValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var f validateFlags
	fs.StringVar(&f.file, "f", "", "YAML config path (required)")
	fs.StringVar(&f.common.server, "server", defaultServer(), "NATS server URL")
	fs.StringVar(&f.common.creds, "creds", "", "path to NATS credentials file")
	fs.StringVar(&f.common.nkey, "nkey", "", "path to NATS nkey seed file")
	fs.StringVar(&f.common.token, "token", "", "NATS token")
	fs.StringVar(&f.common.timeout, "timeout", "30s", "operation timeout (e.g. 30s, 1m)")
	fs.BoolVar(&f.live, "live", false, "perform live validation (requires NATS connectivity)")
	fs.BoolVar(&f.jsonOut, "json", false, "emit JSON output (Report-shaped envelope)")
	fs.StringVar(&f.instance, "instance", "", "restrict results to resources with matching parti.io/instance")

	if err := fs.Parse(args); err != nil {
		// flag.ContinueOnError already wrote the error message to stderr.
		return ExitValidation
	}

	// Validate -timeout before any NATS connect so invalid values exit 3 even
	// on the static (non-live) path.
	timeoutDur, err := parseTimeout(f.common.timeout)
	if err != nil {
		fmt.Fprintln(stderr, err)

		return ExitValidation
	}

	// Warn when -instance is set alongside -f: validate is config-scoped and
	// -instance has no effect.
	if f.instance != "" && f.file != "" {
		fmt.Fprintln(stderr, "partictl: -instance ignored because -f scopes resources from the config file")
	}

	if f.file == "" {
		fmt.Fprintln(stderr, "partictl validate: -f is required")
		fs.Usage()

		return ExitValidation
	}

	// Load YAML config.
	cfg, err := loadConfig(f.file)
	if err != nil {
		fmt.Fprintln(stderr, err)

		return ExitValidation
	}

	// Static validation (no I/O).
	if err := provision.Validate(cfg); err != nil {
		if f.jsonOut {
			_ = jsonOutput(stdout, validateResultJSON(false, []string{err.Error()}))
		} else {
			renderValidateText(stdout, false, []string{err.Error()})
		}

		return ExitValidation
	}

	// Non-live path: report OK and return.
	if !f.live {
		if f.jsonOut {
			_ = jsonOutput(stdout, validateResultJSON(true, nil))
		} else {
			renderValidateText(stdout, true, nil)
		}

		return ExitOK
	}

	// Live validation path.
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

	report, err := provision.ValidateLive(ctx, conn.js, cfg)
	if err != nil {
		if exitIfTimeout(ctx) {
			if f.jsonOut {
				_ = jsonOutput(stdout, report)
			}

			return ExitNATS
		}
		if f.jsonOut {
			_ = jsonOutput(stdout, report)
		} else {
			errs := make([]string, 0, len(report.Errors)+1)
			for _, re := range report.Errors {
				errs = append(errs, re.Error)
			}
			if len(errs) == 0 {
				errs = append(errs, err.Error())
			}
			renderValidateText(stdout, false, errs)
		}

		return classifyError(err)
	}

	if f.jsonOut {
		_ = jsonOutput(stdout, report)
	} else {
		renderValidateText(stdout, true, nil)
	}

	return ExitOK
}
