package testutil

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

var (
	// binaryCache caches the compiled NATS server binary path
	binaryCache     string
	binaryCacheLock sync.Mutex
)

// findModuleRoot finds the Go module root directory by looking for go.mod.
//
// Returns:
//   - string: Path to module root directory
//   - error: Error if go.mod not found
func findModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}

	// Walk up directory tree looking for go.mod
	for {
		goModPath := filepath.Join(dir, "go.mod")
		if _, err := os.Stat(goModPath); err == nil {
			return dir, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached filesystem root without finding go.mod
			return "", errors.New("go.mod not found")
		}
		dir = parent
	}
}

// getOrBuildNATSBinary returns path to compiled NATS server binary.
// Binary is cached in temp directory and reused across test runs.
//
// The cache key is based on SHA256 hash of the source file, ensuring
// the binary is automatically rebuilt when source code changes.
//
// Returns:
//   - string: Path to compiled binary
//   - error: Any error during compilation or cache access
func getOrBuildNATSBinary() (string, error) {
	binaryCacheLock.Lock()
	defer binaryCacheLock.Unlock()

	// Return cached binary if already compiled
	if binaryCache != "" {
		if _, err := os.Stat(binaryCache); err == nil {
			return binaryCache, nil
		}
	}

	// Find module root (directory containing go.mod)
	moduleRoot, err := findModuleRoot()
	if err != nil {
		return "", fmt.Errorf("failed to find module root: %w", err)
	}

	// Compute stable cache path based on source code hash
	// This ensures binary is rebuilt if source changes
	sourceFile := filepath.Join(moduleRoot, "test/cmd/nats-server/main.go")
	sourceHash, err := hashFile(sourceFile)
	if err != nil {
		return "", fmt.Errorf("failed to hash source: %w", err)
	}

	// Use system temp dir with hash-based name for caching across test runs
	cachePath := filepath.Join(os.TempDir(),
		fmt.Sprintf("parti-nats-server-%s", sourceHash[:16]))

	// Check if cached binary exists and is valid
	if _, err := os.Stat(cachePath); err == nil {
		binaryCache = cachePath
		return cachePath, nil
	}

	// Compile NATS server binary from module root
	//nolint:noctx // Context not applicable for test utility compilation
	cmd := exec.Command("go", "build", "-o", cachePath, sourceFile)
	cmd.Dir = moduleRoot // Ensure we're in the module root for compilation
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("compilation failed: %w\nOutput: %s", err, output)
	}

	binaryCache = cachePath

	return cachePath, nil
}

// hashFile computes SHA256 hash of file for cache key.
//
// Parameters:
//   - path: Path to file to hash
//
// Returns:
//   - string: Hex-encoded SHA256 hash
//   - error: Any error reading file
func hashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)

	return fmt.Sprintf("%x", hash), nil
}

// StartExternalNATS starts a NATS server in a separate process.
//
// This provides process-isolated NATS server for accurate memory benchmarking
// of the parti library without NATS server overhead contaminating measurements.
//
// The compiled binary is cached in the system temp directory and reused across
// test runs, making this approach nearly as fast as embedded NATS after first run.
//
// Benefits over embedded NATS:
//   - Process-isolated memory measurements (no NATS overhead in parti metrics)
//   - Accurate goroutine counts for parti library only
//   - Better reflects production setup (external NATS)
//   - Still fast due to binary caching (milliseconds after first compile)
//
// The function automatically registers cleanup with t.Cleanup(), so no manual
// defer is needed. The NATS server will be gracefully shutdown when the test
// completes.
//
// Parameters:
//   - t: Testing context for logging and cleanup
//
// Returns:
//   - *nats.Conn: Connected client to external NATS server
//   - func(): Cleanup function (also registered with t.Cleanup())
//
// Example:
//
//	nc, cleanup := testutil.StartExternalNATS(t)
//	defer cleanup()
//
//	// Now measure parti worker memory without NATS overhead
//	monitor := NewResourceMonitor()
//	monitor.Start(1 * time.Second)
//	// ... run parti workers ...
//	report := monitor.Stop()
func StartExternalNATS(t *testing.T) (*nats.Conn, func()) {
	t.Helper()

	// Get or compile NATS server binary (cached)
	binaryPath, err := getOrBuildNATSBinary()
	require.NoError(t, err, "Failed to get NATS server binary")

	// Start NATS server in separate process
	ctx, cancel := context.WithCancel(context.Background())
	natsCmd := exec.CommandContext(ctx, binaryPath)

	// Capture stdout to get connection URL
	stdout, err := natsCmd.StdoutPipe()
	require.NoError(t, err)

	// Capture stderr for debugging
	stderr, err := natsCmd.StderrPipe()
	require.NoError(t, err)

	err = natsCmd.Start()
	require.NoError(t, err)

	// Parse connection URL from stdout
	scanner := bufio.NewScanner(stdout)
	var natsURL string
	ready := false

	// Timeout for server startup
	startTimeout := time.NewTimer(10 * time.Second)
	defer startTimeout.Stop()

	resultChan := make(chan bool)
	go func() {
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "NATS_URL=") {
				natsURL = strings.TrimPrefix(line, "NATS_URL=")
			}
			if strings.Contains(line, "NATS_READY=true") {
				resultChan <- true
				return
			}
		}
		resultChan <- false
	}()

	select {
	case ready = <-resultChan:
	case <-startTimeout.C:
		// Timeout - capture stderr for debugging
		stderrData, _ := io.ReadAll(stderr)
		cancel()
		_ = natsCmd.Wait()
		require.Fail(t, "NATS server startup timeout", "stderr: %s", stderrData)
	}

	require.True(t, ready, "NATS server failed to start")
	require.NotEmpty(t, natsURL, "Failed to parse NATS URL")

	// Connect to external NATS
	nc, err := nats.Connect(natsURL,
		nats.MaxReconnects(-1),
		nats.ReconnectWait(100*time.Millisecond))
	require.NoError(t, err, "Failed to connect to external NATS at %s", natsURL)

	// Setup JetStream
	js, err := nc.JetStream()
	require.NoError(t, err, "Failed to get JetStream context")

	// Verify JetStream is working
	_, err = js.AccountInfo()
	require.NoError(t, err, "JetStream not responding")

	cleanup := func() {
		nc.Close()
		cancel() // Send SIGTERM to NATS process
		_ = natsCmd.Wait()
	}

	// Register cleanup with testing.T
	t.Cleanup(cleanup)

	return nc, cleanup
}

