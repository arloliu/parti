package coordinator

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestEncodeDecodeIPCFrame_RoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		kind    string
		payload any
	}{
		{
			name: "received_message",
			kind: IPCKindReceivedMessage,
			payload: IPCPayloadReceivedMessage{
				PartitionID:       42,
				PartitionSequence: 12345,
			},
		},
		{
			name: "assignment_report",
			kind: IPCKindAssignmentReport,
			payload: IPCPayloadAssignmentReport{
				Partitions: []int{1, 3, 7, 12},
			},
		},
		{
			name: "start_latency",
			kind: IPCKindStartLatency,
			payload: IPCPayloadStartLatency{
				LatencyMS:       250,
				IsInitialCohort: true,
			},
		},
		{
			name:    "state",
			kind:    IPCKindState,
			payload: IPCPayloadState{State: 4},
		},
		{
			name:    "leader",
			kind:    IPCKindLeader,
			payload: IPCPayloadLeader{IsLeader: true},
		},
		{
			name:    "degraded",
			kind:    IPCKindDegraded,
			payload: IPCPayloadDegraded{Reason: "source_unavailable"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line, err := EncodeIPCFrame(tc.kind, "worker-3", "stable-7", 1700000000000, tc.payload)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if !strings.HasPrefix(line, IPCSentinel) {
				t.Fatalf("missing sentinel: %q", line)
			}
			if !strings.HasSuffix(line, "\n") {
				t.Fatalf("missing trailing newline: %q", line)
			}
			trimmed := strings.TrimSuffix(line, "\n")
			frame, err := DecodeIPCLine(trimmed)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if frame.V != IPCSchemaVersion {
				t.Fatalf("schema version: got %d want %d", frame.V, IPCSchemaVersion)
			}
			if frame.Kind != tc.kind {
				t.Fatalf("kind: got %q want %q", frame.Kind, tc.kind)
			}
			if frame.WorkerID != "worker-3" || frame.StableID != "stable-7" || frame.AtMS != 1700000000000 {
				t.Fatalf("envelope mismatch: %+v", frame)
			}
			// Verify payload round-trip: marshal expected, compare bytes.
			want, _ := json.Marshal(tc.payload)
			if string(want) != string(frame.Payload) {
				t.Fatalf("payload bytes: got %s want %s", string(frame.Payload), string(want))
			}
		})
	}
}

func TestDecodeIPCLine_NonIPC(t *testing.T) {
	_, err := DecodeIPCLine("some random log line")
	if !errors.Is(err, ErrNotIPC) {
		t.Fatalf("expected ErrNotIPC, got %v", err)
	}
}

func TestDecodeIPCLine_SchemaMismatch(t *testing.T) {
	// Manually construct a v=2 line.
	line := IPCSentinel + `{"v":2,"kind":"state","worker_id":"w","at":0,"payload":{}}`
	_, err := DecodeIPCLine(line)
	if err == nil || !strings.Contains(err.Error(), "schema mismatch") {
		t.Fatalf("expected schema mismatch error, got %v", err)
	}
}

func TestIPCSmokeOracle_AllKindsSeen_NoViolation(t *testing.T) {
	now := time.Now()
	clock := now
	oracle := NewIPCSmokeOracle(5*time.Second, func() time.Time { return clock })

	for _, k := range smokeKinds {
		oracle.Ingest("worker-0", k, now)
	}
	clock = now.Add(10 * time.Second)
	oracle.Check()
	if v := oracle.Violations(); v != 0 {
		t.Fatalf("expected 0 violations, got %d", v)
	}
}

func TestIPCSmokeOracle_MissingKinds_CountedOnceEach(t *testing.T) {
	now := time.Now()
	clock := now
	oracle := NewIPCSmokeOracle(5*time.Second, func() time.Time { return clock })

	// Only one kind seen.
	oracle.Ingest("worker-0", IPCKindAssignmentReport, now)
	// Window not yet closed.
	clock = now.Add(2 * time.Second)
	oracle.Check()
	if v := oracle.Violations(); v != 0 {
		t.Fatalf("violations during open window: %d", v)
	}
	// Window closed.
	clock = now.Add(10 * time.Second)
	oracle.Check()
	if v := oracle.Violations(); v != 3 {
		t.Fatalf("expected 3 missing-kind violations, got %d", v)
	}
	// Idempotent: re-check does not double count.
	oracle.Check()
	if v := oracle.Violations(); v != 3 {
		t.Fatalf("expected idempotent 3 violations, got %d", v)
	}
}

func TestIPCSmokeOracle_PerWorker(t *testing.T) {
	now := time.Now()
	clock := now
	oracle := NewIPCSmokeOracle(5*time.Second, func() time.Time { return clock })

	for _, k := range smokeKinds {
		oracle.Ingest("worker-good", k, now)
	}
	oracle.Ingest("worker-bad", IPCKindState, now)

	clock = now.Add(10 * time.Second)
	oracle.Check()
	if v := oracle.Violations(); v != 3 {
		t.Fatalf("expected 3 violations from bad worker only, got %d", v)
	}
}

func TestProcessWorkerObserver_Concurrent(t *testing.T) {
	obs := NewProcessWorkerObserver()
	obs.SetState(int(4))
	obs.SetLeader(true)
	obs.SetStableID("stable-9")
	if obs.WorkerStateInt() != 4 || !obs.IsLeader() || obs.StableWorkerID() != "stable-9" {
		t.Fatalf("unexpected observer state: state=%d leader=%v id=%s", obs.WorkerStateInt(), obs.IsLeader(), obs.StableWorkerID())
	}
}
