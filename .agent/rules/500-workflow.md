# 500 - Development Workflow

## Before Commit
1. Run `make lint` — Fix all issues.
2. Run `make test` — All unit tests must pass with race detector.
3. Verify docs are updated if API changed.

## Git Conventions
- **Branches:** `feat/`, `fix/`, `docs/`, `chore/`, `test/`.
- **Commits:** Conventional format. Present tense. First line < 50 chars.
    - `feat: add weighted consistent hash strategy`
    - `fix: handle nil partition source`

## Code Review Checklist
- [ ] Correctness
- [ ] Performance (no unnecessary allocs in hot paths)
- [ ] Test coverage for new code
- [ ] Docs updated for exported API changes
- [ ] No import cycles introduced

## Make Targets Reference
```bash
make help              # Show all targets
make lint              # Run golangci-lint
make fmt               # Format code (gofmt + goimports)
make vet               # Run go vet
make test              # Unit tests with race detector
make test-all          # Unit + integration + stress
make coverage          # Generate coverage report
make ci                # Full CI pipeline (lint + vet + test-all + coverage)
make gomod-tidy        # Tidy go.mod/go.sum
```