// StartExternalNATSWithContext starts external NATS with custom context.
//
// This variant allows caller to control the server lifecycle independently
// of test cleanup, useful for benchmarks that need precise control over
// when the server is stopped.
//
// Parameters:
//   - ctx: Context for controlling server lifetime
//   - t: Testing context for assertions (can be nil for non-test usage)
//
// Returns:
//   - *nats.Conn: Connected client
//   - string: NATS server URL
//   - func(): Cleanup function
//   - error: Any error during startup
//
// Example:
//
//	ctx, cancel := context.WithCancel(context.Background())
//	defer cancel()
//
//	nc, url, cleanup, err := StartExternalNATSWithContext(ctx, t)
//	if err != nil {
//	    t.Fatal(err)
//	}
//	defer cleanup()
//
//nolint:revive // Test utility needs all 4 return values (conn, url, cleanup, error)
func StartExternalNATSWithContext(ctx context.Context, t *testing.T) (*nats.Conn, string, func(), error) {
	// Get or compile NATS server binary (cached)
	binaryPath, err := getOrBuildNATSBinary()
	if err != nil {
		return nil, "", nil, fmt.Errorf("failed to get NATS binary: %w", err)
	}

	// Start NATS server in separate process
	natsCmd := exec.CommandContext(ctx, binaryPath)

	// Capture stdout to get connection URL
	stdout, err := natsCmd.StdoutPipe()
	if err != nil {
		return nil, "", nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	if err := natsCmd.Start(); err != nil {
		return nil, "", nil, fmt.Errorf("failed to start NATS server: %w", err)
	}

	// Parse connection URL from stdout with timeout
	scanner := bufio.NewScanner(stdout)
	var natsURL string
	var ready bool

	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()

	resultChan := make(chan bool)
	go func() {
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "NATS_URL=") {
				natsURL = strings.TrimPrefix(line, "NATS_URL=")
			}
			if strings.Contains(line, "NATS_READY=true") {
				resultChan <- true
				return
			}
		}
		resultChan <- false
	}()

	select {
	case ready = <-resultChan:
	case <-timeout.C:
		_ = natsCmd.Process.Kill()
		_ = natsCmd.Wait()
		return nil, "", nil, errors.New("NATS server startup timeout")
	}

	if !ready || natsURL == "" {
		_ = natsCmd.Process.Kill()
		_ = natsCmd.Wait()
		return nil, "", nil, errors.New("failed to get NATS URL from server output")
	}

	// Connect to external NATS
	nc, err := nats.Connect(natsURL,
		nats.MaxReconnects(-1),
		nats.ReconnectWait(100*time.Millisecond))
	if err != nil {
		_ = natsCmd.Process.Kill()
		_ = natsCmd.Wait()
		return nil, "", nil, fmt.Errorf("failed to connect to NATS: %w", err)
	}

	cleanup := func() {
		nc.Close()
		_ = natsCmd.Process.Kill()
		_ = natsCmd.Wait()
	}

	return nc, natsURL, cleanup, nil
}

// CleanupNATSBinaryCache removes cached NATS server binary.
// Useful for testing or forcing recompilation.
//
// Returns:
//   - error: Any error during cleanup
//
// Example:
//
//	// Force recompilation for next test
//	err := testutil.CleanupNATSBinaryCache()
//	require.NoError(t, err)
func CleanupNATSBinaryCache() error {
	binaryCacheLock.Lock()
	defer binaryCacheLock.Unlock()

	if binaryCache != "" {
		if err := os.Remove(binaryCache); err != nil && !os.IsNotExist(err) {
			return err
		}
		binaryCache = ""
	}

	return nil
}
