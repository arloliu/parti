// Package main demonstrates wiring Parti's OnDegraded hook to a k8s
// readiness probe.
//
// When NATS data loss is sustained long enough for the manager to enter
// Degraded mode (e.g. a bucket wipe while the worker is running), the
// readiness probe flips unhealthy. Kubernetes then stops routing traffic
// to this pod and, depending on your Deployment's updateStrategy, may
// restart it — which is the correct recovery, since Parti's restart path
// recreates missing buckets via ensureKVBucket.
//
// The probe endpoint is GET /readyz:
//   - 200 OK while the manager is Stable / Scaling / Rebalancing / Emergency
//   - 503 Service Unavailable while in Degraded or Shutdown
//
// See docs/OPERATIONS.md "Live NATS data loss" for the underlying runbook.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func main() {
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = nats.DefaultURL
	}

	nc, err := nats.Connect(natsURL)
	if err != nil {
		log.Fatalf("failed to connect to NATS: %v", err)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		log.Fatalf("failed to init JetStream: %v", err)
	}

	cfg := parti.Config{
		WorkerIDPrefix: "readiness-demo",
		WorkerIDMin:    0,
		WorkerIDMax:    9,
	}

	partitions := []parti.Partition{
		{Keys: []string{"partition-0"}, Weight: 100},
		{Keys: []string{"partition-1"}, Weight: 100},
		{Keys: []string{"partition-2"}, Weight: 100},
	}

	// ready is the single source of truth for the readiness probe. It is
	// set to 0 on Degraded entry and back to 1 on Degraded exit. Writes
	// happen from hook goroutines; reads happen from the HTTP handler.
	var ready atomic.Int32
	ready.Store(1)

	hooks := &parti.Hooks{
		OnDegraded: func(_ context.Context, reason string) error {
			log.Printf("entered Degraded: %s — failing readiness probe", reason)
			ready.Store(0)
			return nil
		},
		OnStateChanged: func(_ context.Context, from, to types.State) error {
			// Transitioning OUT of Degraded back to a normal state means the
			// manager recovered (e.g. NATS connectivity came back). Restore
			// readiness so traffic resumes.
			if from == types.StateDegraded && to != types.StateDegraded {
				log.Printf("left Degraded: %s → %s — restoring readiness", from, to)
				ready.Store(1)
			}
			return nil
		},
	}

	mgr, err := parti.NewManager(&cfg, js, source.NewStatic(partitions), strategy.NewConsistentHash(), parti.WithHooks(hooks))
	if err != nil {
		log.Fatalf("failed to create manager: %v", err)
	}

	startCtx, startCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer startCancel()
	if err := mgr.Start(startCtx); err != nil {
		log.Fatalf("failed to start manager: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready.Load() == 1 {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, "ok")
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, "degraded")
	})

	probeSrv := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
	}
	go func() {
		log.Printf("readiness probe listening on %s/readyz", probeSrv.Addr)
		if err := probeSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("probe server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := probeSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("probe shutdown error: %v", err)
	}
	if err := mgr.Stop(shutdownCtx); err != nil {
		log.Printf("manager shutdown error: %v", err)
	}
}
