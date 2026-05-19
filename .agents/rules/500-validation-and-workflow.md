# 500 - Validation and Workflow

Apply these rules before validation, commits, or PR work.

## Validation Gates
- Run `make lint` after Go changes and fix all issues.
- Run `make test` for unit coverage before calling implementation work done.
- Run broader gates (`make test-integration`, `make test-all`, `make ci`) when the task touches integration, stress, or release-sensitive behavior.
- Verify docs are updated when exported API changes.

## Git Conventions
- **Branches:** `feat/`, `fix/`, `docs/`, `chore/`, `test/`.
- **Commits:** Conventional format. Present tense. First line < 50 chars.
    - `feat: add weighted consistent hash strategy`
    - `fix: handle nil partition source`
- **Attribution:** Never add `Co-Authored-By` or any other attribution trailers.

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
make clean-linter-cache # Clear golangci-lint cache
make fmt               # Format code (gofmt + goimports)
make vet               # Run go vet
make test              # Unit tests with race detector
make test-integration  # Integration tests with embedded NATS
make test-stress       # Stress tests (PARTI_STRESS=1)
make test-all          # Unit + integration + stress
make test-smoke        # Quick stress smoke test
make coverage          # Generate coverage report
make ci                # Full CI pipeline (lint + vet + test-all + coverage)
make gomod-tidy        # Tidy go.mod/go.sum
```
