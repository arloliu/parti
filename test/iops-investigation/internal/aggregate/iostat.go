package aggregate

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// IostatSample is one second's reads/writes/sec summed across the
// devices reported by `iostat -x -d -t 1`. Used only for the cross-check
// against cgroup totals; never written to the output CSV.
type IostatSample struct {
	TSec      int64
	IOPSRead  float64
	IOPSWrite float64
}

// ParseIostat reads a capture-iostat.sh raw file and returns one
// IostatSample per timestamped block (after the first "since boot"
// block, which iostat always emits and the capture script preserves).
func ParseIostat(path string) ([]IostatSample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open iostat: %w", err)
	}
	defer f.Close()
	return parseIostat(f)
}

// parseIostat consumes the iostat block format:
//
//	# iostat -x -d -t 1                  (capture-iostat.sh header)
//	Linux 6.8.0 (host) 03/14/2024 ...    (iostat header, optional)
//	03/14/2024 11:22:33 AM
//	Device  r/s  w/s ... (header)
//	sda      0.5  1.2 ...
//	(blank line)
//	03/14/2024 11:22:34 AM
//	Device  r/s ...
//	...
//
// We skip the first block (since-boot averages) and emit one sample per
// later block.
func parseIostat(r io.Reader) ([]IostatSample, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		samples       []IostatSample
		currentTs     int64
		haveTs        bool
		inDeviceBlock bool
		rIdx, wIdx    int
		rSum, wSum    float64
		blocks        int
	)

	flush := func() {
		if !haveTs || !inDeviceBlock {
			return
		}
		blocks++
		// Skip the first block (since-boot averages from iostat).
		if blocks > 1 {
			samples = append(samples, IostatSample{TSec: currentTs, IOPSRead: rSum, IOPSWrite: wSum})
		}
		haveTs = false
		inDeviceBlock = false
		rSum, wSum = 0, 0
	}

	for sc.Scan() {
		line := sc.Text()
		trim := strings.TrimSpace(line)
		if trim == "" {
			flush()
			continue
		}
		if strings.HasPrefix(trim, "#") {
			continue
		}
		// Timestamp line: matches "MM/DD/YYYY HH:MM:SS [AM|PM]" or
		// ISO-ish "YYYY-MM-DD HH:MM:SS". iostat emits the locale form;
		// be permissive about both common shapes.
		if ts, ok := tryParseIostatTimestamp(trim); ok {
			flush()
			currentTs = ts
			haveTs = true
			continue
		}
		// Device header (e.g. "Device r/s rkB/s ... w/s wkB/s ...").
		if strings.HasPrefix(trim, "Device") {
			fields := strings.Fields(trim)
			rIdx, wIdx = -1, -1
			for i, f := range fields {
				if f == "r/s" {
					rIdx = i
				}
				if f == "w/s" {
					wIdx = i
				}
			}
			if rIdx < 0 || wIdx < 0 {
				return nil, fmt.Errorf("iostat: device header missing r/s or w/s: %q", trim)
			}
			inDeviceBlock = true
			continue
		}
		if inDeviceBlock {
			fields := strings.Fields(trim)
			if len(fields) <= rIdx || len(fields) <= wIdx {
				// Probably the "Linux …" banner; ignore.
				continue
			}
			rs, errR := strconv.ParseFloat(fields[rIdx], 64)
			ws, errW := strconv.ParseFloat(fields[wIdx], 64)
			if errR != nil || errW != nil {
				// Non-numeric in expected slot → not a device row; ignore.
				continue
			}
			rSum += rs
			wSum += ws
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan iostat: %w", err)
	}
	return samples, nil
}

// tryParseIostatTimestamp parses the locale formats iostat commonly
// emits. We use the local timezone (matching iostat's own behavior) and
// return the unix-second epoch.
func tryParseIostatTimestamp(s string) (int64, bool) {
	layouts := []string{
		"01/02/2006 03:04:05 PM",
		"01/02/2006 03:04:05",
		"2006-01-02 15:04:05",
		"01/02/06 15:04:05",
		"2006-01-02T15:04:05",
	}
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return t.Unix(), true
		}
	}
	return 0, false
}
