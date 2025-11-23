package strategy

import (
	"math/rand"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"testing"
	"time"

	"github.com/arloliu/parti/types"
)

// generatePartitions creates n partitions with deterministic IDs and optional weight variance.
func generatePartitions(n int, weightPattern string) []types.Partition {
	parts := make([]types.Partition, n)
	for i := range n {
		w := int64(100)
		switch weightPattern {
		case "skew-10":
			// 10% are 5x heavier
			if i%10 == 0 {
				w = 500
			}
		case "random":
			w = int64(50 + rand.Intn(151)) // 50..200
		default:
			// equal weights (no change)
		}
		parts[i] = types.Partition{
			Keys:   []string{"topic", "p", fmtInt(i)},
			Weight: w,
		}
	}

	return parts
}

// fmtInt returns an integer as a string without allocating via fmt (simple base10).
func fmtInt(i int) string {
	return strconv.Itoa(i)
}

// generateWorkers returns a slice of worker IDs.
func generateWorkers(n int) []string {
	workers := make([]string, n)
	for i := range n {
		workers[i] = "worker-" + fmtInt(i)
	}

	return workers
}

// BenchmarkWeightedConsistentHash measures Assign performance with ring cache reuse.
// We exercise multiple scenarios to compare equal vs skewed weights.
func BenchmarkWeightedConsistentHash(b *testing.B) { //nolint:cyclop
	// Use a local RNG; global seeding deprecated.
	_ = rand.New(rand.NewSource(time.Now().UnixNano()))

	// Optional profiling controlled by WCH_PROFILE_DIR (if set, write CPU/heap profiles).
	profileDir := os.Getenv("WCH_PROFILE_DIR")
	if profileDir != "" {
		if err := os.MkdirAll(profileDir, 0o755); err != nil {
			b.Fatalf("mkdir profile dir: %v", err)
		}
	}

	testCases := []struct {
		name          string
		workers       int
		partitions    int
		weightPattern string
	}{
		{"W16_P1K_equal", 16, 1000, "equal"},
		{"W16_P1K_skew10", 16, 1000, "skew-10"},
		{"W64_P5K_equal", 64, 5000, "equal"},
		{"W64_P5K_random", 64, 5000, "random"},
		{"W128_P10K_equal", 128, 10000, "equal"},
	}

	for _, tc := range testCases {
		b.Run(tc.name, func(b *testing.B) {
			b.Logf("env WCH_PROFILE_DIR=%q WCH_VNODES=%q", os.Getenv("WCH_PROFILE_DIR"), os.Getenv("WCH_VNODES"))
			workers := generateWorkers(tc.workers)
			parts := generatePartitions(tc.partitions, tc.weightPattern)
			// Optional override for virtual node count via env var
			var opts []WeightedConsistentHashOption
			if vn := os.Getenv("WCH_VNODES"); vn != "" {
				if n, err := strconv.Atoi(vn); err == nil && n > 0 {
					opts = append(opts, WithWeightedVirtualNodes(n))
				}
			}

			b.Run("warm-cache", func(b *testing.B) {
				wch := NewWeightedConsistentHash(opts...)
				var cpuFile *os.File
				if profileDir != "" {
					f, err := os.Create(filepath.Join(profileDir, tc.name+"_warm_cpu.pprof"))
					if err != nil {
						b.Fatalf("create cpu profile: %v", err)
					}
					cpuFile = f
					if err := pprof.StartCPUProfile(cpuFile); err != nil {
						b.Fatalf("start cpu profile: %v", err)
					}
				}
				b.ResetTimer()
				for b.Loop() {
					_, err := wch.Assign(workers, parts)
					if err != nil {
						b.Fatalf("Assign error: %v", err)
					}
				}
				if cpuFile != nil {
					pprof.StopCPUProfile()
					_ = cpuFile.Close()
				}
				if profileDir != "" {
					hf, err := os.Create(filepath.Join(profileDir, tc.name+"_warm_heap.pprof"))
					if err == nil {
						_ = pprof.WriteHeapProfile(hf)
						_ = hf.Close()
					}
				}
			})

			b.Run("cold-no-cache", func(b *testing.B) {
				var cpuFile *os.File
				if profileDir != "" {
					f, err := os.Create(filepath.Join(profileDir, tc.name+"_cold_cpu.pprof"))
					if err != nil {
						b.Fatalf("create cpu profile: %v", err)
					}
					cpuFile = f
					if err := pprof.StartCPUProfile(cpuFile); err != nil {
						b.Fatalf("start cpu profile: %v", err)
					}
				}
				b.ResetTimer()
				for b.Loop() {
					wch := NewWeightedConsistentHash(opts...) // forces ring rebuild every time
					_, err := wch.Assign(workers, parts)
					if err != nil {
						b.Fatalf("Assign error: %v", err)
					}
				}
				if cpuFile != nil {
					pprof.StopCPUProfile()
					_ = cpuFile.Close()
				}
				if profileDir != "" {
					hf, err := os.Create(filepath.Join(profileDir, tc.name+"_cold_heap.pprof"))
					if err == nil {
						_ = pprof.WriteHeapProfile(hf)
						_ = hf.Close()
					}
				}
			})
		})
	}
}
