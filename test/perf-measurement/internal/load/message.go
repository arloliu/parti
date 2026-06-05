package load

import (
	"encoding/binary"
	"fmt"
)

// PayloadSize is the fixed wire size of every message (~256 B "small
// message", design §5). A 24-byte header carries the fields; the rest is
// zero padding so payload size is constant across the matrix.
const PayloadSize = 256

const headerSize = 24 // 3 × int64

// Message is the producer payload. IntendedMonoNs is the SCHEDULED send
// instant (CLOCK_MONOTONIC ns), not the actual publish instant — latency
// is recv−intended so producer lateness is captured (coordinated-omission
// correction, design §5).
type Message struct {
	IntendedMonoNs int64
	Seq            int64
	PartitionIndex int64
}

// Encode serialises m into a PayloadSize byte slice (little-endian header
// + zero padding).
func (m Message) Encode() []byte {
	b := make([]byte, PayloadSize)
	// G115 false positives: int64->uint64 is a lossless fixed-width bit
	// reinterpret for the wire header; Decode reverses it exactly.
	binary.LittleEndian.PutUint64(b[0:8], uint64(m.IntendedMonoNs))   //nolint:gosec // G115: lossless wire round-trip
	binary.LittleEndian.PutUint64(b[8:16], uint64(m.Seq))             //nolint:gosec // G115: lossless wire round-trip
	binary.LittleEndian.PutUint64(b[16:24], uint64(m.PartitionIndex)) //nolint:gosec // G115: lossless wire round-trip
	return b
}

// Decode parses the header from a payload. Padding is ignored.
func Decode(b []byte) (Message, error) {
	if len(b) < headerSize {
		return Message{}, fmt.Errorf("payload too short: %d < %d", len(b), headerSize)
	}
	// G115 false positives: uint64->int64 reverses the lossless round-trip
	// written by Encode (same fixed-width bit pattern).
	return Message{
		IntendedMonoNs: int64(binary.LittleEndian.Uint64(b[0:8])),   //nolint:gosec // G115: lossless wire round-trip
		Seq:            int64(binary.LittleEndian.Uint64(b[8:16])),  //nolint:gosec // G115: lossless wire round-trip
		PartitionIndex: int64(binary.LittleEndian.Uint64(b[16:24])), //nolint:gosec // G115: lossless wire round-trip
	}, nil
}
