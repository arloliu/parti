// Command reduce-m41 turns one M4.1 grid point's raw artifacts into the
// per-node table M3 consumes.
//
// Inputs (under --point-dir):
//   - calibrate.csv     — single-row driver output from `calibrate idle-pull`
//   - cgroup_io.raw     — cumulative cgroup v2 io.stat samples per container
//
// Output: two CSV rows on stdout (or to --output), one with
// node_role=leader and one with node_role=follower. Schema:
//
//	c_stream,fetch_timeout_s,data_storage,replicas,node_role,
//	block_read_iops,block_write_iops,ci_low,ci_high
//
// Header is emitted only when --emit-header is set; the consolidation
// step in run-m4.sh writes the header once and then appends headerless
// rows from each point.
//
// See docs/plans/iops-investigation/00-attribution-plan.md §M4.1
// (lines 700-775) for the spec.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/arloliu/parti/test/iops-investigation/internal/aggregate"
	"github.com/arloliu/parti/test/iops-investigation/internal/calibrate"
)

func main() {
	var (
		pointDir    string
		output      string
		emitHeader  bool
		allowSingle bool
	)
	flag.StringVar(&pointDir, "point-dir", "", "directory containing calibrate.csv + cgroup_io.raw for one M4.1 grid point")
	flag.StringVar(&output, "output", "", "write rows here (default: stdout)")
	flag.BoolVar(&emitHeader, "emit-header", false, "prepend the canonical M4.1 schema header")
	flag.BoolVar(&allowSingle, "allow-single-node", false, "do not fail when no leader can be identified (smoke / single-node case); emits a follower-only row")
	flag.Parse()
	if pointDir == "" {
		fmt.Fprintln(os.Stderr, "reduce-m41: --point-dir is required")
		os.Exit(2)
	}

	if err := run(pointDir, output, emitHeader, allowSingle); err != nil {
		fmt.Fprintf(os.Stderr, "reduce-m41: %v\n", err)
		os.Exit(1)
	}
}

func run(pointDir, output string, emitHeader, allowSingle bool) error {
	calibCSV := filepath.Join(pointDir, "calibrate.csv")
	cgroupRaw := filepath.Join(pointDir, "cgroup_io.raw")

	df, err := os.Open(calibCSV)
	if err != nil {
		return fmt.Errorf("open driver csv: %w", err)
	}
	driver, err := calibrate.ParseDriverRow(df)
	_ = df.Close()
	if err != nil {
		return fmt.Errorf("parse driver csv %q: %w", calibCSV, err)
	}

	samples, err := aggregate.ParseCgroup(cgroupRaw)
	if err != nil {
		return fmt.Errorf("parse cgroup: %w", err)
	}

	leader, follower, warn := calibrate.ReduceM41(samples, driver)
	if warn != nil && !allowSingle {
		return fmt.Errorf("reduce: %w (pass --allow-single-node to accept follower-only output)", warn)
	}
	if warn != nil {
		fmt.Fprintf(os.Stderr, "reduce-m41: warning: %v\n", warn)
	}

	var w io.Writer = os.Stdout
	if output != "" {
		f, err := os.Create(output)
		if err != nil {
			return fmt.Errorf("create output: %w", err)
		}
		defer f.Close()
		w = f
	}

	if emitHeader {
		if _, err := fmt.Fprintln(w, calibrate.M41RowSchemaHeader); err != nil {
			return err
		}
	}
	// Suppress the empty leader row when ReduceM41 returned only a
	// follower (no-leader path).
	if leader.NodeRole != "" {
		if _, err := fmt.Fprintln(w, leader.CSV()); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w, follower.CSV()); err != nil {
		return err
	}

	return nil
}
