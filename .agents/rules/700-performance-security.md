# 700 - Performance and Security

Apply these rules when editing hot paths, external input handling,
authentication, credentials, or network-facing code.

## Performance
Apply these in **hot paths** (inner loops, per-request code, assignment calculation):

- **Allocations:**
    - Pre-allocate slices: `make([]T, 0, expectedCap)`
    - Pre-allocate maps: `make(map[K]V, expectedSize)`
    - Avoid `append` in tight loops if size is predictable.
- **Inlining:** Keep hot functions small and simple.
- **Pointers:** Pass small structs by value. Use pointers only when mutation is needed.
- **Interfaces:** Avoid in critical paths (indirect calls have overhead).
- **Hashing:** Use `zeebo/xxh3` for partition key hashing (project standard).
- **Profiling:** Use `pprof` to find bottlenecks before optimizing.
- **Concurrency:** Use `sync/atomic` for simple flags/counters. Use `sync.Mutex` for complex state.

## Security
- **Input:** Validate ALL external input (use `go-playground/validator` for struct validation).
- **Secrets:** Never log secrets. Never commit secrets.
- **Transport:** HTTPS for all external calls.
- **NATS Auth:** Support NATS credential-based authentication where applicable.
