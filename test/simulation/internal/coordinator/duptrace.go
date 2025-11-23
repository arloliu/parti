package coordinator

import (
	"sort"
	"time"
)

// DupTraceSettings configures duplicate tracing.
type DupTraceSettings struct {
	Enabled         bool
	Window          time.Duration
	ThresholdPerMin float64
	TopN            int
}

// DupTracer maintains a sliding window of duplicate events and can produce snapshots
// when a sustained rate threshold is exceeded.
type DupTracer struct {
	enabled         bool
	window          time.Duration
	thresholdPerMin float64
	topN            int

	events []dupEvent
	// consecutive intervals above threshold to trigger snapshot
	consecutive int
}

type dupEvent struct {
	When        time.Time
	PartitionID int
	Sequence    int64
	WorkerID    string
}

// DupSnapshot is a diagnostic snapshot.
type DupSnapshot struct {
	Window        time.Duration
	RatePerMin    float64
	Total         int
	TopPartitions []PartitionDup
	Recent        []dupEvent
}

type PartitionDup struct {
	PartitionID int
	Count       int
}

func NewDupTracer(cfg DupTraceSettings) DupTracer {
	return DupTracer{
		enabled:         cfg.Enabled,
		window:          cfg.Window,
		thresholdPerMin: cfg.ThresholdPerMin,
		topN:            cfg.TopN,
		events:          make([]dupEvent, 0, 1024),
	}
}

func (d *DupTracer) RecordDuplicate(partitionID int, workerID string, seq int64, ts time.Time) {
	if !d.enabled {
		return
	}
	d.events = append(d.events, dupEvent{When: ts, PartitionID: partitionID, Sequence: seq, WorkerID: workerID})
}

// MaybeSnapshot prunes old events, computes rate, and returns a snapshot when
// the sustained threshold condition is met for 3 consecutive checks.
func (d *DupTracer) MaybeSnapshot(now time.Time) (DupSnapshot, bool) {
	if !d.enabled || d.window <= 0 {
		return DupSnapshot{}, false
	}
	// prune
	cutoff := now.Add(-d.window)
	i := 0
	for i < len(d.events) && d.events[i].When.Before(cutoff) {
		i++
	}
	if i > 0 {
		d.events = append([]dupEvent(nil), d.events[i:]...)
	}

	total := len(d.events)
	if total == 0 {
		d.consecutive = 0
		return DupSnapshot{}, false
	}
	ratePerMin := float64(total) / (d.window.Minutes())
	if ratePerMin >= d.thresholdPerMin && d.thresholdPerMin > 0 {
		d.consecutive++
	} else {
		d.consecutive = 0
	}
	if d.consecutive < 3 {
		return DupSnapshot{}, false
	}

	// build snapshot
	byPartition := make(map[int]int)
	for _, e := range d.events {
		byPartition[e.PartitionID]++
	}
	parts := make([]PartitionDup, 0, len(byPartition))
	for pid, cnt := range byPartition {
		parts = append(parts, PartitionDup{PartitionID: pid, Count: cnt})
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].Count > parts[j].Count })
	if d.topN > 0 && len(parts) > d.topN {
		parts = parts[:d.topN]
	}

	// last up to 10 events
	recent := d.events
	if len(recent) > 10 {
		recent = recent[len(recent)-10:]
	}

	// reset consecutive so we don't spam every report
	d.consecutive = 0

	return DupSnapshot{
		Window:        d.window,
		RatePerMin:    ratePerMin,
		Total:         total,
		TopPartitions: parts,
		Recent:        append([]dupEvent(nil), recent...),
	}, true
}
