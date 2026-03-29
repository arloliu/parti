---
name: go_api_review
description: Performs a critical review focused on discoverability, clarity, and ease of use (Developer Experience) of a Go package, relying exclusively on the exported API (Godoc) and the README, without reading internal source code.
---

# Go API Review Skill

As an agent using this skill, your task is to evaluate a Go package as a potential user evaluating it. You must rely exclusively on the exported API (Godoc) and the README. **Do not read the internal source code.**

Please perform a critical review focused on **discoverability, clarity, and ease of use (Developer Experience)**, specifically addressing the following points using idiomatic Go standards:

## Scope

When reviewing specific packages, specify them by name. The reviewable packages in Parti:
- Root package (`parti`): Entry point — `Manager`, `Config`, `NewManager`, `Hooks`, `Options`
- `consumer/`: JetStream consumer types — `NewQueue`, `NewStatic`, `NewDynamic`, `NewBroadcast`
- `partition/`: Static routing — `NewPublisher`, `NewJSPublisher`, `NewSubscriber`, `PartitionConfig`
- `strategy/`: Assignment — `NewConsistentHash`, `NewWeightedConsistentHash`, `NewRoundRobin`
- `source/`: Partition sources — `NewStatic`, `NewNatsKV`
- `types/`: Shared interfaces — `AssignmentStrategy`, `PartitionSource`, `Logger`, `Hooks`

## 1. API and Interface Quality (The "Clean" Test)

1. **Interface Design:**
    * Are the exported interfaces small, single-purpose, and named by their behavior (e.g., `PartitionSource`, `AssignmentStrategy`)?
    * Does the library adhere to the "Accept Interfaces, Return Structs" principle?
    * Are the interfaces in `types/` appropriately scoped, or do any try to do too much?

2. **Method Clarity & Idioms:**
    * Are all public functions and methods focused on a single responsibility?
    * Is there any ambiguity regarding when to use a **value receiver** vs. a **pointer receiver** on methods, and are pointer receivers used consistently where mutation occurs?
    * Are method signatures simple, or do they involve complex, unnecessary nesting of custom types?
    * Do functional options (`WithX` pattern) follow idiomatic Go conventions?

3. **Error and Context Handling:**
    * Are **`context.Context`** and standard **`error`** returns implemented idiomatically?
    * Is the error handling strategy clearly documented? Can users reliably use `errors.Is()` with the exported sentinel errors from `types/errors.go`?
    * Are the sentinel errors in `errors.go` comprehensive enough to cover common failure modes?

## 2. Documentation and Examples (The "Good Enough" Test)

1. **Clarity and Purpose:**
    * Does the **package-level Godoc** (`doc.go`) clearly state the package's purpose and value proposition?
    * Are there any exported functions, types, or fields that lack a clear, helpful top-level Godoc comment?
    * Does the root package doc show the worker state machine (`INIT → CLAIMING_ID → ELECTION → WAITING_ASSIGNMENT → STABLE`)?

2. **Examples and Learning Curve:**
    * Does the **README** contain a clear, working code example for the most common use case?
    * Are there `Example` functions in `example_test.go` files that appear correctly in Godoc?
    * Is the relationship between `Manager`, consumers, strategies, and sources clear from examples alone?
    * Can a new user understand the setup flow: connect NATS → create strategy → create source → create manager → start?

## 3. Ambiguity and Misuse (The "Caveat" Test)

1. **Potential Misuse:**
    * Are there public functions or types that could be easily misused? For example:
        - Calling `Manager.Start()` without calling `SetDefaults()` first
        - Using `CurrentAssignment()` and mutating the returned slice
        - Starting a consumer without connecting it to the Manager via `WithWorkerConsumerUpdater`
    * Is there clear documentation on what happens if `Stop()` is not called?

2. **Concurrency and Lifecycle:**
    * Are there documented **thread-safety guarantees** for `Manager` and consumer types?
    * Is the recommended lifecycle explicitly documented: `NewManager` → `Start` → use → `Stop`?
    * Is it clear that `Manager` must be the single coordinator and consumers should not be started independently when used with the Manager?
