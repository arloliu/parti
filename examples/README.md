# Examples

This directory contains runnable examples demonstrating Parti features.

## Available Examples

### [basic/](basic/)

Minimal Parti setup: connects to NATS, creates a static partition source and
consistent-hash strategy, starts a Manager with hooks, and waits for Ctrl-C.

**Demonstrates:** Config, static partitions, hooks, Manager lifecycle.

```bash
# Requires a running NATS server with JetStream enabled
NATS_URL=nats://localhost:4222 go run ./examples/basic
```

### [kv-watcher/](kv-watcher/)

Explores NATS JetStream KV watch semantics (specific key, wildcard, bucket-level)
using an embedded NATS server. Useful for understanding the KV primitives that
Parti's `source.NatsKV` builds upon.

**Demonstrates:** KV watch patterns, embedded NATS for testing.

```bash
go run ./examples/kv-watcher
```

## See Also

- [Example tests in the root package](../example_test.go) — Godoc-visible examples for
  `NewManager`, `DefaultConfig`, `SetDefaults`, and `NewCompositeConsumerUpdater`.
- [User Guide](../docs/USER_GUIDE.md) — Step-by-step introduction and core concepts.
