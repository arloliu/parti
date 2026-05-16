package main

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/test/iops-investigation/internal/instrumentedjs"
)

// startEmbeddedNATS spins up an in-process NATS server with JetStream on a
// random port. Mirrors the helper in cmd/harness so the smoke test does not
// require a docker rig.
func startEmbeddedNATS(t *testing.T) string {
	t.Helper()
	opts := &server.Options{
		JetStream: true,
		Port:      -1,
		StoreDir:  t.TempDir(),
	}
	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second), "NATS server not ready")
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})
	return ns.ClientURL()
}

// newTestIJS connects to the given URL and returns a wrapped InstrumentedJS
// plus a cleanup function.
func newTestIJS(t *testing.T, url string) *instrumentedjs.InstrumentedJS {
	t.Helper()
	nc, err := nats.Connect(url)
	require.NoError(t, err)
	t.Cleanup(func() { nc.Close() })
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	return instrumentedjs.New(js)
}

// parseCSVRow splits a single CSV line (no newline needed) into a slice of
// fields, using Go's csv.Reader so quoting / escaping is handled correctly.
func parseCSVRow(t *testing.T, line string) []string {
	t.Helper()
	r := csv.NewReader(strings.NewReader(line))
	row, err := r.Read()
	require.NoError(t, err)
	return row
}

// TestEmitCSV_Format verifies that emitCSV writes the expected 9-column
// CSV row to the configured writer. We redirect writer for this test.
func TestEmitCSV_Format(t *testing.T) {
	old := writer // save
	var buf bytes.Buffer
	setWriter(&buf) // redirect
	defer setWriter(old)

	emitCSV("kv-put", "file", 3, 100, 60.0, 256, 12, 34)
	got := strings.TrimRight(buf.String(), "\n")
	fields := parseCSVRow(t, got)
	require.Len(t, fields, 9, "expected 9 CSV columns")
	require.Equal(t, "kv-put", fields[0], "path")
	require.Equal(t, "file", fields[1], "storage")
	require.Equal(t, "3", fields[2], "replicas")
	require.Equal(t, "100", fields[3], "rate")
	require.Equal(t, "60.0", fields[4], "duration_s")
	require.Equal(t, "256", fields[5], "bytes_per_op")
	require.Equal(t, "12", fields[6], "read_rpc_ops")
	require.Equal(t, "34", fields[7], "write_rpc_ops")
	require.Equal(t, "46", fields[8], "wrapper_total_ops")
}

// TestCountRPCs_Classification ensures countRPCs categorises Gets as reads
// and Puts as writes, and ignores __js__-bucket entries entirely.
func TestCountRPCs_Classification(t *testing.T) {
	snap := instrumentedjs.Snapshot{
		{Bucket: "my-bucket", Op: "Get"}:                               5,
		{Bucket: "my-bucket", Op: "Keys"}:                              3,
		{Bucket: "my-bucket", Op: "Put"}:                               7,
		{Bucket: "my-bucket", Op: "Create"}:                            2,
		{Bucket: instrumentedjs.JetStreamBucket, Op: "CreateKeyValue"}: 99, // excluded: __js__ bucket
		{Bucket: instrumentedjs.JetStreamBucket, Op: "Stream"}:         10, // excluded: __js__ bucket
	}
	reads, writes := countRPCs(snap)
	require.Equal(t, int64(8), reads, "Get(5)+Keys(3)=8; __js__ entries excluded")
	require.Equal(t, int64(9), writes, "Put(7)+Create(2)=9; __js__ entries excluded")
}

// TestParseIntList covers the comma-split helper.
func TestParseIntList(t *testing.T) {
	got, err := parseIntList("1000,3000")
	require.NoError(t, err)
	require.Equal(t, []int{1000, 3000}, got)

	got, err = parseIntList("42")
	require.NoError(t, err)
	require.Equal(t, []int{42}, got)

	_, err = parseIntList("abc")
	require.Error(t, err)

	_, err = parseIntList("")
	require.Error(t, err)
}

// ---- smoke tests -----------------------------------------------------------
// Each smoke test drives the sub-command for a short duration against an
// embedded NATS, then verifies:
//   (a) the path column in stdout matches the expected value, and
//   (b) the relevant RPC counter (read or write side) is non-zero.
//
// We use a writer-redirect approach (see setWriter/writer below) to capture
// emitCSV output without spawning a subprocess.

