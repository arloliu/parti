# Auto-Healing Quorum-Loss Reproduction

Reproduce and baseline the v2.5.0 production incident where workers stopped
entirely after the handoff KV bucket lost quorum and **did not auto-recover**
until a pod restart. Source incident: `tmp/parti_auto_healing_issue.md`.

**Scope:** reproduce + black-box baseline + version-compare (v2.4.1 / v2.5.0 /
HEAD). Designing/implementing the fix is a separate later step.

## Files
- **`02-consolidated-design.md` — the working design (READ THIS).** Merges A + B:
  two independent defects on the non-recovery path, the asymmetry the repro must
  honor, the verified HEAD verdict, and the three-tier repro plan.
- `03-execution-model-effort.md` — how to dispatch Tier 0: model (Sonnet, Claude
  Agent), the in-tree harness pointers that save time, and the control pairings
  that prevent a meaningless green. Corrects Tier 0's placement (same-package
  white-box, not the standalone module). Also records the verified Tier 1
  injection seam (wrap the `jetstream.JetStream` interface, not a KV).
- `04-tier1-s3-verdict.md` — the Tier 1 S3 discrimination verdict (verified
  empirically + in source): the rebalance apply self-heals (Defect 3 falsified
  for a running fleet), and a newly-found **startup-timed** restart-only path via
  an empty-prepare-diff retry self-exit. Corrects `02` §2's Defect 3 claim.
- **`05-final-synthesis.md` — the capstone verdict (READ THIS for the answer).**
  Combines the NATS-only trigger probe (real quorum loss *does* produce the
  `Keys`-ok/`Get`-fail window), the end-to-end read-fault reproduction, and the
  incident timeline into one attribution: **Defect 2 (resolver tombstone)** is the
  cause, Defect 1 is the enabler, Defect 3-startup is a latent co-contributor.
  Includes the honest caveats (probe `ErrNoResponders` vs incident deadline-exceeded;
  nats-server v2.14.1 vs v2.10.29; leader-kill conditionality).
- `00-repro-design.md` — perspective A (Claude). Original draft; its primary
  hypothesis (Defect 1, manager-never-Degraded) is correct but missed the deeper
  resolver-tombstone mechanism. Retained as an input.
- `01-codex-investigation.md` — perspective B (independent blind Codex). Captured
  from the job result (Codex's sandbox was read-only). Contributed the
  resolver-tombstone root cause; Claude verified it against source.
