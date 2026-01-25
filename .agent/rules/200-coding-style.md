# 200 - Coding Standards & Conventions

## Go Style
- **Idioms:** Follow [Effective Go](https://go.dev/doc/effective_go). Use `goimports`.
- **Types:** Use `any` instead of `interface{}`.
- **Collections:** Use `slices` and `maps` packages from stdlib.
- **Context:** Use `context.Context` for request-scoped values/cancellation.
- **Sync:** Prefer `sync/atomic` for simple counters.

## Error Handling (CRITICAL)
- **Static:** Use `errors.New("message")`.
- **Wrap:** Use `fmt.Errorf("context: %w", err)`.
- **Check:** Use `errors.Is()` and `errors.As()`.
- **Naming:**
    - Sentinel: `var ErrNotFound = errors.New(...)`
    - Types: `type ValidationError struct{...}`
- **Type Assert:** Always use comma-ok: `v, ok := x.(Type)`

## Interface Assertions
- Pattern: `var _ Interface = (*Type)(nil)`
- **Internal pkgs:** Immediately after type definition.
- **Public pkgs:** In `_test.go` files to avoid cycles.

## File Layout (STRICT)
1. Package
2. Imports (stdlib, external, internal)
3. Constants
4. Variables
5. Types
5.5. Interface Assertions (internal only)
6. Factory Functions (`NewType`)
7. Exported Functions
8. Unexported Functions
9. Exported Methods (grouped by receiver)
10. Unexported Methods (grouped by receiver)

## Function Limits
- **Max Lines:** 100 (prefer < 50).
- **Max Complexity:** 22 (cyclop linter).
- **Naked Returns:** Avoid in functions > 40 lines.

## Naming
- **Packages:** Short, lowercase.
- **Functions/Types:** CamelCase (Exported), camelCase (private).
- **Receivers:** Short, consistent (e.g., `l` for `Loader`).

## Loop Patterns
- Index needed: `for i := range slice`
- No index: `for range slice`
- Simple N: `for range N` (Go 1.22+)
- Benchmarks: `for b.Loop()` (Go 1.24+)