// TestSmoke_KVPut verifies that kv-put emits a row with path="kv-put" and
// a non-zero write_rpc_ops counter.
func TestSmoke_KVPut(t *testing.T) {
	url := startEmbeddedNATS(t)

	var buf bytes.Buffer
	old := writer
	setWriter(&buf)
	defer setWriter(old)

	err := runKVPut([]string{
		"--nats-urls", url,
		"--rate", "10",
		"--duration", "500ms",
		"--storage", "memory",
		"--replicas", "1",
	})
	require.NoError(t, err)

	row := parseCSVRow(t, strings.TrimSpace(buf.String()))
	require.Len(t, row, 9)
	require.Equal(t, "kv-put", row[0], "path")
	requireNonZeroInt64(t, row[7], "write_rpc_ops should be non-zero for kv-put")
}

// TestSmoke_KVGet verifies that kv-get emits a row with path="kv-get" and
// a non-zero read_rpc_ops counter.
func TestSmoke_KVGet(t *testing.T) {
	url := startEmbeddedNATS(t)

	var buf bytes.Buffer
	old := writer
	setWriter(&buf)
	defer setWriter(old)

	err := runKVGet([]string{
		"--nats-urls", url,
		"--rate", "10",
		"--duration", "500ms",
		"--storage", "memory",
		"--replicas", "1",
	})
	require.NoError(t, err)

	row := parseCSVRow(t, strings.TrimSpace(buf.String()))
	require.Len(t, row, 9)
	require.Equal(t, "kv-get", row[0], "path")
	requireNonZeroInt64(t, row[6], "read_rpc_ops should be non-zero for kv-get")
}

// TestSmoke_KVKeys verifies that kv-keys emits one row per population size
// with path="kv-keys-k<N>" and a non-zero read_rpc_ops counter (Keys is a read op).
func TestSmoke_KVKeys(t *testing.T) {
	url := startEmbeddedNATS(t)

	var buf bytes.Buffer
	old := writer
	setWriter(&buf)
	defer setWriter(old)

	err := runKVKeys([]string{
		"--nats-urls", url,
		"--keys", "50",
		"--samples", "5",
		"--storage", "memory",
		"--replicas", "1",
	})
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 1, "expected one row for one population")
	row := parseCSVRow(t, lines[0])
	require.Len(t, row, 9)
	require.Equal(t, "kv-keys-k50", row[0], "path should encode population")
	require.Equal(t, "0", row[3], "rate should be 0 for kv-keys")
	requireNonZeroInt64(t, row[6], "read_rpc_ops should be non-zero for kv-keys")
}

// TestSmoke_KVWatch verifies that kv-watch emits one row per watcher count
// with path="kv-watch-w<W>", non-zero write_rpc_ops (puts were issued), and
// non-zero read_rpc_ops (Watch calls were counted).
func TestSmoke_KVWatch(t *testing.T) {
	url := startEmbeddedNATS(t)

	var buf bytes.Buffer
	old := writer
	setWriter(&buf)
	defer setWriter(old)

	err := runKVWatch([]string{
		"--nats-urls", url,
		"--watchers", "1",
		"--rate", "10",
		"--duration", "500ms",
		"--storage", "memory",
		"--replicas", "1",
	})
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 1, "expected one row for one watcher count")
	row := parseCSVRow(t, lines[0])
	require.Len(t, row, 9)
	require.Equal(t, "kv-watch-w1", row[0], "path should encode watcher count")
	requireNonZeroInt64(t, row[6], "read_rpc_ops should be non-zero (Watch calls)")
	requireNonZeroInt64(t, row[7], "write_rpc_ops should be non-zero (Put calls)")
}

