package metrics

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// PrometheusServer serves Prometheus metrics via HTTP.
type PrometheusServer struct {
	addr      string
	collector *Collector
	server    *http.Server
}

// NewPrometheusServer creates a new Prometheus metrics server.
//
// Parameters:
//   - addr: Address to listen on (e.g., ":9090")
//   - collector: Metrics collector to expose
//
// Returns:
//   - *PrometheusServer: Initialized server
func NewPrometheusServer(addr string, collector *Collector) *PrometheusServer {
	return &PrometheusServer{
		addr:      addr,
		collector: collector,
	}
}

// Start starts the Prometheus HTTP server.
//
// Parameters:
//   - ctx: Context for cancellation
//
// Returns:
//   - error: Error if server fails to start
func (s *PrometheusServer) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/health", s.healthHandler)

	s.server = &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Start periodic system metrics collection
	go s.collectSystemMetrics(ctx)

	log.Printf("[Metrics] Starting Prometheus server on %s", s.addr)

	go func() {
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[Metrics] Server error: %v", err)
		}
	}()

	// Wait for context cancellation
	<-ctx.Done()

	return s.Shutdown()
}

// Shutdown gracefully shuts down the server.
//
// Returns:
//   - error: Error if shutdown fails
func (s *PrometheusServer) Shutdown() error {
	log.Println("[Metrics] Shutting down Prometheus server")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return s.server.Shutdown(ctx)
}

// healthHandler handles health check requests.
func (s *PrometheusServer) healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "OK\n")
}

// collectSystemMetrics periodically collects system-level metrics.
func (s *PrometheusServer) collectSystemMetrics(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var m runtime.MemStats
			runtime.ReadMemStats(&m)

			goroutines := runtime.NumGoroutine()
			memoryBytes := m.Alloc

			s.collector.UpdateSystemMetrics(goroutines, memoryBytes)
		}
	}
}
