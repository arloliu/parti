# Counter-Proposal to F1: Refs-Always with Content-Addressable Payloads

## Context

The architect's feedback (`docs/plans/cache-freeze-improvement/reviews/plan-reviews/architect-feedback.md`)
accepts F1 and proposes a two-tier design: inline payloads in
`assignment.commit` as the fast path, refs fallback when compressed
commit size exceeds a configured threshold.

This counter-proposal accepts the same two-mode underlying machinery,
but flips the default: **refs as the only path**, with
content-addressable payload keys to recover the bandwidth advantage
that the architect's design optimizes for. The driving consideration
is **commit update frequency**, an axis the architect's feedback does
not yet address.

The user's operational profile is 20–40 workers, 1000–2000 partitions,
~50 partitions/worker. The size axis at this profile (≈30 KB gzipped
inline commit) is comfortable for either mode. The frequency axis is
where the modes diverge sharply.

## The frequency problem with inline

Every rebalance rewrites the full inline commit, regardless of how
much actually changed. A typical incremental partition update (one
new partition lands, one worker's slice grows by one) costs:

| Mode | Writes | Wire bytes per worker | Cluster wire/commit |
|---|---:|---:|---:|
| **Inline** | 1 commit write (~30 KB) | 30 KB broadcast to all 30 workers | ~930 KB |
| **Refs (with reuse)** | 1 new payload (~1 KB) + 1 commit (~2 KB) | 2 KB commit broadcast + only changed worker fetches its 1 KB payload | ~62 KB |

The ratio is structural — inline rebroadcasts every worker's slice to
every worker on every commit, even when 29 of 30 workers have
identical slices to V−1. The architect's size-only fallback does not
trigger at this scale (30 KB stays under any reasonable threshold), so
operators with frequent-update workloads stay on inline indefinitely
and pay the ~15× bandwidth premium silently.

At realistic frequencies:

| Rebalance frequency | Inline cluster bw | Refs cluster bw |
|---|---:|---:|
| 1 commit / min (stable cluster) | ~15 KB/s | ~1 KB/s |
| 1 commit / 10s (active source) | ~93 KB/s | ~6 KB/s |
| 1 commit / sec (chatty source) | ~930 KB/s | ~62 KB/s |

The bandwidth is tolerable in absolute terms either way, but the
inefficiency compounds with NATS KV stream pressure (every commit is
a stream message that must be retained per history policy), and the
inline mode's cost is proportional to fleet size × partition count
rather than to *actual change*.

## Proposed design

Keep the architect's commit shape, but make `Payloads` always present
(not just in fallback) and key payloads by **digest**, not by
`(leaderRev, version, workerID)`:

```go
type AssignmentCommit struct {
    Version             int64
    LeaderRevision      uint64
    SourceRevision      uint64
    SourceRevisionKnown bool
    PublishedAt         time.Time
    Workers             []string

    Payloads map[string]AssignmentPayloadRef   // always refs

    BatchDigest   uint64
    PrevCommitRev uint64
}

type AssignmentPayloadRef struct {
    Key      string  // "assignment.payload.<hex(digest)>"  ← content-addressable
    Revision uint64  // KV revision returned by kv.Create
    Digest   uint64  // matches the suffix on Key
}
```

### Consequences of content-addressable keys

1. **Cross-commit payload reuse.** If worker W's slice between V and
   V+1 is byte-identical, the digest is identical, so the new commit
   references the same `assignment.payload.<digest>` key. No write,
   no broadcast, no fetch. Only the workers whose slices actually
   changed cost any wire activity. This is what reclaims the
   bandwidth advantage the architect attributed to inline.
2. **Natural deduplication.** Two workers happening to have identical
   slices share one payload key. Minor effect in practice but free.
3. **Immutability by construction.** `kv.Create` on a digest-keyed
   payload either succeeds (first writer with this content) or fails
   with `ErrKeyExists` (someone — possibly a losing leader — already
   created it with the same content, so we just reference the
   existing key). The losing-leader-corrupts-payload scenario from
   F1 is impossible: any payload that exists at a given digest key
   has, by definition, the right content.

### Publish flow (single path, no mode branching)

1. Compute assignments against a source snapshot.
2. Verify publish-time set equality (existing §3.6).
3. For each worker W, compute `digest_W = xxh3(sorted partition IDs)`.
4. For each W: `kv.Create(assignment.payload.<hex(digest_W)>, gzip(payload_W))`.
   Treat `ErrKeyExists` as success — the payload at that digest already
   exists and is by definition correct.
5. Re-verify leadership.
6. CAS-write `assignment.commit` containing `Payloads[W] = {Key, Revision, Digest}`
   for every W in this batch. CAS on `PrevCommitRev`.
7. Best-effort write/update legacy `assignment.<W>` aliases for old workers.
8. Best-effort GC: see below.

The commit is still the single atomic decision point. Step 4 produces
inert orphans if step 6 fails (losing leader's content-addressable
payloads exist but are not referenced by the committed commit; GC
reaps them later).

### Worker flow (single path)

1. Watch `assignment.commit`.
2. On update, locate own entry in `commit.Payloads[W]`.
   - If absent: synthesize empty assignment at `commit.Version`, apply
     through receipt path, publish empty-digest ack.
3. Verify `commit.Payloads[W].Digest == hex-decode(strip-prefix(commit.Payloads[W].Key))`.
   (Self-consistency check: ref's stated digest matches its key.)
4. Fetch `commit.Payloads[W].Key` from KV.
5. Verify `xxh3(fetched payload sorted IDs) == commit.Payloads[W].Digest`.
6. Verify version, leader revision, source revision metadata.
7. Apply through receipt path; publish ack.

If the payload key is missing (impossible if publish flow succeeded,
but defensive): classify as malformed commit, do not apply, surface
metric `parti.worker.commit_payload_missing`. Audit will detect and
re-publish.

### GC

Conservative, non-correctness-critical (per architect):

- Maintain a small index `assignment.payload_index` listing all
  currently-referenced digest keys (or scan recent commits to derive
  the live set).
- Periodically (every N commits or every T minutes): delete payload
  keys not referenced by the current commit AND not in the last K
  commits' reference sets.
- Bound by retention policy: keep last K=10 commits' payloads even if
  unreferenced, for forensics.
- Failures are non-fatal; metrics-only.

At your scale, steady state is ~30 active payloads + up to
~30 × 10 = 300 retained-but-stale payloads. NATS KV handles this
trivially.

## Trade-offs vs. the architect's design

| Concern | Architect's hybrid | Refs-always |
|---|---|---|
| Common-case publish writes | 1 (inline) | 1 + N_changed (refs) |
| Common-case wire | Heavier (~30 KB/commit broadcast) | Lighter (~2 KB/commit broadcast + per-worker fetch) |
| Cost scales with | Fleet size × partitions, every commit | Actual changes, regardless of frequency |
| GC loop | Only when fallback triggered | Always required (one loop, ~50 lines) |
| Code paths | Two (inline / refs) | One |
| Operator knob | `MaxInlineCommitBytes` | None required |
| Worst-case size scaling | Falls back gracefully via threshold | Never an issue (payload keys are per-worker) |
| Worst-case frequency scaling | Inline rewrites everything every commit | Linear in actual change |
| F1 split-brain immunity | Per architect's CAS-on-commit design | Same, plus content-addressable keys make payload corruption logically impossible |

## Where I'd defer

If the architect prefers two paths in order to keep the GC loop out
of the common case, the hybrid still works. The frequency concern can
be addressed with a config knob added to their design:

```go
type AssignmentPublishConfig struct {
    Mode                 AssignmentCommitMode  // Auto | Inline | Refs (default: Auto)
    MaxInlineCommitBytes int                   // governs Auto threshold
}
```

Operators with frequent-update workloads set `Mode: Refs`; default
Auto switches on size as the architect specified. This is a fully
backward-compatible refinement of their design — the frequency axis
becomes operator-visible rather than silently expensive.

But the simpler design — refs-always with content-addressable
payloads — handles the frequency concern with no operator knob, no
two-path complexity, no fallback-boundary edge cases. The GC loop is
small, well-isolated, and the architect already spec'd most of it
under the "GC Policy" section for the fallback case.

## Ask

Two specific decisions for the architect:

1. **Default mode**: refs-always (my proposal) vs. inline-first with
   refs fallback (architect's proposal) vs. hybrid with operator knob
   (compromise).
2. **Payload key scheme** (regardless of mode default): content-
   addressable `assignment.payload.<hex(digest)>` (my proposal) vs.
   version-scoped `assignment.payload.<leaderRev>.<version>.<W>`
   (architect's proposal). Content-addressable enables cross-commit
   reuse and makes payload corruption logically impossible; version-
   scoped is simpler to reason about for GC.

If (1) is refs-always and (2) is content-addressable, the plan
collapses to one code path. If (1) is inline-first and (2) is
version-scoped, the plan ships the architect's design as-stated and
operators with frequent-update workloads pay the bandwidth cost
documented above. The hybrid in between is also fine.

No strong objection to any of the three; just want the frequency
axis on the record before the plan locks in inline as default.
