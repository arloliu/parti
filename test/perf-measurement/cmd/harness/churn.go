// Churn schedule — rig-only measurement plumbing for perf-measurement
// experiment E4 (rebalance burst anatomy). See
// docs/research/load-overhead-research-and-pprof-plan-v1.md §5 E4/E5.
//
// When Options.ChurnWorkerIdx >= 0, Run (main.go) launches
// runChurnSchedule as a background goroutine right after the capture
// window opens: after a fixed idle plateau it performs
// Options.ChurnWaves repetitions of kill worker -> wait convergence ->
// re-add worker -> wait convergence, writing each phase transition's
// wall-clock timestamp to <output-dir>/churn-waves.csv so a post-hoc
// pass can slice rpc_counts.csv into per-wave windows.
//
// This file is throwaway rig tooling for one measurement campaign — it
// does not touch any library package, only cmd/harness's own
// WorkerHandle/StartWorker/WaitStableAll primitives.
package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/arloliu/parti/v2"
)

// workerSet is a mutex-protected view over the harness's worker
// handles. The plain []*WorkerHandle slice built in Run is sufficient
// for every pre-existing code path (all of which run either before the
// churn goroutine starts or after it has been joined via
// Run's churnWG.Wait()), but the churn goroutine replaces one element
// (the killed-then-restarted worker's handle) concurrently with the
// capture loop's per-tick AggregateSnapshots read of the same slice.
// workerSet is the smallest synchronization that makes that safe.
type workerSet struct {
	mu      sync.RWMutex
	workers []*WorkerHandle
}

// newWorkerSet wraps an existing worker slice. The caller must not
// mutate the backing array directly afterward — only through the
// returned workerSet's methods.
func newWorkerSet(workers []*WorkerHandle) *workerSet {
	return &workerSet{workers: workers}
}

// Snapshot returns a shallow copy of the current handle list, safe for
// the caller to range over without holding any lock.
func (ws *workerSet) Snapshot() []*WorkerHandle {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	out := make([]*WorkerHandle, len(ws.workers))
	copy(out, ws.workers)

	return out
}

// Get returns the handle whose .idx == idx, or nil if not present.
func (ws *workerSet) Get(idx int) *WorkerHandle {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	for _, w := range ws.workers {
		if w.idx == idx {
			return w
		}
	}

	return nil
}

// Replace swaps the handle whose .idx == idx for wh. No-op if idx is
// not found (should not happen — the idx space is fixed at startup and
// the churn schedule only ever replaces an index it already read via
// Get).
func (ws *workerSet) Replace(idx int, wh *WorkerHandle) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	for i, w := range ws.workers {
		if w.idx == idx {
			ws.workers[i] = wh
			return
		}
	}
}

// churnWaveEvent is one row of churn-waves.csv.
type churnWaveEvent struct {
	Wave      int
	Phase     string // "kill" | "post_kill_stable" | "readd" | "post_readd_stable" | "wave_failed"
	At        time.Time
	LeaderIdx int // -1 if no in-process worker currently holds leadership
	Note      string
}

// churnWaveWriter appends churnWaveEvents to <outputDir>/churn-waves.csv,
// flushing after every row so a truncated run still leaves usable
// partial data — matches the rest of the rig's crash-safety posture
// (e.g. the streamed rpc_counts.csv).
type churnWaveWriter struct {
	f *os.File
	w *csv.Writer
}

func newChurnWaveWriter(outputDir string) (*churnWaveWriter, error) {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir output dir: %w", err)
	}
	f, err := os.Create(filepath.Join(outputDir, "churn-waves.csv"))
	if err != nil {
		return nil, fmt.Errorf("create churn-waves.csv: %w", err)
	}
	w := csv.NewWriter(f)
	if err := w.Write([]string{"wave", "phase", "t_unix_ns", "t_rfc3339", "leader_idx", "note"}); err != nil {
		_ = f.Close()

		return nil, fmt.Errorf("write churn-waves.csv header: %w", err)
	}
	w.Flush()

	return &churnWaveWriter{f: f, w: w}, nil
}

// Write appends ev and flushes immediately.
func (cw *churnWaveWriter) Write(ev churnWaveEvent) {
	_ = cw.w.Write([]string{
		fmt.Sprintf("%d", ev.Wave),
		ev.Phase,
		fmt.Sprintf("%d", ev.At.UnixNano()),
		ev.At.UTC().Format(time.RFC3339Nano),
		fmt.Sprintf("%d", ev.LeaderIdx),
		ev.Note,
	})
	cw.w.Flush()
}

// Close flushes and closes the underlying file. Best-effort: errors are
// dropped, matching the rig's other best-effort teardown paths.
func (cw *churnWaveWriter) Close() {
	cw.w.Flush()
	_ = cw.f.Close()
}

