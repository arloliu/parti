// Package main provides a standalone NATS server for isolated memory benchmarking.package natsserver

// This server runs in a separate process to isolate NATS server memory overhead
// from parti library measurements. It uses net.Listen to obtain a random available
// port and outputs connection information to stdout for the parent test process.
package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nats-io/nats-server/v2/server"
)

func main() {
	// Get random available port using net.Listen
	// This is the idiomatic Go way to obtain a free port
	//nolint:noctx // Context not applicable for test utility main
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal("Failed to get available port:", err)
	}

	// Extract the assigned port
	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		log.Fatal("Failed to get TCP address from listener")
	}
	port := tcpAddr.Port

	// Close listener - NATS server will bind to this port
	// Small race condition window, but acceptable for tests
	_ = listener.Close() // Best effort, error not critical

	// Create temporary directory for JetStream storage
	tempDir := filepath.Join(os.TempDir(), fmt.Sprintf("nats-test-%d", os.Getpid()))
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		log.Fatal("Failed to create temp directory:", err)
	}

	// Cleanup temp directory on exit
	defer func() {
		_ = os.RemoveAll(tempDir) // Best effort cleanup
	}()

	// Create NATS server with JetStream
	opts := &server.Options{
		Host:      "127.0.0.1",
		Port:      port,
		JetStream: true,
		StoreDir:  tempDir,
		// Disable logging to reduce noise
		NoLog:  true,
		NoSigs: true, // We handle signals ourselves
	}

	srv, err := server.NewServer(opts)
	if err != nil {
		// Use os.Exit instead of log.Fatal to allow deferred cleanup
		_, _ = fmt.Fprintf(os.Stderr, "Failed to create NATS server: %v\n", err)
		os.Exit(1) //nolint:gocritic // OS will clean up temp directory on process exit
	}

	// Start server
	go srv.Start()

	// Wait for server to be ready
	if !srv.ReadyForConnections(10 * time.Second) {
		_, _ = fmt.Fprintln(os.Stderr, "NATS server not ready within timeout")
		os.Exit(1)
	}

	// Write connection info to stdout for parent process
	// Parent process parses this to get the connection URL
	fmt.Printf("NATS_URL=nats://%s:%d\n", opts.Host, opts.Port)
	fmt.Println("NATS_READY=true")
	_, _ = fmt.Fprintf(os.Stderr, "NATS server started on port %d (PID: %d)\n", port, os.Getpid())

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	_, _ = fmt.Fprintln(os.Stderr, "Shutting down NATS server...")

	// Graceful shutdown
	srv.Shutdown()
	srv.WaitForShutdown()

	_, _ = fmt.Fprintln(os.Stderr, "NATS server stopped")
}
