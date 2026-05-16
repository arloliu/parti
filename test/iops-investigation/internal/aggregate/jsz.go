package aggregate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
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

// jszBody is the minimal subset of /jsz output we read. NATS includes
// account_details[*].stream_detail[*] with state.messages / state.bytes.
type jszBody struct {
	AccountDetails []struct {
		StreamDetail []struct {
			Name  string `json:"name"`
			State struct {
				Messages uint64 `json:"messages"`
				Bytes    uint64 `json:"bytes"`
			} `json:"state"`
		} `json:"stream_detail"`
	} `json:"account_details"`
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
	return parseJSZ(f)
}

func parseJSZ(r io.Reader) ([]JSZSample, error) {
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
	sort.Slice(out, func(i, j int) bool {
		if out[i].TSec != out[j].TSec {
			return out[i].TSec < out[j].TSec
		}
		return out[i].Stream < out[j].Stream
	})
	return out
}