// runChurnSchedule executes o.ChurnWaves repetitions of
// kill -> converge -> re-add -> converge against the worker at index
// o.ChurnWorkerIdx, after an initial o.ChurnPlateau idle wait. It is
// designed to run as a single background goroutine started right after
// the capture window opens (captureStart) and joined (via a
// sync.WaitGroup) before Run proceeds past the capture loop.
//
// ctx bounds the ENTIRE schedule with a hard ceiling the caller derives
// independently of the harness's outer signal context (see Run's
// churnCtx construction) — a stuck convergence wait must not hang the
// harness process indefinitely. Each per-wave convergence wait
// additionally has its own o.ChurnConvergeTimeout budget; a wave whose
// convergence check exceeds that budget is logged to the CSV as
// "wave_failed" (via a non-empty Note on the relevant phase row) and the
// schedule proceeds to the next wave — it does not retry.
func runChurnSchedule(
	ctx context.Context,
	o Options,
	cfg parti.Config,
	ws *workerSet,
	lt *LeaderTracker,
	errLog io.Writer,
) {
	cww, err := newChurnWaveWriter(o.OutputDir)
	if err != nil {
		fmt.Fprintf(errLog, "churn: failed to open churn-waves.csv: %v (schedule aborted)\n", err)

		return
	}
	defer cww.Close()

	idx := o.ChurnWorkerIdx
	fmt.Fprintf(errLog, "churn: schedule starting — worker=%d waves=%d plateau=%s converge_timeout=%s\n",
		idx, o.ChurnWaves, o.ChurnPlateau, o.ChurnConvergeTimeout)

	if err := sleepCtx(ctx, o.ChurnPlateau); err != nil {
		fmt.Fprintf(errLog, "churn: aborted during idle plateau: %v\n", err)

		return
	}

	leaderAt := func() int {
		if i, ok := lt.Current(); ok {
			return i
		}

		return -1
	}

	for wave := 1; wave <= o.ChurnWaves; wave++ {
		target := ws.Get(idx)
		if target == nil {
			fmt.Fprintf(errLog, "churn: wave %d: worker %d not found — aborting schedule\n", wave, idx)
			cww.Write(churnWaveEvent{Wave: wave, Phase: "wave_failed", At: time.Now(), LeaderIdx: leaderAt(), Note: "worker not found"})

			return
		}

		// --- kill ---
		killAt := time.Now()
		stopCtx, stopCancel := context.WithTimeout(ctx, 30*time.Second)
		stopErr := target.Stop(stopCtx)
		stopCancel()
		cww.Write(churnWaveEvent{Wave: wave, Phase: "kill", At: killAt, LeaderIdx: leaderAt(), Note: errString(stopErr)})
		if stopErr != nil {
			fmt.Fprintf(errLog, "churn: wave %d: kill worker %d: %v (continuing — worker is down regardless)\n", wave, idx, stopErr)
		}

		// --- wait convergence: remaining (non-killed) workers Stable ---
		remaining := remainingWorkers(ws.Snapshot(), idx)
		cErr := WaitStableAll(remaining, o.ChurnConvergeTimeout)
		postKillAt := time.Now()
		cww.Write(churnWaveEvent{Wave: wave, Phase: "post_kill_stable", At: postKillAt, LeaderIdx: leaderAt(), Note: errString(cErr)})
		if cErr != nil {
			fmt.Fprintf(errLog, "churn: wave %d: post-kill convergence timed out: %v\n", wave, cErr)
		}

		// --- re-add ---
		readdAt := time.Now()
		newWH, startErr := StartWorker(ctx, idx, o, cfg, lt)
		if startErr != nil {
			fmt.Fprintf(errLog, "churn: wave %d: re-add worker %d FAILED: %v — schedule aborted\n", wave, idx, startErr)
			cww.Write(churnWaveEvent{Wave: wave, Phase: "wave_failed", At: readdAt, LeaderIdx: leaderAt(), Note: "re-add failed: " + startErr.Error()})

			return
		}
		ws.Replace(idx, newWH)
		cww.Write(churnWaveEvent{Wave: wave, Phase: "readd", At: readdAt, LeaderIdx: leaderAt(), Note: ""})

		// --- wait convergence: ALL workers (incl. re-added) Stable ---
		all := ws.Snapshot()
		cErr2 := WaitStableAll(all, o.ChurnConvergeTimeout)
		postReaddAt := time.Now()
		cww.Write(churnWaveEvent{Wave: wave, Phase: "post_readd_stable", At: postReaddAt, LeaderIdx: leaderAt(), Note: errString(cErr2)})
		if cErr2 != nil {
			fmt.Fprintf(errLog, "churn: wave %d: post-readd convergence timed out: %v\n", wave, cErr2)
		}

		fmt.Fprintf(errLog, "churn: wave %d complete — kill=%s post_kill=%s readd=%s post_readd=%s\n",
			wave, killAt.Format(time.RFC3339), postKillAt.Format(time.RFC3339), readdAt.Format(time.RFC3339), postReaddAt.Format(time.RFC3339))
	}

	fmt.Fprintf(errLog, "churn: schedule complete (%d waves)\n", o.ChurnWaves)
}

// remainingWorkers returns all handles in all except the one whose
// .idx == excludeIdx.
func remainingWorkers(all []*WorkerHandle, excludeIdx int) []*WorkerHandle {
	out := make([]*WorkerHandle, 0, len(all))
	for _, w := range all {
		if w.idx != excludeIdx {
			out = append(out, w)
		}
	}

	return out
}

// errString returns "" for a nil error and err.Error() otherwise, so
// churn-waves.csv's note column is empty on the happy path.
func errString(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}
