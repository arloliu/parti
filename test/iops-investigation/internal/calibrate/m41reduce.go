// M4.1 per-node reduction step.
//
// Input per grid point: the calibrate.csv driver row (for `c_stream`,
// `fetch_timeout_s`, storage, replicas, leader_pre) and the
// cgroup_io.raw capture (for per-second per-container block IOPS).
//
// Output (per docs/plans/iops-investigation/00-attribution-plan.md
// §M4.1 line 772-775): two rows per grid point in the schema
//
//	C_stream, FetchTimeout, data_storage, R, node_role,
//	block_read_iops, block_write_iops, ci_low, ci_high
//
// where `node_role` is `leader` for the leader of the calibration data
// stream during the capture and `follower` for every other node
// averaged together. ci_low / ci_high are the 95 % confidence interval
// on the sum of read+write block IOPS (used as a single noise estimate;
// per-direction CIs would double the row width without changing the
// downstream interpolation in M3).
//
// The cgroup parser lives in internal/aggregate; we import it here
// rather than duplicate the io.stat decoder (Rule 6).
package calibrate

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/arloliu/parti/test/iops-investigation/internal/aggregate"
)

// M41RowSchemaHeader is the canonical M4.1 CSV header. M3 depends on
// these column names + order; do not reorder without updating the M3
// lookup code.
const M41RowSchemaHeader = "c_stream,fetch_timeout_s,data_storage,replicas,node_role,block_read_iops,block_write_iops,ci_low,ci_high"

// M41Row is one row in the M4.1 calibration table.
type M41Row struct {
	CStream        int
	FetchTimeoutS  float64
	DataStorage    string
	Replicas       int
	NodeRole       string // "leader" | "follower"
	BlockReadIOPS  float64
	BlockWriteIOPS float64
	CILow          float64
	CIHigh         float64
}

// CSV serialises one row. Numbers use %.3f so M3 doesn't have to choose
// a tolerance for trailing-digit noise.
func (r M41Row) CSV() string {
	return fmt.Sprintf("%d,%.3f,%s,%d,%s,%.3f,%.3f,%.3f,%.3f",
		r.CStream, r.FetchTimeoutS, r.DataStorage, r.Replicas, r.NodeRole,
		r.BlockReadIOPS, r.BlockWriteIOPS, r.CILow, r.CIHigh)
}

// DriverRow is the parsed subset of the per-grid-point calibrate.csv
// row we need to assemble M4.1 output. The driver row also carries
// leader_post / leader_changed / poll samples; those are not needed for
// reduction because the script already discards moved points.
type DriverRow struct {
	CStream       int
	FetchTimeoutS float64
	DataStorage   string
	Replicas      int
	LeaderPre     string
}

// ParseDriverRow reads the single-row calibrate.csv (no header) produced
// by `calibrate idle-pull` and returns the fields the reducer needs.
//
// Column indices match emitIdlePullCSV's layout (cmd/calibrate/main.go).
func ParseDriverRow(r io.Reader) (DriverRow, error) {
	cr := csv.NewReader(r)
	rec, err := cr.Read()
	if err != nil {
		return DriverRow{}, fmt.Errorf("read driver row: %w", err)
	}
	if len(rec) < 14 {
		return DriverRow{}, fmt.Errorf("driver row has %d columns, want >=14", len(rec))
	}
	storage := rec[1]
	replicas, err := strconv.Atoi(rec[2])
	if err != nil {
		return DriverRow{}, fmt.Errorf("replicas: %w", err)
	}
	cStream, err := strconv.Atoi(rec[9])
	if err != nil {
		return DriverRow{}, fmt.Errorf("c_stream: %w", err)
	}
	ftS, err := strconv.ParseFloat(rec[10], 64)
	if err != nil {
		return DriverRow{}, fmt.Errorf("fetch_timeout_s: %w", err)
	}
	leaderPre := rec[11]
	return DriverRow{
		CStream:       cStream,
		FetchTimeoutS: ftS,
		DataStorage:   storage,
		Replicas:      replicas,
		LeaderPre:     leaderPre,
	}, nil
}

