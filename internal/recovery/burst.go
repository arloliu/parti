package recovery

import (
	"sort"
	"sync"
	"time"
)

// BurstDetector tracks iterator failures in a sliding window to detect
// sustained error bursts that indicate a consumer may be gone.
//
// All methods are safe for concurrent use.
type BurstDetector struct {
	mu        sync.Mutex
	failures  []time.Time
	window    time.Duration
	threshold int
}

// DefaultBurstThreshold is the default number of iterator failures within
// DefaultBurstWindow that triggers burst detection.
const DefaultBurstThreshold = 3

// DefaultBurstWindow returns the default sliding window for burst detection
// based on the consumer's fetch timeout.
func DefaultBurstWindow(fetchTimeout time.Duration) time.Duration {
	return fetchTimeout*time.Duration(DefaultBurstThreshold+1) + 3*time.Second
}

// NewBurstDetector creates a burst detector with the given window and threshold.
// A burst is detected when at least threshold failures occur within window.
func NewBurstDetector(window time.Duration, threshold int) BurstDetector {
	return BurstDetector{
		window:    window,
		threshold: threshold,
	}
}

// Record records a failure at the current time and returns true if the burst
// threshold has been reached within the sliding window.
func (bd *BurstDetector) Record() bool {
	bd.mu.Lock()
	defer bd.mu.Unlock()

	now := time.Now()
	bd.failures = append(bd.failures, now)
	bd.failures = TrimTimes(bd.failures, now.Add(-bd.window))

	return len(bd.failures) >= bd.threshold
}

// Reset clears all recorded failures. Called after successful recovery.
func (bd *BurstDetector) Reset() {
	bd.mu.Lock()
	bd.failures = bd.failures[:0]
	bd.mu.Unlock()
}

// TrimTimes removes all entries before cutoff. The slice must be sorted ascending.
func TrimTimes(times []time.Time, cutoff time.Time) []time.Time {
	idx := sort.Search(len(times), func(i int) bool {
		return !times[i].Before(cutoff)
	})

	return append([]time.Time(nil), times[idx:]...)
}
