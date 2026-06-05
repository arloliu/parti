package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/arloliu/parti/test/iops-investigation/internal/testnats"
)

// TestRun_LoadMode_EmitsLatency drives the full harness lifecycle in --load
// mode against an embedded JetStream server and asserts that latency.json is
// emitted with delivered messages, a healthy delivery ratio, and no
// producer-bound trip. FetchTimeout is set to 200ms so the post-capture drain
// (3×FetchTimeout) stays short.
func TestRun_LoadMode_EmitsLatency(t *testing.T) {
	url, shutdown := testnats.Start(t)
	defer shutdown()
	dir := t.TempDir()
	o := Options{
		NATSURLs: url, Workers: 2, N: 8, Replicas: 1,
		ConsumerMode: ConsumerModeDynamic, KVStorage: jetstream.MemoryStorage, DataStorage: jetstream.MemoryStorage,
		DataStreamName: "iops-rig-data", PartitionSourceKey: DefaultPartitionSourceKey,
		Warmup: time.Second, CaptureWindow: 2 * time.Second, RPCDumpInterval: 500 * time.Millisecond,
		FetchTimeout: time.Second, // NATS pull-expiry minimum is 1s; sub-1s zeroes delivery. 3× drain = 3s, still short.
		OutputDir:    dir, FastConfig: true,
		Load: true, PerWorkerRate: 50, BatchSize: 8, MaxWaiting: 64, MaxAckPending: 256, AckWait: 30 * time.Second,
	}
	if err := Run(context.Background(), o, io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	buf, err := os.ReadFile(filepath.Join(dir, "latency.json"))
	if err != nil {
		t.Fatalf("read latency.json: %v", err)
	}
	var lr LatencyReport
	if err := json.Unmarshal(buf, &lr); err != nil {
		t.Fatal(err)
	}
	if lr.Count == 0 || lr.DeliveryRatio < 0.5 || lr.ProducerBound {
		t.Fatalf("unexpected latency report: %+v", lr)
	}
}
