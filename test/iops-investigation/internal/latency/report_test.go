package latency

import "testing"

func TestReport_SampleGating(t *testing.T) {
	r := NewRecorder()
	r.SetWindow(0, 1<<62)
	// Record 2400 samples (the N=1000,k=1 cell): P99.9 must be gated to n/a
	// because n·(1−p) = 2400·0.001 = 2.4 < 10 (design §6).
	for i := 0; i < 2400; i++ {
		_ = r.Histogram().RecordValue(1_000_000) // 1ms
	}
	r.count = 2400
	rep := BuildReport([]*Recorder{r})
	if rep.Count != 2400 {
		t.Fatalf("count = %d", rep.Count)
	}
	if rep.P50Ns == 0 || rep.P99Ns == 0 {
		t.Fatalf("P50/P99 should be present: %+v", rep)
	}
	if rep.P999Present {
		t.Fatal("P99.9 should be gated off at n=2400")
	}

	// 20000 samples ⇒ n·(1−p)=20 ≥ 10 ⇒ P99.9 present.
	r2 := NewRecorder()
	r2.SetWindow(0, 1<<62)
	for i := 0; i < 20000; i++ {
		_ = r2.Histogram().RecordValue(1_000_000)
	}
	r2.count = 20000
	rep2 := BuildReport([]*Recorder{r2})
	if !rep2.P999Present {
		t.Fatal("P99.9 should be present at n=20000")
	}

	// Exact boundary: n=10000 ⇒ n·(1−p) = 10.0 ≥ 10 ⇒ present (pins the
	// FP-subtle gate at its discriminating value).
	r3 := NewRecorder()
	r3.SetWindow(0, 1<<62)
	for i := 0; i < 10000; i++ {
		_ = r3.Histogram().RecordValue(1_000_000)
	}
	r3.count = 10000
	if !BuildReport([]*Recorder{r3}).P999Present {
		t.Fatal("P99.9 should be present at the n=10000 boundary")
	}
}
