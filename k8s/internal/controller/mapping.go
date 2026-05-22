package controller

import (
	"fmt"

	"github.com/arloliu/parti/v2/k8s/api/v1alpha1"
	"github.com/arloliu/parti/v2/provision"
)

// toProvisionConfig maps a ProvisionedPartiEnvSpec to a provision.Config.
//
// It is a near-pure function: it stamps APIVersion, copies scalar fields, and
// recursively maps each nested struct. It does no semantic validation —
// provision.Validate owns that.
//
// The only error path is the History narrowing conversion (int32 in the CRD
// vs uint8 in the SDK). The CRD OpenAPI Maximum=255 bound makes an
// out-of-range value unreachable through a real API server, but the fake
// client used in tests does not enforce OpenAPI, so the mapper guards the
// conversion explicitly.
func toProvisionConfig(spec v1alpha1.ProvisionedPartiEnvSpec) (provision.Config, error) {
	cfg := provision.Config{
		APIVersion: provision.APIVersionV1,
		Instance:   spec.Instance,
		Policy:     provision.ReconcilePolicy(spec.Policy),
	}

	if spec.ControlPlane != nil {
		cp := spec.ControlPlane
		cfg.ControlPlane = &provision.ControlPlaneConfig{
			StableIDBucket:        cp.StableIDBucket,
			ElectionBucket:        cp.ElectionBucket,
			HeartbeatBucket:       cp.HeartbeatBucket,
			AssignmentBucket:      cp.AssignmentBucket,
			HandoffBucket:         cp.HandoffBucket,
			WorkerIDTTL:           cp.WorkerIDTTL.Duration,
			ElectionTimeout:       cp.ElectionTimeout.Duration,
			HeartbeatTTL:          cp.HeartbeatTTL.Duration,
			AssignmentTTL:         cp.AssignmentTTL.Duration,
			HandoffTTL:            cp.HandoffTTL.Duration,
			EnableTwoPhaseHandoff: cp.EnableTwoPhaseHandoff,
			Replicas:              int(cp.Replicas),
			AllowDeleteRecreate:   cp.AllowDeleteRecreate,
		}
	}

	if spec.PartitionSource != nil {
		ps := spec.PartitionSource
		if ps.History < 0 || ps.History > 255 {
			return provision.Config{}, fmt.Errorf("partitionSource.history %d out of range [0,255]", ps.History)
		}
		cfg.PartitionSource = &provision.PartitionSourceConfig{
			Bucket:              ps.Bucket,
			Key:                 ps.Key,
			Replicas:            int(ps.Replicas),
			Storage:             ps.Storage,
			History:             uint8(ps.History),
			MaxValueSize:        ps.MaxValueSize,
			TTL:                 ps.TTL.Duration,
			AllowDeleteRecreate: ps.AllowDeleteRecreate,
			// Partitions deliberately omitted — managed by partictl, not the operator.
		}
	}

	if len(spec.Streams) > 0 {
		cfg.Streams = make([]provision.StreamCfg, len(spec.Streams))
		for i, s := range spec.Streams {
			cfg.Streams[i] = provision.StreamCfg{
				Name:                s.Name,
				Subjects:            s.Subjects,
				Retention:           s.Retention,
				Storage:             s.Storage,
				Discard:             s.Discard,
				Replicas:            int(s.Replicas),
				MaxAge:              s.MaxAge.Duration,
				MaxBytes:            s.MaxBytes,
				MaxMsgs:             s.MaxMsgs,
				Description:         s.Description,
				AllowDeleteRecreate: s.AllowDeleteRecreate,
			}
		}
	}

	// DynamicConsumers deliberately omitted — not managed by the operator (Non-Goal).

	return cfg, nil
}
