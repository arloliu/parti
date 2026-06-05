// Package costmodel fits a load-aware affine model cost ≈ a + b·N + c·X per
// metric per storage type and predicts cost at arbitrary (N,X) (design §11).
package costmodel

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
)

// Point is one measured cell. N is the structural axis = PARTITION COUNT
// (== consumer count, what drives IOPS / metacontroller / state-file cost,
// design §11) — NOT the worker/member count. X is the aggregate load (k·M).
// Cost is the observed metric. Whoever builds Points (cmd/fitmodel) and whoever
// predicts (cmd/estimator) MUST both use partition count for N, or the
// structural term is off by the M=N/50 factor.
type Point struct{ N, X, Cost float64 }

// Fit holds the fitted coefficients and goodness of fit.
type Fit struct {
	A, B, C float64 // cost = A + B·N + C·X
	R2      float64
	N       int // number of points
}

// Predict evaluates the fitted model.
func (f Fit) Predict(n, x float64) float64 { return f.A + f.B*n + f.C*x }

// FitAffine solves the least-squares normal equations for a + b·N + c·X.
// Requires ≥ 3 points spanning ≥ 3 distinct (N,X) rows or it returns an error
// (under-determined ⇒ meaningless extrapolation, design §11).
func FitAffine(pts []Point) (Fit, error) {
	if len(pts) < 3 {
		return Fit{}, errors.New("costmodel: need at least 3 points")
	}
	// Design matrix columns: [1, N, X]. Build normal matrix M (3×3) and rhs.
	var m [3][3]float64
	var rhs [3]float64
	for _, p := range pts {
		xs := [3]float64{1, p.N, p.X}
		for i := 0; i < 3; i++ {
			for j := 0; j < 3; j++ {
				m[i][j] += xs[i] * xs[j]
			}
			rhs[i] += xs[i] * p.Cost
		}
	}
	coef, ok := solve3(m, rhs)
	if !ok {
		return Fit{}, errors.New("costmodel: singular system (points not spanning N and X)")
	}
	f := Fit{A: coef[0], B: coef[1], C: coef[2], N: len(pts)}
	// R²
	var mean, ssTot, ssRes float64
	for _, p := range pts {
		mean += p.Cost
	}
	mean /= float64(len(pts))
	for _, p := range pts {
		pred := f.Predict(p.N, p.X)
		ssRes += (p.Cost - pred) * (p.Cost - pred)
		ssTot += (p.Cost - mean) * (p.Cost - mean)
	}
	if ssTot == 0 {
		f.R2 = 1
	} else {
		f.R2 = 1 - ssRes/ssTot
	}

	return f, nil
}

// solve3 solves a 3×3 linear system by Gaussian elimination with partial
// pivoting. Returns ok=false if the matrix is singular.
func solve3(m [3][3]float64, b [3]float64) ([3]float64, bool) {
	a := [3][4]float64{
		{m[0][0], m[0][1], m[0][2], b[0]},
		{m[1][0], m[1][1], m[1][2], b[1]},
		{m[2][0], m[2][1], m[2][2], b[2]},
	}
	for col := 0; col < 3; col++ {
		piv := col
		for r := col + 1; r < 3; r++ {
			if math.Abs(a[r][col]) > math.Abs(a[piv][col]) {
				piv = r
			}
		}
		if math.Abs(a[piv][col]) < 1e-12 {
			return [3]float64{}, false
		}
		a[col], a[piv] = a[piv], a[col]
		for r := 0; r < 3; r++ {
			if r == col {
				continue
			}
			f := a[r][col] / a[col][col]
			for c := col; c < 4; c++ {
				a[r][c] -= f * a[col][c]
			}
		}
	}

	return [3]float64{a[0][3] / a[0][0], a[1][3] / a[1][1], a[2][3] / a[2][2]}, true
}

// Model maps a metric name to a per-storage fitted cost model
// (metric → storage → Fit), e.g. Model["write_iops"]["file"].
type Model map[string]map[string]Fit

// WriteModel serialises m as indented JSON to path.
func WriteModel(path string, m Model) error {
	buf, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("costmodel: marshal model: %w", err)
	}

	if err := os.WriteFile(path, buf, 0o600); err != nil {
		return fmt.Errorf("costmodel: write %s: %w", path, err)
	}

	return nil
}

// LoadModel reads and decodes a Model previously written by WriteModel.
func LoadModel(path string) (Model, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("costmodel: read %s: %w", path, err)
	}

	var m Model
	if err := json.Unmarshal(buf, &m); err != nil {
		return nil, fmt.Errorf("costmodel: unmarshal %s: %w", path, err)
	}

	return m, nil
}
