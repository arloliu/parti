# Tier 0 Execution — Model & Effort Recommendation

- **Date:** 2026-05-29
- **Status:** Recommendation for the Tier 0 dispatch (`02-consolidated-design.md` §4 Tier 0).
- **Scope:** how to run the Tier 0 sub-agent — model, "effort", and the
  scaffolding/controls that actually save time. Tier 1/2 get a one-line
  forward note; they are not dispatched yet.

> **The headline:** for Tier 0 the model choice is the *least* important
> variable. The two things that decide wall-time and correctness are (a) handing
> the sub-agent the existing harness pointers so it doesn't re-discover them, and
> (b) the **control pairings** that stop the test from passing for the wrong
> reason. Both are encoded in the dispatch prompt below.

---

## 1. Decision (Tier 0)

| Knob | Recommendation | Why |
|---|---|---|
| **Agent** | **Claude Agent** (Agent tool), not Codex | Tier 0 is a same-package white-box Go test on a harness *we already hold context for*. A Codex dispatch re-orients blind (its prior run here was read-only and could not even write its file) — throwing away the exact context that makes Tier 0 cheap. |
| **Model** | **Sonnet 4.6** (`model: "sonnet"`) | White-box unit test, low LOC, no NATS, no concurrency, mature reusable harness, and an exceptionally detailed spec (mechanism + line refs in `02` §2). Squarely Sonnet's competence; Opus's reasoning headroom buys little on a task this well-specified, and Sonnet is the time/cost-efficient choice — which is the goal. |
| **"Effort"** | Rigor lives in the **prompt**, not a dial | A Claude Agent has no literal effort knob (that exists only for Codex). "Effort" here = the verification controls in §3 mandated in the prompt. Express it that way; don't leave it ambiguous. |

**Escalation rule (evidence, not speculation):** if the sub-agent's first pass
gets the revision-guard semantics wrong (e.g. asserts a tombstone that the guard
would actually overwrite, or a control that passes vacuously), re-dispatch the
*correction* on Opus. Don't pre-spend Opus on the first pass.

---

## 2. The real time-saver — scaffolding pointers (put these in the prompt)

The main session already located everything a fresh sub-agent would otherwise
spend its first minutes re-finding. Hand it these verbatim:

**Placement (a correction to `02` §3/§4 — non-negotiable):**
`reconcileOnce`, `applyPendingBatch`, `handleWatcherUpdate`, `warm` are
**unexported**, so Tier 0 **must** be a same-package white-box test —
`package durable`, new file e.g.
`internal/durable/claim_resolver_quorumloss_test.go`. It **cannot** live in the
standalone `tmp/parti-repro/` module: Go's internal-package rule forbids a
separate module from importing `github.com/arloliu/parti/v2/internal/durable`,
even with a `replace`. (Only Tier 1/2, which use the **public** `consumer`/
`manager` API, belong in `tmp/parti-repro/`.) This correction also *reduces*
Tier 0's difficulty — it means reusing the in-package harness below.

