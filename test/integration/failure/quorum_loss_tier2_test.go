package failure_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestRF3SelectivePeerFault_HandoffQuorumLoss(t *testing.T) {
	if os.Getenv("PARTI_RUN_QUORUM_LOSS_TIER2") != "1" {
		t.Skip("set PARTI_RUN_QUORUM_LOSS_TIER2=1 to run")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	servers, nc := partitest.StartEmbeddedNATSClusterN(t, 5)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	const bucket = "tier2-handoff"
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:   bucket,
		Storage:  jetstream.FileStorage,
		Replicas: 3,
	})
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/p0", []byte(`{"partition_id":"p0","owner":"w1","state":"stable","epoch":1}`))
	require.NoError(t, err)

	info := rf3BucketStreamInfo(t, ctx, kv)
	require.NotNil(t, info.Cluster, "RF3 bucket must expose cluster metadata")
	streamPeers := streamPeerNames(info.Cluster)
	require.Len(t, streamPeers, 3, "RF3 bucket must have three stream peers")
	t.Logf("rf3 bucket cluster leader=%s replicas=%s peers=%s",
		info.Cluster.Leader, peerNames(info.Cluster.Replicas), strings.Join(streamPeers, ","))

	metaLeader := jetStreamMetaLeaderName(t, servers)
	peersToStop := chooseStreamPeersToStop(streamPeers, metaLeader)
	require.Len(t, peersToStop, 2, "probe needs two non-meta stream peers to stop")
	t.Logf("meta leader=%s selected stopped peers=%s", metaLeader, strings.Join(peersToStop, ","))

	stopped := stopBucketPeers(t, servers, peersToStop)
	t.Logf("stopped handoff bucket peers=%s", strings.Join(stopped, ","))

	require.Eventually(t, func() bool { return nc.Status() == nats.CONNECTED },
		10*time.Second, 100*time.Millisecond, "client did not reconnect to surviving peers")

	require.Eventually(t, func() bool {
		accountErr := probeErr(ctx, time.Second, func(ctx context.Context) error {
			_, err := js.AccountInfo(ctx)
			return err
		})
		return accountErr == nil
	}, 10*time.Second, 100*time.Millisecond,
		"JetStream meta quorum should remain available after stopping two of five nodes")

	results := []probeSurfaceResult{
		runProbeSurface(ctx, "kv-get", func(ctx context.Context) error {
			_, err := kv.Get(ctx, "claims/p0")
			return err
		}),
		runProbeSurface(ctx, "kv-put", func(ctx context.Context) error {
			_, err := kv.Put(ctx, "claims/p1", []byte(`{"partition_id":"p1","owner":"w2","state":"stable","epoch":1}`))
			return err
		}),
		runProbeSurface(ctx, "kv-status", func(ctx context.Context) error {
			_, err := kv.Status(ctx)
			return err
		}),
	}

	errorCount := 0
	for _, result := range results {
		t.Logf("tier2 surface=%s class=%s err=%v matrix=M1/M7", result.surface, result.class, result.err)
		if result.err != nil {
			errorCount++
		}
	}
	require.Positive(t, errorCount,
		"selective peer fault did not produce an observable handoff-bucket error surface")
}

func rf3BucketStreamInfo(t *testing.T, ctx context.Context, kv jetstream.KeyValue) *jetstream.StreamInfo {
	t.Helper()
	status, err := kv.Status(ctx)
	require.NoError(t, err)
	bs, ok := status.(*jetstream.KeyValueBucketStatus)
	require.True(t, ok)
	require.NotNil(t, bs.StreamInfo())

	return bs.StreamInfo()
}

func stopBucketPeers(t *testing.T, servers []*server.Server, peers []string) []string {
	t.Helper()
	serversByName := make(map[string]*server.Server, len(servers))
	for _, srv := range servers {
		serversByName[srv.Name()] = srv
	}

	stopped := make([]string, 0, len(peers))
	for _, peer := range peers {
		srv := serversByName[peer]
		require.NotNilf(t, srv, "bucket peer %q not found in embedded cluster", peer)
		srv.Shutdown()
		srv.WaitForShutdown()
		stopped = append(stopped, peer)
	}

	return stopped
}

func streamPeerNames(cluster *jetstream.ClusterInfo) []string {
	seen := make(map[string]struct{}, len(cluster.Replicas)+1)
	names := make([]string, 0, len(cluster.Replicas)+1)
	if cluster.Leader != "" {
		names = append(names, cluster.Leader)
		seen[cluster.Leader] = struct{}{}
	}
	for _, peer := range cluster.Replicas {
		if _, ok := seen[peer.Name]; ok {
			continue
		}
		names = append(names, peer.Name)
		seen[peer.Name] = struct{}{}
	}

	return names
}

func peerNames(peers []*jetstream.PeerInfo) string {
	names := make([]string, len(peers))
	for i, peer := range peers {
		names[i] = peer.Name
	}

	return strings.Join(names, ",")
}

func jetStreamMetaLeaderName(t *testing.T, servers []*server.Server) string {
	t.Helper()
	for _, srv := range servers {
		if srv.JetStreamIsLeader() {
			return srv.Name()
		}
	}
	t.Fatal("no JetStream meta leader found")

	return ""
}

func chooseStreamPeersToStop(streamPeers []string, metaLeader string) []string {
	selected := make([]string, 0, 2)
	for _, peer := range streamPeers {
		if peer == metaLeader {
			continue
		}
		selected = append(selected, peer)
		if len(selected) == 2 {
			return selected
		}
	}

	return selected
}

type probeSurfaceResult struct {
	surface string
	class   string
	err     error
}

func runProbeSurface(parent context.Context, surface string, fn func(context.Context) error) probeSurfaceResult {
	err := probeErr(parent, 3*time.Second, fn)
	return probeSurfaceResult{
		surface: surface,
		class:   classifyProbeErr(err),
		err:     err,
	}
}

func probeErr(parent context.Context, timeout time.Duration, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	return fn(ctx)
}

func classifyProbeErr(err error) string {
	if err == nil {
		return "ok"
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "context-deadline-exceeded"
	case errors.Is(err, nats.ErrNoResponders):
		return "nats-no-responders"
	case errors.Is(err, nats.ErrTimeout):
		return "nats-timeout"
	default:
		return fmt.Sprintf("%T", err)
	}
}
