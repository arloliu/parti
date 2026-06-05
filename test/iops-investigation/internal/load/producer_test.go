package load

import (
	"testing"
	"time"
)

func TestPartitionSubject(t *testing.T) {
	if got := PartitionSubject(0); got != "iops.rig.p-0" {
		t.Fatalf("got %q", got)
	}
	if got := PartitionSubject(4999); got != "iops.rig.p-4999" {
		t.Fatalf("got %q", got)
	}
}

func TestIntervalForRate(t *testing.T) {
	// X = 100 msg/s ⇒ 10ms interval.
	if got := intervalForRate(100); got != 10*time.Millisecond {
		t.Fatalf("got %v", got)
	}
	// X <= 0 ⇒ zero interval sentinel (idle).
	if got := intervalForRate(0); got != 0 {
		t.Fatalf("got %v", got)
	}
}
