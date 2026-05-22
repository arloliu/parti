package provision

import (
	"context"
	"errors"
	"fmt"

	"github.com/arloliu/parti/v2/internal/partcodec"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// partitionEntry is the result of a partitionKV re-read: the decoded table,
// its KV revision, and whether the key exists. Found is false when the key is
// absent — never-written, deleted, and purged are indistinguishable through
// nats.go and all yield Found == false (the zero value).
type partitionEntry struct {
	Parts    []types.Partition
	Revision uint64
	Found    bool
}

// partitionKV is the KV-access seam for the ApplyPartitions path. The
// production implementation (jsPartitionKV) wraps a live jetstream.KeyValue;
// tests inject a fake to deterministically interleave the re-read, no-op,
// stale-before, removal-gate, and CAS-write steps without a NATS server.
type partitionKV interface {
	// Get re-reads and decodes the partition-source key.
	Get(ctx context.Context) (partitionEntry, error)
	// Create writes data to a key that does not exist. A concurrent creator
	// surfaces as jetstream.ErrKeyExists.
	Create(ctx context.Context, data []byte) error
	// Update writes data with compare-and-swap on revision. A stale revision
	// (a concurrent writer landed first) surfaces as jetstream.ErrKeyExists.
	Update(ctx context.Context, data []byte, revision uint64) error
}

// jsPartitionKV adapts a live jetstream.KeyValue to partitionKV.
type jsPartitionKV struct {
	kv  jetstream.KeyValue
	key string
}

func (p jsPartitionKV) Get(ctx context.Context) (partitionEntry, error) {
	entry, err := p.kv.Get(ctx, p.key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return partitionEntry{}, nil
	}
	if err != nil {
		return partitionEntry{}, err
	}

	parts, err := partcodec.Decode(entry.Value())
	if err != nil {
		return partitionEntry{}, err
	}

	return partitionEntry{Parts: parts, Revision: entry.Revision(), Found: true}, nil
}

func (p jsPartitionKV) Create(ctx context.Context, data []byte) error {
	_, err := p.kv.Create(ctx, p.key, data)

	return err
}

func (p jsPartitionKV) Update(ctx context.Context, data []byte, revision uint64) error {
	_, err := p.kv.Update(ctx, p.key, data, revision)

	return err
}

// openPartitionKV opens the partition-source bucket and returns a partitionKV
// over its key. A missing bucket returns ErrPartitionBucketMissing — the
// partitions subcommand never creates the bucket.
func openPartitionKV(ctx context.Context, js jetstream.JetStream, bucket, key string) (partitionKV, error) {
	kv, err := js.KeyValue(ctx, bucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		return nil, fmt.Errorf("%w: bucket %q not found; run `partictl apply` to provision it first",
			ErrPartitionBucketMissing, bucket)
	}
	if err != nil {
		return nil, fmt.Errorf("provision: open partition-source bucket %q: %w", bucket, err)
	}

	return jsPartitionKV{kv: kv, key: key}, nil
}

// ApplyPartitions executes the single write-partitions action in plan: it
// writes the declared partition table to the partition-source key with a
// compare-and-swap.
//
// Algorithm:
//
//   - Extract the write-partitions action. Zero actions → an envelope Report
//     with nothing executed. More than one, or a wrong Resource type → an
//     error wrapping ErrInvalidConfig.
//   - Re-validate the action's After set (the apply-boundary safety gate:
//     PlannedAction.Resource is mutable, so a caller may have forged or
//     mutated it since PlanPartitions) and deep-copy it as the write target.
//   - Open the bucket; a missing bucket → ErrPartitionBucketMissing.
//   - Re-read the key, short-circuit a no-op, reject a genuine stale-before
//     race, enforce the removal gate, then CAS-write.
//
// Failure-return contract (every returned Report carries the envelope fields):
//
//   - Resource-level failure — stale-before, removal gate, CAS conflict,
//     encode/decode error: one ResourceError, Aborted false, empty Skipped,
//     and a non-nil non-sentinel error → CLI exit 1.
//   - Context cancellation — from the bucket open, the re-read, or the write:
//     Aborted true, no ResourceError, the write-partitions action in Skipped
//     with reason context-cancelled, and ctx.Err() → CLI exit 4.
//   - Plan-shape / apply-boundary validation failure: an error wrapping
//     ErrInvalidConfig and an envelope Report with nothing populated → exit 3.
//
// Success semantics — honest scope: a nil-error return means the single CAS
// landed, not that the table stays converged afterward. The runtime
// source.NatsKV.Update is a last-writer-wins replace that retries through CAS
// conflicts, so a runtime writer racing from the same revision can overwrite
// the partictl result with no error surfaced to either side. ApplyPartitions
// adds no cross-writer coordination; the contract is best-effort.
func ApplyPartitions(ctx context.Context, js jetstream.JetStream, plan PlanResult, prune bool) (Report, error) {
	res, err := extractWritePartitionsAction(plan)
	if err != nil {
		return newReport(), err
	}
	if res == nil {
		// No write-partitions action: nothing to do (e.g. an empty-diff plan).
		return newReport(), nil
	}

	// Apply-boundary validation: deep-copy After first, then validate the
	// copy. PlannedAction.Resource is mutable, so validating a private clone
	// — rather than the caller-owned slice — guarantees that what was
	// validated is exactly what gets encoded and written.
	target := clonePartitions(res.After)
	if err := ValidatePartitionSet(target); err != nil {
		return newReport(), err
	}

	pkv, err := openPartitionKV(ctx, js, res.Bucket, res.Key)
	if err != nil {
		if ctx.Err() != nil {
			report := newReport()
			report.Aborted = true
			report.Skipped = append(report.Skipped, skippedWritePartitions(res))

			return report, ctx.Err()
		}

		return newReport(), err
	}

	return applyWritePartitionsAction(ctx, pkv, res, target, prune)
}

// extractWritePartitionsAction scans plan for the single write-partitions
// action. It returns (nil, nil) when there is none, and an error wrapping
// ErrInvalidConfig when there is more than one or the Resource is the wrong
// type — both are forged/corrupt-plan shapes Plan never produces.
func extractWritePartitionsAction(plan PlanResult) (*WritePartitionsResource, error) {
	var found *WritePartitionsResource
	count := 0
	for _, action := range plan.Actions {
		if action.Kind != ActionWritePartitions {
			continue
		}
		count++
		res, ok := action.Resource.(*WritePartitionsResource)
		if !ok {
			return nil, fmt.Errorf(
				"%w: write-partitions action %q carries an unexpected resource type %T",
				ErrInvalidConfig, action.Name, action.Resource)
		}
		if res == nil {
			// A typed-nil *WritePartitionsResource passes the assertion but
			// is a corrupt plan shape — reject it rather than silently
			// collapsing the action into a no-op.
			return nil, fmt.Errorf("%w: write-partitions action %q carries a nil resource",
				ErrInvalidConfig, action.Name)
		}
		found = res
	}

	if count > 1 {
		return nil, fmt.Errorf("%w: plan contains %d write-partitions actions; expected at most one",
			ErrInvalidConfig, count)
	}

	return found, nil
}

// applyWritePartitionsAction runs steps 4-9 of the ApplyPartitions algorithm
// against the partitionKV seam: re-read, no-op short-circuit, stale-before
// check, removal gate, CAS write. target is the validated, deep-copied
// desired table.
func applyWritePartitionsAction(
	ctx context.Context,
	pkv partitionKV,
	res *WritePartitionsResource,
	target []types.Partition,
	prune bool,
) (Report, error) {
	// Step 4: re-read live state.
	entry, err := pkv.Get(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return cancelledPartitionReport(res, ctx.Err())
		}

		return partitionResourceError(res, fmt.Sprintf(
			"re-read partition source %q/%q: %v", res.Bucket, res.Key, err))
	}
	current := entry.Parts

	// Step 5: no-op short-circuit — runs before the removal gate. A table
	// already converged to target is a success with no write, even for a
	// plan whose diff contained removals: --prune gates an actual removal
	// write, not the plan's history. Raced is true when a concurrent writer
	// converged the table since plan time.
	if partitionTablesEqual(current, target) {
		report := newReport()
		report.Executed = append(report.Executed, ExecutedAction{
			Kind:  ActionWritePartitions,
			Name:  res.Bucket,
			Raced: !partitionTablesEqual(current, res.Before),
		})

		return report, nil
	}

	// Step 6: stale-before / genuine race. current differs from target (step
	// 5) and now also from the plan-time Before: a concurrent writer moved
	// the table to a third state, so the plan is stale.
	if !partitionTablesEqual(current, res.Before) {
		return partitionResourceError(res,
			"partition source changed since plan; re-run partictl partitions plan")
	}

	// Step 7: removal gate, computed from the re-read. Reaching here means
	// current == Before, so liveRemovals equals the plan-time removals.
	if removals := countRemovals(current, target); removals > 0 && !prune {
		return partitionResourceError(res, fmt.Sprintf(
			"apply would remove %d partition(s); re-run with --prune to apply removals", removals))
	}

	// Step 8: CAS write.
	data, err := partcodec.Encode(target)
	if err != nil {
		return partitionResourceError(res, fmt.Sprintf("encode partition table: %v", err))
	}
	if entry.Found {
		err = pkv.Update(ctx, data, entry.Revision)
	} else {
		err = pkv.Create(ctx, data)
	}
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return cancelledPartitionReport(res, ctx.Err())
		case errors.Is(err, jetstream.ErrKeyExists):
			// A concurrent writer landed between the re-read and this write.
			// One CAS attempt, no retry loop — the race is surfaced honestly.
			return partitionResourceError(res, fmt.Sprintf(
				"partition source %q key %q changed concurrently during apply; "+
					"re-run partictl partitions plan", res.Bucket, res.Key))
		default:
			return partitionResourceError(res, fmt.Sprintf(
				"write partition source %q/%q: %v", res.Bucket, res.Key, err))
		}
	}

	// Step 9: success.
	report := newReport()
	report.Executed = append(report.Executed, ExecutedAction{
		Kind: ActionWritePartitions,
		Name: res.Bucket,
	})

	return report, nil
}

