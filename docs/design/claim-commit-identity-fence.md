# Claim-level commit-identity fence — design v7 (FINAL)

**Status**: Proposed — deferred by decision on 2026-07-17
**Origin**: Issue #74 (claim-level commit-identity fence: close equal-Version
divergence and cross-worker claim staleness), spun out of the v2.10.0
load-overhead hardening review rounds
**Review provenance**: 7 external cross-model review rounds — 5 architectural
(P0 trajectory 4→3→2→1→0, round 5: "architecturally converged"), one final
precision pass (0 P0), one closure verification (all findings folded)
**Deferral rationale**: The hazard preconditions were measured at ZERO across
the production-shaped load campaign (~300 discontinuity events, all benign
first-Apply shapes); the most reachable interleaving of the family was closed
by v2.10.0's accepted-target fence; and the two standalone slices shipped in
v2.10.2 (the publisher CAS-loss coherent reseed closing the F2 emission path,
and the `IncEqualVersionDivergence` precondition counter). The remaining
F1/F3/F4 shapes require narrow multi-failure interleavings and are pinned as
documented-open behavior by tests (e.g.
`TestStaleCrossWorkerRetry_StealsClaim`, which MUST FLIP when this design
lands).
**Re-activation triggers**: (a) the equal-version divergence counter goes
nonzero in production; (b) a scale roadmap that needs the R2 diff-restricted
walks (the ~2P→~2T read reduction) this design gates; (c) infrastructure
prone to split-brain leadership (aggressive lease timeouts, unstable NATS
connectivity)
**Artifact note**: `tmp/...` paths cited below are design-time working
artifacts (drafts v1-v6 and the full review ledger) retained outside the
repository; the design is self-contained without them. Code anchors were
verified against main @ c690f3e (v2.10.1).


> **Historical note (2026-07-17):** the rollout language in this section and
> throughout the body — "Closes issue #74", "Targets v2.11.0", "Next gate …
> implementation", the commit train in §10 — reflects the design's state at
> convergence. The project is **deferred** (see the Status preamble above);
> the F2 publisher-reseed slice and the divergence precondition counter
> shipped separately in v2.10.2, so issue #74 is NOT closed by anything here.
> Read the body as the settled *design*, not a committed rollout plan.

