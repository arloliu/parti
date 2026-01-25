# 100 - Project Overview & Prime Directives

## Identity
- **Project:** Fuda (Go Configuration Library)
- **Module:** `github.com/arloliu/fuda`
- **Language:** Go >=1.25.0
- **Linting:** `golangci-lint` (via `make lint`)

## Prime Directives
1.  **Plan First:** Create/Update `implementation_plan.md` before writing code. Wait for approval on architectural changes.
2.  **Small Diffs:** Break work into small, verifiable chunks. Do not rewrite files unnecessarily.
3.  **Dependencies:** Check `go.mod`. Prefer stdlib. Ask before adding new deps.

## Preferred Libraries
- **Testing:** `testify` (assertions and mocking)
- **Validation:** `go-playground/validator`
- **YAML:** `gopkg.in/yaml.v3`