// partitionResourceError builds the resource-level-failure return: an envelope
// Report carrying one ResourceError, plus a non-nil non-sentinel error so the
// CLI lands on exit 1.
func partitionResourceError(res *WritePartitionsResource, msg string) (Report, error) {
	report := newReport()
	report.Errors = append(report.Errors, ResourceError{
		Kind:  ActionWritePartitions,
		Name:  res.Bucket,
		Error: msg,
	})

	return report, fmt.Errorf("provision: apply write-partitions %q: %s", res.Bucket, msg)
}

// cancelledPartitionReport builds the cancellation return: an envelope Report
// with Aborted set and the write-partitions action in Skipped, plus ctx.Err().
func cancelledPartitionReport(res *WritePartitionsResource, ctxErr error) (Report, error) {
	report := newReport()
	report.Aborted = true
	report.Skipped = append(report.Skipped, skippedWritePartitions(res))

	return report, ctxErr
}

// skippedWritePartitions is the SkippedAction for the write-partitions action
// when apply is cancelled.
func skippedWritePartitions(res *WritePartitionsResource) SkippedAction {
	return SkippedAction{
		Kind:   ActionWritePartitions,
		Name:   res.Bucket,
		Reason: SkipReasonContextCancelled,
	}
}

// partitionTablesEqual reports whether a and b describe the same partition
// table: the same set of CanonicalIDs with the same per-record Weight, order
// independent. Both inputs are duplicate-free (partcodec.Decode and
// ValidatePartitionSet reject duplicate CanonicalIDs), so an equal length plus
// a matching lookup is a bijection.
func partitionTablesEqual(a, b []types.Partition) bool {
	if len(a) != len(b) {
		return false
	}

	weightByID := make(map[string]int64, len(a))
	for _, p := range a {
		weightByID[p.CanonicalID()] = p.Weight
	}
	for _, p := range b {
		w, ok := weightByID[p.CanonicalID()]
		if !ok || w != p.Weight {
			return false
		}
	}

	return true
}

// countRemovals returns how many records in current are absent from target —
// the number of partitions the write would delete.
func countRemovals(current, target []types.Partition) int {
	targetIDs := make(map[string]struct{}, len(target))
	for _, p := range target {
		targetIDs[p.CanonicalID()] = struct{}{}
	}

	removals := 0
	for _, p := range current {
		if _, ok := targetIDs[p.CanonicalID()]; !ok {
			removals++
		}
	}

	return removals
}
