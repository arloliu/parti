# Independent Codex Investigation (Perspective B)

> Produced by an independent **blind** `codex:codex-rescue` run (job
> `task-mpqurj0r-o24gym`, 5m57s, model default). It did **not** read perspective A
> (`00-repro-design.md`). Codex's sandbox was **read-only**, so it could not write its
> intended structured report file; the genuine deliverable was its returned summary,
> captured verbatim below in §A. §B captures one load-bearing nuance from its
> reasoning log (clearly marked as such). Claude then independently **verified** the
> central mechanism against the source — see §C.

---

## §A. Codex's returned summary (verbatim)

> I completed the read-only investigation, but I could not create
> `/home/arlo/projects/parti/tmp/codex-auto-healing-investigation.md`: the write was
> rejected by the read-only sandbox.
>
> Core conclusion: the non-recovery is not just "deadline errors do not enter
> degraded mode," although that is true. The stronger root cause is in
> `internal/durable/claim_resolver.go`: if `Keys()` succeeds but per-key `Get()`
> returns `context deadline exceeded`, reconcile can synthesize delete tombstones at
> `oldRevision+1`; after quorum returns, live KV claims at `oldRevision` are treated
> as stale and ignored. Pull gating then permanently sees `GetOwner(...)=ok false`
> and suppresses pulls with `resolve_error` until process restart warms a fresh
> resolver cache.
>
> HEAD verdict: I do not see a HEAD fix. The relevant classifier, pull-gating,
> claim-resolver, force-refresh, and claim-store files are unchanged from `v2.5.0`;
> post-tag changes are mostly debounce/jitter/concurrency/source-recreate/dependency
> work and do not alter this failure path.
>
> Top repro recommendation: first write a deterministic `ClaimBasedResolver` test
> that makes `Keys()` return `claims/USER21`, makes `Get()` return
> `context.DeadlineExceeded`, runs `reconcileOnce`, then restores `Get()` to return
> the original claim at the original revision and proves `GetOwner` remains false.
> That directly captures the irreversible tombstone mechanism without depending on
> real NATS replica placement.
>
> Codex session ID: 019e738b-3230-7333-a394-4202aa574ca3
> Resume in Codex: codex resume 019e738b-3230-7333-a394-4202aa574ca3

---

## §B. The asymmetry nuance (Codex's reasoning log + Claude's code derivation)

The one statement Codex made in its reasoning log (job log, not in the returned
summary) beyond §A, **verbatim**:

> "its reconciler can synthesize delete tombstones when `Keys()` succeeds but per-key
> `Get()` times out, and those tombstones carry a higher synthetic revision than the
> still-existing claim. That can permanently hide valid claims from pull gating until
> a fresh process warms the cache from KV."

**Claude's derivation of the implication** (verified from code, not from Codex): the
poisoning requires the **asymmetric** state `Keys()`-succeeds **and** `Get()`-fails.
If *both* time out (the outage peak), `reconcileOnce` hits the `Keys()` error branch
and **early-returns without touching the cache** (`claim_resolver.go:978-984`) — the
cache stays warm with correct owners, no poisoning. So the poison is specific to the
partial-degradation / recovery edges, **not** the outage peak. **Repro consequence:**
a test that blanket-times-out *every* handoff read may NOT reproduce (false negative);
it must inject the asymmetric `Keys`-ok / `Get`-fail window. Whether real NATS
quorum loss actually produces that asymmetric window is **unverified** (don't guess on
NATS internals) — Tier 2 (§02) exists to check it; Tiers 0/1 engineer it directly.

---

## §C. Claude's independent verification of the tombstone mechanism (FACT)

Read against `internal/durable/claim_resolver.go` at HEAD — the mechanism Codex
describes is **real and irreversible**:

1. `reconcileOnce` snapshots the cache, then `keys, err := r.kv.Keys(ctx)`
   (`claim_resolver.go:977`). If `Keys()` itself errors, it **early-returns without
   touching the cache** (`:978-984`) — the benign path (no poisoning).
2. If `Keys()` succeeds, the per-key loop does `entry, err := r.kv.Get(ctx, k)` and on
   error `continue`s (`:995-999`) — so that pid is **never added to `seen`**.
3. The tombstone loop iterates the pre-`Keys` snapshot; any pid in the snapshot but
   **not in `seen`** is staged as a synthetic delete at `e.revision + 1`
   (`:1021-1035`).
4. `applyPendingBatch` applies it because `existing.revision (R) >= p.revision (R+1)`
   is false (`:859`) → cache entry becomes `{deleted:true, revision:R+1}`.
5. **After recovery** the live claim returns at its real revision `R`. The incoming
   upsert is rejected because now `existing.revision (R+1) >= p.revision (R)` is true
   → `continue` (`:859-862`). The tombstone **wins permanently**.
6. `GetOwner` returns `ok=false` for a `deleted` entry (`:447-455`), so
   `shouldSuppressPull` logs `pull gating resolve failed: partition not found` and
   returns `true, "resolve_error"` (`worker_consumer.go:656-659`) — **forever**.
7. Only a process restart clears it: fresh `warm()` rebuilds the cache from live KV at
   the correct revision, no tombstone.

This is a **distinct, more specific** mechanism than perspective A's primary
hypothesis (manager never enters Degraded because `context.DeadlineExceeded` is
unclassified). The two are **complementary**, not competing — see `02`.
