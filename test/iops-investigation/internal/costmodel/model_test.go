package costmodel

import (
	"path/filepath"
	"testing"
)

func TestModel_RoundTrip(t *testing.T) {
	want := Model{
		"write_iops": {
			"file":   Fit{A: 5, B: 0.04, C: 0.5, R2: 1, N: 12},
			"memory": Fit{A: 1.5, B: 0.01, C: 0.25, R2: 0.999, N: 9},
		},
		"cpu_pct": {
			"file": Fit{A: 0.2, B: 0.001, C: 0.05, R2: 0.97, N: 12},
		},
	}

	path := filepath.Join(t.TempDir(), "model.json")
	if err := WriteModel(path, want); err != nil {
		t.Fatalf("WriteModel: %v", err)
	}

	got, err := LoadModel(path)
	if err != nil {
		t.Fatalf("LoadModel: %v", err)
	}

	for metric, byStorage := range want {
		for storage, wf := range byStorage {
			gf, ok := got[metric][storage]
			if !ok {
				t.Fatalf("missing fit for %s/%s after round-trip", metric, storage)
			}
			if gf != wf {
				t.Fatalf("%s/%s: got %+v want %+v", metric, storage, gf, wf)
			}
			// Predictions must be identical (lossless float64 JSON round-trip).
			for _, tc := range []struct{ n, x float64 }{{1000, 20}, {4000, 160}, {5000, 80}} {
				if gp, wp := gf.Predict(tc.n, tc.x), wf.Predict(tc.n, tc.x); gp != wp {
					t.Fatalf("%s/%s Predict(%v,%v): got %v want %v", metric, storage, tc.n, tc.x, gp, wp)
				}
			}
		}
	}
}

func TestLoadModel_MissingFile(t *testing.T) {
	if _, err := LoadModel(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected error loading nonexistent model file")
	}
}
