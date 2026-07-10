# 500 - Validation and Workflow

Apply these rules before validation, commits, or PR work.

## Validation Gates
- Run `make lint` after Go changes and fix all issues.
- Run `make test` for unit coverage before calling implementation work done.
- Run broader gates (`make test-integration`, `make test-all`, `make ci`) when the task touches integration, stress, or release-sensitive behavior.
- Verify docs are updated when exported API changes.

## Pre-PR Gate
For any PR that touches `manager/`, `source/`, `stableid/`, `recovery/`,
`internal/assignment/`, or `internal/durable/`, run `make pre-pr` before opening
the PR. It chains `lint`, `test` (unit, `-race`), and `test-integration`
(live-NATS, `-race`). The integration suite is the load-bearing gate: it catches
contract regressions (see [900-cross-feature-contracts.md](900-cross-feature-contracts.md))
and concurrency races the unit suite cannot reproduce.

## Git Conventions
See [550-git-conventions.md](550-git-conventions.md) for branch
naming, commit message format and body guidelines, and PR conventions.

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
make pre-pr            # Pre-PR gate: lint + test + test-integration
make coverage          # Generate coverage report
make ci                # Full CI pipeline (lint + vet + test-all + coverage)
make gomod-tidy        # Tidy go.mod/go.sum
```
