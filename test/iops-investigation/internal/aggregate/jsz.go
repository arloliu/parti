package aggregate

import (
	"bufio"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
)

// JSZSample is one snapshot of a single stream's cumulative
// (messages, bytes) at one jsz poll tick.
type JSZSample struct {
	TUnixNs int64
	Stream  string
	Msgs    uint64
	Bytes   uint64
}

// StreamRate is the per-second forward-filled rate for one stream over
// one second-bucket. Aggregator emits stream_msgs_<name> /
// stream_bytes_<name> from these.
type StreamRate struct {
	TSec     int64
	Stream   string
	MsgsRate float64
	BytesRt  float64
}

// jszLine is the wire shape capture-jsz.sh writes.
type jszLine struct {
	TUnixNs  int64           `json:"t_unix_ns"`
	Node     string          `json:"node"`
	Endpoint string          `json:"endpoint"`
	Body     json.RawMessage `json:"body"`
}

// jszStreamState is the per-stream message/byte state in a /jsz body.
type jszStreamState struct {
	Messages uint64 `json:"messages"`
	Bytes    uint64 `json:"bytes"`
}

// jszStreamDetail is one stream entry under an account's stream_detail.
type jszStreamDetail struct {
	Name  string         `json:"name"`
	State jszStreamState `json:"state"`
}

// jszAccountDetail is one account entry under account_details.
type jszAccountDetail struct {
	StreamDetail []jszStreamDetail `json:"stream_detail"`
}

// jszMetaSnapshot is the metacontroller snapshot timing/size block.
type jszMetaSnapshot struct {
	PendingEntries int64  `json:"pending_entries"`
	PendingSize    int64  `json:"pending_size"`
	LastTime       string `json:"last_time"`
	LastDurationNs int64  `json:"last_duration"` // ns, omitempty (0 until first snapshot fires)
}

// jszMetaCluster is the meta_cluster block carrying the snapshot stats.
type jszMetaCluster struct {
	Snapshot jszMetaSnapshot `json:"snapshot"`
}

// jszBody is the minimal subset of /jsz output we read. NATS includes
// account_details[*].stream_detail[*] with state.messages / state.bytes,
// and meta_cluster.snapshot with metacontroller snapshot timing/size data.
type jszBody struct {
	AccountDetails []jszAccountDetail `json:"account_details"`
	MetaCluster    jszMetaCluster     `json:"meta_cluster"`
}

// MetaSnapshotSample holds one poll's metacontroller snapshot stats from
// the /jsz endpoint. LastDurationNs is 0 when the server has not yet
// taken a snapshot (last_duration is omitempty in the wire format).
type MetaSnapshotSample struct {
	TUnixNs        int64
	Node           string
	LastDurationNs int64
	PendingSize    int64
	PendingEntries int64
	LastTime       string
}

// ParseMetaSnapshot reads a capture-jsz.sh ndjson file and returns one
// MetaSnapshotSample per jsz poll line. varz lines are ignored. A missing
// last_duration field (omitempty on the wire) is treated as 0 — not an error.
func ParseMetaSnapshot(path string) ([]MetaSnapshotSample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open jsz: %w", err)
	}
	defer f.Close()
	return parseMetaSnapshotReader(f)
}

func parseMetaSnapshotReader(r io.Reader) ([]MetaSnapshotSample, error) {
	var out []MetaSnapshotSample
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var l jszLine
		if err := json.Unmarshal(raw, &l); err != nil {
			return nil, fmt.Errorf("jsz line %d: %w", lineNo, err)
		}
		if l.Endpoint != "jsz" {
			continue
		}
		var body jszBody
		if err := json.Unmarshal(l.Body, &body); err != nil {
			return nil, fmt.Errorf("jsz line %d body: %w", lineNo, err)
		}
		snap := body.MetaCluster.Snapshot
		out = append(out, MetaSnapshotSample{
			TUnixNs:        l.TUnixNs,
			Node:           l.Node,
			LastDurationNs: snap.LastDurationNs,
			PendingSize:    snap.PendingSize,
			PendingEntries: snap.PendingEntries,
			LastTime:       snap.LastTime,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan jsz: %w", err)
	}

	return out, nil
}

// MetaSnapshotCount returns the number of DISTINCT non-zero LastTime values
// across the samples. This counts how many distinct metacontroller snapshots
// fired during the capture window — used by the §8.1 "count ≥ 5" gate.
func MetaSnapshotCount(samples []MetaSnapshotSample) int {
	seen := map[string]struct{}{}
	for _, s := range samples {
		if s.LastTime != "" {
			seen[s.LastTime] = struct{}{}
		}
	}
	return len(seen)
}

// ParseJSZ reads a capture-jsz.sh ndjson file and returns the per-stream
// cumulative samples (one per stream per jsz poll). varz lines are
// ignored — they aren't used by the aggregator.
func ParseJSZ(path string) ([]JSZSample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open jsz: %w", err)
	}
	defer f.Close()
	return parseJSZReader(f)
}

func parseJSZReader(r io.Reader) ([]JSZSample, error) {
	var out []JSZSample
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var l jszLine
		if err := json.Unmarshal(raw, &l); err != nil {
			return nil, fmt.Errorf("jsz line %d: %w", lineNo, err)
		}
		if l.Endpoint != "jsz" {
			continue
		}
		var body jszBody
		if err := json.Unmarshal(l.Body, &body); err != nil {
			return nil, fmt.Errorf("jsz line %d body: %w", lineNo, err)
		}
		for _, acc := range body.AccountDetails {
			for _, sd := range acc.StreamDetail {
				out = append(out, JSZSample{
					TUnixNs: l.TUnixNs,
					Stream:  sd.Name,
					Msgs:    sd.State.Messages,
					Bytes:   sd.State.Bytes,
				})
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan jsz: %w", err)
	}

	return out, nil
}

// JSZRates converts per-poll cumulative samples into per-second
// forward-filled rates per stream. For each consecutive pair of polls
// on a stream we compute (Δmsgs, Δbytes) / Δseconds, then attribute
// that rate to every t_sec in [prev_t_sec+1, cur_t_sec].
func JSZRates(samples []JSZSample) []StreamRate {
	// Group by stream, preserving file order.
	bySt := map[string][]JSZSample{}
	for _, s := range samples {
		bySt[s.Stream] = append(bySt[s.Stream], s)
	}
	var out []StreamRate
	for _, g := range bySt {
		for i := 1; i < len(g); i++ {
			prev, cur := g[i-1], g[i]
			dtNs := cur.TUnixNs - prev.TUnixNs
			if dtNs <= 0 {
				continue
			}
			dtSec := float64(dtNs) / 1e9
			dMsgs := float64(safeDeltaUint(cur.Msgs, prev.Msgs)) / dtSec
			dBytes := float64(safeDeltaUint(cur.Bytes, prev.Bytes)) / dtSec
			prevSec := prev.TUnixNs / int64(1e9)
			curSec := cur.TUnixNs / int64(1e9)
			for t := prevSec + 1; t <= curSec; t++ {
				out = append(out, StreamRate{
					TSec: t, Stream: cur.Stream, MsgsRate: dMsgs, BytesRt: dBytes,
				})
			}
		}
	}
	slices.SortFunc(out, func(a, b StreamRate) int {
		if a.TSec != b.TSec {
			return cmp.Compare(a.TSec, b.TSec)
		}
		return cmp.Compare(a.Stream, b.Stream)
	})

	return out
}
