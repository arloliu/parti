package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// FailureReport matches the structure in coordinator.go
type FailureReport struct {
	Timestamp    time.Time         `json:"timestamp"`
	Reason       string            `json:"reason"`
	DetailedGaps []MessageGapError `json:"detailed_gaps"`
}

type MessageGapError struct {
	PartitionID int   `json:"PartitionID"`
	ExpectedSeq int64 `json:"ExpectedSeq"`
	ReceivedSeq int64 `json:"ReceivedSeq"`
	LastSent    int64 `json:"LastSent"`
}

type LogEvent struct {
	Timestamp time.Time
	Message   string
	Type      string // "INFO", "ERROR", "CHAOS", "ASSIGN", "WORKER"
	Raw       string
}

type VisualizationData struct {
	Report    FailureReport
	Events    []LogEvent
	Partition int
}

var logTsRegex = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2})`)

func main() {
	reportPath := flag.String("report", "failure_report.json", "Path to failure report JSON")
	logPath := flag.String("log", "run.log", "Path to run.log")
	outPath := flag.String("out", "trace.html", "Output HTML file path")
	flag.Parse()

	// 1. Read Failure Report
	reportData, err := os.ReadFile(*reportPath)
	if err != nil {
		fmt.Printf("Error reading report: %v\n", err)
		os.Exit(1)
	}

	var report FailureReport
	if err := json.Unmarshal(reportData, &report); err != nil {
		fmt.Printf("Error parsing report: %v\n", err)
		os.Exit(1)
	}

	if len(report.DetailedGaps) == 0 {
		fmt.Println("No detailed gaps found in report.")
		os.Exit(0)
	}

	// Focus on the first gap for now
	targetGap := report.DetailedGaps[0]
	targetPartition := targetGap.PartitionID
	fmt.Printf("Analyzing gap for Partition %d (Expected: %d, Received: %d)\n",
		targetPartition, targetGap.ExpectedSeq, targetGap.ReceivedSeq)

	// 2. Parse Log File
	events, err := parseLog(*logPath, targetPartition)
	if err != nil {
		fmt.Printf("Error parsing log: %v\n", err)
		os.Exit(1)
	}

	// 3. Generate HTML
	data := VisualizationData{
		Report:    report,
		Events:    events,
		Partition: targetPartition,
	}

	if err := generateHTML(*outPath, data); err != nil {
		fmt.Printf("Error generating HTML: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Trace visualization written to %s\n", *outPath)
}

func parseLog(path string, partitionID int) ([]LogEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var events []LogEvent
	scanner := bufio.NewScanner(f)

	// Regex to match partition ID in logs (e.g., "partition=123" or "p=123" or just "123")
	// This is a heuristic.
	partRegex := regexp.MustCompile(fmt.Sprintf(`\b(partition|p)=?%d\b`, partitionID))

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

		msg := line[len(tsStr):]

		// Filter logic:
		// 1. Always include CHAOS events
		// 2. Always include WORKER start/stop events
		// 3. Include ASSIGNMENT events
		// 4. Include specific partition logs

		isRelevant := false
		eventType := "INFO"

		switch {
		case strings.Contains(msg, "[Chaos]"):
			isRelevant = true
			eventType = "CHAOS"
		case strings.Contains(msg, "Starting worker") || strings.Contains(msg, "Stopping worker"):
			isRelevant = true
			eventType = "WORKER"
		case strings.Contains(msg, "Assignment change"):
			isRelevant = true
			eventType = "ASSIGN"
		case strings.Contains(msg, "ERROR") || strings.Contains(msg, "panic"):
			isRelevant = true
			eventType = "ERROR"
		case partRegex.MatchString(msg):
			isRelevant = true
			eventType = "PARTITION"
		}

		if isRelevant {
			events = append(events, LogEvent{
				Timestamp: t,
				Message:   strings.TrimSpace(msg),
				Type:      eventType,
				Raw:       line,
			})
		}
	}

	slices.SortFunc(events, func(a, b LogEvent) int {
		return a.Timestamp.Compare(b.Timestamp)
	})

	return events, scanner.Err()
}

func generateHTML(path string, data VisualizationData) error {
	tmpl := `<!DOCTYPE html>
<html>
<head>
	<title>Trace Visualization: Partition {{.Partition}}</title>
	<style>
		body { font-family: sans-serif; margin: 20px; }
		.event { padding: 5px; border-bottom: 1px solid #eee; }
		.timestamp { color: #666; font-family: monospace; margin-right: 10px; }
		.type { font-weight: bold; display: inline-block; width: 80px; }
		.CHAOS { color: red; }
		.WORKER { color: blue; }
		.ASSIGN { color: green; }
		.ERROR { color: darkred; background: #fee; }
		.PARTITION { color: purple; }
		.gap-info { background: #f9f9f9; padding: 15px; border: 1px solid #ddd; margin-bottom: 20px; }
	</style>
</head>
<body>
	<h1>Trace Visualization</h1>

	<div class="gap-info">
		<h2>Gap Analysis: Partition {{.Partition}}</h2>
		<p><strong>Reason:</strong> {{.Report.Reason}}</p>
		<p><strong>Timestamp:</strong> {{.Report.Timestamp}}</p>
		{{range .Report.DetailedGaps}}
		{{if eq .PartitionID $.Partition}}
		<ul>
			<li>Expected Sequence: {{.ExpectedSeq}}</li>
			<li>Received Sequence: {{.ReceivedSeq}}</li>
			<li>Last Sent: {{.LastSent}}</li>
		</ul>
		{{end}}
		{{end}}
	</div>

	<h2>Timeline</h2>
	<div class="timeline">
		{{range .Events}}
		<div class="event {{.Type}}">
			<span class="timestamp">{{.Timestamp.Format "15:04:05.000"}}</span>
			<span class="type {{.Type}}">{{.Type}}</span>
			<span class="message">{{.Message}}</span>
		</div>
		{{end}}
	</div>
</body>
</html>`

	t, err := template.New("trace").Parse(tmpl)
	if err != nil {
		return err
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return t.Execute(f, data)
}
