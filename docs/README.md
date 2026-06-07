# Parti Documentation

**Version**: 2.0.0
**Last Updated**: 2026-04-09

---

## 📍 Start Here

**New to Parti?** Start with the [Overview](README.md) and then move to the focused guides below.

### Quick Start

```go
nc, _ := nats.Connect(nats.DefaultURL)
js, _ := jetstream.New(nc)

cfg := &parti.Config{
    WorkerIDPrefix: "worker",
    WorkerIDMax:    99,
}
partitions := []parti.Partition{
    {Keys: []string{"0"}},
    {Keys: []string{"1"}},
    {Keys: []string{"2"}},
}
src := source.NewStatic(partitions)

mgr, _ := parti.NewManager(cfg, js, src, strategy.NewConsistentHash())
mgr.Start(context.Background())
```

---

## 📂 Documentation

### Getting Started

| Document                          | Description                                |
|-----------------------------------|--------------------------------------------|
| [Reference](REFERENCE.md)         | Hooks, errors, best practices, glossary    |
| [Architecture](ARCHITECTURE.md)   | System architecture, components, data flow |
| [Configuration](CONFIGURATION.md) | Configuration options, presets, tuning     |

### Core Features

| Document                                      | Description                                                 |
|-----------------------------------------------|-------------------------------------------------------------|
| [Lifecycle](LIFECYCLE.md)                     | Worker states, stable IDs, two-phase handoff, degraded mode |
| [Consumer Package](CONSUMERS.md)              | Queue, Static, Dynamic, Broadcast consumers; stream retention policy; storage tuning |
| [Strategies & Sources](STRATEGIES.md)         | Assignment strategies, partition sources                    |
| [Static Partitioning](STATIC_PARTITIONING.md) | Key-based routing plus static partition publisher/subscriber helpers |
| [Scaling](SCALING.md)                         | Bounded-cost partitioning: NATS `partition()` + `Dynamic` over a fixed K |

### Reference

| Document                          | Description                                     |
|-----------------------------------|-------------------------------------------------|
| [Reference](REFERENCE.md)         | Hooks, error handling, best practices, glossary |
| [API Reference](API_REFERENCE.md) | Detailed API documentation                      |
| [Operations Guide](OPERATIONS.md) | Deployment, monitoring, troubleshooting         |
| [Provision Guide](PROVISION.md)   | provision SDK and partictl CLI for NATS resource management |
| [Kubernetes Operator](KUBERNETES.md) | ProvisionedPartiEnv CRD, install steps, CRD reference |

### Migration Guides

| Document                                                | Description                                                              |
|---------------------------------------------------------|--------------------------------------------------------------------------|
| [Migrating from v1 to v2](MIGRATING_TO_V2.md)           | Module path, import renames, Manager/Config/Metrics/Partition changes    |
| [Migrating: `Manager.Start` returns at `StateWaitingAssignment`](MIGRATING_MANAGER_START.md) | Breaking change in the upcoming release — `Start` no longer blocks until `StateStable`; use `WaitState` |

---

## 🛠️ Development

```bash
make test-unit         # Run unit tests
make test-integration  # Run integration tests
make test-all          # Run all tests
make lint              # Check for errors
```

---

## 📖 Reading Order

1. **[Architecture](ARCHITECTURE.md)** - Concepts and data flow
2. **[Configuration](CONFIGURATION.md)** - Configure for your environment
3. **[Lifecycle](LIFECYCLE.md)** - Learn worker states and handoff
4. **[Consumer Package](CONSUMERS.md)** - Set up JetStream consumers
5. **[Strategies](STRATEGIES.md)** - Choose assignment strategy
6. **[Reference](REFERENCE.md)** - Hooks, errors, best practices
