# Parti Documentation

**Last Updated**: November 2, 2025

## 📍 Start Here

**New to Parti? Read these first:**

1. **[USER_GUIDE.md](USER_GUIDE.md)** - Complete user guide with examples and best practices
2. **[API_REFERENCE.md](API_REFERENCE.md)** - Detailed API documentation
3. **[OPERATIONS.md](OPERATIONS.md)** - Deployment and operations guide

---

## 📂 Documentation Structure

### User Documentation (NEW - Priority 2 Complete ✅)

#### 🚀 [User Guide](USER_GUIDE.md)
**For developers integrating Parti into applications**
- Getting started with quick examples
- Core concepts and worker lifecycle
- Configuration guide with templates
- Assignment strategies comparison
- Partition source implementations
- Hooks and callbacks patterns
- Best practices and troubleshooting

#### � [API Reference](API_REFERENCE.md)
**For detailed interface documentation**
- Complete Manager interface documentation
- All core interfaces (AssignmentStrategy, PartitionSource, ElectionAgent, etc.)
- Configuration types and validation
- Data structures (State, Partition, Assignment)
- Strategy and source packages
- Error types and handling
- Thread safety guarantees

#### ⚙️ [Operations Guide](OPERATIONS.md)
**For deployment and production operations**
- Prerequisites and requirements
- Configuration templates (dev, staging, production)
- Deployment patterns (Kubernetes, Docker, standalone)
- Health check implementation
- Observability foundation (logging, metrics)
- Common operational procedures
- Troubleshooting guide

---

### Performance & Benchmarking
- **[MEMORY_BENCHMARK_QUICKSTART.md](MEMORY_BENCHMARK_QUICKSTART.md)** - Quick reference guide
  - 5-minute comparison test
  - How external NATS isolation works
  - When to use each approach
  - Troubleshooting and FAQ

### Design Documentation (Reference)
Design documents are organized by category in `design/` subdirectories:

- **01-requirements/** - Requirements and constraints
  - [overview.md](design/01-requirements/overview.md)
  - [constraints.md](design/01-requirements/constraints.md)
  - [performance-goals.md](design/01-requirements/performance-goals.md)

- **02-problem-analysis/** - Problem domain analysis
  - [kafka-limitations.md](design/02-problem-analysis/kafka-limitations.md)

- **03-architecture/** - System architecture
  - [high-level-design.md](design/03-architecture/high-level-design.md)
  - [state-machine.md](design/03-architecture/state-machine.md)
  - [data-flow.md](design/03-architecture/data-flow.md)
  - [design-decisions.md](design/03-architecture/design-decisions.md)

- **04-components/** - Component details
  - [README.md](design/04-components/README.md)
  - [defender/overview.md](design/04-components/defender/overview.md)
  - [message-broker/](design/04-components/message-broker/)

- **05-operational-scenarios/** - Operational behavior
  - [cold-start.md](design/05-operational-scenarios/cold-start.md)
  - [rolling-update.md](design/05-operational-scenarios/rolling-update.md)
  - [scale-up.md](design/05-operational-scenarios/scale-up.md)
  - [crash-recovery.md](design/05-operational-scenarios/crash-recovery.md)

- **06-implementation/** - Implementation guidelines
  - [project-structure.md](design/06-implementation/project-structure.md)
  - [test-organization.md](design/06-implementation/test-organization.md)
  - [testing-guidelines.md](design/06-implementation/testing-guidelines.md)

---

## 🛠️ Development Workflow

```bash
# Run unit tests
make test-unit

# Run integration tests (requires longer time)
make test-integration

# Run all tests
make test-all

# Build examples
cd examples/basic && go build

# Check for errors
make lint
```

---

## 📖 Philosophy

**Test first, document later. Be honest about status.**

1. Implementation and verification BEFORE documentation
2. Integration tests prove distributed behavior
3. Honest assessment over aspirational claims
4. Production-ready means thoroughly tested
5. One phase at a time - don't advance until proven

---

## 📝 Document Status Legend

- ✅ **Complete**: Implemented and verified
- 🟡 **Partial**: Implemented but not fully tested
- 🔴 **Incomplete**: Missing or not implemented
- ⏳ **Planned**: On the roadmap
- ⚪ **Optional**: Nice-to-have
