# GitHub Copilot Instructions

This file provides context and guidelines for GitHub Copilot when working on the `parti` project.

## Project Overview

Parti is a Go library for NATS-based work partitioning that provides dynamic partition assignment across worker instances with stable worker IDs and leader-based coordination.

**Key Features:**
- **Stable Worker IDs:** Workers claim stable IDs for consistent assignment during rolling updates
- **Leader-Based Assignment:** One worker calculates assignments without external coordination
- **Adaptive Rebalancing:** Different stabilization windows for cold start (30s) vs planned scale (10s)
- **Cache Affinity:** Preserves >80% partition locality during rebalancing with consistent hashing
- **Weighted Assignment:** Supports partition weights for load balancing

**Project Details:**
- **Language:** Go >=1.25.0
- **Module:** github.com/arloliu/parti
- **Project Structure:**
  ```
  parti/                        # Root = main public package
  ├── doc.go                    # Package documentation
  ├── interfaces.go             # Core interfaces
  ├── config.go                 # Configuration types
  ├── options.go                # Functional options & factories
  ├── *_test.go                 # Unit tests
  ├── strategy/                 # Assignment strategies subpackage
  │   ├── consistent_hash.go
  │   ├── round_robin.go
  │   └── *_test.go
  ├── source/                   # Partition sources subpackage
  │   ├── static.go
  │   └── *_test.go
  ├── subscription/             # Subscription helper subpackage
  │   ├── helper.go
  │   └── *_test.go
  ├── internal/                 # Private implementation
  │   ├── manager/              # Manager implementation
  │   ├── election/             # Election (NATS KV, external agent)
  │   ├── heartbeat/            # Heartbeat publisher
  │   ├── assignment/           # Assignment calculation
  │   ├── stableid/             # Stable ID claiming
  │   └── hash/                 # Hash utilities
  ├── test/                     # Integration tests
  │   ├── integration/          # Cross-component scenarios
  │   └── testutil/             # Shared test utilities
  ├── examples/                 # Example programs
  │   ├── basic/
  │   ├── defender/
  │   └── custom-strategy/
  ├── docs/                     # Design documentation
  └── .github/
      └── copilot-instructions.md
  ```

## Coding Standards & Conventions

