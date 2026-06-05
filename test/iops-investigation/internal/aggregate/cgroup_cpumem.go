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

// CPUMemSample is one row from capture-cgroup-cpumem.sh: the cumulative CPU
// time (cpu.stat usage_usec) and instantaneous resident bytes (memory.current)
// at a single 1 Hz tick for one container. Unlike CgroupSample there is no
// per-device axis — cpu.stat / memory.current are per-cgroup, not per-device.
type CPUMemSample struct {
	TUnixNs     int64
	Container   string
	UsageUsec   int64 // cumulative CPU time, microseconds (cpu.stat usage_usec)
	MemoryBytes int64 // instantaneous resident bytes (memory.current)
}

// CPUMemDelta is one second's per-container CPU + memory figure.
//
// CPUCores is the fraction-of-one-core CPU usage over the interval between the
// two samples: Δusage_usec / Δwall_usec. Both numerator and denominator are
// microseconds, so the ratio is dimensionless: 1.0 == one fully-busy core, 2.5
// == two and a half cores' worth of CPU time consumed in that wall second.
//
// MemoryBytes is instantaneous (the later sample's memory.current); it is NOT
// a delta — it is carried through unchanged.
type CPUMemDelta struct {
	TSec        int64
	Container   string
	CPUCores    float64 // fraction of one core (1.0 = one full core)
	MemoryBytes int64   // instantaneous resident bytes
}

// ParseCgroupCPUMem reads a capture-cgroup-cpumem.sh raw file and returns its
// samples in file order. Empty/comment lines are skipped; malformed lines abort
// with a clear message.
func ParseCgroupCPUMem(path string) ([]CPUMemSample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open cgroup cpumem: %w", err)
	}
	defer f.Close()
	return parseCgroupCPUMemReader(f)
}

func parseCgroupCPUMemReader(r io.Reader) ([]CPUMemSample, error) {
	var out []CPUMemSample
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
		if len(fields) != 4 {
			return nil, fmt.Errorf("cgroup cpumem line %d: want 4 fields, got %d: %q", lineNo, len(fields), line)
		}
		ts, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("cgroup cpumem line %d: bad t_unix_ns: %w", lineNo, err)
		}
		usage, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("cgroup cpumem line %d: bad usage_usec: %w", lineNo, err)
		}
		mem, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("cgroup cpumem line %d: bad memory_current_bytes: %w", lineNo, err)
		}
		out = append(out, CPUMemSample{
			TUnixNs:     ts,
			Container:   fields[1],
			UsageUsec:   usage,
			MemoryBytes: mem,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan cgroup cpumem: %w", err)
	}

	return out, nil
}

// CPUMemDeltas converts cumulative usage_usec samples to a per-second CPU
// fraction-of-one-core, carrying memory.current through instantaneously.
// Returns one CPUMemDelta per (t_sec, container) for every consecutive pair of
// samples on the same container. The t_sec is floor(later_sample.TUnixNs/1e9).
//
// The CPU rate uses the wall-clock gap between the two samples — both the CPU
// delta (usage_usec) and the wall gap are in microseconds, so CPUCores is
// dimensionless (1.0 = one full core). A slow poller tick therefore does not
// inflate the rate.
//
// Unlike CgroupDeltas there is no per-device axis to sum over: cpu.stat and
// memory.current are per-cgroup, so each container yields exactly one delta per
// consecutive pair.
func CPUMemDeltas(samples []CPUMemSample) []CPUMemDelta {
	// Group by container, keeping file order within group.
	groups := make(map[string][]CPUMemSample)
	order := make([]string, 0)
	for _, s := range samples {
		if _, ok := groups[s.Container]; !ok {
			order = append(order, s.Container)
		}
		groups[s.Container] = append(groups[s.Container], s)
	}

	var out []CPUMemDelta
	for _, c := range order {
		g := groups[c]
		for i := 1; i < len(g); i++ {
			prev, cur := g[i-1], g[i]
			dtNs := cur.TUnixNs - prev.TUnixNs
			if dtNs <= 0 {
				continue
			}
			// Wall gap in microseconds, to match usage_usec's unit.
			dtUsec := float64(dtNs) / 1e3
			dUsage := safeDeltaInt(cur.UsageUsec, prev.UsageUsec)
			tSec := cur.TUnixNs / int64(1e9)
			out = append(out, CPUMemDelta{
				TSec:        tSec,
				Container:   cur.Container,
				CPUCores:    float64(dUsage) / dtUsec,
				MemoryBytes: cur.MemoryBytes,
			})
		}
	}
	slices.SortFunc(out, func(a, b CPUMemDelta) int {
		if a.TSec != b.TSec {
			return cmp.Compare(a.TSec, b.TSec)
		}
		return cmp.Compare(a.Container, b.Container)
	})

	return out
}

// safeDeltaInt returns cur-prev, or 0 if cur < prev (which would imply a
// counter reset; treat as no progress rather than a negative rate).
func safeDeltaInt(cur, prev int64) int64 {
	if cur < prev {
		return 0
	}
	return cur - prev
}
