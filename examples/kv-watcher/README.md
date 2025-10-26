# NATS KV Watcher Behavior Test

This example demonstrates the correct behavior of NATS JetStream KeyValue watchers.

## Purpose

This program was created to verify and understand how NATS KV watchers handle the `nil` entry that is sent after initial values are replayed.

## Key Findings

### 1. Default Watch() and WatchAll() Behavior
- Watchers **replay initial values** from the KV store
- Send a **`nil` entry** as a marker indicating "end of initial values"
- **Continue to deliver future updates** after the `nil` marker
- The `nil` entry is **NOT a close signal** - it's just a marker!

### 2. UpdatesOnly() Option
- Skips the initial value replay
- Does NOT send the `nil` marker
- Only delivers future updates from the moment the watcher is created

## Running the Example

```bash
cd examples/kv-watcher
go run main.go
```

## Expected Output

The program runs 4 tests demonstrating different watcher configurations:

1. **Watch(key)** - Watch specific key, receives initial value + nil marker + future updates
2. **WatchAll()** - Watch all keys, receives all initial values + nil marker + future updates
3. **WatchAll() with UpdatesOnly()** - Only future updates, no initial values, no nil marker
4. **Watch(key) with UpdatesOnly()** - Only future updates for specific key

## The Bug We Fixed

**Original buggy code:**
```go
case entry := <-watcher.Updates():
    if entry == nil {
        // ❌ WRONG: Treating nil as channel close!
        return
    }
```

**Corrected code:**
```go
case entry := <-watcher.Updates():
    if entry == nil {
        // ✅ CORRECT: nil is just a marker, continue watching
        continue
    }
```

## References

- [NATS JetStream KV Watch Documentation](https://pkg.go.dev/github.com/nats-io/nats.go/jetstream#KeyValue.Watch)
- NATS Docs: "By default, the watcher will send the latest value for each key and all future updates. Watch will send a nil entry when it has received all initial values."
