package load

import (
	"testing"
	"time"
)

func TestMonoNanos_Monotonic(t *testing.T) {
	a := MonoNanos()
	time.Sleep(2 * time.Millisecond)
	b := MonoNanos()
	if b <= a {
		t.Fatalf("expected monotonic increase, got a=%d b=%d", a, b)
	}
	if d := time.Duration(b - a); d < time.Millisecond || d > time.Second {
		t.Fatalf("implausible elapsed %v", d)
	}
}