Would close issue #74 (F1-F4) if implemented; gates the deferred R2 walks
(`tmp/diff-restricted-walks-design.md` §3). Targeted v2.11.0. Supersedes v6
per `tmp/fence-project-design_final_precision_review.md` (0 P0 / 4 P1
precision items / 2 P2, all folded as [FP-Pn]; "no further architectural
round is indicated"). Architectural lineage: rounds 1-5
(`_r1.._r5_review.md`), P0 trajectory 4→3→2→1→0. Anchors re-verified
against main @ c690f3e (v2.10.1); nats.go citations are MODULE-CACHED
paths (`$GOMODCACHE/github.com/nats-io/nats.go@v1.52.0/...`), not
vendored. Closure verification completed; deferred before implementation.

---

## 0. Problem in one paragraph

parti's staleness protection is Version-scalar end to end; Version is not a
unique identity (F1 alias/commit divergence, F2 CAS-loss reuse), and claims
carry no assignment identity, so stale or reset-raced walks mutate them
(F3, F4). Remediation: a semantic authority coordinate carried losslessly
through the manager, and claim-store transitions fenced on it with
phase-specific, IDEMPOTENT rules.

## 1. The authority coordinate

```
Authority = (BucketEpoch†, Version, LeaderRevision, SourceKind, EntryRev)
```

- `BucketEpoch` — assignment bucket stream `Created` identity
  (kvutil/bucket.go:122-151), equality-only (†never ordered).
- `Version`, `LeaderRevision` — today's semantic pair.
- `SourceKind` — commit(1) > alias(0) at equal (V, LR): matches the
  dual-read selector (manager_select_authority.go:53-65; round 2 confirmed
  commit and legacy alias are the ONLY two sources; cold-empty bootstrap is
  absence, not a third).
- `EntryRev` — delivering entry's KV revision; orders only within the same
  (V, LR, SourceKind) class (re-publications, e.g. F2's overwrite).

### 1.1 Comparison relations [zero-epoch semantics per R2-P1-2]

The comparator returns one of FOUR results — newer / equal / older /
**unproven** — and epoch participation is explicit:

- **Manager-local ordering** (dispatch gates, stashes, fence): all
  deliveries observed by one Manager come from ITS assignment bucket, so
  the manager compares only (V, LR, SourceKind, EntryRev). A failed epoch
  capture does NOT disable manager-side semantic ordering (else F1 would
  reopen locally). BucketEpoch is carried for stamping, not for
  manager-local comparison.
- **Claim-side proof** (fence checks against a stored stamp): requires a
  VALID epoch on both sides. stamp.epoch zero/absent → stamp UNPROVEN;
  walk.epoch zero (capture failed) → walk cannot PROVE, §6.5; epochs
  valid and different → UNPROVEN (foreign); epochs valid and equal → full
  lex ordering on the remaining four.
- Exact equality of all five = duplicate/idempotent retry — admissible
  everywhere equality is admissible today (isApplyResultStale model,
  manager_assignment.go:1344-1366).

## 2. Delivery envelope and manager reform

### 2.1 The envelope

As v2: `authorityEnvelope{kind, bucketEpoch, entryRev, payload}` plumbed
through every holder that today discards revisions — watcher debounce
(manager_assignment.go:751-838, :929-943), reconcile (:903-913), startup
Gets (:352-362, manager.go:785-838), `stashedCommit`, apply-retry stash,
fetch-retry stash, drain (:1213-1221), current/committed snapshots,
degraded recovery comparison (:1522-1530, manager_degraded.go:504-520),
fleet-size observation (:1029-1039), accepted-target fence.

### 2.2 Two-stage admission [rewritten per R2-P0-3]

Admission splits into two distinct decisions:

1. **Authority admission** — the envelope comparator + dual-read
   selection, decided BEFORE payload construction (the commit handler
   has every input at manager_assignment.go:1082-1129 ONCE COMMIT 1's
   envelope plumbing lands — today `decodeCommitEntry` discards the
   entry revision, :929-943 [FP-P2 wording]).
   - Authority-REJECTED deliveries (e.g. the post-commit compat alias
     losing to its equal-(V,LR) commit) NEVER raise the accepted-target
     fence. The alias handler's raise moves after ITS authority
     admission.
   - Authority-ADMITTED deliveries raise the fence AT ADMISSION — before
     payload fetch — preserving the v2.10.0 rule that a fetch failure
     still fences out older sibling retries (:1132-1164). This is the
     explicit reconciliation of "rejected never raises" with "raise
     pre-fetch": the subject of each clause is a different decision.
   - A successfully STASHED commit is authority-admitted: it raises
     immediately at stash time (the current branch returns before the
     raise, :1118-1129 — changed).
2. **Materialization/applicability** — payload fetch, decode, digest,
   label-incarnation validation (:1224-1269, :1533-1578). Failures here
   never un-raise: an authoritative-but-unfetchable commit keeps its
   fence (fetch-retry converges to it); a terminal incarnation rejection
   keeps it too (convergence waits for the next label-correct commit,
   matching :1533-1538's existing terminal disposition).

Startup follows the same split: once the commit is authority-admitted over
the equal-authority alias, startup RETAINS the commit envelope and retries
its materialization; it must NOT fall back to applying the lower-ranked
alias (today's fallback at manager.go:785-838 — changed).

Dispatch reform proper (closes F1): both handlers admit iff strictly newer
by §1.1 manager-local ordering; equal-(V,LR) commit-after-alias admits via
SourceKind; admitted equal-Version deliveries full-apply (adjacency
already fails open on equality, twophase.go:304-318 post-v2.10.1). Rule
900: no pinned
surface touched (round 1); pinned tests run regardless.

## 3. Claim fence [rewritten per R2-P0-1, R2-P0-2]

### 3.1 Schema

```go
// Authority is the coordinate of the walk that last legitimately mutated
// this claim; nil = unproven (pre-fence writer, erased by an old worker,
// or fence-inactive stamper). Nil-safe by construction.
Authority *ClaimAuthority `json:"authority,omitempty"`
```

Embedded pointer per round-2 Q3: nil cleanly encodes absent, no partially
populated coordinates, one serialization point.

### 3.2 Phase rules — idempotent by construction

Discriminators available at each check: the claim's Authority (proof
relation per §1.1), `Owner`, `PendingOwner`, `State`. For same-epoch
proven stamps, strict-newer vs exact is decidable from the coordinate;
Owner/PendingOwner/State then distinguish "already completed" from
"missing". Across foreign epochs chronology is UNDECIDABLE by design —
foreign/unproven always takes the fail-open row.

"Progressed state" below always means `PendingOwner == ""` EXACTLY — not
merely "no foreign PendingOwner". `NextCommit` clears it (claims.go:76-85)
and the eager sweep relies on it being empty (twophase.go:1313-1316); a
defensive `State∈{commit,stable}` + `PendingOwner==self` record matches NO
progressed row and takes the repair row deterministically. [R3-P0-1]

| Phase | Claim observation | Action |
|---|---|---|
| Prepare | exact + `State==prepare` + `PendingOwner==self` | **idempotent no-op** — this walk's prepare already completed; re-running `NextPrepare` would rewrite every gained claim on each whole-Apply retry (e.g. under repeated consumer-updater failures) [R4-P1-2] |
| Prepare | mutation REQUIRED (new / foreign-owned / stale in-flight claim), stamp unproven or proven ≤ incoming | perform the prepare/reset mutation + stamp incoming |
| Prepare | clean self-owned stable claim, stamp UNPROVEN | stamp-only backfill (+ epoch advance, §3.3) |
| Prepare | clean self-owned stable claim, stamp proven ≤ incoming | **no-op** — a proven older stamp identifies the last ownership-affecting authority; routine discontinuous full walks (previous treated as empty, twophase.go:359-377) must NOT restamp all of `next` [R3-P1: this row is what keeps steady-state claim writes at zero] |
| Prepare | proven > incoming | per-partition skip (stale walk) |
| Commit | exact + `State==prepare` + `PendingOwner==self` | promote + restamp |
| Commit | `Owner==self`, `PendingOwner==""`, `State==stable`, proof ≤ incoming or unproven | **idempotent success** (fully progressed); stamp-only backfill iff unproven |
| Commit | `Owner==self`, `PendingOwner==""`, `State==commit`, EXACT authority | commit success (this walk's own chain); stabilize will finalize |
| Commit | `Owner==self`, `PendingOwner==""`, `State==commit`, proven < incoming or unproven | **adopt-and-restamp**: CAS `State: commit` unchanged, Authority := incoming, epoch advance — the stranded commit (crash between commit and stabilize of an EARLIER walk) is adopted into this walk's chain so stabilize's exact row finalizes it in THIS Apply. Not an error, not a bare success: the observation must change so the pipeline converges. [R3-P0-1's fix — the v3 row pair (broad commit success + exact-only stabilize) looped here] |
| Commit | proven > incoming | per-partition skip |
| Commit | otherwise (lost/foreign prepare, no newer proven authority) | **inline re-prepare**: CAS the claim to `prepare/PendingOwner=self` stamped with incoming (fresh read, PutIfEpoch), then return a RETRIABLE Apply error so the next attempt commits it. [Round-2 Q2 mechanism; runs in the COMMIT arm, so prepare's gained-only visit set (:544-559) is irrelevant] |
| Stabilize | `Owner==self`, `State==stable`, `PendingOwner==""`, proof ≤ incoming or unproven | **idempotent success** (a concurrent sweep legally finalized, :1313-1316); stamp-only backfill iff unproven |
| Stabilize | exact + `Owner==self` + `State==commit` + `PendingOwner==""` | finalize + restamp (the commit rows repair defensive `PendingOwner==self` records first; the predicate here is table precision [R4-P2]) |
| Stabilize | proven > incoming | per-partition skip |
| Stabilize | otherwise | retriable error (the commit rows above rebuild the chain on retry) |

Livelock bound: every retriable-error row either (a) CASes the claim
toward the incoming authority's chain (progress: re-prepare and
adopt-and-restamp both change the decidable observation), or (b) on the
next observation finds a strictly newer proven authority (skip) or a
fully-progressed self-owned state (success). Commit and stabilize rows
now agree on every self-owned observation — the v3 gap (commit succeeds,
stabilize errors, nothing changes) has no remaining row pair. [R3-P0-1
test list §9.]

### 3.3 Universal epoch-advance invariant [per R2-P0-2]

**Every successful mutation of a Claim record advances logical
`Claim.Epoch` exactly once.** Already true for NextPrepare/NextCommit/
NextStable (claims.go:66-96) and the prepare cleanup branch
(twophase.go:625-642). This project extends it to:

- the coordinator sweep expiry reset (twophase.go:1318-1324),
- the STARTUP-HYGIENE expiry reset (manager_handoff.go:167-185 — missed
  in v2),
- stamp-only backfill writes (§4),
- any future authority-only restamp.

Rationale: `PutIfEpoch` compares only the logical epoch on its re-read
(kv_store.go:126-155); a mutation that preserves it does not invalidate a
transform computed before it. Round 2's consumer audit: the resolver
caches/exposes epoch but the processing and pull gates decide on
owner/state only (claim_resolver.go:505-516, processing_gate.go:156-173,
worker_consumer.go:737-746) — the invariant is compatible.

Sweep transitions still never stamp Authority (they act without
assignment authority); they only advance Epoch.

## 4. Mixed-fleet convergence [conditions per R2-P1-1, R2-P1-3]

1. **Backfill condition = UNPROVEN only** (absent, zero-invalid, or
   foreign epoch) — NOT "older". A proven older stamp identifies the last
   ownership-affecting authority; rewriting it on every global Version
   bump would be O(P) claim writes per commit forever (every publish
   advances Version, assignment_publisher.go:368-370; commit+stabilize
   walk all of next, twophase.go:674-713,728-759). With unproven-only,
   the first post-uniform walk writes once per unproven claim (bounded by
   PhaseConcurrency + the claim-write limiter) and steady state writes
   nothing.
2. Backfill writes advance Epoch (§3.3) and set Authority to the walk's
   coordinate; they run in the idempotent owner-stable branches only.
3. **Convergence signal = a GAUGE, not the event counter**:
   `claim_fence_unproven_remaining` — an unlabeled per-process gauge
   (cardinality shape of the existing `claim_store_size`,
   types/handoff_metrics.go:37-41) — set after a COMPLETED full walk.
   Interrupted-walk encoding [R3-P2]: a companion unlabeled 0/1 gauge
   `claim_fence_convergence_valid`; an interrupted or not-yet-completed
   walk sets valid=0 and leaves remaining at its last value. Fleet
   query: converged ⇔ every active worker reports valid==1 AND
   sum(remaining)==0. The (phase, reason) event counter stays for
   diagnostics but cannot "reach zero". R2's activation gate reads the
   gauge pair, not the counter.
4. Mixed-state safety statement, scoped per R2-P1-2: at every mixed
   version/stamp/epoch state the fence's OWNERSHIP-SAFETY behavior is
   never worse than today's unfenced behavior (fail-open on unproven;
   PutIfEpoch serializes writers; every mutation advances Epoch). Load
   behavior is NOT claimed identical: backfill adds bounded claim writes
   during convergence windows.

## 5. Wire format

As v2: claims additive (`authority` object, omitempty); payloads
unchanged; PrevCommitRev stays diagnostic; MIGRATING gains the
mixed-fleet + convergence story.

## 6. Bucket epoch

1. Clear-on-trip rejected (round 1). Stamps carry Created identity;
   epochs compare for identity only (§1.1).
2. Foreign-epoch stamps are UNPROVEN → fail-open re-stamp; during an
   epoch transition protection degrades to baseline and re-converges via
   §4. The resumed-old-epoch-walk bound is the §4.4 ownership-safety
   statement, now underwritten by the universal Epoch-advance invariant
   (round 2 concurrence).
3. MemoryStorage restart = recreation (new Created) ⇒ same path. A
   restart shape preserving Created while resetting revisions gets a
   reproducer (spec 6b).
4. **Delivery-generation binding** [R3-P0-2, rewritten per R4-P0]: a
   stamp's BucketEpoch must be the generation of the DELIVERY, never a
   cached value that may predate a recreation. Current code captures
   `Created` once at Start and never rebinds (manager.go:139-145,
   manager_setup.go:648-695). **Channel closure is NOT a generation
   boundary**: nats.go closes `Updates()` only when the subscription
   closes (module-cached nats.go@v1.52.0 jetstream/kv.go:1304-1358),
   silently recreates an
   inactive ordered consumer in place (js.go:2346-2374, :2255-2311), and
   parti's own code documents watchers stalling open across server
   restart (manager.go:641-648, claim_resolver.go:864-873, and the
   project's standing empirical finding that the reconciler — not
   channel closure — is the load-bearing recovery path). The protocol
   therefore proves generations directly:
   - **Watcher establishment bracket**: pre-status → `Watch` →
     post-status. Establishment succeeds only when both reads succeed
     and agree on `Created`; that value is the SESSION identity. On
     mismatch/error: stop and discard the watcher BEFORE dispatching any
     buffered entry, retry establishment.
   - **Per-entry proof with a two-outcome failure taxonomy** [R5-P1]:
     an entry dequeued from a session is admitted with the session
     identity only under `pre == session == post` around
     dequeue/admission (deliveries are rebalance-rate; two status reads
     per delivery are cheap). Failure is CLASSIFIED, never conflated:
     - **Unproved** — a probe ERRORED or was unavailable, and no
       successful probe contradicts the bound generation: envelope goes
       EPOCH-LESS (unproven) — or the Apply is a retriable error under
       `RequireClaimFence` (§6.5). Baseline-bounded, per §4.4.
     - **Proven mismatch** — a SUCCESSFUL probe returned a Created that
       contradicts the bound generation: the candidate is DROPPED
       (never admitted epoch-less), the §6.4a sticky cutover latches,
       and the logical session terminates. Epoch-less admission of a
       proven-foreign delivery is prohibited — it would re-enter
       manager-local ordering against old-generation holders, exactly
       the comparison §6.4a exists to prevent.
   - **Explicit session termination**: on any proven mismatch, terminate
     the LOGICAL session (stop the watcher; re-establishment is moot
     once the cutover has latched) — never wait for `Updates()` to
     close. Applies to BOTH the assignment and commit watcher loops
     (manager_assignment.go:387-449, :709-748).
   - **Probe handles**: status probes use dedicated or mutex-serialized
     handles — the natsutil contract forbids concurrent Status and
     production ops on one handle (kvstreampos.go:67-81).
   - **Authority-producing Get inventory** (each gets the double-status
     bracket; the envelope is not published before the second proof):
     startup alias Get (manager_assignment.go:352-362), startup commit
     Get (manager.go:785-798), assignment-watcher reconcile (:616-637),
     commit-watcher reconcile (:903-913), and degraded-recovery
     `refreshAssignmentFromNATS` (:2184-2219, manager_degraded.go:
     491-520). **Startup common-generation rule**: the alias and commit
     Gets must prove the SAME generation before composing one authority
     decision — two individually-valid brackets returning E1-alias +
     E2-commit are rejected (retry).
   - **Non-authority Gets**: the removal guard's `_commit` read and
     payload Gets (:2358-2445) validate an already-selected delivery;
     they RETAIN the selected envelope and never substitute their own
     captured epoch.
   - **Recapture**: establishment/bracket sites re-run capture, so a
     transiently failed Start-time capture can recover
     (RequireClaimFence retries can succeed).
   - Spec 24's oracle (clarified per R4-P1-1): (a) an E1 delivery is
     never mislabeled E2 (and vice versa), and (b) the §6.4a cutover
     rule below holds.

   **4a. Manager-wide cross-generation cutover — bound generation +
   sticky atomic admission barrier** [R4-P1-1, contract per R5-P1]:

   - **Manager-bound generation**: one atomic manager-level
     `authorityGeneration` value. It is bound by the STARTUP
     common-generation proof — or, when Start-time capture was absent
     (transient failure), by the FIRST later successful proof (late
     binding). Until bound, all admissions are epoch-less/unproved-path
     by construction.
   - **One admission gate with an ATOMIC ADMISSION+PUBLICATION scope**
     [FP-P1-1]: every authority-producing path — both watcher loops,
     both reconciles, startup, and degraded-recovery
     `refreshAssignmentFromNATS` (which can re-arm an Apply,
     manager_degraded.go:481-520) — passes through a single gate. The
     gate is a mutex (house style: the applyStoreMu pattern) HELD FROM
     the latch/proof check THROUGH the candidate's publication into the
     comparator holders (`lastSeenAlias`, `lastObservedCommit`,
     debounce state, stashes, accepted fence, current/committed
     snapshots, manager.go:162-241) — check-then-unlock-then-publish is
     PROHIBITED: the alias and commit monitors are independent
     goroutines (manager_startup_async.go:123-135), and a latch landing
     between an unlocked check and a later publication would violate
     spec 49's ordering oracle. Work already PENDING at latch time
     (debounced entries, armed retries) re-validates under the gate
     before dispatch — a latched gate discards it.
   - **Sticky latch**: the first PROVEN mismatch atomically sets a
     one-way `generationCutover` flag inside the gate. From that point
     every authority admission — including pending debounce work, which
     is discarded, and any retry that would be armed from the
     mismatching delivery, which is not — is dropped with a distinct
     log/metric. The manager rides its existing terminal-degraded path
     to rotation (epoch mismatch is already terminal,
     manager_degraded.go:607-654; `enterDegraded` alone is NOT an
     admission lock, :315-364 — the gate is). The fresh process binds
     the new generation. **Recovery-exit hold keyed on the latch
     itself** [FP-P1-1]: the degraded-recovery exit guard must check
     `generationCutover` DIRECTLY, not (only) the `bucketEpochs` map —
     a LATE-BOUND manager has no map entry (failed Start-time capture
     leaves none, manager_setup.go:648-695; the existing guard only
     ranges map entries, manager_degraded.go:568-575,639-654), and
     without the direct hold it could exit Degraded into a Stable state
     whose sticky latch drops all future authority. Spec 51b pins this.
   - **Qualified invariant** [R5 answer 1]: no PROVED cross-generation
     candidate is ever compared against a holder, and NOTHING is
     admitted after cutover. An UNPROVED candidate (probe unavailable,
     no contradicting proof) may take the epoch-less baseline-bounded
     path — that is the documented §4.4 fail-open, not a violation.
   - No partial holder reset is ever attempted; the cutover removes the
     cross-generation comparison problem by construction and matches
     the existing operator contract (bucket recreation is a disruptive
     action already requiring rotation).
5. **Fence-inactive handling** [round-2 Q1]: epoch capture failure keeps
   manager-local ordering fully active (§1.1) but claim stamps are
   written epoch-less = unproven-by-construction; WARN + gauge
   ("claim fence inactive: no bucket epoch identity"). Default is NOT
   fail-closed (enterDegraded neither stops assignment processing,
   manager_degraded.go:324-364, nor blocks Apply — it is not a
   fail-closed mechanism). A separate opt-in `RequireClaimFence` config
   makes handoff Apply return a retriable error while epoch identity is
   absent, for operators who want fail-closed. Scoped as a small §10
   commit; default off.

## 7. Observability [names + commit ownership per FP-P1-3]

All series are per-process, exposed via the optional-capability recorder
pattern (as v2.10.1's sweep counters); a recorder without the capability
loses nothing else.

| Series | Kind | Owner commit |
|---|---|---|
| `claim_fence_decisions_total{phase, reason}` — a DECISION/event counter (not rejections-only: `backfill` is a successful mutation); reason ∈ {stale_walk, lost_prepare, lost_commit, foreign_epoch, backfill}; partition COUNT added per event (no partition-ID label) | counter | 4 |
| per-walk summary log (count + bounded partition-ID sample, rate-limited) | log | 4 |
| `claim_fence_unproven_remaining` (set after a completed full walk, §4.3) | gauge | 6 |
| `claim_fence_convergence_valid` (0/1, §4.3) | gauge | 6 |
| `claim_fence_inactive` (0/1 — no bucket epoch identity; clears on late binding) | gauge | 5 |
| `authority_generation_cutover` (0/1 — sticky latch state) | gauge | 5 |

The convergence signal is the §4.3 gauge pair, never the counter.

## 8. Publisher fix (F2)

As v2 (round-2 closure confirmed): lock-aware coherent recovery advancing
{lastCommitRev, currentVersion, lastCommit, lastCommitObservedAt}
(bootstrap shape assignment_publisher.go:1227-1250 not callable under
p.mu :1270-1272 — new helper); winner unreadable ⇒ fail closed, no
publish. Reproducers: V+2-never-V+1, transient-Get, malformed winner.

## 9. Test plan

### 9A. RED-first reproducer specs

Every numbered spec below is a genuine RED-first reproducer EXCEPT 16,
27, and 35, whose entries are pointers into §9B (GREEN-first gates) —
they are listed in place to keep historical numbering stable.

Carried from v2 (1-17, renumbered where noted) plus round 2's additions:

1. F1 alias-then-equal-V-commit → full Apply; V+1 trusted only after.
2. F2 V+2-never-V+1 + transient-Get + malformed-winner.
3. F3 cross-worker late retry → no steal; pinned steal test FLIPS.
4. F4 prepare-expiry → gainer re-prepares (commit-arm inline re-prepare),
   never finalizes with old owner; variant with reset between transform
   and PutIfEpoch (epoch bump defeats it).
5. Mixed-fleet stamp erasure → fail-open, baseline-bounded.
6. Epoch: (a) recreation → new-epoch walks unfenced by old stamps;
   (b) Created-preserving revision reset [FP-P1-4, DECIDED; keying per
   closure verification]: the shape is made HARMLESS by design — an
   EntryRev HIGH-WATER guard with this exact keying:
   - **Keyed per (Created identity, authority SOURCE)** — one
     high-water for the commit key, one for the alias key, per
     manager. The alias and commit watchers are independent goroutines
     with no cross-source delivery-order guarantee (a manager-wide
     scalar would false-trigger when an alias at bucket-seq 101 is
     dequeued after a commit at 102); within ONE source, deliveries
     are watcher-ordered, so per-source monotonicity is sound.
   - **Manager-held, not session-held**: the high-water lives on the
     manager keyed by the Created identity, surviving same-`Created`
     session re-establishments (session-scoped state would lose the
     baseline exactly when a reset-across-reestablishment must be
     caught). It resets only when Created changes — which is the
     §6.4a cutover anyway.
   - **Equality admits** (same-entry redelivery is the same bucket
     seq); only a STRICTLY-BELOW EntryRev with matching Created is a
     regression → the §6.4 UNPROVED path (epoch-less stamp, or
     retriable under RequireClaimFence) + WARN.
   - **Baseline-less startup**: the first delivery per (Created,
     source) establishes the baseline with no check — a pre-baseline
     reset is undetectable in-process, which is exactly the narrowed
     empirical item: pre-commit-5, confirm whether any
     locally-supported restart shape can produce a Created-preserving
     revision reset at all (evidence recorded either way).
   Spec 6b is a REAL spec owned by commit 5: inject a Created-matching
   EntryRev regression per source → unproved path + WARN, never a
   proven admission; include the cross-source reordering case (alias
   below the COMMIT high-water but at-or-above its own → NOT a
   regression).
7. Failed commit's later compat alias must not raise the fence
   [§2.2 stage 1].
8. Lower-LR/higher-EntryRev vs higher-LR/lower-EntryRev → semantic winner
   keeps the claim.
9. Sweep finalizes commit→stable between commit and stabilize → stabilize
   idempotent success, no armed retry [R2-P0-1].
10. Exact-authority owner-self stable claim → commit AND stabilize
    idempotent success.
11. Adjacent assignment, unchanged partition, claim foreign-owned/older →
    commit-arm re-prepare converges; never an infinite retry [R2-P0-1].
12. Strictly newer proven authority appears during a stale retry → skip,
    terminate, nothing restashed.
13. Stamp-only backfill between a transform and PutIfEpoch → backfill's
    epoch bump defeats the stale transform [R2-P0-2].
14. Startup-hygiene reset before/between transform and PutIfEpoch → both
    stale writes lose [R2-P0-2].
15. Zero-epoch: claim-side zero vs zero stays unproven; manager-local
    ordering still admits equal-(V,LR) commit-after-alias [R2-P1-2].
16. → GREEN-first gate, see §9B (pre-fetch fence raise).
17. Stashed newer commit raises the fence before an older retry acquires
    the apply lock [§2.2].
18. Startup: authoritative commit unfetchable → keeps envelope, no alias
    fallback [§2.2].
19. Convergence at P=10k: one write per unproven claim on the first walk
    (limiter-bounded); a later unchanged authority writes ZERO claims
    [R2-P1-1].
20. Convergence gauge zero only after a complete walk; an interrupted
    walk sets `claim_fence_convergence_valid=0` with `remaining`
    unchanged (per §4.3 encoding) [R2-P1-3, wording per R4-P2].
21. Foreign-epoch interleavings per sweep arm (eager finalize, expiry
    reset, backfill CAS).
22. Watcher shapes: same-revision redelivery, close/reopen replay,
    reconcile replay, startup alias/commit orderings.
23. Envelope holder table test — COMPARATOR behavior across every
    holder, owned by commit 3 ONLY [FP-P1-2 closure: comparator-only].
    Commit 1's adapter-predicate characterization (each holder's current
    behavior pinned bit-for-bit) is an UNNUMBERED prerequisite test
    suite described in §10 commit 1, not part of this spec.
24. Bucket recreation with paused old-epoch Apply → bound holds.
25. Partial-walk crash (after prepare / consumer update / partial
    commits) → newer authority → stale retry.
26. Concurrent Apply-origin + ticker sweep vs claim writers.
27. → GREEN-first gate, see §9B (Rule 900 pinned contract tests).
28. RequireClaimFence: absent epoch identity ⇒ retriable Apply error;
    default off ⇒ WARN+gauge only [§6.5].

Added by round 3:

29. Owner-self, `PendingOwner==""`, `State==commit`, proven-OLDER
    authority: ONE Apply converges to stable (adopt-and-restamp row);
    variants with nil authority and foreign epoch [R3-P0-1].
30. Defensive `State∈{commit,stable}` + `PendingOwner==self` record:
    repaired or rejected deterministically via the repair row; never
    satisfies a progressed row and loops [R3-P0-1].
31. Discontinuous full prepare over clean self-owned stable claims with
    proven older stamps: ZERO claim writes; same setup with unproven
    stamps: exactly one backfill per claim [R3-P1].
32. Bucket recreated between epoch capture and a startup/reconcile Get:
    the bracket rejects/retries or the envelope carries the NEW
    generation — never the old one [§6.4].
33. Recreation during an active watcher session: queued old-session
    entries and new-session entries never share an epoch [§6.4].
34. RequireClaimFence with a transient initial capture failure: Apply
    returns retriable errors until a later successful proof LATE-BINDS
    the manager generation (§6.4a) and unblocks it — Start does NOT
    fail (missing identity is an Apply-time condition in this design)
    [§6.4a, wording per R5-P2].
35. → GREEN-first gate, see §9B (terminal incarnation rejection keeps
    the fence).
36. Commit-arm re-prepare after the consumer update: processing gate
    stays closed for the re-prepared partition, exactly one retry
    target is scheduled, the retry re-runs the (idempotent) updater,
    then commits and stabilizes [§11 item 1 resolution].
37. Interrupted convergence after a previously reported zero:
    `claim_fence_convergence_valid` drops to 0 per the §4.3 encoding;
    the fleet query stops reporting converged [R3-P2].

Added by round 4 (generation-proof + idempotence family):

38. Establishment race, forward: pre-status E1, recreation before
    `Watch`, post-status E2 → watcher stopped, no buffered entry
    dispatched, and (post-binding) the proven mismatch latches the
    §6.4a cutover [§6.4, R5-P2].
39. Establishment race, reverse: `Watch` queues an E1 initial value,
    recreation before post-status → in a bound manager: value DROPPED +
    terminal cutover (never epoch-less; the mismatch is proven)
    [§6.4, wording per R5-P2].
40. **Silent in-place reset**: `Updates()` stays OPEN across E1→E2 (the
    nats.go ordered-consumer reset shape) → an E2 entry is never
    stamped E1; the proven mismatch drops the entry, latches cutover,
    and terminates the session [R4-P0; supersedes spec 33's
    close/reopen-only shape].
41. Queued E1 entry, recreation before dequeue → `pre==session==post`
    proof yields a PROVEN mismatch → drop + terminal cutover (never
    epoch-less) [§6.4, wording per R5-P2].
42. Alias and commit watcher sessions cross the generation change at
    different times → no E1 holder is compared against E2; the §6.4a
    cutover drops proven-E2 admissions and the manager rides to
    rotation [R4-P1-1].
43. Recreation between the startup alias Get and startup commit Get →
    startup rejects the mixed-generation pair (common-generation rule),
    retries [§6.4].
44. Recreation around `refreshAssignmentFromNATS` → the recovery-
    triggered Apply carries proven-current-generation or
    epoch-less/retriable per the taxonomy, never cached E1; a proven
    mismatch inside recovery latches the cutover BEFORE any Apply is
    re-armed [§6.4, §6.4a].
45. Reconcile observes a generation different from its watcher session
    → proven mismatch: the manager LATCHES CUTOVER AND STAYS CUT OVER
    (merely terminating/rebinding the watcher is insufficient); the Get
    is not staged beside old-session entries [§6.4a, per R5-P2].
46. Exact `prepare/PendingOwner==self/exact-authority` + repeated
    consumer-updater failures → ZERO additional claim writes across
    retries [R4-P1-2].
47. Spec-29 variants with the eager sweep injected before adoption,
    after adoption, and between transform and `PutIfEpoch`
    [tmp/fence-project-design_r4_review.md "Answers to §12" item 2].
48. Spec-30 for both `commit` and `stable` defensive records, asserting
    `PendingOwner==""` in the final stable claim
    [tmp/fence-project-design_r4_review.md "Answers to §12" item 3].

Added by round 5 (cutover-contract family):

49. Atomic cutover race: an E1-bound manager receives concurrent alias
    and commit activity while one path proves E2 → cutover linearizes
    before any comparator/holder/fence/stash update; pending debounce
    work discarded; no retry armed from the mismatching delivery;
    manager stays terminally degraded [R5-P1].
50. Proof-failure taxonomy table: matching successful probes → admit
    with bound epoch; probe errors with no proven mismatch →
    epoch-less (default) or retriable (RequireClaimFence); ANY
    successful non-matching Created → terminal drop, never epoch-less
    [R5-P1].
51. Late binding: Start-time capture fails → startup common-generation
    bracket later binds E1 → a subsequent E2 proof trips the sticky
    latch even though the original capture map entry was absent
    [§6.4a].
52. Reconcile/debounce interleaving: an E1 entry pending in EACH
    debouncer when reconcile proves E2 → neither pending entry nor the
    E2 Get is dispatched after cutover [R5-P1].
53. Non-authority Get retention: recreate during the selected commit's
    payload fetch AND during the removal guard's `_commit` read → the
    walk keeps the selected envelope's authority or fails/retries;
    never substitutes the validation Get's generation [R5-P2].

Added by the final precision pass:

51b. Late-bound cutover vs recovery exit: Start-time capture fails →
     late binding → proven mismatch latches cutover → drive
     degraded-recovery: the manager must STAY Degraded (the exit guard
     holds on `generationCutover` directly, not the absent
     `bucketEpochs` entry) until rotation [FP-P1-1].
54. Wire round-trip (§5): legacy claim JSON without `authority`
    decodes to `Authority == nil`; nil marshals to an OMITTED field;
    a populated five-field authority round-trips losslessly; existing
    assignment/commit payload encodings byte-identical [FP-P1-3].
55. Claim-phase observability (§7): the `(phase, reason)` event
    counter receives the correct partition COUNT per rejection event;
    the per-walk summary log is emitted once, rate-limited, with a
    bounded ID sample; a recorder WITHOUT the optional capability
    changes no behavior [FP-P1-3].
56. Cutover/inactive observability (§7): the `authority_generation_cutover`
    0/1 gauge sets on latch; `claim_fence_inactive` sets while epoch
    identity is absent and clears on late binding [FP-P1-3].

### 9B. Regression/contract gates (GREEN-first) [FP-P1-2]

EXISTING-behavior gates: green on the parent commit, stay green after
each train commit. Implementers must NOT attempt to make them fail
first.

- **Gate 16** — authoritative commit fetch fails with an older retry
  pending → the pre-fetch fence raise suppresses the older retry
  (behavior exists at manager_assignment.go:1132-1164; commit 3
  verifies it survives the two-stage admission refactor).
- **Gate 27** — the Rule-900 pinned contract tests (must already pass;
  re-verified at commit 3 and at the consolidated review).
- **Gate 35** — fence-before-apply + terminal incarnation rejection
  keeps the fence and performs no claim or consumer mutation (exists at
  :1132-1140, :1559-1620; commit 3 verifies).

## 10. Delivery shape [split per R2-P1-4]

Branch `feat/claim-authority-fence`, scope-pure commits:

1. `refactor(manager)`: envelope plumbing ONLY, each holder behind an
   ADAPTER predicate reproducing its current behavior bit-for-bit
   (debouncer :810-838, stashes :1409-1483 etc. keep their exact
   semantics); holder table test pins them. NO comparator activation.
2. `feat(publisher)`: F2 coherent recovery (+ spec 2).
3. `feat(manager)`: comparator activation + two-stage admission +
   fence-raise placement + startup retention (+ RED-first specs 1, 7,
   8, 17, 18, 22, 23; GREEN-first gates 16, 27, 35 verified here; spec
   12 moves to commit 4 [FP-P1-2: claim authority does not exist until
   commit 4]; commit 1's adapter characterization is an UNNUMBERED
   prerequisite of spec 23, whose numbered spec belongs here).
4. `feat(handoff)`: claim schema + phase rules + universal epoch-advance
   (both resets) + backfill (+ specs 3, 4, 5, 9-15 incl. 12, 19, 21,
   25, 26, 29-31, 36, 46-48, 54, 55; flip the pinned steal test).
5. `feat(manager+handoff)`: generation binding (§6.4: brackets,
   per-entry proof taxonomy, session termination, Get inventory,
   common-generation rule) + §6.4a bound-generation/sticky-latch
   cutover + epoch identity semantics + RequireClaimFence
   (+ specs 6a, 6b [now a real spec: EntryRev-regression monotonicity
   guard], 24, 28, 32-34, 38-45, 49-53, 51b, 56; the narrowed empirical
   confirmation of 6b's constructibility is a pre-commit-5 checklist
   item).
6. `feat(handoff)`: convergence gauges (+ specs 20, 37).
7. `docs`: MIGRATING, LIFECYCLE/ARCHITECTURE (incl. the §11-item-1
   no-overclaim rule), CHANGELOG v2.11.0.

Per-commit codex rounds + consolidated whole-branch review before PR;
full `make pre-pr` + sims; rig optional.

## 11. Resolved in round 3 (design notes, no longer open)

1. **Re-prepare vs consumer-update ordering** — compatible; no updater
   re-run inside the failing attempt. The updater received the complete
   `next.Partitions` set before commit began (twophase.go:400-415); the
   retriable error re-enters the whole Apply pipeline including another
   idempotent updater call (manager_assignment.go:1734-1759,
   :1982-1993); while the claim is prepared, the OPTIONAL processing and
   pull gates suppress work on owner/state (processing_gate.go:156-184,
   worker_consumer.go:737-763). Wording rule adopted: the fence does NOT
   strengthen the consume-path guarantee — without the optional gates,
   two-phase handoff orders release but does not prevent consumption
   overlap (docs/LIFECYCLE.md:217-238,265-274), unchanged by this
   project. MIGRATING/LIFECYCLE text must not overclaim.
2. **Gauge cardinality** — no objection: both gauges are unlabeled
   per-process series, the shape of the existing `claim_store_size`
   (metrics_prometheus.go registration, no labels).

## 12. Round-4 confirmations carried as design facts

- Adopt-and-restamp + defensive-record sequences verified convergent by
  round 4 against NextCommit/NextStable/PutIfEpoch (incl. eager-sweep
  interleavings; the sweep predicate is `State==commit &&
  PendingOwner==""`, twophase.go:1287-1317).
- The double-status bracket is sufficient for a single synchronous Get
  iff both reads succeed, agree, surround the Get, use serialized probe
  handles, and the envelope is not published before the second proof.
- Channel-closure-based watcher binding is NOT implementable (silent
  ordered-consumer reset); the §6.4 per-entry-proof redesign is the
  implementable form.

## 13. Round-5 confirmations carried as design facts

- Zero P0: no path assigns a proved E1 delivery an E2 stamp or vice
  versa; the Get inventory is complete (round-5 answer 1).
- Phase table is fully retry-idempotent: no remaining shape writes a
  claim while changing only Epoch/LastUpdated (round-5 answer 2).
- Specs 49-53 + the taxonomy/cutover wording above are the folded
  round-5 P1/P2; commit 5 of §10 owns them.

## 14. Status

**DEFERRED (2026-07-17)** — see the Status preamble at the top of this
document for the deferral rationale and re-activation triggers. The design
is complete and settled; implementation has not started.

Architecture CONVERGED per round 5 (0 P0). Final precision pass ran
(0 P0 / 4 precision P1 / 2 P2 — all folded in this v7): atomic
admission+publication scope + cutover-keyed recovery hold, spec
ownership made one-to-one with GREEN-first gates separated, §5/§7 specs
and metric names added with commit owners, 6b resolved by the EntryRev
monotonicity guard, anchors refreshed to main @ c690f3e. Closure
verification of these folds completed (all findings folded). At that
point the project was deferred by decision rather than proceeding to the
§10 implementation train; the standalone F2 publisher-reseed fix and the
equal-version divergence precondition counter shipped instead in v2.10.2.
