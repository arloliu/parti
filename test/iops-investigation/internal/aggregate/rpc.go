package aggregate

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
)

// RPCRow is one row from harness rpc_counts.csv:
// t_unix_ns,worker_idx,bucket,op,count where count is cumulative since
// Reset() was called at the start of the capture window.
type RPCRow struct {
	TUnixNs int64
	Worker  int
	Bucket  string
	Op      string
	Count   int64
}

// BucketRate is per-bucket read/write ops/s for one second-bucket.
type BucketRate struct {
	TSec      int64
	Bucket    string
	ReadOps   float64
	WriteOps  float64
}

// ParseRPC reads rpc_counts.csv and returns rows in file order.
func ParseRPC(path string) ([]RPCRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open rpc_counts: %w", err)
	}
	defer f.Close()
	return parseRPC(f)
}

func parseRPC(r io.Reader) ([]RPCRow, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	head, err := cr.Read()
	if err == io.EOF {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rpc header: %w", err)
	}
	want := []string{"t_unix_ns", "worker_idx", "bucket", "op", "count"}
	if len(head) != len(want) {
		return nil, fmt.Errorf("rpc header: want %v, got %v", want, head)
	}
	for i, h := range want {
		if head[i] != h {
			return nil, fmt.Errorf("rpc header col %d: want %q, got %q", i, h, head[i])
		}
	}
	var out []RPCRow
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("rpc read: %w", err)
		}
		if len(rec) != 5 {
			return nil, fmt.Errorf("rpc record: want 5 fields, got %d", len(rec))
		}
		ts, err := strconv.ParseInt(rec[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("rpc t_unix_ns: %w", err)
		}
		w, err := strconv.Atoi(rec[1])
		if err != nil {
			return nil, fmt.Errorf("rpc worker_idx: %w", err)
		}
		c, err := strconv.ParseInt(rec[4], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("rpc count: %w", err)
		}
		out = append(out, RPCRow{TUnixNs: ts, Worker: w, Bucket: rec[2], Op: rec[3], Count: c})
	}
	return out, nil
}

// readOps and writeOps classify each op name that the instrumentedjs
// wrapper emits into the M3 read-RPC vs write-mutation column.
//
// This table is intentionally exhaustive against
// internal/instrumentedjs/instrumentedjs.go's method list so a reviewer
// can diff the two sources line-by-line. Adding a new op there means
// updating one of these sets here; an unknown op is treated as a
// write (the more pessimistic attribution) and surfaced via
// UnknownOps for diagnostics.
//
// Plan §R3 classification:
//   - reads: Get, GetRevision, Keys, ListKeys, Watch, WatchAll, History,
//     Status, Bucket, AccountInfo, Stream.
//   - writes: Put, PutString, Create, Update, Delete, Purge,
//     CreateKeyValue, CreateOrUpdateKeyValue, UpdateKeyValue,
//     CreateStream, CreateOrUpdateConsumer, Publish*.
//
// Additions vs the plan text — kept consistent with the wrapper:
//   - reads: WatchFiltered, ListKeysFiltered (variants of the named ops),
//     KeyValue (lookup of an existing bucket; the *creation* path is
//     covered by CreateKeyValue / CreateOrUpdateKeyValue / UpdateKeyValue).
//   - writes: PurgeDeletes (bucket-wide purge), PublishMsg,
//     PublishAsync, PublishMsgAsync.
var (
	readOps = map[string]bool{
		"Get": true, "GetRevision": true,
		"Keys": true, "ListKeys": true, "ListKeysFiltered": true,
		"Watch": true, "WatchAll": true, "WatchFiltered": true,
		"History": true, "Status": true, "Bucket": true,
		"AccountInfo": true, "Stream": true,
		"KeyValue": true,
	}
	writeOps = map[string]bool{
		"Put": true, "PutString": true,
		"Create": true, "Update": true,
		"Delete": true, "Purge": true, "PurgeDeletes": true,
		"CreateKeyValue": true, "CreateOrUpdateKeyValue": true, "UpdateKeyValue": true,
		"CreateStream":           true,
		"CreateOrUpdateConsumer": true,
		"Publish":                true, "PublishMsg": true,
		"PublishAsync": true, "PublishMsgAsync": true,
	}
)

// CaptureStartNs returns the minimum t_unix_ns across the supplied rows,
// or 0 if rows is empty. The harness emits an initial snapshot
// immediately after Reset(), so the minimum row timestamp approximates
// the start of the capture window — see RPCBucketRates for how this is
// used as the synthetic-anchor for first-observed diffs.
func CaptureStartNs(rows []RPCRow) int64 {
	if len(rows) == 0 {
		return 0
	}
	mn := rows[0].TUnixNs
	for _, r := range rows[1:] {
		if r.TUnixNs < mn {
			mn = r.TUnixNs
		}
	}
	return mn
}

