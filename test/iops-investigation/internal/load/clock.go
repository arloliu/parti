// Package load implements the open-loop producer and the host-wide
// monotonic clock used to stamp messages for end-to-end latency
// measurement (design §5). All timestamps are CLOCK_MONOTONIC nanoseconds,
// which on Linux are consistent across processes on one host and never
// step backward — see 00-design.md §5.
package load

import "golang.org/x/sys/unix"

// MonoNanos returns the current CLOCK_MONOTONIC reading in nanoseconds.
// Producer and consumers (in-process or the §8.2 out-of-process cell)
// read the same host-wide source, so recv-minus-intended is a valid
// latency even across process boundaries on the same machine.
func MonoNanos() int64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		// CLOCK_MONOTONIC is always available on Linux; a failure here is
		// catastrophic for the measurement. Fail loud rather than silently
		// poisoning every latency sample with a bogus zero.
		panic("load: CLOCK_MONOTONIC unavailable: " + err.Error())
	}

	return ts.Nano()
}
