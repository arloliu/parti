# degraded-readiness

Wires Parti's `OnDegraded` hook to an HTTP `/readyz` endpoint so Kubernetes
rotates pods that cannot reach a healthy NATS state.

## Why this example exists

Parti does not auto-heal live NATS bucket loss — the recovery action is a
process restart, which recreates missing buckets via `ensureKVBucket`. The
`OnDegraded` hook is the trigger that lets a k8s readiness probe notice the
problem and stop sending work to the affected pod.

See `docs/OPERATIONS.md` → "Live NATS data loss (Bucket Wipe While Workers
Run)" for the full runbook.

## Running

```bash
# Requires a running NATS server with JetStream enabled.
NATS_URL=nats://localhost:4222 go run ./examples/degraded-readiness

# In another shell:
curl -i http://localhost:8080/readyz
# 200 OK while the manager is Stable.

# Simulate live NATS data loss:
nats kv rm parti-heartbeat --force

# Within ~2s, the manager enters Degraded and the probe flips to 503.
curl -i http://localhost:8080/readyz
# HTTP/1.1 503 Service Unavailable
```

## Kubernetes wiring

```yaml
readinessProbe:
  httpGet:
    path: /readyz
    port: 8080
  initialDelaySeconds: 5
  periodSeconds: 5
  failureThreshold: 3
```

With `failureThreshold: 3` and `periodSeconds: 5`, a Degraded pod is marked
NotReady within ~15s. Combined with a rolling-update Deployment, pods in this
state are rotated and restart on healthy NATS state.
