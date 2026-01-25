# 500 - Development Workflow

## Before Commit
1. Run `make lint` — Fix all issues.
2. Run `make test` — All tests must pass.
3. Verify docs are updated if API changed.

## Git Conventions
- **Branches:** `feat/`, `fix/`, `docs/`, `chore/`, `test/`.
- **Commits:** Conventional format. Present tense. First line < 50 chars.
    - `feat: add dotenv support`
    - `fix: handle empty env values`

## Code Review Checklist
- [ ] Correctness
- [ ] Performance (no unnecessary allocs)
- [ ] Test coverage for new code
- [ ] Docs updated

## Environment
- **Dev:** `go run`, `go test`
- **Prod:** Logging, health checks, graceful shutdown.
