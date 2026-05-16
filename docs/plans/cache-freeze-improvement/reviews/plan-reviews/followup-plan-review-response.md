# Response: Follow-Up Review

## Summary

All six findings accepted. No pushbacks. The P0 heartbeat
wire-format catch is particularly important — I had missed that
existing heartbeats are raw RFC3339 timestamp bytes, not JSON, and
the migration story was silently wrong on that point.

Plan edits landed in `docs/plans/cache-freeze-improvement/00-original-plan.md`.

## Per-finding actions

### P0 — Heartbeat wire format
- Replaced the "JSON-additive" framing in §4.1 with an explicit
  **dual decoder** specification: `DecodeHeartbeat` tries v1 JSON
  first (payload begins with `{`), falls back to RFC3339Nano /
  RFC3339 string parsing for legacy. Malformed payloads surface
  as parse errors (not silently degraded to `CapAckV1=0`).
- `WorkerMonitor.GetHeartbeats` documented as decode-via-dual-decoder,
  omitting parse-failure entries from the returned map.
- Publish flow step 6 (legacy alias barrier) explicitly classifies
  legacy-timestamp heartbeats into `legacy_in_batch` via the
  decoder's `Capabilities=0` output.
- Added tests #54-58: `TestHeartbeat_DecodeLegacyTimestampString`,
  `TestHeartbeat_DecodeV1JSON`, `TestHeartbeat_DecodeMalformed_ReturnsError`,
  `TestWorkerMonitor_GetHeartbeats_MixedLegacyTimestampAndJSON`,
  `TestPublisher_LegacyAliasBarrier_UsesTimestampHeartbeatAsLegacyWorker`.

### P1 — Stale "ignore alias" text
- §3.7 "Legacy alias for old workers" rewritten as "Legacy alias
  during rolling upgrade" — explicitly says new workers watch
  both keys per §3.6.
- KV schema table row for `assignment.<W>` updated to say "new
  worker: dual-read fallback path per §3.6 — applies when no
  usable commit exists or alias `LeaderRevision` is fresher."

### P1 — Delete/purge revision preservation in §2.5
- §2.5 snippet now uses `s.applyLocal(nil, entry.Revision(), true /*known*/, true /*notify*/)`
  matching the §1.1 design. Explicit comment: do not pass `revision=0`,
  which would conflate known-empty with never-written.

### P1 — Source dedupe uses `CanonicalID()`
- §4.6 dedupe pseudocode replaced `p.ID()` with `p.CanonicalID()`.
  Error message changed from "duplicate partition ID" to
  "duplicate partition canonical ID" for clarity.

### P1 — Alias-barrier leadership fence + documented exposure
- Inserted **pre-alias leadership recheck as step 5** of the
  publish flow. Aborts batch before any alias write if leadership
  is lost.
- Existing pre-commit leadership recheck shifted to step 7 (after
  alias barrier, before commit CAS).
- New subsection "Documented mixed-version exposure:
  alias-published-but-commit-failed" — names the residual
  exposure (stale leader's aliases visible to old workers between
  successful alias writes and a failed commit CAS), explains why
  it's no worse than today, notes it disappears once the
  unverifiable-worker set empties.
- Added tests #59-60: `TestPublisher_AliasBarrier_RechecksLeadershipBeforeAliasWrites`
  and `TestPublisher_AliasBarrier_CASFailureAfterAliases_DocumentedMigrationExposure`.

### P2 — `/` validation unnecessary
- §3.3 `CanonicalID` docs updated to say "any character (including
  `/`, `-`, `:`) may appear in keys without ambiguity" because
  the parser is length-driven, not separator-driven.
- Removed the proposed expansion of `Partition.Validate` to
  forbid `/`.
- Strategy doc (Phase 1) updated to match.

## Plan status

The plan is now a precision pass away from implementation. The
remaining work is execution per the strategy doc
(`docs/plans/cache-freeze-improvement/02-implementation-strategy.md`), which
gates each phase on model/effort choice, review gates, and
worktree isolation for the correctness-critical phases.
