package natsutil

import (
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// StartEmbeddedNATS starts an embedded NATS server with JetStream.
//
// Returns:
//   - *server.Server: Running NATS server
//   - *nats.Conn: Client connection to the server
//   - error: Error if startup fails
func StartEmbeddedNATS() (*server.Server, *nats.Conn, error) {
	opts := &server.Options{
		JetStream: true,
		Port:      -1, // Random port
		StoreDir:  "", // In-memory
	}

	ns, err := server.NewServer(opts)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create NATS server: %w", err)
	}

	go ns.Start()

	if !ns.ReadyForConnections(10 * 1000000000) { // 10 seconds
		return nil, nil, errors.New("NATS server not ready")
	}

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		ns.Shutdown()
		return nil, nil, fmt.Errorf("failed to connect to NATS: %w", err)
	}

	return ns, nc, nil
}

// CreateStream creates a JetStream stream for simulation.
//
// Parameters:
//   - nc: NATS connection
//   - partitionCount: Number of partitions
//
// Returns:
//   - error: Error if stream creation fails
func CreateStream(nc *nats.Conn, partitionCount int) error {
	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("failed to get JetStream context: %w", err)
	}

	// Create stream that matches all partition subjects
	subjects := make([]string, partitionCount)
	for i := 0; i < partitionCount; i++ {
		subjects[i] = fmt.Sprintf("simulation.partition.%d", i)
	}

	streamConfig := &nats.StreamConfig{
		Name:      "SIMULATION",
		Subjects:  subjects,
		Storage:   nats.MemoryStorage,     // Use memory storage for embedded mode
		Retention: nats.WorkQueuePolicy,   // Auto-delete after acknowledged
		MaxMsgs:   10000000,               // Limit total messages (10M)
		MaxBytes:  3 * 1024 * 1024 * 1024, // 3GB limit (leave 1GB for overhead)
		MaxAge:    24 * time.Hour,         // Auto-delete messages older than 24h
	}

	// Try to delete existing stream first
	_ = js.DeleteStream("SIMULATION")

	_, err = js.AddStream(streamConfig)
	if err != nil {
		return fmt.Errorf("failed to create stream: %w", err)
	}

	// Clean up KV buckets from previous runs
	if err := cleanupKVBuckets(js); err != nil {
		return fmt.Errorf("failed to cleanup KV buckets: %w", err)
	}

	return nil
}

// cleanupKVBuckets removes all KV buckets from previous simulation runs.
func cleanupKVBuckets(js nats.JetStreamContext) error {
	// List of KV buckets used by parti
	buckets := []string{
		"parti-stableid",
		"parti-election",
		"parti-heartbeat",
		"parti-assignment",
	}

	for _, bucket := range buckets {
		// Delete bucket (ignore error if it doesn't exist)
		_ = js.DeleteKeyValue(bucket)
	}

	return nil
}
