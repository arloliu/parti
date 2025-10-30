# Thread-Safe Set Analysis for Calculator

**Date**: October 30, 2025
**Status**: Analysis & Recommendation
**Reviewer**: AI Assistant

## Current Situation

### Current Implementation
The Calculator currently uses `map[string]bool` to represent worker sets:
- `currentWorkers map[string]bool` (line 54)
- `lastWorkers map[string]bool` (line 62)

These are protected by a single `sync.RWMutex` (`c.mu`) along with many other fields.

### Lock/Unlock Count
I found **50+ lock/unlock pairs** in calculator.go, many of which are protecting access to these worker sets and other shared state.

## Detailed Analysis

### Pros of Using xsync.Map-based Thread-Safe Set

✅ **1. Improved Code Readability**
- Eliminates explicit lock/unlock pairs for set operations
- Makes the intent clearer: "this is a thread-safe set"
- Reduces cognitive load when reading the code

✅ **2. Reduced Lock Contention**
- `xsync.Map` uses fine-grained locking internally (sharding)
- Better concurrency for set operations compared to single mutex
- Multiple goroutines can read/write different workers concurrently

✅ **3. Fewer Race Condition Risks**
- No risk of forgetting to lock before accessing the set
- No risk of holding locks too long or too short
- Encapsulates synchronization within the data structure

✅ **4. Cleaner API**
```go
// Current (requires manual locking)
c.mu.RLock()
exists := c.lastWorkers[workerID]
c.mu.RUnlock()

// With thread-safe set
exists := c.lastWorkers.Contains(workerID)
```

### Cons of Using xsync.Map-based Thread-Safe Set

❌ **1. NOT Actually Simplifying Much in This Case**
The main issue: **Most lock operations protect MULTIPLE fields, not just the worker sets.**

Looking at the code:
```go
// Line 905-915: checkForChanges
c.mu.RLock()
changed := c.hasWorkersChangedMap(workers)
cooldownActive := time.Since(c.lastRebalance) < c.cooldown  // ← Also needs lastRebalance
currentState := types.CalculatorState(c.calcState.Load())   // ← Uses atomic, doesn't need lock
lastWorkerCount := len(c.lastWorkers)
lastWorkersCopy := make(map[string]bool, len(c.lastWorkers))
for w := range c.lastWorkers {
    lastWorkersCopy[w] = true
}
c.mu.RUnlock()
```

Even if `lastWorkers` were thread-safe, you'd STILL need the lock for `c.lastRebalance`.

❌ **2. Coordination Problem**
Many operations need to update BOTH `lastWorkers` AND `currentWorkers` atomically:
```go
// Line 467-471: Start() - needs atomic update of both
c.mu.Lock()
c.lastWorkers = make(map[string]bool)
for w := range c.currentWorkers {
    c.lastWorkers[w] = true
}
c.mu.Unlock()
```

With separate thread-safe sets, you lose atomicity guarantees between the two sets.

❌ **3. Comparison Operations Become Complex**
Current comparison is simple under a lock:
```go
func (c *Calculator) hasWorkersChangedMap(workers map[string]bool) bool {
    if len(workers) != len(c.lastWorkers) {  // ← Simple with map
        return true
    }
    for w := range workers {
        if !c.lastWorkers[w] {  // ← Simple membership check
            return true
        }
    }
    return false
}
```

