package main

// Command inspect_consumers connects to a NATS JetStream server and prints a
// concise summary of consumer configuration fields relevant to gap analysis.
//
// Environment variables:
//   NATS_URL   (default: nats://127.0.0.1:4222)
//   NATS_USER  (optional)
//   NATS_PASS  (optional)
//   STREAM     (required if -stream not provided)
//
// Flags:
//   -stream string   Stream name to inspect (required)
//   -json            Emit JSON instead of table
//   -subjects        Include filter subjects for each consumer
//
// Example:
//   go run ./scripts/inspect_consumers -stream WORKER_TEST
//   NATS_URL=nats://demo.nats.io:4222 go run ./scripts/inspect_consumers -stream my-stream -json
//
// Output columns (table mode):
//   NAME  ACK_WAIT  MAX_DELIVER  MAX_ACK_PENDING  DELIVER_POLICY  BATCH_SIZE  INACTIVE_THRESHOLD  FILTERS
//
// Notes:
// - For per-subject helper consumers the durable name typically encodes the worker and partition.
// - AckWait should exceed processing p99; MaxDeliver should be high enough to avoid premature loss.
// - Low MaxAckPending relative to BatchSize can induce artificial gaps under load.
// - InactiveThreshold too small during reassignment can trigger cleanup.
// - DeliverPolicy LastPerSubject may skip intermediate messages during recreation phases.
//
// This tool does not modify state; safe to run in production.

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

type consumerSummary struct {
	Name              string        `json:"name"`
	AckWait           time.Duration `json:"ack_wait"`
	MaxDeliver        int           `json:"max_deliver"`
	MaxAckPending     int           `json:"max_ack_pending"`
	DeliverPolicy     string        `json:"deliver_policy"`
	BatchMax          int           `json:"max_request_batch"`
	BatchExpires      time.Duration `json:"max_request_expires"`
	InactiveThreshold time.Duration `json:"inactive_threshold"`
	FilterSubjects    []string      `json:"filter_subjects,omitempty"`
}

func main() {
	var streamName string
	var emitJSON bool
	var includeSubjects bool
	flag.StringVar(&streamName, "stream", "", "JetStream stream name to inspect (required)")
	flag.BoolVar(&emitJSON, "json", false, "Emit JSON instead of table")
	flag.BoolVar(&includeSubjects, "subjects", false, "Include filter subjects in output")
	flag.Parse()

	if streamName == "" {
		if env := os.Getenv("STREAM"); env != "" {
			streamName = env
		}
	}
	if streamName == "" {
		_, _ = fmt.Fprintln(os.Stderr, "error: stream name is required (flag -stream or env STREAM)")
		os.Exit(2)
	}

	url := os.Getenv("NATS_URL")
	if url == "" {
		url = nats.DefaultURL
	}
	opts := []nats.Option{}
	if u := os.Getenv("NATS_USER"); u != "" {
		opts = append(opts, nats.UserInfo(u, os.Getenv("NATS_PASS")))
	}

	nc, err := nats.Connect(url, opts...)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "connect error: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = nc.Drain() }()

	js, err := jetstream.New(nc)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "jetstream error: %v\n", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := js.Stream(ctx, streamName)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "stream error: %v\n", err)
		return
	}

	cl := stream.ConsumerNames(ctx)
	summaries := make([]consumerSummary, 0, 1)
	for name := range cl.Name() {
		c, err := stream.Consumer(ctx, name)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "warn: consumer %s fetch error: %v\n", name, err)
			continue
		}
		info, err := c.Info(ctx)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "warn: consumer %s info error: %v\n", name, err)
			continue
		}
		cfg := info.Config
		deliverPolicy := strings.ToLower(cfg.DeliverPolicy.String())
		s := consumerSummary{
			Name:              name,
			AckWait:           cfg.AckWait,
			MaxDeliver:        cfg.MaxDeliver,
			MaxAckPending:     cfg.MaxAckPending,
			DeliverPolicy:     deliverPolicy,
			BatchMax:          cfg.MaxRequestBatch,
			BatchExpires:      cfg.MaxRequestExpires,
			InactiveThreshold: cfg.InactiveThreshold,
		}
		if includeSubjects {
			s.FilterSubjects = append([]string(nil), cfg.FilterSubjects...)
		}
		summaries = append(summaries, s)
	}

	if err := cl.Err(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "consumer listing error: %v\n", err)
	}

	slices.SortFunc(summaries, func(a, b consumerSummary) int { return cmp.Compare(a.Name, b.Name) })

	if emitJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(summaries); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "encode error: %v\n", err)
			return
		}
		return
	}

	// Table output
	fmt.Printf("%-40s %-8s %-11s %-15s %-14s %-10s %-18s %s\n", "NAME", "ACKWAIT", "MAXDELIVER", "MAXACKPENDING", "DELIVER_POLICY", "BATCHMAX", "INACTIVE_THRESHOLD", "FILTERS")
	for _, s := range summaries {
		filters := ""
		if includeSubjects && len(s.FilterSubjects) > 0 {
			filters = strings.Join(s.FilterSubjects, ";")
		}
		fmt.Printf("%-40s %-8s %-11d %-15d %-14s %-10d %-18s %s\n",
			truncate(s.Name, 40), durShort(s.AckWait), s.MaxDeliver, s.MaxAckPending, s.DeliverPolicy, s.BatchMax, durShort(s.InactiveThreshold), truncate(filters, 60))
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

func durShort(d time.Duration) string {
	if d == 0 {
		return "0"
	}
	if d%time.Second == 0 {
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
	return d.String()
}