// TestSmoke_IdlePull verifies that idle-pull emits a row with the
// M4.1-extended schema and the path/c_stream columns populated.
// We do not assert on RPC counters: embedded NATS may produce
// internal protocol traffic that surfaces differently from a real
// cluster, and the wrapper RPC counters are an ancillary signal —
// the plan's authoritative metric is the externally-captured block
// IOPS (see package doc).
func TestSmoke_IdlePull(t *testing.T) {
	url := startEmbeddedNATS(t)

	var buf bytes.Buffer
	old := writer
	setWriter(&buf)
	defer setWriter(old)

	err := runIdlePull([]string{
		"--nats-urls", url,
		"--c-stream", "2",
		"--fetch-timeout", "1s",
		"--data-storage", "memory",
		"--replicas", "1",
		"--duration", "500ms",
	})
	require.NoError(t, err)

	row := parseCSVRow(t, strings.TrimSpace(buf.String()))
	require.Len(t, row, 16, "expected 16 CSV columns for idle-pull (incl. leader_samples_count + leader_samples)")
	require.Equal(t, "idle-pull-c2-t1s", row[0], "path")
	require.Equal(t, "memory", row[1], "storage")
	require.Equal(t, "1", row[2], "replicas")
	require.Equal(t, "0", row[3], "rate is 0 for idle-pull")
	require.Equal(t, "0", row[5], "bytes_per_op is 0 for idle-pull")
	require.Equal(t, "2", row[9], "c_stream column")
	require.Equal(t, "1.000", row[10], "fetch_timeout_s column")
	require.Equal(t, "false", row[13], "leader_changed should be false on stable embedded NATS")
	// row[14] is leader_samples_count; row[15] is the ;-joined samples
	// list (quoted by the CSV writer). On embedded single-node NATS
	// every sample is the literal "single" so the count may be 0+ and
	// each entry, if present, equals "single".
}

// TestSmoke_IdlePull_ZeroConsumers covers the M4.1 baseline grid point:
// C_stream=0 (stream exists, no consumers attached).
func TestSmoke_IdlePull_ZeroConsumers(t *testing.T) {
	url := startEmbeddedNATS(t)

	var buf bytes.Buffer
	old := writer
	setWriter(&buf)
	defer setWriter(old)

	err := runIdlePull([]string{
		"--nats-urls", url,
		"--c-stream", "0",
		"--fetch-timeout", "1s",
		"--data-storage", "memory",
		"--replicas", "1",
		"--duration", "200ms",
	})
	require.NoError(t, err)

	row := parseCSVRow(t, strings.TrimSpace(buf.String()))
	require.Len(t, row, 16)
	require.Equal(t, "idle-pull-c0-t1s", row[0], "path")
	require.Equal(t, "0", row[9], "c_stream=0 baseline")
	require.Equal(t, "false", row[13], "leader_changed false")
}

// TestSmoke_IdlePull_ColumnCount_AfterPolling guards the schema width
// against silent drift now that runIdlePull emits leader_samples_count
// + leader_samples columns.
func TestSmoke_IdlePull_ColumnCount_AfterPolling(t *testing.T) {
	url := startEmbeddedNATS(t)

	var buf bytes.Buffer
	old := writer
	setWriter(&buf)
	defer setWriter(old)

	// Disable abort-on-leader-move: embedded NATS sometimes returns a
	// transient stream.Info error during a fast 100ms cadence, which
	// LeaderMoved surfaces as a move per Rule 10. The schema-width
	// invariant is independent of move detection, so we accept either
	// outcome and only assert the column count + samples-count > 0.
	err := runIdlePull([]string{
		"--nats-urls", url,
		"--c-stream", "0",
		"--fetch-timeout", "1s",
		"--data-storage", "memory",
		"--replicas", "1",
		"--duration", "300ms",
		"--leader-poll-interval", "100ms",
		"--abort-on-leader-move=false",
	})
	require.NoError(t, err)

	row := parseCSVRow(t, strings.TrimSpace(buf.String()))
	require.Len(t, row, 16)
	// At least two samples should land in 300ms with a 100ms cadence.
	require.NotEqual(t, "0", row[14], "leader_samples_count should be > 0 with --leader-poll-interval 100ms over 300ms")
}

// TestSmoke_IdlePull_LeaderMoveDetection — skipped because embedded
// single-node NATS has no peers to fail over to, so we cannot reliably
// force a leader change. Real-cluster validation lives in the grid
// sweep script (Phase 5 driver), which retries grid points whose
// leader_changed column is true. The detection code path is exercised
// indirectly via leaderPre==leaderPost being "single".
func TestSmoke_IdlePull_LeaderMoveDetection(t *testing.T) {
	t.Skip("embedded single-node NATS cannot force a leader change; covered by grid-sweep script in Phase 5")
}

// requireNonZeroInt64 parses s as an int64 and fails the test if it is <= 0.
func requireNonZeroInt64(t *testing.T, s, msg string) {
	t.Helper()
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	require.NoError(t, err, "parse %q", s)
	require.Greater(t, n, int64(0), msg)
}

// unused — ensures nats import compiles in test builds.
var _ = nats.DefaultURL
