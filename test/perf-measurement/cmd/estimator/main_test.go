package main

import (
	"testing"

	"github.com/arloliu/parti/test/perf-measurement/internal/costmodel"
)

func testModel() costmodel.Model {
	// cost = A + B·N + C·X where N is the PARTITION COUNT n (structural axis,
	// design §11) and X = k·M with M = n/50.
	return costmodel.Model{
		"write_iops": {
			"file":   {A: 5, B: 0.04, C: 0.5},
			"memory": {A: 1, B: 0.01, C: 0.1},
		},
		"cpu_pct": {
			"file": {A: 0.2, B: 0.001, C: 0.05},
		},
	}
}

func TestPredict(t *testing.T) {
	m := testModel()

	tests := []struct {
		name          string
		n, k, rf      int
		storage       string
		wantErr       bool
		wantExtrap    bool
		wantMetricVal map[string]float64
	}{
		{
			name: "on-grid file no extrapolation",
			n:    2000, k: 20, rf: 5, storage: "file",
			wantExtrap: false,
			// N = 2000 (partition count), M = 2000/50 = 40, X = 20*40 = 800.
			// cost is predicted at (N=2000, X=800).
			wantMetricVal: map[string]float64{
				"write_iops": 5 + 0.04*2000 + 0.5*800, // 85 + 400 = 485
				"cpu_pct":    0.2 + 0.001*2000 + 0.05*800,
			},
		},
		{
			name: "lower-bound n=1000 not extrapolation",
			n:    1000, k: 10, rf: 5, storage: "file",
			wantExtrap: false,
		},
		{
			name: "upper-bound n=5000 not extrapolation",
			n:    5000, k: 10, rf: 5, storage: "file",
			wantExtrap: false,
		},
		{
			name: "below range extrapolation",
			n:    500, k: 10, rf: 5, storage: "file",
			wantExtrap: true,
		},
		{
			name: "above range extrapolation",
			n:    8000, k: 10, rf: 5, storage: "file",
			wantExtrap: true,
		},
		{
			name: "rf not 5 errors",
			n:    2000, k: 20, rf: 3, storage: "file",
			wantErr: true,
		},
		{
			name: "unknown storage errors",
			n:    2000, k: 20, rf: 5, storage: "nvme",
			wantErr: true,
		},
		{
			// "memory" is present for write_iops but absent for cpu_pct ⇒ a
			// partially-covered storage must error, not silently drop a metric.
			name: "partial storage coverage errors",
			n:    2000, k: 20, rf: 5, storage: "memory",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := predict(m, tt.n, tt.k, tt.rf, tt.storage)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got result %+v", res)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Extrapolation != tt.wantExtrap {
				t.Fatalf("Extrapolation = %v, want %v", res.Extrapolation, tt.wantExtrap)
			}
			for metric, want := range tt.wantMetricVal {
				got, ok := res.Metrics[metric]
				if !ok {
					t.Fatalf("missing metric %q in result", metric)
				}
				if got != want {
					t.Fatalf("metric %q = %v, want %v", metric, got, want)
				}
			}
		})
	}
}

func TestPredict_AxisAndLoad(t *testing.T) {
	// Single-coefficient models isolate each axis: the structural term uses the
	// PARTITION COUNT n (not M), and the load term uses X = k·(n/50).
	m := costmodel.Model{
		"only_n": {"file": {A: 0, B: 1, C: 0}}, // returns N = n (partition count)
		"only_x": {"file": {A: 0, B: 0, C: 1}}, // returns X = k·M = k·(n/50)
	}
	res, err := predict(m, 2000, 3, 5, "file")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := res.Metrics["only_n"], 2000.0; got != want { // partition count, NOT 40
		t.Fatalf("N (only_n) = %v, want %v", got, want)
	}
	if got, want := res.Metrics["only_x"], 120.0; got != want { // 3 * (2000/50) = 3*40
		t.Fatalf("X (only_x) = %v, want %v", got, want)
	}
}
