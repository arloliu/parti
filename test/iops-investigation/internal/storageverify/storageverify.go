// Package storageverify confirms that every JetStream stream the rig
// cares about carries the storage class and replica count specified in
// the run manifest. It exists because Parti v2.3.0 does not expose
// storage class as a public configuration option — the harness must
// pre-create streams with the desired class and then verify the
// realised configuration before starting any measurement, so a silent
// misconfiguration cannot confound the M5 memory-KV control runs (see
// docs/plans/iops-investigation/00-attribution-plan.md §M5 and the
// "Storage class is not Parti-public on v2.3.0" risk note).
package storageverify

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
)

// Expected declares the storage configuration the rig requires for one
// JetStream stream.
type Expected struct {
	// Stream is the JetStream stream name.
	Stream string
	// Storage is the expected storage type (FileStorage or MemoryStorage).
	Storage jetstream.StorageType
	// Replicas is the expected replication factor.
	Replicas int
}

// Verify checks every entry in expected against the live stream
// configuration reported by the NATS server. It collects all
// mismatches and returns them joined via [errors.Join], so the caller
// gets a single error that names every offending stream, the expected
// value, and the actual value. Returns nil when every stream matches.
func Verify(ctx context.Context, js jetstream.JetStream, expected []Expected) error {
	var errs []error

	for _, e := range expected {
		s, err := js.Stream(ctx, e.Stream)
		if err != nil {
			errs = append(errs, fmt.Errorf("stream %q: lookup failed: %w", e.Stream, err))
			continue
		}

		info, err := s.Info(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("stream %q: info failed: %w", e.Stream, err))
			continue
		}

		cfg := info.Config
		if cfg.Storage != e.Storage {
			errs = append(errs, fmt.Errorf(
				"stream %q: storage mismatch: expected %s, got %s",
				e.Stream, e.Storage, cfg.Storage,
			))
		}
		if cfg.Replicas != e.Replicas {
			errs = append(errs, fmt.Errorf(
				"stream %q: replicas mismatch: expected %d, got %d",
				e.Stream, e.Replicas, cfg.Replicas,
			))
		}
	}

	return errors.Join(errs...)
}
