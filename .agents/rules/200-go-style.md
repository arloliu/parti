# 200 - Coding Standards & Conventions

Apply these rules when editing Go code. Do not refactor existing code solely to
satisfy this file unless the task already touches that code or the violation is
blocking validation.

## Go Style
- **Idioms:** Follow [Effective Go](https://go.dev/doc/effective_go). Use `goimports`.
- **Types:** Use `any` instead of `interface{}`.
- **Collections:** Use `slices` and `maps` packages from stdlib.
- **Context:** Use `context.Context` for request-scoped values/cancellation.
- **Sync:** Prefer `sync/atomic` for simple counters and flags.

## Error Handling (CRITICAL)
- **Static:** Use `errors.New("message")`.
- **Wrap:** Use `fmt.Errorf("context: %w", err)`.
- **Check:** Use `errors.Is()` and `errors.As()`.
- **Naming:**
    - Sentinel: `var ErrNotFound = errors.New(...)` (prefix `Err`)
    - Types: `type ValidationError struct{...}` (suffix `Error`)
- **Type Assert:** Always use comma-ok: `v, ok := x.(Type)`
- **Return:** Errors are always the last return value. Use early returns to reduce nesting.

## Interface Assertions
- Pattern: `var _ Interface = (*Type)(nil)`
- **Internal pkgs:** Immediately after type definition.
- **Public pkgs (`strategy/`, `source/`, `consumer/`):** In `_test.go` files to avoid import cycles with root `parti` package.

## File Layout

Apply this order to new files, new top-level declarations, and touched regions.
Do not reorder unrelated existing code solely to satisfy this layout.

1. Package declaration
2. Imports (stdlib, external, internal)
3. Constants (exported first)
4. Variables (exported first)
5. Types (exported first)
6. Factory Functions (`NewType`)
7. Exported Functions
8. Unexported Functions
9. Exported Methods (grouped by receiver)
10. Unexported Methods (grouped by receiver)

For internal packages, place interface assertions immediately after the relevant
type definition.

## Function Limits
- **Max Lines:** 100 (prefer < 50).
- **Max Complexity:** 22 (cyclop linter).
- **Naked Returns:** Avoid in functions > 40 lines.

## Naming
- **Packages:** Short, lowercase.
- **Functions/Types:** CamelCase (Exported), camelCase (private).
- **Receivers:** Short, consistent (e.g., `m` for `Manager`, `c` for `ConsistentHash`).

## Loop Patterns (Go 1.22+)
- Index needed: `for i := range slice`
- No index: `for range slice`
- Simple N: `for range N`
- Benchmarks: `for b.Loop()` (Go 1.24+)
- **Key point:** If you're not using the index variable, don't declare it.
