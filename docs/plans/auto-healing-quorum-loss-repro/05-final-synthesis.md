# Final Synthesis — What Caused the Quorum-Loss Non-Recovery

- **Date:** 2026-05-30
- **Status:** REPRODUCTION + DISCRIMINATION COMPLETE. (Fix design remains a separate, later effort — still out of scope.)
- **Inputs (all verified first-hand under `-race`):** Tier 0 unit test (committed), the
  NATS-only trigger probe (Agent A), the read-fault end-to-end test (Agent B), the
  S3 write-fault verdict (`04`), the incident timeline (`tmp/parti_auto_healing_issue.md`).

## Verdict
The v2.5.0 incident's non-recovery is **Defect 2 — the irreversible resolver-cache
tombstone**. The reproduction now **strongly supports** the one premise that was
previously only assumed — **real bucket quorum loss provably produces the `Keys`-ok /
`Get`-fail window the tombstone requires** (demonstrated end-to-end on a current NATS
server, including the incident's exact versions) — with **one named gap that remains
open**: the incident's *sustained* read failure is not reproduced. That gap is now
characterized (see "v2.10.29 confirmation" below): it is a **fault-nature** effect
(PVC volume-offline / read-only storage), **not** a NATS-version effect, and a clean
process-kill probe structurally cannot reproduce wedged storage. So the mechanism is
proven real and reachable; *which* fault dynamic fired is strongly supported, not
confirmed. Defect
1 (unclassified timeout) is the enabler that keeps the manager out of its recovery
machinery; the Defect-3 startup empty-diff path is a real, v2.5.0-applicable, but
*not-evidenced-here* co-contributor.

## The evidence chain (premise → mechanism → consequence → incident)
1. **Trigger provably occurs — PREMISE STRONGLY SUPPORTED, one gap (Agent A, NATS-only 5-node probe).** Killing the
   2 nodes that include the bucket's RAFT **leader** (1 replica survives) while
   meta-quorum holds (3/5) and the client stays connected produces a genuine
   `NoLeader` window in which **`kv.Keys()` succeeds while per-key `kv.Get()` fails**
   — reproduced 6/6 trials, ~107 asymmetric samples, confirmed against an in-process
   `server.Jsz` ground-truth instrument. The control (kill 2 *followers*, leader
   survives) shows **no** asymmetry (both reads serve stale). The watcher's
   `Updates()` channel does **not** close (confirms the prior empirical finding) —
   so the tombstone is a *reconciler*-path artifact, not a watcher one.
2. **Mechanism — PINNED (Tier 0, committed unit test).** A `Keys`-ok/`Get`-fail
   `reconcileOnce` synthesizes a delete tombstone at revision `R+1` that permanently
   beats the live claim at `R` (`existing.revision >= p.revision` guard); `GetOwner`
   returns `ok=false`; only a fresh `warm()` (process restart) clears it.
3. **Consequence — REPRODUCED END-TO-END (Agent B, public `NewManager`+`NewDynamic`).**
   Injecting the asymmetric read fault through the public stack: `(a)` the claim stays
   in the handoff KV at unchanged `R`, `(b)` the consumer stays suppressed without a
   restart, the manager never enters Degraded, and a fresh start resumes. The live
   watcher did **not** clear it — Tier 0's irreversibility holds at integration scale.
4. **Incident match (timeline).** Running fleet; handoff bucket loses quorum 09:39;
   NATS auto-recovers ~11:35; the consumer is **still** suppressed at 12:04
   (`pull suppressed reason=resolve_error`, `pull gating resolve failed: partition not
   found, partition USER21`); pod restart 12:24 = the fix. Claims were committed before
   the outage and writes never failed, so a post-recovery `ok=false` = a poisoned cache.
   This is precisely the `(a)`/`(b)` signature reproduced in step 3. (Inference, not a
   logged fact: USER21's pre-outage `Stable` claim is not in the excerpt, but a running
   fleet that was Stable and pulling necessarily had it resolved `ok=true`; writes never
   failed, so a post-recovery `ok=false` is a poisoned cache, not claim-absence.)

   **Trigger *timing* — an equally/more plausible variant (still Defect 2).** Because the
   incident's reads were failing for the *whole* outage (the logged `list keys failed` is
   the benign both-fail branch on the old server), the tombstone most likely formed at the
   **recovery edge** (~11:35, when `Keys` recovers but `Get` briefly lags) rather than in
   a during-outage leader-loss window. Either timing is the same defect; this just makes
   the attribution robust to the obvious "the logs show `Keys` failing, not the asymmetry"
   objection.

