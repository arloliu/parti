# Feedback on Refs-Always Counter-Proposal

## Summary

I agree with the counter-proposal's main direction: **refs-always is cleaner than inline-first plus fallback** for a P0 assignment protocol. The frequency argument is real, and the single-code-path property is valuable. At the stated production profile (20-40 workers, 1000-2000 partitions, about 50 partitions per worker), inline would probably be fine on size, but refs-always avoids making commit size and watcher traffic scale with total partition count on every small change.

My recommendation: adopt **refs-always**, but tighten the content-addressable design before putting it in the main plan.

The two key corrections are:

1. Content-address keys must hash the **canonical payload bytes**, not only sorted partition IDs.
2. `ErrKeyExists` cannot be treated as proof unless the existing payload is read and verified, or the key uses a strong content hash.

## Decision Recommendation

Choose:

- Default mode: **refs-always**.
- Payload key scheme: **content-addressable**, but based on a strong hash of canonical payload bytes.
- Inline payloads: defer. They can be added later as an optimization if measurements show refs are too chatty, but I would not start with two modes.

This gives the protocol one worker state machine and one publish path:

```text
source snapshot -> assignments -> immutable payload refs -> CAS commit -> workers fetch own payload -> apply receipt
```

That shape is easier to reason about than inline/ref branching and avoids fallback-boundary edge cases.

## Important Corrections

### 1. Hash The Payload, Not Just Partition IDs

The counter-proposal uses:

```go
digest_W = xxh3(sorted partition IDs)
assignment.payload.<hex(digest_W)>
```

That is fine for a **coverage digest**, but it is not sufficient for a **content-addressed payload key**.

A payload may change while partition IDs stay the same:

- partition `Weight` changes;
- partition key ordering/canonical representation changes;
- payload schema version changes;
- future assignment metadata is added;
- the payload includes `Version`, `LeaderRevision`, `SourceRevision`, or lifecycle fields.

If the key hashes only partition IDs, `ErrKeyExists` might point to a payload whose bytes are not the payload this commit intended to reference.

Recommended split:

```go
type AssignmentPayload struct {
    SchemaVersion uint8
    Partitions    []Partition
}

type AssignmentPayloadRef struct {
    Key         string // assignment.payload.<sha256(canonical payload bytes)>
    PayloadHash string // same hash as key suffix
    SetDigest   uint64 // xxh3 over sorted partition IDs, for fast audit/metrics
}
```

The commit carries version/leader/source metadata. The payload should be stable across commits when the worker's actual partition slice is unchanged. That preserves cross-commit reuse without accidentally reusing stale version metadata.

### 2. Use A Strong Hash Or Verify On `ErrKeyExists`

The proposal says `ErrKeyExists` means the payload already exists and is "by definition correct." That is only true if the key is a collision-resistant hash of the exact canonical payload bytes.

For P0 robustness, use `sha256` or another strong content hash for the key. Keep `xxh3` as a fast set digest if desired, but do not use 64-bit `xxh3` alone as the content-address identity.

Publish behavior should be:

```go
payloadBytes := canonicalMarshal(payload)
hash := sha256(payloadBytes)
key := "assignment.payload." + hex(hash)

rev, err := kv.Create(ctx, key, gzip(payloadBytes))
switch {
case err == nil:
    ref := AssignmentPayloadRef{Key: key, PayloadHash: hex(hash), Revision: rev, SetDigest: setDigest(payload.Partitions)}
case errors.Is(err, jetstream.ErrKeyExists):
    existing, err := kv.Get(ctx, key)
    if err != nil { return err }
    if sha256(decompress(existing.Value())) != hash { return ErrPayloadHashCollisionOrCorruption }
    ref := AssignmentPayloadRef{Key: key, PayloadHash: hex(hash), Revision: existing.Revision(), SetDigest: setDigest(payload.Partitions)}
default:
    return err
}
```

With `sha256`, the verification-on-exists path is mostly defensive. It also protects against accidental key reuse, corruption, or a future implementation bug.

