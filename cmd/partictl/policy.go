package main

import (
	"fmt"
	"io"

	"github.com/arloliu/parti/v2/provision"
)

// resolveAndStampPolicy resolves the --policy flag against the YAML policy in
// cfg, stamps the effective value onto cfg.Policy, and reports whether to
// proceed. cfg.Policy is read (as the raw YAML value) before it is
// overwritten. On a flag-vs-config conflict it writes the error to w and
// returns false.
func resolveAndStampPolicy(flagVal, subcmd, file string, cfg *provision.Config, w io.Writer) bool {
	effective, ok := resolvePolicy(flagVal, cfg.Policy, subcmd, file, w)
	if !ok {
		return false
	}
	cfg.Policy = effective

	return true
}

// resolvePolicy applies the flag-vs-config conflict-resolution rules:
//
//	flag absent + YAML absent  → warn
//	flag absent + YAML X       → X
//	flag X      + YAML absent  → X
//	flag X      + YAML X       → X
//	flag X      + YAML Y       → exit 3 with conflict message
//
// flagVal is the raw string from --policy ("" when the flag was not supplied).
// rawCfgPolicy is cfg.Policy as unmarshalled from YAML, before provision.Validate
// defaults it ("" when the YAML omits or explicitly sets policy: "").
// subcmd is the command name used in the error message ("apply", "plan", "adopt").
// file is the YAML file path used in the error message.
//
// Returns (effective, true) on success; ("", false) on conflict (the error has
// already been written to w).
func resolvePolicy(flagVal string, rawCfgPolicy provision.ReconcilePolicy, subcmd, file string, w io.Writer) (provision.ReconcilePolicy, bool) {
	yamlAbsent := rawCfgPolicy == ""
	flagAbsent := flagVal == ""

	switch {
	case flagAbsent && yamlAbsent:
		return provision.PolicyWarn, true
	case flagAbsent && !yamlAbsent:
		return rawCfgPolicy, true
	case !flagAbsent && yamlAbsent:
		return provision.ReconcilePolicy(flagVal), true
	default:
		// Both flag and YAML are present (the two yamlAbsent cases above
		// cover the empty string), so rawCfgPolicy is non-empty here.
		if flagVal == string(rawCfgPolicy) {
			return provision.ReconcilePolicy(flagVal), true
		}
		fmt.Fprintf(w, "partictl %s: --policy=%s conflicts with cfg.policy=%s in %s. Pick one or remove the cfg.policy field.\n",
			subcmd, flagVal, rawCfgPolicy, file)

		return "", false
	}
}

// validatePolicyFlag checks that the --policy flag value is one of the accepted
// values (warn, adopt, safe-update, force). Returns true when valid or when
// flagVal is empty (flag absent).
func validatePolicyFlag(flagVal string, subcmd string, w io.Writer) bool {
	if flagVal == "" {
		return true
	}
	switch flagVal {
	case string(provision.PolicyWarn), string(provision.PolicyAdopt), string(provision.PolicySafeUpdate), string(provision.PolicyForce):
		return true
	default:
		fmt.Fprintf(w, "partictl %s: --policy=%q is not recognized (accepted: warn, adopt, safe-update, force)\n", subcmd, flagVal)
		return false
	}
}