### Go Style Guidelines
- Follow the [official Go style guide](https://golang.org/doc/effective_go.html)
- Use `goimports` for formatting and import management
- Use `golangci-lint` for comprehensive code quality checks
- Use `any` instead of `interface{}` for empty interfaces
- Use `slices` and `maps` packages from the standard library for common operations
- Use `sync` package for synchronization primitives
- Prefer atomic operations from `sync/atomic` for simple counters and flags
- Always use `errors.New` if the error message is static without formatting needed
- Use `fmt.Errorf` with `%w` verb for wrapping errors with context
- Prefer `errors.Is` and `errors.As` for error handling
- Use `context` package for request-scoped values, cancellation, and timeouts
- **Compile-time interface assertions:** Use strategically to avoid import cycles
  - Pattern: `var _ InterfaceName = (*ConcreteType)(nil)`
  - **Use in:** `internal/*` packages only (safe to import root package)
  - **DO NOT use in:** Public subpackages like `strategy/`, `source/`, `subscription/` (causes import cycles)
  - Place immediately after type definition
  - Comment format: `// Compile-time assertion that TypeName implements InterfaceName.`
  - Benefits: Compile-time safety, documentation, refactoring protection
  - Example: `var _ parti.Logger = (*NopLogger)(nil)` in `internal/logger/nop.go`
  - **Alternative:** Add interface tests in `_test.go` files for public subpackages:
    ```go
    func TestImplementsInterface(t *testing.T) {
        var _ parti.AssignmentStrategy = (*ConsistentHash)(nil)
    }
    ```
- Follow Go naming conventions:
  - Package names: lowercase, short, descriptive
  - Functions: CamelCase (exported) or camelCase (unexported)
  - Variables: camelCase
  - Constants: CamelCase for package-level constants
  - Receiver names: short and consistent (enforced by receiver-naming rule)

### File Content Order

**All Go source files MUST follow this declaration order:**

| Order | Code Element | Convention/Reason |
|-------|--------------|-------------------|
| 1 | **Package declaration** | `package name` |
| 2 | **Imports** | Grouped: standard library, external, internal |
| 3 | **Constants** (`const`) | Grouped together, exported first |
| 4 | **Variables** (`var`) | Grouped together, exported first |
| 5 | **Types** (`type`) | Structs, interfaces, custom types. Exported first |
| 5.5 | **Interface assertions** | `var _ Interface = (*Type)(nil)` immediately after type (internal packages ONLY) |
| 6 | **Factory Functions** | `NewType() *Type` immediately after type/assertion |
| 7 | **Exported Functions** | Public standalone functions (not methods) |
| 8 | **Unexported Functions** | Private helper functions (not methods) |
| 9 | **Exported Methods** | Methods on types `(t *Type) Method()`. Group by type |
| 10 | **Unexported Methods** | Private methods `(t *Type) helper()`. Group by type |

**Example Structure (internal package):**

```go
package logger

import (
    "github.com/arloliu/parti"
)

// NopLogger implements a no-op logger.
type NopLogger struct{}

// Compile-time assertion that NopLogger implements Logger.
var _ parti.Logger = (*NopLogger)(nil)

// NewNop creates a new no-op logger.
func NewConsistentHash(opts ...Option) *ConsistentHash {
    // implementation
}

// Assign calculates partition assignments.
func (c *ConsistentHash) Assign(workers []string, partitions []parti.Partition) (map[string][]parti.Partition, error) {
    // implementation
}
```

**Key Rules:**
- ✅ Group related items together (all constants, all types, all methods for same receiver)
- ✅ Exported items come before unexported items within each category
- ✅ **Interface assertions come immediately after type definition (internal packages ONLY)**
- ✅ Factory functions (`NewX`) come immediately after the type/assertion
- ✅ Methods are grouped by receiver type, not alphabetically
- ✅ Maintain logical grouping over strict alphabetical ordering

### Loop Patterns (forlooprange rule)
- Use `for i := range slice` when you need the index: `for i := range items { process(i, items[i]) }`
- Use `for range slice` when you don't need the index: `for range items { doSomething() }`
- Use `for b.Loop()` in benchmarks (Go 1.24+): `for b.Loop() { benchmarkedCode() }`
- Use `for range N` (Go 1.22+) for simple iteration: `for range 10 { repeat() }`
- **Key point:** If you're not using the index variable `i`, don't declare it

### Code Organization
- Keep functions small and focused (max 100 lines, prefer under 50)
- Function complexity should not exceed 22 (enforced by cyclop linter)
- Package average complexity should stay under 15.0
- Use meaningful variable and function names
- Group related functionality in the same package
- Separate concerns using interfaces
- Use dependency injection for better testability
- Avoid naked returns in functions longer than 40 lines
- Use struct field tags for marshaling/unmarshaling (enforced by musttag)

### File Organization: 3-File Maximum Rule

**Each type, struct, or logical component should have at most 3 Go source files:**

1. **Implementation file** - Contains the main logic, types, and methods
   - Example: `numeric_raw.go`, `ts_delta.go`, `blob.go`

2. **Test file** (`*_test.go`) - Contains unit tests for the implementation
   - Example: `numeric_raw_test.go`, `ts_delta_test.go`, `blob_test.go`

3. **Benchmark file** (`*_bench_test.go`) - Contains performance benchmarks (optional)
   - Example: `numeric_bench_test.go`, `ts_delta_bench_test.go`, `blob_bench_test.go`

**Key Principles:**
- ✅ Each component follows this 3-file pattern
- ✅ Tests belong with their implementation (no cross-cutting test files)
- ✅ Related code stays together by type/component
- ❌ Avoid creating additional files like `*_reuse_test.go`, `*_helper_test.go`, etc.
- ❌ No cross-cutting test files that test multiple unrelated types

**Benefits:**
- **Predictability:** Easy to find where code lives
- **Organization:** Related code stays together by component
- **Navigation:** Developers know exactly where to look
- **Maintainability:** Prevents file sprawl and confusion
- **Consistency:** Uniform structure across all packages

**Example Structure:**
[FILL IN WITH A CODE EXAMPLE IF NEEDED]

**Exceptions:**
- Package-level constants/types file (e.g., `types.go`, `const.go`) is acceptable
- If a component doesn't need benchmarks, 2 files (impl + test) is fine

### Error Handling
- Always handle errors explicitly (enforced by errcheck)
- Check type assertions with comma ok idiom (check-type-assertions: true)
- Use the standard `error` interface
- Wrap errors with context using `fmt.Errorf` with %w verb for error wrapping
- Return errors as the last return value
- Use early returns to reduce nesting
- Prefix sentinel errors with "Err" and suffix error types with "Error" (errname linter)
- Handle specific error wrapping scenarios properly (errorlint)

Example:
[FILL IN WITH A CODE EXAMPLE IF NEEDED]

### Testing Guidelines

**Test Organization** (Hybrid Approach):
- **Unit tests**: Co-located with implementation (`*_test.go`)
- **Integration tests**: Dedicated directory (`test/integration/`)
- See `docs/design/06-implementation/test-organization.md` for complete details

**Unit Test Guidelines:**
- Write unit tests for all public functions
- **Use table-driven tests ONLY when you have multiple test cases**
- **Avoid over-engineering:** Don't use table-driven structure for single test cases - write simple, direct tests instead
- Place tests in `_test.go` files in the same package
- Use the standard `testing` package
- Use `b.Loop()` (introduced in Go 1.24) for benchmarks
- Use `testify` for assertions and mocking
- Mock external dependencies for isolated tests
- Test edge cases and error scenarios
- Aim for high test coverage (>80%)
- Use meaningful test names that describe the scenario
- Use `t.Setenv()` instead of `os.Setenv()` in tests (tenv linter)
- Use `t.Parallel()` appropriately (tparallel linter)
- Ensure examples are testable with expected output (testableexamples linter)
- Test files are excluded from certain linters (bodyclose, dupl, gosec, noctx)

**Integration Test Guidelines:**
- Place in `test/integration/` directory
- Use build tags: `//go:build integration`
- Package name: `integration_test`
- Always include `testing.Short()` guard
- Use embedded NATS via `testutil.StartEmbeddedNATS(t)`
- Test cross-component scenarios (leader failover, scaling, etc.)
- Clean up resources with `defer`

**Running Tests:**
```bash
make test-unit          # Fast unit tests only
make test-integration   # Integration tests only
make test-all          # Both unit + integration
make test              # Unit tests with race detector
```

**Table-driven test (use when you have multiple test cases):**
```go
func TestFunctionName(t *testing.T) {
    tests := []struct {
        name     string
        input    InputType
        expected ExpectedType
        wantErr  bool
    }{
        {
            name:     "valid input",
            input:    validInput,
            expected: expectedOutput,
            wantErr:  false,
        },
        {
            name:     "invalid input",
            input:    invalidInput,
            expected: nil,
            wantErr:  true,
        },
        // more test cases...
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            result, err := FunctionName(tt.input)
            if (err != nil) != tt.wantErr {
                t.Errorf("FunctionName() error = %v, wantErr %v", err, tt.wantErr)
                return
            }
            if !reflect.DeepEqual(result, tt.expected) {
                t.Errorf("FunctionName() = %v, want %v", result, tt.expected)
            }
        })
    }
}
```

**Simple test (use for single test cases):**
```go
func TestGetLittleEndianEngine(t *testing.T) {
    engine := GetLittleEndianEngine()

    require.Equal(t, binary.LittleEndian, engine)
    require.Implements(t, (*EndianEngine)(nil), engine)

    // Test actual behavior
    var testValue uint16 = 0x0102
    bytes := make([]byte, 2)
    engine.PutUint16(bytes, testValue)
    require.Equal(t, byte(0x02), bytes[0]) // LSB first
    require.Equal(t, byte(0x01), bytes[1]) // MSB second
}
```

### Best Practices
- Prefer standard library when possible
- Pre-allocate slices when size is known
- Pre-allocate maps when size is known
- Try to pre-allocate as much as possible
- Use dependency injection for testability
- Document all exported items
- Validate all input data
- **Performance-focused coding:**
  - Prefer branchless code in critical/hot path functions
  - Write code aggressively for inlining potential (small, simple functions)
  - Avoid over-using interfaces when performance is critical (interface calls have overhead)
  - Avoid unnecessary pointer creation for small structs - pass by value unless you need pointer receivers

## Linting Rules & Quality Standards

This project uses a comprehensive `golangci-lint` configuration with strict rules:

### Code Quality Rules
- **Function length**: Maximum 100 lines (revive), prefer shorter functions
- **Cyclomatic complexity**: Maximum 25 per function (cyclop)
- **Package complexity**: Average should be under 15.0
- **Naked returns**: Allowed only in functions ≤40 lines (nakedret)
- **Context handling**: Always pass context as first parameter (context-as-argument)
- **Import shadowing**: Avoid shadowing package names (import-shadowing)

### Security & Safety
- **Type assertions**: Always use comma ok idiom: `val, ok := x.(Type)`
- **SQL operations**: Always close `sql.Rows` and `sql.Stmt` (sqlclosecheck, rowserrcheck)
- **HTTP responses**: Always close response bodies (bodyclose)
- **Nil checks**: Avoid returning nil error with invalid value (nilnil)
- **Unicode safety**: Check for dangerous unicode sequences (bidichk)

### Performance & Memory
- **Pre-allocation**: Consider pre-allocating slices when size is known (prealloc)
- **Unnecessary conversions**: Remove unnecessary type conversions (unconvert)
- **Wasted assignments**: Avoid assignments that are never used (wastedassign)
- **Duration arithmetic**: Be careful with duration multiplications (durationcheck)

### Code Style
- **Variable naming**: Follow Go conventions, avoid stuttering
- **Receiver naming**: Use consistent, short receiver names
- **Comment spacing**: Use proper spacing in comments (comment-spacings)
- **Standard library**: Use standard library variables/constants when available (usestdlibvars)
- **Printf functions**: Name printf-like functions with 'f' suffix (goprintffuncname)

## Documentation Standards

### Code Documentation
- Use clear and concise comments
- Document all exported functions, types, and constants
- Use Go doc comments (start with the name of the item being documented)
- Include examples in documentation when helpful

### Godoc Format for Methods and Functions

**All exported functions and methods MUST follow this standardized format:**

```go
// FunctionName provides a brief one-line description of what the function does.
//
// Optional: More detailed description explaining the purpose, behavior, and usage.
// This section can span multiple paragraphs and include implementation details,
// algorithm descriptions, or other relevant context.
//
// Parameters:
//   - param1: Description of the first parameter and its constraints
//   - param2: Description of the second parameter and its expected values
//   - paramN: Additional parameters with their descriptions
//
// Returns:
//   - returnType1: Description of what the first return value represents
//   - returnType2: Description of the second return value (e.g., error conditions)
//
// Example:
//
//	encoder := NewEncoder()
//	data := []byte("example")
//	result, err := encoder.Process(data)
//	if err != nil {
//	    log.Fatal(err)
//	}
func FunctionName(param1 Type1, param2 Type2) (returnType1, error) {
    // implementation
}
```

**Key Requirements:**
1. **First line:** Brief description starting with the function/method name
2. **Blank line:** Separates the summary from detailed description
3. **Detailed description:** Optional but recommended for complex functions
4. **Blank line:** Before Parameters section
5. **Parameters section:** List all parameters with clear descriptions
   - Use bullet list format with `-` for each parameter
   - Describe constraints, expected values, and special cases
   - For constructors, mention what engine/configuration parameters do
6. **Blank line:** Before Returns section
7. **Returns section:** List all return values with descriptions
   - Describe what each return value represents
   - Explain error conditions for error returns
   - For iterators, mention what the iterator yields
8. **Example section:** Optional but highly recommended
   - Show realistic usage scenarios
   - Include error handling when applicable
   - Use proper indentation (tab character)

**Examples by Function Type:**

**Constructor Function:**
```go
// NewTimestampEncoder creates a new timestamp encoder using the specified endian engine.
//
// The encoder uses delta-of-delta compression to minimize storage space for sequential
// timestamps. This provides 60-87% space savings compared to raw encoding for regular
// interval data.
//
// Parameters:
//   - engine: Endian engine for byte order (typically little-endian)
//
// Returns:
//   - *TimestampEncoder: A new encoder instance ready for timestamp encoding
//
// Example:
//
//	encoder := NewTimestampEncoder(endian.GetLittleEndianEngine())
//	encoder.Write(time.Now().UnixMicro())
//	data := encoder.Bytes()
func NewTimestampEncoder(engine endian.EndianEngine) *TimestampEncoder {
    // implementation
}
```

**Method with Multiple Parameters:**
```go
// Write encodes a single timestamp using delta-of-delta compression.
//
// The timestamp is encoded based on its position:
//   - First timestamp: Full varint-encoded microseconds (5-9 bytes)
//   - Second timestamp: Delta from first (1-9 bytes)
//   - Subsequent timestamps: Delta-of-delta (1-9 bytes)
//
// Parameters:
//   - timestampUs: Timestamp in microseconds since Unix epoch
func (e *TimestampEncoder) Write(timestampUs int64) {
    // implementation
}
```

**Method Returning Values:**
```go
// Bytes returns the encoded byte slice containing all written timestamps.
//
// The returned slice is valid until the next call to Write, WriteSlice, or Reset.
// The caller must not modify the returned slice as it references the internal buffer.
//
// Returns:
//   - []byte: Encoded byte slice (empty if no timestamps written since last Reset)
func (e *TimestampEncoder) Bytes() []byte {
    // implementation
}
```

**Method Returning Multiple Values:**
```go
// At retrieves the timestamp at the specified index from the encoded data.
//
// This method provides efficient random access by decoding only up to the
// target index. For sequential access, use All() iterator instead.
//
// Parameters:
//   - data: Encoded byte slice from TimestampEncoder.Bytes()
//   - index: Zero-based index of the timestamp to retrieve
//   - count: Total number of timestamps in the encoded data
//
// Returns:
//   - int64: The timestamp at the specified index (microseconds since Unix epoch)
//   - bool: true if the index exists and was successfully decoded, false otherwise
//
// Example:
//
//	decoder := NewTimestampDecoder()
//	timestamp, ok := decoder.At(encodedData, 5, 10)
//	if ok {
//	    fmt.Printf("Timestamp at index 5: %v\n", time.UnixMicro(timestamp))
//	}
func (d TimestampDecoder) At(data []byte, index int, count int) (int64, bool) {
    // implementation
}
```

**Method Returning Iterator:**
```go
// All returns an iterator that yields all timestamps from the encoded data.
//
// This method provides zero-allocation iteration using Go's iter.Seq pattern.
// The iterator processes data sequentially without creating intermediate slices.
//
// Parameters:
//   - data: Encoded byte slice from TimestampEncoder.Bytes()
//   - count: Expected number of timestamps (used for optimization)
//
// Returns:
//   - iter.Seq[int64]: Iterator yielding decoded timestamps (microseconds since Unix epoch)
//
// Example:
//
//	decoder := NewTimestampDecoder()
//	for ts := range decoder.All(encodedData, expectedCount) {
//	    fmt.Printf("Timestamp: %v\n", time.UnixMicro(ts))
//	    if someCondition {
//	        break // Can break early if needed
//	    }
//	}
func (d TimestampDecoder) All(data []byte, count int) iter.Seq[int64] {
    // implementation
}
```

**Common Patterns:**
- **No parameters:** Omit Parameters section (e.g., `Bytes()`, `Len()`, `Size()`)
- **No return values:** Omit Returns section (e.g., `Write()`, `Reset()`)
- **Error returns:** Always describe error conditions in Returns section
- **Simple getters:** Can have minimal documentation if self-explanatory
- **Complex algorithms:** Include algorithm description before Parameters section

**Reference Implementation:**
See `encoding/metric_names.go` for the canonical implementation of this format.

### README and Documentation
- Keep README.md up to date with installation and usage instructions
- Document API endpoints if this is a web service
- Include configuration examples
- Provide troubleshooting guides for common issues

## Dependencies

### Dependency Management
- Use Go modules for dependency management
- Keep dependencies minimal and well-maintained
- Prefer standard library when possible
- Pin major versions and update dependencies regularly
- Use `go mod tidy` to clean up unused dependencies
- **Blocked dependencies** (use alternatives):
  - `github.com/golang/protobuf` → use `google.golang.org/protobuf`
  - `github.com/satori/go.uuid` → use `github.com/google/uuid`
  - `github.com/gofrs/uuid` → use `github.com/google/uuid`

### Preferred Libraries
- **Testing:** testify for assertions and mocking
- **HTTP Router:** [specify preferred router, e.g., gorilla/mux, gin, chi]
- **Database:** [specify preferred database driver, e.g., lib/pq for PostgreSQL]
- **Logging:** [specify preferred logging library, e.g., logrus, zap]
- **Configuration:** [specify preferred config library, e.g., viper, envconfig]

## Security & Performance Guidelines

### Security Considerations
- Validate all input data
- Use proper authentication and authorization
- Handle sensitive data securely (no secrets in logs)
- Follow OWASP guidelines for web applications
- Use HTTPS for all external communications
- Implement proper rate limiting for APIs

### Performance Guidelines
- Profile code for performance bottlenecks
- Use goroutines for concurrent operations when appropriate
- Implement proper context handling for timeouts and cancellation
- Consider memory usage and garbage collection impact
- Use connection pooling for database operations
- Cache expensive operations when possible

## Development Workflow

### Branch Naming
- `feat/` - New features
- `fix/` - Bug fixes
- `docs/` - Documentation updates
- `chore/` - Maintenance tasks
- `test/` - Test-related changes

### Commit Messages
- Use conventional commit format
- Start with a verb in present tense (add, fix, update, remove)
- Keep the first line under 50 characters
- Include detailed description when necessary

### Code Review Guidelines
- Review for correctness, performance, and maintainability
- Check test coverage for new code
- Ensure documentation is updated
- Verify error handling is appropriate

## Environment-Specific Notes

### Development
- Use `go run` for quick testing
- Use `go build` for local builds
- Set up proper IDE configuration for Go development

### Production
- Use proper logging levels
- Implement health checks
- Set up monitoring and alerting
- Use graceful shutdown for services

---

**Note:** Update this file as the project evolves to keep Copilot's suggestions relevant and helpful.