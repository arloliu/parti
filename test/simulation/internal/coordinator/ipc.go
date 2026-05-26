package coordinator

// Phase 7a — process-mode IPC.
//
// Workers spawned via ProcessManager.StartWorker emit observability frames as
// NDJSON over stdout, each line prefixed with the IPCSentinel constant. The
// orchestrator's StartWorker reader strips the sentinel, decodes the JSON
// envelope, and forwards each frame into the same coordinator channels the
// all-in-one harness drives (assignmentsCh, receivedCh, startLatenciesCh,
// degradedReportsCh) plus a per-worker ProcessWorkerObserver shim that backs
// the registry-polling oracles (LeaderUniquenessWatcher, StateReconcileWatcher).
//
// Schema is versioned (IPCSchemaVersion); decoder rejects unknown versions so
// a future format change is not silently misinterpreted.

import (
	"encoding/json"
	"fmt"
)

const (
	// IPCSentinel prefixes every IPC NDJSON line on stdout so the
	// orchestrator-side reader can separate IPC frames from regular log
	// output. The reader emits a single warn-and-drop log on any non-IPC
	// line so debugging visibility is preserved.
	IPCSentinel = "__PARTI_IPC__"

	// IPCSchemaVersion is the on-wire schema version. Bump on any
	// breaking change to IPCFrame's shape; the decoder errors on
	// mismatches so a stale binary cannot silently corrupt oracles.
	IPCSchemaVersion = 1
)

// IPC frame discriminators. Each maps to a distinct payload schema; see
// IPCDecodePayload for the typed payload structs.
const (
	// IPCKindReceivedMessage carries a single message arrival
	// (partition_id, partition_seq).
	IPCKindReceivedMessage = "received_message"
	// IPCKindAssignmentReport carries the latest assignment snapshot
	// (partitions list).
	IPCKindAssignmentReport = "assignment_report"
	// IPCKindStartLatency carries the Start-to-first-assignment latency.
	IPCKindStartLatency = "start_latency"
	// IPCKindState carries the manager state as an int (parti.State).
	IPCKindState = "state"
	// IPCKindLeader carries the IsLeader() bool snapshot.
	IPCKindLeader = "leader"
	// IPCKindDegraded carries an OnDegraded reason string.
	IPCKindDegraded = "degraded"
)

// IPCFrame is the on-wire envelope. All fields are tagged for json so an
// off-the-shelf json.Encoder/Decoder round-trips faithfully.
//
// Payload is raw json so each kind can carry its own typed sub-struct without
// inflating the envelope.
type IPCFrame struct {
	V        int             `json:"v"`
	Kind     string          `json:"kind"`
	WorkerID string          `json:"worker_id"`
	StableID string          `json:"stable_id,omitempty"`
	AtMS     int64           `json:"at"`
	Payload  json.RawMessage `json:"payload"`
}

// IPCPayloadReceivedMessage carries one message arrival.
type IPCPayloadReceivedMessage struct {
	PartitionID       int   `json:"partition_id"`
	PartitionSequence int64 `json:"partition_seq"`
}

// IPCPayloadAssignmentReport carries the worker's partition list.
type IPCPayloadAssignmentReport struct {
	Partitions []int `json:"partitions"`
}

// IPCPayloadStartLatency carries the Start-to-first-assignment latency.
type IPCPayloadStartLatency struct {
	LatencyMS       int64 `json:"latency_ms"`
	IsInitialCohort bool  `json:"is_initial_cohort"`
}

// IPCPayloadState carries the worker state as a parti.State int.
type IPCPayloadState struct {
	State int `json:"state"`
}

// IPCPayloadLeader carries the leader flag.
type IPCPayloadLeader struct {
	IsLeader bool `json:"is_leader"`
}

// IPCPayloadDegraded carries the OnDegraded reason.
type IPCPayloadDegraded struct {
	Reason string `json:"reason"`
}

// EncodeIPCFrame builds a single NDJSON line ready for stdout. The returned
// string includes the sentinel prefix and a trailing newline.
//
// payload is marshalled with encoding/json; callers should pass one of the
// IPCPayload* structs. Any json marshal error is returned (caller drops the
// frame and logs).
func EncodeIPCFrame(kind, workerID, stableID string, atMS int64, payload any) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("ipc payload marshal: %w", err)
	}
	frame := IPCFrame{
		V:        IPCSchemaVersion,
		Kind:     kind,
		WorkerID: workerID,
		StableID: stableID,
		AtMS:     atMS,
		Payload:  raw,
	}
	out, err := json.Marshal(&frame)
	if err != nil {
		return "", fmt.Errorf("ipc envelope marshal: %w", err)
	}
	return IPCSentinel + string(out) + "\n", nil
}

// DecodeIPCLine parses one NDJSON line emitted by EncodeIPCFrame. The line
// must include the sentinel prefix; lines without the prefix return
// (nil, ErrNotIPC) so callers can route plain log lines elsewhere.
//
// A wrong schema version returns an error so a stale binary surfaces loudly
// rather than corrupting oracle counters.
func DecodeIPCLine(line string) (*IPCFrame, error) {
	if len(line) < len(IPCSentinel) || line[:len(IPCSentinel)] != IPCSentinel {
		return nil, ErrNotIPC
	}
	body := line[len(IPCSentinel):]
	var frame IPCFrame
	if err := json.Unmarshal([]byte(body), &frame); err != nil {
		return nil, fmt.Errorf("ipc envelope unmarshal: %w", err)
	}
	if frame.V != IPCSchemaVersion {
		return nil, fmt.Errorf("ipc schema mismatch: got v=%d, want v=%d", frame.V, IPCSchemaVersion)
	}
	return &frame, nil
}

// ErrNotIPC signals that a stdout line lacked the IPC sentinel prefix and
// should be routed to the log passthrough rather than the IPC reader.
var ErrNotIPC = errNotIPC{}

type errNotIPC struct{}

func (errNotIPC) Error() string { return "not an IPC frame (no sentinel prefix)" }
