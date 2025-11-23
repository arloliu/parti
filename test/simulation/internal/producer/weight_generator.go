package producer

// WeightGenerator generates partition weights based on distribution pattern.
type WeightGenerator interface {
	// GenerateWeights generates weights for the specified number of partitions.
	GenerateWeights(partitionCount int) []int64
}

// UniformWeightGenerator generates uniform weights (all partitions equal).
type UniformWeightGenerator struct {
	weight int64
}

// NewUniformWeightGenerator creates a new uniform weight generator.
//
// Parameters:
//   - weight: Weight to assign to all partitions
//
// Returns:
//   - *UniformWeightGenerator: Initialized uniform weight generator
func NewUniformWeightGenerator(weight int64) *UniformWeightGenerator {
	if weight <= 0 {
		weight = 1
	}

	return &UniformWeightGenerator{weight: weight}
}

// GenerateWeights creates uniform weight distribution.
//
// Parameters:
//   - partitionCount: Total number of partitions
//
// Returns:
//   - []int64: Slice of weights (all equal)
func (g *UniformWeightGenerator) GenerateWeights(partitionCount int) []int64 {
	weights := make([]int64, partitionCount)
	for i := range partitionCount {
		weights[i] = g.weight
	}

	return weights
}

// ExponentialWeightGenerator generates exponential weight distribution.
// A small percentage of partitions get extreme weights, rest get normal weights.
type ExponentialWeightGenerator struct {
	extremePercent float64 // 0.05 = 5% of partitions
	extremeWeight  int64   // Weight for extreme partitions
	normalWeight   int64   // Weight for normal partitions
}

// NewExponentialWeightGenerator creates a new exponential weight generator.
//
// Parameters:
//   - extremePercent: Percentage of partitions that are extreme (0.0-1.0)
//   - extremeWeight: Weight for extreme partitions
//   - normalWeight: Weight for normal partitions
//
// Returns:
//   - *ExponentialWeightGenerator: Initialized exponential weight generator
func NewExponentialWeightGenerator(extremePercent float64, extremeWeight, normalWeight int64) *ExponentialWeightGenerator {
	if extremePercent <= 0 || extremePercent >= 1 {
		extremePercent = 0.05 // Default 5%
	}
	if extremeWeight <= 0 {
		extremeWeight = 100
	}
	if normalWeight <= 0 {
		normalWeight = 1
	}

	return &ExponentialWeightGenerator{
		extremePercent: extremePercent,
		extremeWeight:  extremeWeight,
		normalWeight:   normalWeight,
	}
}

// GenerateWeights creates exponential weight distribution.
//
// For 1500 partitions with 5% extreme:
//   - 75 partitions (5%) get extreme weight (e.g., 100)
//   - 1425 partitions (95%) get normal weight (1)
//
// Parameters:
//   - partitionCount: Total number of partitions
//
// Returns:
//   - []int64: Slice of weights with exponential distribution
func (g *ExponentialWeightGenerator) GenerateWeights(partitionCount int) []int64 {
	weights := make([]int64, partitionCount)
	extremeCount := int(float64(partitionCount) * g.extremePercent)

	for i := range partitionCount {
		if i < extremeCount {
			weights[i] = g.extremeWeight // Extreme partitions
		} else {
			weights[i] = g.normalWeight // Normal partitions
		}
	}

	return weights
}
