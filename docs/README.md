# Parti Documentation

**Last Updated**: October 27, 2025

## 📍 Start Here

**New to Parti? Read these first:**

1. **[STATUS.md](STATUS.md)** - Current implementation status (what works, what doesn't)
2. **[PRACTICAL_IMPLEMENTATION_PLAN.md](PRACTICAL_IMPLEMENTATION_PLAN.md)** - Execution roadmap

**Current Reality**: ~97% complete - Phase 4 complete, reliability testing next

---

## 📂 Documentation Structure

### Implementation Planning (Current Focus)
- **[STATUS.md](STATUS.md)** - Honest status assessment
  - Component-by-component status (✅🟡🔴)
  - What works vs what's missing
  - Testing coverage gaps
  - Known blockers

- **[PRACTICAL_IMPLEMENTATION_PLAN.md](PRACTICAL_IMPLEMENTATION_PLAN.md)** - Actionable execution plan
  - 7 phases with clear tasks and deliverables
  - Timeline: 6-7 weeks to production-ready
  - Success criteria for each phase
  - Appendix with technical implementation details

### Performance & Benchmarking
- **[MEMORY_BENCHMARK_QUICKSTART.md](MEMORY_BENCHMARK_QUICKSTART.md)** - Quick reference guide
  - 5-minute comparison test
  - How external NATS isolation works
  - When to use each approach
  - Troubleshooting and FAQ

### Library Specification
- **[library-specification.md](library-specification.md)** - Complete API specification
  - Core interfaces and types
  - Configuration options
  - State machine design
  - Assignment strategies
  - Observability hooks

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

### Historical Context
- **[migration-module-discussion.md](migration-module-discussion.md)** - Original design discussion
  - Why "parti" name was chosen
  - Generic terminology decisions
  - Library design approach

---

## 🎯 Current Work

**Phase**: 0 - Foundation Audit ✅ COMPLETE
**Next Phase**: 1 - State Machine Implementation (2 weeks)

**Focus Areas**:
1. Complete state machine (StateScaling, StateRebalancing, StateEmergency)
2. Wire calculator state to Manager states
3. Implement adaptive stabilization windows
4. Integration tests for all state transitions

**See**: [PRACTICAL_IMPLEMENTATION_PLAN.md](PRACTICAL_IMPLEMENTATION_PLAN.md) for details

---

## 🧭 Navigation Guide

### I want to...

**...understand what's working right now**
→ Read [STATUS.md](STATUS.md)

**...know what needs to be done**
→ Read [PRACTICAL_IMPLEMENTATION_PLAN.md](PRACTICAL_IMPLEMENTATION_PLAN.md)

**...understand the library API**
→ Read [library-specification.md](library-specification.md)

**...understand the architecture**
→ Read [design/03-architecture/high-level-design.md](design/03-architecture/high-level-design.md)

**...understand the state machine**
→ Read [design/03-architecture/state-machine.md](design/03-architecture/state-machine.md)
→ See Appendix in [PRACTICAL_IMPLEMENTATION_PLAN.md](PRACTICAL_IMPLEMENTATION_PLAN.md)

**...understand operational scenarios**
→ Browse [design/05-operational-scenarios/](design/05-operational-scenarios/)

**...contribute to implementation**
→ Start with Phase 1 in [PRACTICAL_IMPLEMENTATION_PLAN.md](PRACTICAL_IMPLEMENTATION_PLAN.md)

**...run tests**
→ See [design/06-implementation/test-organization.md](design/06-implementation/test-organization.md)

---

## 📋 Quick Reference

### Project Status (As of Oct 26, 2025)
- **Foundation**: 🟢 Complete (NATS, KV, heartbeat, election)
- **State Machine**: 🟡 Partial (6 of 9 states implemented)
- **Testing**: 🔴 Minimal (~10% integration coverage)
- **Production Ready**: 🔴 No (needs 6 weeks)

### What Works ✅
- Multi-worker coordination
- Leader election
- Stable worker IDs
- Basic partition assignment
- ConsistentHash and RoundRobin strategies

### What's Missing ❌
- State machine (3 states never entered)
- Adaptive rebalancing windows
- Comprehensive integration tests
- Leader failover verification
- Error scenario handling

### Timeline
- **Now**: Phase 0 complete (honest assessment)
- **Next 2 weeks**: Phase 1 (state machine)
- **Weeks 3-4**: Phase 2-3 (leader election, assignments)
- **Weeks 5-6**: Phase 4-5 (reliability, performance)
- **Week 7**: Phase 7 (documentation)

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

---

For questions or clarifications, see [STATUS.md](STATUS.md) for current blockers and [PRACTICAL_IMPLEMENTATION_PLAN.md](PRACTICAL_IMPLEMENTATION_PLAN.md) for execution details.