// ContainerToNode maps the Docker container name as recorded in
// cgroup_io.raw (e.g. `iops-nats-3`) to the NATS server name as
// reported by `stream.Info().Cluster.Leader` (e.g. `nats-3`). The rig's
// docker-compose.yaml pairs these one-to-one via `container_name:
// iops-nats-N` / `SERVER_NAME=nats-N`.
//
// If the leader is the sentinel "single" (LeaderOf's stub for a
// non-clustered server) no container ever matches and every node is
// classified as follower — which is the correct no-op for the embedded
// single-node smoke path.
func ContainerToNode(container string) string {
	return strings.TrimPrefix(container, "iops-")
}

// ReduceM41 consumes the parsed cgroup samples and the driver row, and
// returns the two M4.1 rows (leader, follower). The leader's IOPS come
// from the container whose mapped node name equals driver.LeaderPre;
// follower IOPS are the per-second mean across every other container.
//
// CI is the 95 % CI on the per-second (read+write) total within the
// window, using t_{0.975, n-1} ≈ 2.0 for n>=10 and t_inf=1.96 as the
// minimum (we only have ~60 samples in a 60s capture and the small-n
// effect is dwarfed by per-second variance — Rule 2). The CI bracketing
// is applied symmetrically to the read+write *total*, so ci_low and
// ci_high describe the total band; callers can read either column as a
// noise estimate. We deliberately do not emit a separate CI per
// direction because M3 interpolates the directional means independently
// and only uses the CI as a sanity gate.
//
// If the leader name is "single" or absent from the capture, ReduceM41
// emits a single "follower" row averaging across all containers and
// returns a non-nil warning the caller can log; the leader row is
// omitted because the data does not support classifying one. This is
// the documented behavior for the embedded-NATS smoke path.
func ReduceM41(samples []aggregate.CgroupSample, driver DriverRow) (leader, follower M41Row, warn error) {
	deltas := aggregate.CgroupDeltas(samples)
	leaderContainer := nodeToContainer(driver.LeaderPre)

	// Index per-second deltas by container.
	byContainer := make(map[string]*bucket)
	for _, d := range deltas {
		b, ok := byContainer[d.Container]
		if !ok {
			b = &bucket{}
			byContainer[d.Container] = b
		}
		b.reads = append(b.reads, d.IOPSRead)
		b.writes = append(b.writes, d.IOPSWrite)
	}

	hasLeader := leaderContainer != "" && byContainer[leaderContainer] != nil

	if !hasLeader {
		// No leader identifiable — emit a single combined "follower"
		// row averaged across every container. Stable container order
		// keeps the result deterministic for tests.
		all := aggregateContainers(byContainer, sortedKeys(byContainer))
		follower = m41RowFromSamples(driver, "follower", all)
		warn = fmt.Errorf("leader container %q not found in capture; emitted follower-only row", leaderContainer)
		return M41Row{}, follower, warn
	}

	// Leader row from the leader container alone.
	leaderSamples := byContainer[leaderContainer]
	leader = m41RowFromSamples(driver, "leader", leaderSamples)

	// Follower row averaged across all non-leader containers, per second:
	// for each second present in the capture, average the read IOPS
	// across every follower container that has a sample at that second,
	// then take the time-series of those averages.
	followerKeys := make([]string, 0, len(byContainer)-1)
	for k := range byContainer {
		if k == leaderContainer {
			continue
		}
		followerKeys = append(followerKeys, k)
	}
	sort.Strings(followerKeys)
	avg := perSecondAverage(byContainer, followerKeys)
	follower = m41RowFromSamples(driver, "follower", avg)

	return leader, follower, nil
}

