---
name: go_api_review
description: Performs a critical review focused on discoverability, clarity, and ease of use (Developer Experience) of a Go package, relying exclusively on the exported API (Godoc) and the README, without reading internal source code.
---

# Go API Review Skill

As an agent using this skill, your task is to evaluate a Go package as a potential user evaluating it. You must rely exclusively on the exported API (Godoc) and the README. **Do not read the internal source code.**

Please perform a critical review focused on **discoverability, clarity, and ease of use (Developer Experience)**, specifically addressing the following points using idiomatic Go standards:

## 1. API and Interface Quality (The "Clean" Test)

1.  **Interface Design:**
    * Are the exported interfaces small, single-purpose, and named by their behavior (e.g., `io.Reader`), or are they large and monolithic?
    * Does the library adhere to the "Accept Interfaces, Return Structs" principle?

2.  **Method Clarity & Idioms:**
    * Are all public functions and methods focused on a single responsibility?
    * Is there any ambiguity regarding when to use a **value receiver** vs. a **pointer receiver** on methods, and are pointer receivers used consistently where mutation occurs?
    * Are method signatures simple, or do they involve complex, unnecessary nesting of custom types?

3.  **Error and Context Handling:**
    * Are **`context.Context`** and standard **`error`** returns implemented idiomatically?
    * Is the error handling strategy (e.g., sentinel errors vs. wrapped errors with `%w`) clearly documented for users?

## 2. Documentation and Examples (The "Good Enough" Test)

1.  **Clarity and Purpose:**
    * Does the **package-level Godoc** clearly and concisely state the package's single purpose and value proposition (i.e., *what problem does this solve and why should I use it?*).
    * Are there any exported functions, types, or fields that lack a clear, helpful top-level Godoc comment, making their purpose ambiguous?

2.  **Examples and Learning Curve:**
    * Does the **README** contain a clear, minimal, and **working code example** that demonstrates the single most common use case?
    * Are there specific **`Example`** functions within the tests (`_test.go` files) that demonstrate essential functionality (e.g., initialization, common API calls, and simple error handling) and appear correctly in the Godoc?
    * Are there examples for non-trivial scenarios, such as **initialization with dependencies** (e.g., database clients) or **custom configuration**?

## 3. Ambiguity and Misuse (The "Caveat" Test)

1.  **Potential Misuse:**
    * Are there any public functions or types that could be easily misused or lead to unexpected behavior (e.g., side effects or hidden dependencies)? If so, is there a clear warning or **"When NOT to Use"** section in the documentation?

2.  **Concurrency and Lifecycle:**
    * Are there documented **thread-safety guarantees** or requirements for concurrent use? If the main struct holds state, is it clear whether a single instance is safe for concurrent use?
    * Is the recommended lifecycle (e.g., creating the main object via **`New()`** and cleaning up via **`Close()`**) explicitly documented?
