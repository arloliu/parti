package main

// Command gap_timeline parses simulation artifacts (timeseries.csv, run.log)
// to produce a comprehensive timeline of events, including gaps, chaos injection,
// state changes, and worker lifecycle events.
//
// Usage:
//   go run ./scripts/gap_timeline -dir /tmp/parti-sim-stress/latest
//
// Flags:
//   -dir          Directory containing timeseries.csv and run.log
//   -json         Emit JSON output
//   -verbose      Include all log events, not just significant ones

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var (
	colTimestamp = []string{"timestamp", "ts", "time"}
	colGaps      = []string{"gaps", "gap_count", "gapcounter"}
	colWorkers   = []string{"workers", "active_workers", "active"}

	// Regex for standard Go log timestamp: YYYY/MM/DD HH:MM:SS
	logTsRegex = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2})`)
)

type EventType string

const (
	TypeGap        EventType = "GAP"
	TypeChaos      EventType = "CHAOS"
	TypeState      EventType = "STATE"
	TypeWorker     EventType = "WORKER"
	TypeAssignment EventType = "ASSIGN"
	TypeError      EventType = "ERROR"
	TypeInfo       EventType = "INFO"
)

type TimelineEvent struct {
	Timestamp time.Time      `json:"timestamp"`
	Type      EventType      `json:"type"`
	Message   string         `json:"message"`
	Details   map[string]any `json:"details,omitempty"`
}

type TimelineSummary struct {
	TotalGaps              int           `json:"total_gaps"`
	TotalChaosEvents       int           `json:"total_chaos_events"`
	TotalWorkerRestarts    int           `json:"total_worker_restarts"`
	TotalMissingData       time.Duration `json:"total_missing_data"`
	TotalAssignmentChanges int           `json:"total_assignment_changes"`
}

type TimelineReport struct {
	Events  []TimelineEvent `json:"events"`
	Summary TimelineSummary `json:"summary"`
}

func main() {
	var dir string
	var jsonOut bool
	var verbose bool
	flag.StringVar(&dir, "dir", "", "Directory containing timeseries.csv and run.log")
	flag.BoolVar(&jsonOut, "json", false, "Emit JSON instead of table")
	flag.BoolVar(&verbose, "verbose", false, "Include all log events")
	flag.Parse()

	if dir == "" {
		_, _ = fmt.Fprintln(os.Stderr, "error: must supply -dir")
		os.Exit(2)
	}

	timeseriesPath := filepath.Join(dir, "timeseries.csv")
	runlogPath := filepath.Join(dir, "run.log")

	var events []TimelineEvent

	// 1. Parse Timeseries for Gaps
	tsEvents, err := parseTimeseriesGaps(timeseriesPath)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: timeseries parse error: %v\n", err)
	} else {
		events = append(events, tsEvents...)
	}

	// 2. Parse Run Log for Context
	logEvents, err := parseRunLog(runlogPath, verbose)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: runlog parse error: %v\n", err)
	} else {
		events = append(events, logEvents...)
	}

	// 3. Sort by Timestamp
	slices.SortFunc(events, func(a, b TimelineEvent) int {
		return a.Timestamp.Compare(b.Timestamp)
	})

	// 4. Generate Summary
	summary := generateSummary(events)

	// 5. Output
	if jsonOut {
		report := TimelineReport{
			Events:  events,
			Summary: summary,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "encode error: %v\n", err)
			os.Exit(3)
		}

		return
	}

	printTable(events, summary)
}

func generateSummary(events []TimelineEvent) TimelineSummary {
	var s TimelineSummary
	for _, e := range events {
		switch e.Type {
		case TypeGap:
			if delta, ok := e.Details["delta"].(int); ok {
				s.TotalGaps += delta
			}
		case TypeChaos:
			s.TotalChaosEvents++
		case TypeWorker:
			if strings.Contains(e.Message, "Starting worker") {
				s.TotalWorkerRestarts++
			}
		case TypeAssignment:
			s.TotalAssignmentChanges++
		case TypeError:
			if strings.Contains(e.Message, "Missing metrics data") {
				// Extract duration from message "Missing metrics data for 24s"
				parts := strings.Split(e.Message, " for ")
				if len(parts) == 2 {
					if d, err := time.ParseDuration(parts[1]); err == nil {
						s.TotalMissingData += d
					}
				}
			}
		case TypeState, TypeInfo:
			_ = 0 // Dummy op to make branch different
		default:
			// Ignore any other unknown types
		}
	}

	return s
}

func printTable(events []TimelineEvent, summary TimelineSummary) {
	fmt.Printf("%-25s %-8s %s\n", "TIME", "TYPE", "MESSAGE")
	fmt.Println(strings.Repeat("-", 100))

	for _, e := range events {
		ts := e.Timestamp.Format("15:04:05.000")

		// Colorize/Highlight based on type (using ANSI codes if we wanted, but keeping it simple for now)
		prefix := ""
		if e.Type == TypeGap || e.Type == TypeError {
			prefix = "!!! "
		}

		msg := e.Message
		if len(e.Details) > 0 {
			// Append key details
			var details []string
			for k, v := range e.Details {
				details = append(details, fmt.Sprintf("%s=%v", k, v))
			}
			msg += fmt.Sprintf(" (%s)", strings.Join(details, ", "))
		}

		fmt.Printf("%-25s %-8s %s%s\n", ts, e.Type, prefix, truncate(msg, 80))
	}

	fmt.Println(strings.Repeat("-", 100))
	fmt.Println("SUMMARY:")
	fmt.Printf("  Total Gaps:              %d\n", summary.TotalGaps)
	fmt.Printf("  Total Chaos Events:      %d\n", summary.TotalChaosEvents)
	fmt.Printf("  Total Worker Restarts:   %d\n", summary.TotalWorkerRestarts)
	fmt.Printf("  Total Assignment Changes:%d\n", summary.TotalAssignmentChanges)
	if summary.TotalMissingData > 0 {
		fmt.Printf("  Total Missing Data:      %v\n", summary.TotalMissingData)
	}
}

func parseTimeseriesGaps(path string) ([]TimelineEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(bufio.NewReader(f))
	header, err := r.Read()
	if err != nil {
		return nil, err
	}

	idxTimestamp, err := findColumn(header, colTimestamp)
	if err != nil {
		return nil, err
	}
	idxGaps, err := findColumn(header, colGaps)
	if err != nil {
		return nil, err
	}
	idxWorkers, _ := findColumn(header, colWorkers)

	var events []TimelineEvent
	prevGaps := -1
	var prevTime time.Time

	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}

		tsRaw := strings.TrimSpace(rec[idxTimestamp])
		t, ok := parseFlexibleTime(tsRaw)
		if !ok {
			continue
		}

		// Check for missing data (jumps > 2s)
		if !prevTime.IsZero() {
			diff := t.Sub(prevTime)
			if diff > 2*time.Second {
				events = append(events, TimelineEvent{
					Timestamp: prevTime.Add(1 * time.Second),
					Type:      TypeError,
					Message:   fmt.Sprintf("Missing metrics data for %v", diff),
				})
			}
		}
		prevTime = t

		gapsVal, err := strconv.Atoi(strings.TrimSpace(rec[idxGaps]))
		if err != nil {
			continue
		}

		if prevGaps != -1 && gapsVal > prevGaps {
			delta := gapsVal - prevGaps
			details := map[string]any{
				"delta": delta,
				"total": gapsVal,
			}
			if idxWorkers >= 0 {
				if w, err := strconv.Atoi(strings.TrimSpace(rec[idxWorkers])); err == nil {
					details["workers"] = w
				}
			}

			events = append(events, TimelineEvent{
				Timestamp: t,
				Type:      TypeGap,
				Message:   fmt.Sprintf("Gap count increased by %d", delta),
				Details:   details,
			})
		}
		prevGaps = gapsVal
	}

	return events, nil
}

func parseRunLog(path string, verbose bool) ([]TimelineEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var events []TimelineEvent
	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		line := scanner.Text()

		// Extract timestamp
		matches := logTsRegex.FindStringSubmatch(line)
		if len(matches) < 2 {
			continue
		}
		tsStr := matches[1]
		t, err := time.ParseInLocation("2006/01/02 15:04:05", tsStr, time.Local)
		if err != nil {
			continue
		}

		// Remove timestamp from message
		msg := strings.TrimSpace(line[len(tsStr):])

		// Categorize
		var et EventType
		var keep bool

		switch {
		case strings.Contains(msg, "[Chaos]"):
			et = TypeChaos
			keep = true
		case strings.Contains(msg, "Assignment change"):
			et = TypeAssignment
			keep = true
		case strings.Contains(msg, "state transition"):
			et = TypeState
			keep = true
		case strings.Contains(msg, "Starting worker") || strings.Contains(msg, "Stopping worker"):
			et = TypeWorker
			keep = true
		case strings.Contains(msg, "ERROR") || strings.Contains(msg, "panic"):
			et = TypeError
			keep = true
		default:
			et = TypeInfo
			keep = verbose
		}

		if keep {
			events = append(events, TimelineEvent{
				Timestamp: t,
				Type:      et,
				Message:   msg,
			})
		}
	}

	return events, scanner.Err()
}

// Helpers

func findColumn(header []string, candidates []string) (int, error) {
	for i, h := range header {
		lo := strings.ToLower(strings.TrimSpace(h))
		for _, c := range candidates {
			if lo == c {
				return i, nil
			}
		}
	}

	return -1, errors.New("missing required column")
}

func parseFlexibleTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	// Try RFC3339
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}

	return time.Time{}, false
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