### 3. Be Careful With `Revision` In Content-Addressed Refs

For version-scoped keys, storing the KV revision in the ref is straightforward. For content-addressed keys, `Revision` is less fundamental because the key already names the content.

If `Revision` is kept, define its semantics clearly:

- publisher records the revision from `Create` or `Get` after `ErrKeyExists`;
- worker may verify the fetched revision equals the ref revision;
- if revision differs but payload hash matches, decide whether that is acceptable or a hard error.

I lean toward treating `PayloadHash` as authoritative and `Revision` as diagnostic. A current commit should never have its payload GC'd, but if the same content is deleted and recreated, the hash still proves the bytes are correct while the revision changes.

### 4. Define Commit History For GC

The proposal says GC can scan recent commits or use `assignment.payload_index`. That needs one concrete choice.

Because `assignment.commit` is a singleton, "last K commits" are only available if one of these is true:

- the KV bucket is configured with sufficient history and the code reads historical revisions;
- the publisher also writes immutable commit records such as `assignment.commit.<version>`;
- a separate `assignment.payload_index` stores recent live reference sets.

Recommendation: keep GC simple and independent from correctness:

```text
assignment.commit             // singleton current commit, watched by workers
assignment.commit_log.<V>      // immutable compact log record of payload refs for GC/debug
assignment.payload.<hash>      // immutable payload bytes
```

Workers only need `assignment.commit`. GC can use `assignment.commit` plus the last K `assignment.commit_log.*` records. If commit log writes fail after the singleton commit succeeds, correctness is unchanged; GC just becomes more conservative.

### 5. Frequency Benefit Depends On Assignment Stability

The refs-always bandwidth win is strongest when an incremental source change changes only a small number of worker slices. That is usually true with consistent hashing, but not necessarily true for every strategy.

For example, a round-robin strategy over a sorted partition list can reshuffle many worker slices when one partition is inserted near the front. In that case refs-always may write many new payloads for a single partition addition. The commit is still small and the design is still safe, but the expected `N_changed = 1` assumption should be stated as strategy-dependent.

Add metrics:

```text
parti.assignment.payloads_reused
parti.assignment.payloads_created
parti.assignment.payload_bytes_written
parti.assignment.commit_bytes_written
```

These metrics will show whether the frequency win appears in real workloads.

## Revised Publish Flow

```text
1. Read source snapshot and source revision.
2. Calculate assignments.
3. Verify publish-time set equality against source partition IDs.
4. For each worker:
   a. Build canonical AssignmentPayload containing only stable payload data.
   b. Hash canonical payload bytes with sha256.
   c. Create assignment.payload.<hash>, or verify existing payload on ErrKeyExists.
   d. Build PayloadRef with PayloadHash and SetDigest.
5. Recheck leadership.
6. CAS-write assignment.commit with worker refs and batch metadata.
7. Best-effort write legacy assignment.<worker> aliases for old workers.
8. Best-effort write compact commit log record for GC/debug.
9. Best-effort GC old unreferenced payloads outside retention.
```

## Revised Worker Flow

```text
1. Watch assignment.commit.
2. On commit V:
   a. If worker is absent from commit.Workers, synthesize empty assignment at V.
   b. Otherwise fetch PayloadRef for worker.
   c. Fetch payload key.
   d. Decompress and verify sha256(payload bytes) == PayloadRef.PayloadHash.
   e. Decode payload and verify SetDigest matches payload partitions.
   f. Verify commit version/leader/source fencing.
   g. Apply through apply-then-store-then-ack path.
```

New workers should treat mutable `assignment.<worker>` as legacy-only. Old workers continue using it, so mixed-version behavior remains no worse than today.

## Verdict

I would support the counter-proposal with the corrections above.

Refs-always is the cleaner P0 design because it removes inline/ref mode branching, makes commit size predictable, and turns payload writes into immutable content objects. The content-addressable idea is good, but it should be keyed by a strong hash of canonical payload bytes and should verify existing payloads on `ErrKeyExists`. With those changes, this is stronger than inline-first for both correctness and operational predictability.
