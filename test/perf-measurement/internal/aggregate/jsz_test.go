package aggregate

import (
	"strings"
	"testing"
)

// TestParseJSZ_UsesLastSeqNotLiveMessageCount is a reproducer for the
// empty stream_msgs_* / stream_bytes_* columns observed in real
// S1/S2 campaign runs (results/s1-e1, results/s2-e8): every parti KV
// bucket (heartbeat, stableid, election, handoff, ...) uses History=1,
// so JetStream evicts a key's prior revision on every Put — the
// bucket's LIVE `state.messages` count settles at the key population
// (e.g. one key per worker) and stops changing once the cluster is
// steady, even though the bucket keeps being written at the
// configured cadence. `state.last_seq`, by contrast, is the
// cumulative count of every message the stream has ever appended
// (including ones since evicted by the per-subject retention limit)
// and keeps climbing.
//
// The fixture below reproduces the exact shape observed in
// results/s1-e1/run-005-E1.b-N2000-rep1/jsz.raw for KV_parti-heartbeat:
// messages pinned at 50 (= worker count) while last_seq climbs 50,
// 100, 150 across three 5s polls.
//
// Before the fix, ParseJSZ read `messages`, so JSZSample.Msgs was
// [50,50,50] and JSZRates derived a MsgsRate of 0 at every tick —
// the "empty" column the operator observed. After the fix, ParseJSZ
// reads `last_seq`, JSZSample.Msgs is [50,100,150], and JSZRates
// derives a nonzero rate.
func TestParseJSZ_UsesLastSeqNotLiveMessageCount(t *testing.T) {
	const ndjson = `{"t_unix_ns":1000000000,"node":"localhost:8222","endpoint":"jsz","body":{"account_details":[{"stream_detail":[{"name":"KV_parti-heartbeat","state":{"messages":50,"bytes":11676,"last_seq":50}}]}]}}
{"t_unix_ns":6000000000,"node":"localhost:8222","endpoint":"jsz","body":{"account_details":[{"stream_detail":[{"name":"KV_parti-heartbeat","state":{"messages":50,"bytes":11677,"last_seq":100}}]}]}}
{"t_unix_ns":11000000000,"node":"localhost:8222","endpoint":"jsz","body":{"account_details":[{"stream_detail":[{"name":"KV_parti-heartbeat","state":{"messages":50,"bytes":11676,"last_seq":150}}]}]}}
`

	samples, err := parseJSZReader(strings.NewReader(ndjson))
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 3 {
		t.Fatalf("expected 3 samples, got %d", len(samples))
	}

	wantMsgs := []uint64{50, 100, 150}
	for i, s := range samples {
		if s.Msgs != wantMsgs[i] {
			t.Fatalf("sample[%d].Msgs = %d, want %d (last_seq, not the flat live-message count 50)", i, s.Msgs, wantMsgs[i])
		}
	}

	rates := JSZRates(samples)
	if len(rates) == 0 {
		t.Fatal("JSZRates produced no rows for a stream with growing last_seq")
	}
	var sawNonZero bool
	for _, r := range rates {
		if r.MsgsRate != 0 {
			sawNonZero = true
			// 50 messages / 5s = 10 msgs/s for both poll gaps.
			if r.MsgsRate != 10 {
				t.Fatalf("MsgsRate = %v, want 10 (Δlast_seq=50 / Δt=5s)", r.MsgsRate)
			}
		}
	}
	if !sawNonZero {
		t.Fatal("MsgsRate was 0 at every tick — regressed to reading the flat live-message count instead of last_seq")
	}
}

// TestParseJSZ_MissingLastSeqDefaultsToZero documents the fallback for a
// /jsz response that (for whatever server-version reason) omits
// last_seq: ParseJSZ must not panic or misparse, it just reports 0 —
// same as any other omitted uint64 JSON field.
func TestParseJSZ_MissingLastSeqDefaultsToZero(t *testing.T) {
	const ndjson = `{"t_unix_ns":1000000000,"node":"localhost:8222","endpoint":"jsz","body":{"account_details":[{"stream_detail":[{"name":"perf-rig-data","state":{"messages":0,"bytes":0}}]}]}}
`
	samples, err := parseJSZReader(strings.NewReader(ndjson))
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 {
		t.Fatalf("expected 1 sample, got %d", len(samples))
	}
	if samples[0].Msgs != 0 {
		t.Fatalf("Msgs = %d, want 0", samples[0].Msgs)
	}
}