## Defect 1 — the enabler (confirmed)
`natsutil.IsConnectivityError`/`IsDegradingJetStreamError` classify **neither**
`context.DeadlineExceeded` (the incident's logged error) **nor** `nats.ErrNoResponders`
(the probe's error). So `recordKVError` never trips and the manager never enters
Degraded on KV read timeouts — confirmed in code and observed `Degraded=false` in every
S2/S3 scenario. Even if the manager *had* degraded, `attemptRecoveryFromDegraded` only
re-pulls assignment / never re-writes claims, so it would not clear the resolver cache.

## Defect 3 startup empty-diff — latent co-contributor (not evidenced here)
A worker *(re)starting during* the outage hits a distinct restart-only path: the
snapshot is pre-advanced (`waitForAssignment`) before claims are written, so the
apply-retry computes an empty prepare diff and self-exits writing zero claims (`04`).
Traced to hold on **v2.5.0** too. But the incident shows no worker restart *during* the
outage (the only restart, 12:24, was the fix), so this is a latent risk here, not an
evidenced contributor. (If any worker had restarted mid-outage, Defect 2 and this path
would be co-contributors, not alternatives.)

## Honest caveats (discrimination rigor — do not overclaim)
- **Error-surface mismatch.** The probe's `Get` failed with `nats.ErrNoResponders`
  (nats-server **v2.14.1**); the incident logged `context deadline exceeded` (nats-server
  **v2.10.29**). Both trigger the tombstone identically (`reconcileOnce` does
  `if err != nil { continue }` on *any* `Get` error), so the mechanism is unaffected —
  but the **exact error surface on the incident's older server is not reproduced**.
  Likely a server-version and/or timeout-budget difference. The asymmetry's *existence*
  under real quorum loss is confirmed; its v2.10.29 error shape is inferred.
- **Conditionality.** The asymmetry requires the bucket's RAFT **leader** among the
  killed nodes and lasts only ~0.4–1.3 s (then degrades to both-ok-stale). Whether the
  incident's surviving replica was leader or follower is unknown from the logs. The
  short window also explains why the logs only ever show the *benign* `list keys failed`
  branch: a reconcile pass has to land inside the window to trip the tombstone.
- **No single test reproduces real-kill → real-parti → restart-only in one shot.** The
  premise (A) and the consequence (B) are proven separately; the full end-to-end Tier 2
  (below) was not run.

## What is settled vs. optional
**Settled (goal met — reproduce + discriminate):** Defect 2 is the primary cause, with
a real trigger (A), a pinned mechanism (Tier 0), an end-to-end consequence (B), and an
incident-log match. Defect 1 is the enabler. Defect 3-startup is a latent co-contributor.

**The v2.10.29 confirmation step — DONE; it REFINED the gap rather than closing it.**
Re-ran the probe pinned to the incident's exact versions (nats-server **v2.10.29**,
nats.go **v1.50.0**; `tmp/nats-quorum-probe-v210/`). Result: **v2.10.29 behaves
identically to v2.14.1** — a ~1.1 s `Get`-fail asymmetry window (surface
`nats.ErrNoResponders`), then **stale** (both-ok, rev=1) reads for the rest of the
outage. So **the incident's sustained `context deadline exceeded` is NOT a NATS-version
effect.** The probe also corrected a framing point: the ground-truth `NoLeader` *state*
is sustained the whole outage on both versions; the ~1 s figure is the *asymmetry*
window, not leadership.

The residual gap is now **precisely characterized**: the incident was a **PVC
volume-offline → read-only/wedged-storage** fault (F1/F2), not a clean process kill.
Wedged-but-present storage can keep the RAFT group unable to serve even *stale* reads
(the leader can't fsync/append), sustaining `context deadline exceeded` in a way a clean
`Shutdown()` cannot — so the clean-kill probe structurally cannot reproduce the sustained
dynamics. Two things remain genuinely untested: (i) a **wedged-storage** fault simulation
(read-only FS on surviving replicas), and (ii) the **recovery-edge** `Keys`-ok/`Get`-fail
timing (the probe's sample loop stops before restore). Either could be the actual
tombstone-formation trigger.

**Bottom line:** the attribution to Defect 2 is unchanged and remains *strongly
supported* — the mechanism is real, reachable, and (per Tier 0 / S2) trips on ANY `Get`
error regardless of surface or timing; the asymmetry provably occurs under real
leader-loss on the incident's exact versions. What stays open is *which* fault dynamic
(during-outage window, sustained wedged-storage, or recovery-edge) fired in production —
not whether the mechanism is the cause. Notably, the incident's logged
`list keys failed: context deadline exceeded` matches the **benign `Keys`-timeout branch**
(reproduced on v2.10.29), consistent with the Conditionality caveat above.

## Deliverables produced
- Committed: Tier 0 unit guard (`internal/durable/claim_resolver_quorumloss_test.go`),
  plan docs `00`–`05`.
- Gitignored harnesses (not version-controlled; promote per `02` §5 once a fix lands):
  `tmp/parti-repro/` (S2 read-fault + S3 write-fault, public-API black-box) and
  `tmp/nats-quorum-probe/` (NATS-only trigger probe).
- For the fix authors: the fix must (1) re-classify connected-but-KV-timing-out as a
  degrading condition, (2) make the resolver reconcile NOT manufacture a destructive
  tombstone from a transient *read* failure (or provide an in-process cache re-resolve /
  gate-reopen path), and (3) close the startup empty-diff window. Defect 2 is the
  load-bearing one for *this* incident.
