package producer

import "encoding/json"

// SimulationMessage represents a message sent by producers.
type SimulationMessage struct {
	PartitionID       int    `json:"partition_id"`       // 0-1499
	ProducerID        string `json:"producer_id"`        // "producer-0"
	PartitionSequence int64  `json:"partition_sequence"` // Per-partition sequence: 0, 1, 2, 3...
	ProducerSequence  int64  `json:"producer_sequence"`  // Per-producer sequence: 0, 1, 2, 3...
	Timestamp         int64  `json:"timestamp"`          // Unix microseconds
	Weight            int64  `json:"weight"`             // Partition weight (100 or 1)
}

// Marshal serializes the message to JSON.
//
// Returns:
//   - []byte: JSON-encoded message
//   - error: Encoding error, if any
func (m *SimulationMessage) Marshal() ([]byte, error) {
	return json.Marshal(m)
}

// Unmarshal deserializes a message from JSON.
//
// Parameters:
//   - data: JSON-encoded message bytes
//
// Returns:
//   - *SimulationMessage: Decoded message
//   - error: Decoding error, if any
func Unmarshal(data []byte) (*SimulationMessage, error) {
	var msg SimulationMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}

	return &msg, nil
}
