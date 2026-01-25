# 600 - Performance & Security

## Performance
Apply these in **hot paths** (inner loops, per-request code):

- **Allocations:**
    - Pre-allocate slices: `make([]T, 0, expectedCap)`
    - Pre-allocate maps: `make(map[K]V, expectedSize)`
    - Avoid `append` in tight loops if size is predictable.
- **Inlining:** Keep hot functions small and simple.
- **Pointers:** Pass small structs by value. Use pointers only when mutation is needed.
- **Interfaces:** Avoid in critical paths (indirect calls have overhead).
- **Profiling:** Use `pprof` to find bottlenecks before optimizing.

## Security
- **Input:** Validate ALL external input.
- **Secrets:** Never log secrets. Never commit secrets.
- **Transport:** HTTPS for all external calls.
- **Auth:** Use proper authentication/authorization.
