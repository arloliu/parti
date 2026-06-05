package costmodel

import (
	"math"
	"testing"
)

func TestFitAffine_RecoversCoefficients(t *testing.T) {
	// Synthetic: cost = 5 + 0.04*N + 0.5*X (no noise) ⇒ exact recovery.
	pts := make([]Point, 0, 12)
	for _, n := range []float64{1000, 2000, 3000, 5000} {
		for _, x := range []float64{20, 40, 80} {
			pts = append(pts, Point{N: n, X: x, Cost: 5 + 0.04*n + 0.5*x})
		}
	}
	f, err := FitAffine(pts)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(f.A-5) > 1e-6 || math.Abs(f.B-0.04) > 1e-9 || math.Abs(f.C-0.5) > 1e-9 {
		t.Fatalf("bad coeffs: %+v", f)
	}
	if f.R2 < 0.999999 {
		t.Fatalf("R2=%v", f.R2)
	}
	if got := f.Predict(4000, 160); math.Abs(got-(5+0.04*4000+0.5*160)) > 1e-6 {
		t.Fatalf("predict=%v", got)
	}
}

func TestFitAffine_RejectsUnderdetermined(t *testing.T) {
	if _, err := FitAffine([]Point{{N: 1, X: 1, Cost: 1}, {N: 2, X: 2, Cost: 2}}); err == nil {
		t.Fatal("expected error: fewer than 3 distinct points")
	}
}
