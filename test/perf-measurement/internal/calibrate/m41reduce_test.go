// Tests for the M4.1 reducer. We synthesise a tiny three-node cgroup
// capture with known per-node read/write deltas, name the leader, and
// assert the two output rows have the expected node_role + mean
// block_*_iops and CI bounds that bracket the mean.
package calibrate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/test/perf-measurement/internal/aggregate"
)

// buildCgroup creates 4 cumulative samples 1 s apart for each
// (container, device). Each step adds (rDelta, wDelta), so the
// per-second IOPS for the 3 inter-sample gaps are exactly
// (rDelta, wDelta).
func buildCgroup(containers map[string]struct{ R, W uint64 }) []aggregate.CgroupSample {
	const device = "8:0"
	out := make([]aggregate.CgroupSample, 0, 4*len(containers))
	baseTs := int64(1_000_000_000)
	for name, step := range containers {
		var r, w uint64
		for tick := range 4 {
			out = append(out, aggregate.CgroupSample{
				TUnixNs:   baseTs + int64(tick)*1_000_000_000,
				Container: name,
				Device:    device,
				RBytes:    0,
				WBytes:    0,
				RIOs:      r,
				WIOs:      w,
			})
			r += step.R
			w += step.W
		}
	}

	return out
}

func TestReduceM41_LeaderFollowerSplit(t *testing.T) {
	// Leader = nats-1 → container perf-nats-1, deltas (200r, 100w)/s.
	// Followers = nats-2, nats-3 with (50r, 25w)/s and (30r, 15w)/s.
	cg := buildCgroup(map[string]struct{ R, W uint64 }{
		"perf-nats-1": {R: 200, W: 100},
		"perf-nats-2": {R: 50, W: 25},
		"perf-nats-3": {R: 30, W: 15},
	})
	driver := DriverRow{
		CStream:       500,
		FetchTimeoutS: 5,
		DataStorage:   "file",
		Replicas:      3,
		LeaderPre:     "nats-1",
	}

	leader, follower, warn := ReduceM41(cg, driver)
	require.NoError(t, warn)

	require.Equal(t, "leader", leader.NodeRole)
	require.InDelta(t, 200.0, leader.BlockReadIOPS, 1e-6, "leader read iops")
	require.InDelta(t, 100.0, leader.BlockWriteIOPS, 1e-6, "leader write iops")
	require.LessOrEqual(t, leader.CILow, leader.BlockReadIOPS+leader.BlockWriteIOPS,
		"ci_low must be <= total mean")
	require.GreaterOrEqual(t, leader.CIHigh, leader.BlockReadIOPS+leader.BlockWriteIOPS,
		"ci_high must be >= total mean")

	require.Equal(t, "follower", follower.NodeRole)
	// Followers average per-second: read = (50+30)/2 = 40, write = (25+15)/2 = 20.
	require.InDelta(t, 40.0, follower.BlockReadIOPS, 1e-6, "follower mean read iops")
	require.InDelta(t, 20.0, follower.BlockWriteIOPS, 1e-6, "follower mean write iops")
	require.LessOrEqual(t, follower.CILow, follower.BlockReadIOPS+follower.BlockWriteIOPS)
	require.GreaterOrEqual(t, follower.CIHigh, follower.BlockReadIOPS+follower.BlockWriteIOPS)

	// Sanity-check the canonical schema header has 9 fields and each
	// CSV row has the same.
	header := strings.Split(M41RowSchemaHeader, ",")
	require.Len(t, header, 9, "schema header must have 9 columns")
	require.Len(t, strings.Split(leader.CSV(), ","), 9, "leader CSV columns")
	require.Len(t, strings.Split(follower.CSV(), ","), 9, "follower CSV columns")
}

func TestReduceM41_SingleNodeLeader_EmitsFollowerOnly(t *testing.T) {
	// Embedded single-node smoke path: LeaderOf returns "single", which
	// matches no container. ReduceM41 must surface a warning and emit a
	// follower-only row averaging across all containers.
	cg := buildCgroup(map[string]struct{ R, W uint64 }{
		"perf-nats-1": {R: 100, W: 50},
	})
	driver := DriverRow{
		CStream:       0,
		FetchTimeoutS: 5,
		DataStorage:   "memory",
		Replicas:      1,
		LeaderPre:     "single",
	}
	leader, follower, warn := ReduceM41(cg, driver)
	require.Error(t, warn, "expected a warning for single-node capture")
	require.Empty(t, leader.NodeRole, "leader row must be empty in single-node case")
	require.Equal(t, "follower", follower.NodeRole)
}

func TestParseDriverRow_RoundTrip(t *testing.T) {
	// 16-column row matches emitIdlePullCSV. We rely on column indices,
	// not header names, so the test guards against accidental column
	// reordering in cmd/calibrate/main.go.
	const row = `idle-pull-c100-t5s,file,3,0,60.0,0,0,0,0,100,5.000,nats-1,nats-1,false,3,"nats-1;nats-1;nats-1"`
	dr, err := ParseDriverRow(strings.NewReader(row))
	require.NoError(t, err)
	require.Equal(t, 100, dr.CStream)
	require.Equal(t, 5.0, dr.FetchTimeoutS)
	require.Equal(t, "file", dr.DataStorage)
	require.Equal(t, 3, dr.Replicas)
	require.Equal(t, "nats-1", dr.LeaderPre)
}

func TestContainerToNode(t *testing.T) {
	require.Equal(t, "nats-1", ContainerToNode("perf-nats-1"))
	require.Equal(t, "nats-5", ContainerToNode("perf-nats-5"))
}