// ClassifyOp returns "read", "write", or "" (unknown) for an op name.
// Exported so tests can assert the classification matches the wrapper.
func ClassifyOp(op string) string {
	if readOps[op] {
		return "read"
	}
	if writeOps[op] {
		return "write"
	}
	return ""
}

// RPCBucketRates converts cumulative per-(worker, bucket, op) snapshots
// into per-second read/write ops rates per bucket, summed across
// workers.
//
// Cadence note: rpc_counts.csv emits one snapshot per RPCDumpInterval
// (default 1 s) containing the cumulative count for every present
// (worker, bucket, op). For each consecutive pair of snapshots at the
// same (worker, bucket, op) we compute (Δcount / Δseconds) and
// attribute that rate to every second-bucket in (prev_t_sec, cur_t_sec].
// This matches the JSZ forward-fill rule and keeps absent ticks at 0
// per the harness sparse-row contract.
//
// Capture-start anchor (Phase 3 P0 fix): The harness writes its first
// snapshot immediately after Reset(), so the minimum t_unix_ns in the
// file approximates the capture-window start (within milliseconds). We
// prepend a synthetic (captureStartNs, 0) row to every group so the
// first observed cumulative count for a (worker, bucket, op) is diffed
// against zero rather than treated as a baseline. Without this fix,
// ops first observed after the initial snapshot tick had their first
// interval silently dropped, undercounting RPC attribution columns.
//
// Zero-width edge case: when a group's first observed row is at
// captureStartNs itself with count=N>0 (i.e. ops fired in the sub-tick
// gap between Reset() and the initial snapshot write), the prepended
// row collides at dtNs=0 and is skipped — the same baseline behavior as
// the pre-fix code. This is accepted noise per the Phase 2 contract
// (sub-tick precision is not guaranteed).
//
// UnknownOps returns the sorted set of op names that were neither
// classified as read nor write — callers should log these for diagnosis.
//
// captureStartNs should be the timestamp of the harness's first snapshot
// (i.e. the minimum t_unix_ns across all rows). Pass 0 (or any value
// >= the first row's timestamp) to disable the synthetic-anchor behavior
// — callers normally derive it from rows[0].TUnixNs after sorting, or
// from a CaptureStartNs helper.
func RPCBucketRates(rows []RPCRow, captureStartNs int64) (rates []BucketRate, unknownOps []string) {
	// Group cumulative samples by (worker, bucket, op).
	type k3 struct{ w int; b, o string }
	groups := map[k3][]RPCRow{}
	for _, r := range rows {
		groups[k3{r.Worker, r.Bucket, r.Op}] = append(groups[k3{r.Worker, r.Bucket, r.Op}], r)
	}
	type bkey struct {
		t int64
		b string
	}
	acc := map[bkey]*BucketRate{}
	unknown := map[string]bool{}
	for k, g := range groups {
		cls := ClassifyOp(k.o)
		if cls == "" {
			unknown[k.o] = true
		}
		// Prepend a synthetic (captureStartNs, 0) anchor so the first
		// observed cumulative count is diffed against zero. If the group's
		// first row is already at captureStartNs the prepended row produces
		// dtNs=0 and is skipped by the loop below (same as pre-fix
		// behavior for that degenerate case).
		if captureStartNs > 0 && (len(g) == 0 || g[0].TUnixNs > captureStartNs) {
			g = append([]RPCRow{{TUnixNs: captureStartNs, Worker: k.w, Bucket: k.b, Op: k.o, Count: 0}}, g...)
		}
		for i := 1; i < len(g); i++ {
			prev, cur := g[i-1], g[i]
			dtNs := cur.TUnixNs - prev.TUnixNs
			if dtNs <= 0 {
				continue
			}
			dtSec := float64(dtNs) / 1e9
			delta := max(cur.Count-prev.Count, 0)
			rate := float64(delta) / dtSec
			prevSec := prev.TUnixNs / int64(1e9)
			curSec := cur.TUnixNs / int64(1e9)
			for t := prevSec + 1; t <= curSec; t++ {
				bk := bkey{t, k.b}
				br, ok := acc[bk]
				if !ok {
					br = &BucketRate{TSec: t, Bucket: k.b}
					acc[bk] = br
				}
				switch cls {
				case "read":
					br.ReadOps += rate
				case "write":
					br.WriteOps += rate
				default:
					// Unknown ops fold into the write column
					// (pessimistic attribution); surfaced via UnknownOps.
					br.WriteOps += rate
				}
			}
		}
	}
	rates = make([]BucketRate, 0, len(acc))
	for _, br := range acc {
		rates = append(rates, *br)
	}
	sort.Slice(rates, func(i, j int) bool {
		if rates[i].TSec != rates[j].TSec {
			return rates[i].TSec < rates[j].TSec
		}
		return rates[i].Bucket < rates[j].Bucket
	})
	unknownOps = make([]string, 0, len(unknown))
	for o := range unknown {
		unknownOps = append(unknownOps, o)
	}
	sort.Strings(unknownOps)
	return rates, unknownOps
}
