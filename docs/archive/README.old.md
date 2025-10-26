# Parti Documentation

## Start Here 📍

### New to the Project?
1. **[STATUS.md](STATUS.md)** - Current implementation status (what works, what doesn't)
2. **[PRACTICAL_IMPLEMENTATION_PLAN.md](PRACTICAL_IMPLEMENTATION_PLAN.md)** - Execution plan for completing the library

### Planning Documents
- **[state-machine-implementation-plan.md](state-machine-implementation-plan.md)** - Detailed technical plan for state machine implementation
- **[NEXT_STEPS.md](NEXT_STEPS.md)** - Aspirational features (do AFTER implementation complete)

### Design Documents
See `design/` directory for architecture and component design.

## Document Status

| Document | Status | Purpose |
|----------|--------|---------|
| STATUS.md | ✅ Current | Honest assessment of what's done |
| PRACTICAL_IMPLEMENTATION_PLAN.md | ✅ Current | Execution roadmap (6 weeks) |
| state-machine-implementation-plan.md | ✅ Current | Technical state machine design |
| NEXT_STEPS.md | ⚠️ Aspirational | Future features (premature) |
| library-specification.md | ⚠️ Outdated | Original spec, needs update |
| migration-module-discussion.md | ⚠️ Outdated | Historical discussion |

## Quick Navigation

**Want to understand current status?**
→ Read [STATUS.md](STATUS.md)

**Want to know what to do next?**
→ Follow [PRACTICAL_IMPLEMENTATION_PLAN.md](PRACTICAL_IMPLEMENTATION_PLAN.md)

**Want to implement state machine?**
→ Use [state-machine-implementation-plan.md](state-machine-implementation-plan.md)

**Want to see future vision?**
→ Check [NEXT_STEPS.md](NEXT_STEPS.md) (but implement first!)

---

**Philosophy**: Test first, document later. Be honest about status.

---

## Old Documentation (Historical)

The content below is a high-level concept for describing a genereal distributed worker.

### 01. Requirements
Fundamental requirements, constraints, and performance goals.

- **[overview.md](./01-requirements/overview.md)** - System overview, scale, key requirements
- **[constraints.md](./01-requirements/constraints.md)** - Technical constraints, existing infrastructure
- **[performance-goals.md](./01-requirements/performance-goals.md)** - SLA targets, timing budgets, percentiles

**Start Here** if you're new to the project.

### 02. Problem Analysis
Analysis of why certain technologies don't fit our requirements.

- **[kafka-limitations.md](./02-problem-analysis/kafka-limitations.md)** - **★ Core Document**: Kafka consumer group rebalancing pain points
- **[operational-challenges.md](./02-problem-analysis/operational-challenges.md)** - Cold start, scaling, crash recovery challenges
- **[comparison-matrix.md](./02-problem-analysis/comparison-matrix.md)** - Kafka vs NATS side-by-side comparison

**Read This** to understand why NATS JetStream was chosen over Kafka.

### 03. Architecture
High-level system design and key architectural decisions.

- **[high-level-design.md](./03-architecture/high-level-design.md)** - **★ Core Document**: System layers, architecture diagrams, data flow
- **[design-decisions.md](./03-architecture/design-decisions.md)** - Rationale for embedded controller, NATS, consistent hashing
- **[data-flow.md](./03-architecture/data-flow.md)** - Message flow, processing pipeline
- **[state-machine.md](./03-architecture/state-machine.md)** - 8-state cluster state machine

**Essential Reading** for understanding the system architecture.

### 04. Components
Detailed documentation for each system component.

#### Defender
- **[overview.md](./04-components/defender/overview.md)** - Defender process responsibilities
- **[implementation.md](./04-components/defender/implementation.md)** - **★ Core Document**: Go code structure, goroutines
- **[assignment-controller.md](./04-components/defender/assignment-controller.md)** - Embedded assignment controller logic
- **[cache-management.md](./04-components/defender/cache-management.md)** - SQLite cache, PVC persistence

#### Message Broker
- **[nats-jetstream-setup.md](./04-components/message-broker/nats-jetstream-setup.md)** - NATS configuration, streams, KV stores
- **[workqueue-pattern.md](./04-components/message-broker/workqueue-pattern.md)** - WorkQueue delivery semantics
- **[failover-behavior.md](./04-components/message-broker/failover-behavior.md)** - Push-based delivery, automatic failover

#### Storage
- **[cassandra-schema.md](./04-components/storage/cassandra-schema.md)** - T-charts and U-charts tables
- **[election-agent.md](./04-components/storage/election-agent.md)** - Leader election service (https://github.com/arloliu/election-agent)
- **[data-persistence.md](./04-components/storage/data-persistence.md)** - Data retention, archiving

#### Kubernetes
- **[statefulset-config.md](./04-components/kubernetes/statefulset-config.md)** - StatefulSet manifest, PVC setup
- **[scaling-strategy.md](./04-components/kubernetes/scaling-strategy.md)** - HPA configuration, scaling policies
- **[deployment-guide.md](./04-components/kubernetes/deployment-guide.md)** - Step-by-step deployment instructions

### 05. Operational Scenarios
Detailed walkthrough of key operational scenarios with timelines.

- **[cold-start.md](./05-operational-scenarios/cold-start.md)** - 0 → 30 defenders startup (45-60 seconds)
- **[scale-up.md](./05-operational-scenarios/scale-up.md)** - 30 → 45 defenders scaling (30-40 seconds)
- **[scale-down.md](./05-operational-scenarios/scale-down.md)** - Graceful defender removal
- **[crash-recovery.md](./05-operational-scenarios/crash-recovery.md)** - Emergency handling, failover (8-10 seconds)
- **[rolling-update.md](./05-operational-scenarios/rolling-update.md)** - **★ Core Document**: Version upgrade with zero rebalancing (30-75 seconds)

**Critical** for understanding system behavior in production.

### 06. Algorithms
Deep dive into assignment and load balancing algorithms.

- **[consistent-hashing.md](./06-algorithms/consistent-hashing.md)** - **★ Core Document**: Algorithm details, virtual nodes (150-200 per defender)
- **[assignment-strategy.md](./06-algorithms/assignment-strategy.md)** - Unified reassignment strategy for all scenarios
- **[affinity-preservation.md](./06-algorithms/affinity-preservation.md)** - Cache-aware movement minimization (80%+ preservation)

**Essential** for understanding load balancing logic.

### 07. Monitoring
Observability, metrics, alerts, and dashboards.

- **[metrics.md](./07-monitoring/metrics.md)** - Prometheus metrics definitions
- **[alerts.md](./07-monitoring/alerts.md)** - PrometheusRule configuration, alert thresholds
- **[dashboards.md](./07-monitoring/dashboards.md)** - Grafana dashboard specifications

**Required** for production operations.

### 08. Configuration
Configuration profiles, tuning, and security.

- **[profiles.md](./08-configuration/profiles.md)** - Production, staging, development configs
- **[tuning-guide.md](./08-configuration/tuning-guide.md)** - Performance tuning parameters
- **[security.md](./08-configuration/security.md)** - Authentication, authorization, network policies

**Reference** for deployment configuration.

### 09. Appendix
Reference materials and deep-dive analyses.

- **[glossary.md](./09-appendix/glossary.md)** - Terms and definitions
- **[reference-architecture.md](./09-appendix/reference-architecture.md)** - Complete V2 design (consolidated)
- **[kafka-deep-dive.md](./09-appendix/kafka-deep-dive.md)** - Detailed Kafka operational analysis with timelines

### 10. Implementation
**Complete implementation examples and code samples.**

- **[complete-example.md](./10-implementation/complete-example.md)** - **★ Full Implementation**: End-to-end working code
- **[testing-guide.md](./10-implementation/testing-guide.md)** - Unit tests, integration tests, chaos tests
- **[deployment-automation.md](./10-implementation/deployment-automation.md)** - CI/CD pipelines, automation scripts

## Quick Start Guides

### For New Developers
1. Start with [Requirements Overview](./01-requirements/overview.md)
2. Read [Kafka Limitations](./02-problem-analysis/kafka-limitations.md) to understand the "why"
3. Study [High-Level Design](./03-architecture/high-level-design.md) for system architecture
4. Review [Defender Implementation](./04-components/defender/implementation.md) for code structure
5. Work through [Complete Implementation Example](./10-implementation/complete-example.md)

### For Operators
1. Read [Performance Goals](./01-requirements/performance-goals.md) for SLA targets
2. Review [Operational Scenarios](./05-operational-scenarios/) for production behavior
3. Configure [Monitoring and Alerts](./07-monitoring/)
4. Follow [Deployment Guide](./04-components/kubernetes/deployment-guide.md)

### For Architects
1. Review [Design Decisions](./03-architecture/design-decisions.md) for rationale
2. Study [Consistent Hashing Algorithm](./06-algorithms/consistent-hashing.md)
3. Analyze [Operational Scenarios](./05-operational-scenarios/) for trade-offs
4. Read [Reference Architecture](./09-appendix/reference-architecture.md) for complete design

## Key Concepts

### Chamber-Level Assignment
System assigns at **chamber level** (`tool_id + chamber_id`), not tool level, providing finer-grained load balancing across 2,400 processing units.

### Embedded Assignment Controller
Assignment controller runs as **goroutine within leader defender**, eliminating need for separate coordinator service. Automatic failover on leader crash.

### Weighted Consistent Hashing
Chambers assigned using **consistent hashing** with workload weights (`SVID_count × freq × duration`). Minimizes movement during scaling (80%+ chambers unchanged).

### Zero-Rebalancing Rolling Updates
During rolling updates, defenders keep **same ID = same assignments**. Cache preserved via PVC. Zero rebalancing events.

### 8-State State Machine
Cluster state machine with 8 states (ColdStart, Stabilizing, Stable, ScalingUp, ScalingDown, Emergency, Rebalancing, RollingUpdate) prevents thrashing.

## Performance Highlights

| Metric | Target | Achievement |
|--------|--------|-------------|
| Process Time (p95) | < 5s | ~1.2-1.5s typical |
| Cold Start (30 defenders) | < 2 min | 45-60 seconds |
| Scale Up (30→45) | < 1 min | 30-40 seconds |
| Crash Recovery | < 15s | 8-10 seconds |
| Rolling Update | < 2 min | 30-75 seconds |
| Cache Hit Rate | > 95% | 97-99% stable |
| Chamber Preservation | > 80% | 80-95% scale up |

## Technology Stack

- **Language**: Go (Golang)
- **Message Broker**: NATS JetStream
- **Storage**: Cassandra (T-charts, U-charts)
- **Leader Election**: Election Agent (https://github.com/arloliu/election-agent)
- **Cache**: SQLite (in-memory with PVC)
- **Orchestration**: Kubernetes StatefulSet with HPA
- **Monitoring**: Prometheus + Grafana + AlertManager

## Design Principles

1. **Self-Coordinating**: No external coordinators (Zookeeper/Consul)
2. **Stability over Speed**: Dampening mechanisms prevent thrashing
3. **Cache Affinity**: Minimize chamber movement to preserve cache locality
4. **Graceful Degradation**: System continues operating during transitions
5. **Observable**: Rich Prometheus metrics for all operations

## Production Readiness

This design is **production-ready** with:
- ✓ Complete implementation examples (Go)
- ✓ Kubernetes manifests (StatefulSet, ConfigMap, Service)
- ✓ Monitoring setup (Prometheus, Grafana, PrometheusRule)
- ✓ Operational runbooks for all scenarios
- ✓ Performance testing guidelines
- ✓ Disaster recovery procedures

## Contributing

When adding new documentation:
1. Follow the hierarchical structure
2. Update this README with new documents
3. Cross-reference related documents
4. Include diagrams (Mermaid preferred)
5. Provide code examples where applicable

## Document Conventions

- **★ Core Document**: Essential reading for understanding the system
- **[Related Doc](./path)**: Cross-references to related content
- Code blocks use language-specific syntax highlighting
- Diagrams use Mermaid (sequence, flowchart, state machine)
- Tables for comparison and specifications

## Revision History

| Version | Date | Description |
|---------|------|-------------|
| V2.0 | 2025-10-24 | Chamber-level assignment, weighted consistent hashing, Election Agent |
| V1.0 | 2025-10-15 | Initial design with equipment-level assignment |

## Support

For questions or clarifications:
- Review [Glossary](./09-appendix/glossary.md) for terminology
- Check [Reference Architecture](./09-appendix/reference-architecture.md) for consolidated design
- Refer to source design document: `../architecture_design_v2.md`

---

**Document Status**: ✅ Complete and Production-Ready

**Last Updated**: October 24, 2025
