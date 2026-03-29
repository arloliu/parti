---
name: QA Review - Go Library Robustness and Correctness
description: Perform a critical review focused on correctness, fault tolerance, and performance implications of a Go library from the perspective of external users.
---

# QA Review - Go Library Robustness and Correctness

**Assumed Role:** Quality Assurance (QA) Engineer.

**Testing Premise:** Your testing plan relies on the public API (Godoc) and the README as the primary specifications. You need to ensure the library is robust, reliable, and compliant with its published contract.

When executing this skill, perform a critical review focused on **correctness, fault tolerance, and performance implications**, specifically addressing the following points from the perspective of a user who intends to misuse the library:

## 1. Functional Correctness and Compliance Testing

1.  **Public API Contract Gaps:**
    * Identify any ambiguity where the Godoc describes behavior but provides insufficient detail, such as **data structure ordering, specific format requirements (e.g., date/time strings), or exact side effects** upon function calls.
    * Are there **implicit, undocumented limitations** on input values (e.g., maximum string length, file size limits, required non-zero values) that are enforced by the code but not specified in the documentation?

2.  **Edge Case Identification & Initialization:**
    * Identify critical **zero-value or nil-pointer dereference** risks in public methods, especially when optional parameters or configurations are not provided during initialization.
    * List required fields or settings that **will panic or return a non-idiomatic error** if omitted during construction or initialization.

## 2. Fault Tolerance and Error Handling

1.  **Error Propagation and Inspection:**
    * Analyze the **error propagation strategy**. Does the library consistently use Go's standard error wrapping (`fmt.Errorf` with `%w`) to preserve the error chain, or does it discard original error information?
    * Does the library define and export **sentinel errors** (e.g., `ErrResourceExhausted`) for common, predictable failure states, allowing external code to reliably check them?

2.  **Resource Management Safety:**
    * For types requiring **cleanup (e.g., database connections, file handles)**, is the required **`Close()`** method safe to call multiple times? Is there documentation on the consequences if `Close()` is *not* called?
    * If the library includes network/external dependency logic, are **retries, backoffs, or timeouts** configurable or clearly documented to prevent test failures due to external instability?

## 3. Non-Functional Concerns (Concurrency and Performance)

1.  **Concurrency Guarantees:**
    * Verify all **thread-safety guarantees** (or lack thereof) for exported types. If a type is safe for concurrent use, is the underlying mechanism transparent or clearly stated in the documentation?
    * Identify public functions or methods that are **known to be slow** (e.g., performing blocking I/O or heavy computation) and verify that the documentation warns the user to execute them asynchronously.

2.  **Performance and Memory:**
    * Identify any function or method that involves **deep copying of large data structures** or **unbounded growth of internal maps/slices** that could lead to unexpected memory usage or performance spikes under load.
    * If the library implements internal caching, are the **cache eviction policies and any documented memory usage limits** transparent to the user?