// m41RowFromSamples turns a per-second bucket into one M41Row using the
// driver-row arguments for the non-statistical columns.
func m41RowFromSamples(driver DriverRow, role string, b *bucket) M41Row {
	if b == nil || len(b.reads) == 0 {
		return M41Row{
			CStream:       driver.CStream,
			FetchTimeoutS: driver.FetchTimeoutS,
			DataStorage:   driver.DataStorage,
			Replicas:      driver.Replicas,
			NodeRole:      role,
		}
	}
	meanR := mean(b.reads)
	meanW := mean(b.writes)

	// CI on the per-second total (read + write).
	totals := make([]float64, len(b.reads))
	for i := range b.reads {
		totals[i] = b.reads[i] + b.writes[i]
	}
	mu := mean(totals)
	sd := stddev(totals, mu)
	se := sd / math.Sqrt(float64(len(totals)))
	// 95% — t_{0.975, n-1} for n ≈ 60 is ~2.00; use 1.96 as a floor.
	const tCrit = 2.0
	half := tCrit * se
	if half < 0 {
		half = 0
	}
	return M41Row{
		CStream:        driver.CStream,
		FetchTimeoutS:  driver.FetchTimeoutS,
		DataStorage:    driver.DataStorage,
		Replicas:       driver.Replicas,
		NodeRole:       role,
		BlockReadIOPS:  meanR,
		BlockWriteIOPS: meanW,
		CILow:          mu - half,
		CIHigh:         mu + half,
	}
}

// bucket is the per-container per-second time-series the reducer
// computes its statistics on. Exported only via the rows returned by
// ReduceM41; consumers don't need the slice directly.
type bucket struct {
	reads  []float64
	writes []float64
}

// aggregateContainers concatenates every container's per-second
// samples into one bucket. Used only on the no-leader path so all-node
// noise is reported as a single row.
func aggregateContainers(in map[string]*bucket, keys []string) *bucket {
	out := &bucket{}
	for _, k := range keys {
		b := in[k]
		if b == nil {
			continue
		}
		out.reads = append(out.reads, b.reads...)
		out.writes = append(out.writes, b.writes...)
	}
	return out
}

// perSecondAverage averages the read and write IOPS across the given
// containers per second-index. Because aggregate.CgroupDeltas returns
// one CgroupDelta per (t_sec, container) the inner slices are already
// indexed by capture second; we average element-wise up to the shortest
// slice length so a container that started a tick late doesn't drag the
// mean.
func perSecondAverage(in map[string]*bucket, keys []string) *bucket {
	if len(keys) == 0 {
		return &bucket{}
	}
	minLen := -1
	for _, k := range keys {
		b := in[k]
		if b == nil {
			continue
		}
		if minLen < 0 || len(b.reads) < minLen {
			minLen = len(b.reads)
		}
	}
	if minLen <= 0 {
		return &bucket{}
	}
	out := &bucket{
		reads:  make([]float64, minLen),
		writes: make([]float64, minLen),
	}
	for _, k := range keys {
		b := in[k]
		if b == nil {
			continue
		}
		for i := 0; i < minLen; i++ {
			out.reads[i] += b.reads[i]
			out.writes[i] += b.writes[i]
		}
	}
	n := float64(len(keys))
	for i := range out.reads {
		out.reads[i] /= n
		out.writes[i] /= n
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func stddev(xs []float64, mu float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	var ss float64
	for _, x := range xs {
		d := x - mu
		ss += d * d
	}
	return math.Sqrt(ss / float64(len(xs)-1))
}

// nodeToContainer is the inverse of ContainerToNode for the rig's
// `iops-` prefix convention. If the input is the sentinel "single"
// (LeaderOf's stub for a non-clustered server) or empty, return "" so
// the caller can detect the no-leader case.
func nodeToContainer(node string) string {
	if node == "" || node == "single" {
		return ""
	}
	if strings.HasPrefix(node, "iops-") {
		return node // already a container name
	}
	return "iops-" + node
}