With `xsync.Map`, you'd need:
- Custom iteration method
- Manual size tracking (xsync.Map doesn't have efficient Len())
- More complex comparison logic

❌ **4. Performance Overhead**
- `xsync.Map` has overhead for sharding/hashing
- For small sets (<20 workers typically), simple map + RWMutex is FASTER
- The sets are small (usually <10 workers), not worth optimization

❌ **5. Snapshot/Copy Operations Become Harder**
Current code often needs a consistent snapshot of the worker set:
```go
// Line 909-913: Making a copy under lock
lastWorkersCopy := make(map[string]bool, len(c.lastWorkers))
for w := range c.lastWorkers {
    lastWorkersCopy[w] = true
}
```

With `xsync.Map`, you need to:
1. Use `Range()` to iterate
2. Can't guarantee atomicity during iteration (set might change)
3. More verbose code

❌ **6. EmergencyDetector Dependency**
The `EmergencyDetector.CheckEmergency()` method expects `map[string]bool`:
```go
func (d *EmergencyDetector) CheckEmergency(prev, curr map[string]bool) (bool, []string)
```

You'd need to either:
- Change EmergencyDetector interface (breaking change)
- Convert thread-safe set → map on every call (overhead)
- Pass snapshots (already doing this)

## The Real Problem

Looking at the code, the **real issue isn't the worker sets themselves**, it's:

1. **Too many responsibilities in one struct**
   - Worker tracking
   - State management
   - Assignment calculation
   - Rebalancing logic
   - All sharing one mutex

2. **Lock granularity is too coarse**
   - Single mutex protects ~10 different fields
   - Unrelated operations block each other

3. **Mixed access patterns**
   - Some fields need atomic updates (currentVersion, calcState)
   - Some need coordinated updates (lastWorkers + currentWorkers)
   - Some need read-heavy access (assignment lookups)

## Recommendation

### ❌ DO NOT use thread-safe sets for lastWorkers/currentWorkers

**Reasons:**
1. **Doesn't reduce locking** - Most locks protect multiple fields
2. **Loses atomicity** - Can't update both sets together atomically
3. **Adds complexity** - Comparison/snapshot operations harder
4. **No performance gain** - Sets are too small (<20 items)
5. **Breaks existing patterns** - EmergencyDetector expects map[string]bool

### ✅ INSTEAD, consider these alternatives:

#### Option 1: Keep Current Design (RECOMMENDED for now)
- ✅ Simple and well-understood
- ✅ Adequate performance for small worker sets
- ✅ Already working correctly after recent fixes
- ✅ Matches existing patterns in codebase

**When to use:** If worker counts stay under 20-30 consistently

#### Option 2: Refactor into Smaller Components (From improvement plan)
```go
type WorkerTracker struct {
    mu            sync.RWMutex
    currentSet    map[string]bool
    lastSet       map[string]bool
    lastRebalance time.Time
}

func (wt *WorkerTracker) GetSnapshot() (current, last map[string]bool, lastRebalanceTime time.Time) {
    wt.mu.RLock()
    defer wt.mu.RUnlock()
    // Return copies
}

func (wt *WorkerTracker) UpdateWorkers(workers map[string]bool) {
    wt.mu.Lock()
    defer wt.mu.Unlock()
    // Atomic update of both sets
}
```

**Benefits:**
- Smaller, focused synchronization scope
- Easier to reason about
- Can optimize WorkerTracker independently
- Reduces Calculator complexity

**When to use:** As part of the architectural refactoring (Phase 3 in improvement plan)

#### Option 3: Use sync.Map for currentAssignments only
The `currentAssignments map[string][]types.Partition` is a better candidate:
- ✅ Read-heavy access pattern
- ✅ Larger map (one entry per worker)
- ✅ Independent of other fields
- ✅ Could benefit from concurrent reads

**When to use:** If assignment lookup becomes a bottleneck

## Conclusion

**Your instinct is good** - the lock/unlock code is indeed messy and hard to read. However, thread-safe sets **won't solve the root problem** because:

1. Most locks protect multiple related fields, not just the worker sets
2. You need atomic operations across multiple sets
3. The sets are too small to benefit from concurrent access optimization

**Better approach:** Follow the improvement plan's Phase 3 (Section 3.1) to separate concerns into smaller, focused components. This will naturally reduce locking complexity while maintaining correctness.

## Related Documents

- `docs/CALCULATOR_IMPROVEMENT_PLAN.md` - Section 3.1: Separate Concerns
- `docs/design/04-components/calculator.md` - Original architecture

## Code Examples

### What You Might Expect (But Won't Work Well)

```go
// This looks cleaner but has problems
type Calculator struct {
    lastWorkers    *WorkerSet     // Thread-safe
    currentWorkers *WorkerSet     // Thread-safe
    lastRebalance  time.Time      // Still needs mutex!
    // ...
}

func (c *Calculator) checkForChanges(ctx context.Context) error {
    // Still need mutex for lastRebalance
    c.rebalanceMu.RLock()
    cooldownActive := time.Since(c.lastRebalance) < c.cooldown
    c.rebalanceMu.RUnlock()

    // Now need separate calls + can't guarantee consistency
    changed := !c.currentWorkers.Equals(c.lastWorkers)  // Race: sets might change between checks

    // ...
}
```

### What Would Actually Help (Component Separation)

```go
// Focused component with clear responsibility
type WorkerTracker struct {
    mu            sync.RWMutex
    currentSet    map[string]bool
    lastSet       map[string]bool
    lastRebalance time.Time
}

// Single lock, atomic operations, clear scope
func (wt *WorkerTracker) CheckAndUpdateIfChanged(workers map[string]bool, cooldown time.Duration) (changed bool, lastCopy, currentCopy map[string]bool) {
    wt.mu.Lock()
    defer wt.mu.Unlock()

    // Check cooldown
    if time.Since(wt.lastRebalance) < cooldown {
        return false, nil, nil
    }

    // Check if changed
    changed = wt.hasChanged(workers)
    if !changed {
        return false, nil, nil
    }

    // Return consistent snapshots
    lastCopy = wt.copyMap(wt.lastSet)
    currentCopy = wt.copyMap(workers)

    // Update for next check
    wt.lastSet = wt.copyMap(workers)
    wt.lastRebalance = time.Now()

    return true, lastCopy, currentCopy
}
```

This gives you:
- ✅ Fewer lock operations (one instead of several)
- ✅ Atomic consistency (all related fields updated together)
- ✅ Clear ownership (WorkerTracker owns worker comparison logic)
- ✅ Easier testing (mock WorkerTracker)
- ✅ Better encapsulation (implementation details hidden)

**Bottom line:** Don't use thread-safe sets. Either keep current design or refactor into focused components.
