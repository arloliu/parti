# 400 - Documentation Standards

## General
- **Godoc:** All exported symbols MUST have doc comments.
- **First Line:** Start with the symbol name. One-line summary.
- **README:** Keep updated with install/usage.

## Godoc Template (MANDATORY)

```go
// FunctionName one-line summary.
//
// Detailed description (optional but recommended).
//
// Parameters:
//   - param1: Description and constraints
//   - param2: Expected values
//
// Returns:
//   - Type: What it represents
//   - error: Conditions that cause errors
//
// Example:
//
//result, err := FunctionName(input)
//if err != nil { ... }
func FunctionName(param1 T1, param2 T2) (Result, error) { }
```

## Examples by Type

**Constructor:**
```go
// NewLoader creates a Loader from config.
//
// Parameters:
//   - cfg: Configuration options
//
// Returns:
//   - *Loader: Ready-to-use loader instance
func NewLoader(cfg Config) *Loader { }
```

**Method with Multiple Returns:**
```go
// Get retrieves value by key.
//
// Parameters:
//   - key: Lookup key (case-sensitive)
//
// Returns:
//   - string: The value if found
//   - bool: true if key exists
func (c *Cache) Get(key string) (string, bool) { }
```

## Omit When Appropriate
- No params → Omit Parameters section.
- No returns → Omit Returns section.
- Simple getters → Minimal doc is OK.
