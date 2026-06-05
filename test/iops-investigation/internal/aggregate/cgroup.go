// Package aggregate parses per-run capture artifacts and reconciles
// them into a single CSV. See cmd/aggregate/main.go for the CLI.
//
// Every parser here is invariant-driven: bad input is loud, not lossy.
// Each per-source parser returns its samples keyed by (t_unix_ns, …)
// so the aggregator can join across sources by floor(t_unix_ns/1e9).
package aggregate

import (
	"bufio"
	"cmp"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
)

// CgroupSample is one row from capture-cgroup-io.sh: cumulative counters
// at a single 1 Hz tick for one (container, device).
type CgroupSample struct {
	TUnixNs   int64
	Container string
	Device    string // "MAJ:MIN"
	RBytes    uint64
	WBytes    uint64
	RIOs      uint64
	WIOs      uint64
}

// CgroupDelta is one second's per-(container) rate, summed across the
// container's devices. Aggregator joins one of these to one row in the
// output CSV per (t_s, container).
type CgroupDelta struct {
	TSec      int64
	Container string
	IOPSRead  float64
	IOPSWrite float64
	BytesRead float64
	BytesWrt  float64
}

// ParseCgroup reads a capture-cgroup-io.sh raw file and returns its
// samples in file order. Empty/comment lines are skipped; malformed
// lines abort with a clear message.
func ParseCgroup(path string) ([]CgroupSample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open cgroup: %w", err)
	}
	defer f.Close()
	return parseCgroupReader(f)
}

func parseCgroupReader(r io.Reader) ([]CgroupSample, error) {
	var out []CgroupSample
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 7 {
			return nil, fmt.Errorf("cgroup line %d: want 7 fields, got %d: %q", lineNo, len(fields), line)
		}
		ts, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("cgroup line %d: bad t_unix_ns: %w", lineNo, err)
		}
		rb, err := strconv.ParseUint(fields[3], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("cgroup line %d: bad rbytes: %w", lineNo, err)
		}
		wb, err := strconv.ParseUint(fields[4], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("cgroup line %d: bad wbytes: %w", lineNo, err)
		}
		ri, err := strconv.ParseUint(fields[5], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("cgroup line %d: bad rios: %w", lineNo, err)
		}
		wi, err := strconv.ParseUint(fields[6], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("cgroup line %d: bad wios: %w", lineNo, err)
		}
		out = append(out, CgroupSample{
			TUnixNs:   ts,
			Container: fields[1],
			Device:    fields[2],
			RBytes:    rb,
			WBytes:    wb,
			RIOs:      ri,
			WIOs:      wi,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan cgroup: %w", err)
	}

	return out, nil
}

// CgroupDeltas converts cumulative samples to per-second deltas, summed
// across each container's devices. Returns one CgroupDelta per
// (t_sec, container) for every consecutive pair of samples on the same
// (container, device). The t_sec is floor(later_sample.TUnixNs/1e9).
//
// Rates use the wall-clock gap between the two samples (in seconds),
// not a fixed 1.0, so a slow poller tick doesn't inflate the rate.
func CgroupDeltas(samples []CgroupSample) []CgroupDelta {
	// Group by (container, device), keep file order within group.
	type key struct{ c, d string }
	groups := make(map[key][]CgroupSample)
	for _, s := range samples {
		k := key{s.Container, s.Device}
		groups[k] = append(groups[k], s)
	}
	// Accumulate into per-(t_sec, container) buckets.
	type bkey struct {
		t int64
		c string
	}
	bucket := make(map[bkey]*CgroupDelta)
	for _, g := range groups {
		for i := 1; i < len(g); i++ {
			prev, cur := g[i-1], g[i]
			dtNs := cur.TUnixNs - prev.TUnixNs
			if dtNs <= 0 {
				continue
			}
			dtSec := float64(dtNs) / 1e9
			rIOs := safeDeltaUint(cur.RIOs, prev.RIOs)
			wIOs := safeDeltaUint(cur.WIOs, prev.WIOs)
			rB := safeDeltaUint(cur.RBytes, prev.RBytes)
			wB := safeDeltaUint(cur.WBytes, prev.WBytes)
			tSec := cur.TUnixNs / int64(1e9)
			bk := bkey{tSec, cur.Container}
			d, ok := bucket[bk]
			if !ok {
				d = &CgroupDelta{TSec: tSec, Container: cur.Container}
				bucket[bk] = d
			}
			d.IOPSRead += float64(rIOs) / dtSec
			d.IOPSWrite += float64(wIOs) / dtSec
			d.BytesRead += float64(rB) / dtSec
			d.BytesWrt += float64(wB) / dtSec
		}
	}
	out := make([]CgroupDelta, 0, len(bucket))
	for _, d := range bucket {
		out = append(out, *d)
	}
	slices.SortFunc(out, func(a, b CgroupDelta) int {
		if a.TSec != b.TSec {
			return cmp.Compare(a.TSec, b.TSec)
		}
		return cmp.Compare(a.Container, b.Container)
	})

	return out
}

// safeDeltaUint returns cur-prev as uint64, or 0 if cur < prev (which
// would imply a counter reset; treat as no progress rather than wrap).
func safeDeltaUint(cur, prev uint64) uint64 {
	if cur < prev {
		return 0
	}
	return cur - prev
}
