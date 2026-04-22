# Parti — Agent Rules Index

> **CONTEXT**: This is a NATS-based work partitioning library (`github.com/arloliu/parti/v2`).
> **ACTION**: Read the files below in order before beginning work.

## Rule Index

### 1. Core Directives
- **[100-overview.md](100-overview.md)**
  *Identity, project structure, architecture notes, dependencies, and prime directives.*

### 2. Standards
- **[200-coding-style.md](200-coding-style.md)**
  *Go idioms, error handling, file layout, naming, loop patterns.*
- **[300-testing.md](300-testing.md)**
  *Unit/integration/stress organization, **CRITICAL** async testing rules, make targets.*
- **[400-documentation.md](400-documentation.md)**
  *Mandatory Godoc format with Parti-specific examples.*

### 3. Workflow & Safety
- **[500-workflow.md](500-workflow.md)**
  *Git conventions, pre-commit checks, make targets reference.*
- **[600-perf-sec.md](600-perf-sec.md)**
  *Performance optimizations (xxh3, allocations) and security boundaries.*
- **[700-lint-after-write.md](700-lint-after-write.md)**
  *Automated linting workflow and common fixes.*
- **[800-modernize-after-write.md](800-modernize-after-write.md)**
  *Run `go fix` on touched packages; avoid repo-wide sweeps in feature commits.*

---
*Rules are split for readability and context optimization.*