**Reusable harness (already in tree — extend, don't rebuild):**
- `internal/durable/claim_resolver_restart_test.go`
  - `mockKVForReconcile` (`:563`), `newMockKVForReconcile` (`:570`),
    `mockKVEntryFull` (`:602`) — a `jetstream.KeyValue` fake with `Keys()`+`Get()`.
  - `TestClaimResolver_ReconcileDoesNotRegressLaterWatcherUpdates` (`:197`) —
    seeds a high-revision cache entry then calls `r.reconcileOnce(ctx)` directly.
    **This is the structural template** for every Tier 0 case. (It is a
    *revision-guard* control, **not** the healthy-reconcile control of §3 — that
    one must be added.)
- `internal/durable/claim_resolver_test.go` — `marshalClaim` helper, `mockKV`,
  direct `applyPendingBatch` / `GetOwner` usage patterns.
- `internal/durable/claim_resolver_consistency_test.go:172` — `mockKVClient`
  with `WatchAll`+`Keys`, if a watcher-driven delivery is needed for A″.

**The one harness gap to close (~30–50 LOC):** `mockKVForReconcile.Get` has **no
per-key error injection**. Add a `getErr map[string]error` (a key returns
`context.DeadlineExceeded` while `Keys()` still lists it = the asymmetric window)
and a configurable `keysErr error` (so `Keys()` itself can fail = Case B). Keep
per-entry revisions configurable so a claim can sit at `R` while a tombstone
lands at `R+1`.

**Mechanism anchors (from `02` §2, verified; anchor on symbol names, not the
line numbers — they drift):** tombstone synthesized at `e.revision + 1`
(`reconcileOnce`, ~`:1021-1035`); apply guard rejects `R` once `R+1` is cached
(`applyPendingBatch`, ~`:862-868`); `GetOwner` returns `ok=false` for a `deleted`
entry (~`:447-455`); benign early-return when `Keys()` errors (~`:978-984`);
per-key `Get` error → `continue`, pid never enters `seen` (~`:995-999`).

---

## 3. False-green defense — the controls (this is the "effort")

Tier 0's assertions are mostly **absence** (`GetOwner` stays `ok=false`). Absence
passes for many *broken* reasons (wrong prefix, claim never warmed, mock returns
nothing). The verify-first memory doesn't map cleanly here — there is no fix, so
no "fails-on-parent / passes-after-fix" cycle. **Controls, not verify-first, are
what make each case meaningful.** Mandate these in the prompt:

1. **Healthy-reconcile control (MISSING from `02` §4 — must be added).**
   `Keys`-ok **and** `Get`-ok at `R` → `reconcileOnce` → assert `GetOwner` =
   `ok=true`. Case A must differ from this control by **exactly one variable**:
   that key's `Get` fails. This pairing is what proves the `Get`-fail is the
   *specific* cause of the tombstone. Without it, Case A could be tombstoning for
   an unrelated setup reason and look like a real repro. (Case B tests the
   `Keys`-fail early-return — a *different* branch — and A″ tests the heal;
   neither substitutes for this control.)
2. **Case A (the bug):** healthy → asymmetric `Keys`-ok/`Get`-fail → `reconcileOnce`
   → restore `Get` to `R` **and** deliver the live claim at `R` via the watcher
   path → assert `GetOwner` **still** `ok=false`. Plus the restart control: a
   fresh `warm()` over the same KV returns `ok=true` (proves restart-fixes-it).
3. **Case A′ (fleet-wide):** `Keys`-ok / **all** `Get` fail in one pass → assert
   **all** pids tombstoned.
4. **Case A″ (the heal):** after Case A's `R+1` tombstone, deliver a claim
   re-write at `R+2` via the watcher → assert `GetOwner` becomes `ok=true`
   (proves a KV re-write beats the tombstone — input the fix authors need).
5. **Case B (boundary):** `Keys()` **itself** returns `DeadlineExceeded` →
   `reconcileOnce` early-returns → assert `GetOwner` **still** `ok=true` (no
   poisoning on this branch).

A/A′/A″/B **plus the healthy-reconcile control** are the regression guard.

---

## 4. Forward notes (not dispatched yet)

- **Tier 1 / Tier 2 → Opus.** Reserve the expensive model for where it returns an
  *unknown verdict* — Tier 1 **S3** (does `scheduleApplyRetry` self-heal a
  mid-apply KV death? → falsifies or keeps Defect 3) and Tier 2 (does real quorum
  loss produce the `Keys`-ok/`Get`-fail window?). Those drive the conclusions;
  Tier 0 only makes-executable an already-code-verified mechanism.
- **Tier 1 injection seam — RESOLVED (verified in source).** `02` §4 described
  wrapping "the handoff bucket's `jetstream.KeyValue`", but there is **no**
  `WithKeyValue` option — both `consumer.NewDynamic(js jetstream.JetStream, …)`
  and `parti.NewManager(cfg, js jetstream.JetStream, …)` take a
  `jetstream.JetStream`, and each builds the handoff KV internally
  (`kvutil.EnsureKVBucket` / `manager_setup.go:ensureKVBucket`, both via
  `js.KeyValue(ctx,bucket)` + `js.CreateKeyValue(ctx,cfg)`). **The correct seam
  is therefore the `jetstream.JetStream` interface, not a KV:** embed it, override
  `KeyValue` / `CreateKeyValue` / `CreateOrUpdateKeyValue` to return a
  fault-injecting `jetstream.KeyValue` wrapper **only** for the handoff bucket
  (pass-through otherwise), and hand that wrapped `js` to **both** `NewManager`
  and `NewDynamic` so it covers the manager's claim-writer and the consumer's
  resolver. The fault KV embeds the real `jetstream.KeyValue` and toggles
  `Get`/`Keys`/`Watch` (reads) and `Create`/`Update`/`Put` (writes — needed for
  the S3 mid-apply kill). Confirmed reachable through the **public** API; Tier 1
  correctly lives in the standalone `tmp/parti-repro/` module (public API only,
  `replace … => <local parti>` for HEAD). Embedded NATS via the **public**
  `partitest.StartEmbeddedNATS`. Wiring templates to mirror: `examples/basic`,
  `test/integration/failure/claim_resolver_nats_restart_test.go`,
  `test/integration/handoff/*`.
