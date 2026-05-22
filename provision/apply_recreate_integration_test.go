package provision_test

// Embedded-NATS integration tests for the W3 recreate apply path: end-to-end
// recreate-stream / recreate-kv / recreate-consumer under PolicyForce, plus the
// W3 stream destructive-consequences documentation test. Shared helpers (newJS,
// createStream, createConsumer, safeUpdateCtx, streamOnlyCfg, crConfig,
// createOrdersStream, durableName) live in the other integration test files in
// this package.

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/provision"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// --- recreate-stream end-to-end ---------------------------------------------

// TestApplyRecreate_Stream_EndToEnd provisions an application stream with
// file storage, then re-applies it with storage:memory + force +
// allowDeleteRecreate and asserts the stream is delete/recreated as memory and
// re-reads marked.
func TestApplyRecreate_Stream_EndToEnd(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	// Step 1: provision the stream with file storage under warn.
	fileCfg := provision.StreamCfg{Name: "orders", Subjects: []string{"orders.>"}, Storage: "file"}
	rep, err := provision.Apply(ctx, js, streamOnlyCfg(provision.PolicyWarn, fileCfg))
	require.NoError(t, err)
	require.Len(t, rep.Executed, 1)

	live := liveOrdersStreamConfig(t, js)
	require.Equal(t, jetstream.FileStorage, live.Storage)

	// Step 2: change config to memory storage + force + allowDeleteRecreate.
	memCfg := provision.StreamCfg{
		Name: "orders", Subjects: []string{"orders.>"}, Storage: "memory",
		AllowDeleteRecreate: true,
	}
	forceCfg := streamOnlyCfg(provision.PolicyForce, memCfg)

	plan, err := provision.Plan(ctx, js, forceCfg)
	require.NoError(t, err)
	require.Len(t, plan.Actions, 1)
	require.Equal(t, provision.ActionRecreateStream, plan.Actions[0].Kind)

	rep2, err := provision.Apply(ctx, js, forceCfg)
	require.NoError(t, err)
	require.False(t, rep2.Aborted)
	require.Empty(t, rep2.Errors)
	require.Len(t, rep2.Executed, 1)
	require.Equal(t, provision.ActionRecreateStream, rep2.Executed[0].Kind)
	require.False(t, rep2.Executed[0].Raced)

	// Step 3: the stream re-reads as memory storage and is marked.
	after := liveOrdersStreamConfig(t, js)
	require.Equal(t, jetstream.MemoryStorage, after.Storage, "stream recreated with the desired storage")
	marker := provision.ParseMarker(after.Metadata)
	require.True(t, marker.IsManaged(), "recreated stream carries the Parti marker")
	require.Equal(t, provision.ComponentStream, marker.Component)
}

// TestApplyRecreate_Stream_DestructiveConsequences is the W3 documentation
// test: it locks the blast radius of a recreate-stream. A stream carrying
// messages and a bound consumer is recreated; the messages and the consumer
// must both be gone afterward.
func TestApplyRecreate_Stream_DestructiveConsequences(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	// Step 1: provision the stream (file storage) and populate it.
	fileCfg := provision.StreamCfg{Name: "orders", Subjects: []string{"orders.>"}, Storage: "file"}
	_, err := provision.Apply(ctx, js, streamOnlyCfg(provision.PolicyWarn, fileCfg))
	require.NoError(t, err)

	for range 5 {
		_, perr := js.Publish(ctx, "orders.a", []byte("payload"))
		require.NoError(t, perr)
	}

	// Bind a durable consumer to the stream.
	_, err = js.CreateConsumer(ctx, "orders", jetstream.ConsumerConfig{
		Durable:       "bound-worker",
		FilterSubject: "orders.a",
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	// Confirm the pre-recreate state: 5 messages, the consumer present.
	stream, err := js.Stream(ctx, "orders")
	require.NoError(t, err)
	preInfo, err := stream.Info(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(5), preInfo.State.Msgs, "5 messages before recreate")

	// Step 2: recreate the stream as memory storage.
	memCfg := provision.StreamCfg{
		Name: "orders", Subjects: []string{"orders.>"}, Storage: "memory",
		AllowDeleteRecreate: true,
	}
	rep, err := provision.Apply(ctx, js, streamOnlyCfg(provision.PolicyForce, memCfg))
	require.NoError(t, err)
	require.Len(t, rep.Executed, 1)
	require.Equal(t, provision.ActionRecreateStream, rep.Executed[0].Kind)

	// Step 3: blast radius — messages and the bound consumer are gone.
	postStream, err := js.Stream(ctx, "orders")
	require.NoError(t, err)
	postInfo, err := postStream.Info(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(0), postInfo.State.Msgs,
		"recreate-stream destroys every message — documented blast radius")

	_, err = js.Consumer(ctx, "orders", "bound-worker")
	require.ErrorIs(t, err, jetstream.ErrConsumerNotFound,
		"recreate-stream cascade-deletes every bound consumer — documented blast radius")
}

// --- recreate-kv end-to-end -------------------------------------------------

// TestApplyRecreate_KV_EndToEnd provisions a partition-source bucket with file
// storage, then re-applies it with storage:memory + force + allowDeleteRecreate
// and asserts the bucket is delete/recreated as memory and re-reads marked.
func TestApplyRecreate_KV_EndToEnd(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	psFile := &provision.PartitionSourceConfig{
		Bucket: "parti-partitions", Key: "partitions/v1", Storage: "file", History: 1,
	}
	baseCfg := provision.Config{
		APIVersion: provision.APIVersionV1, Instance: "prod", Policy: provision.PolicyWarn,
		PartitionSource: psFile,
	}
	rep, err := provision.Apply(ctx, js, baseCfg)
	require.NoError(t, err)
	require.Len(t, rep.Executed, 1)
	require.Equal(t, provision.ActionCreateKV, rep.Executed[0].Kind)

	// Confirm file storage live.
	preStream, err := js.Stream(ctx, "KV_parti-partitions")
	require.NoError(t, err)
	preInfo, err := preStream.Info(ctx)
	require.NoError(t, err)
	require.Equal(t, jetstream.FileStorage, preInfo.Config.Storage)

	// Recreate as memory storage under force + allowDeleteRecreate.
	psMem := &provision.PartitionSourceConfig{
		Bucket: "parti-partitions", Key: "partitions/v1", Storage: "memory", History: 1,
		AllowDeleteRecreate: true,
	}
	forceCfg := provision.Config{
		APIVersion: provision.APIVersionV1, Instance: "prod", Policy: provision.PolicyForce,
		PartitionSource: psMem,
	}

	plan, err := provision.Plan(ctx, js, forceCfg)
	require.NoError(t, err)
	require.Len(t, plan.Actions, 1)
	require.Equal(t, provision.ActionRecreateKV, plan.Actions[0].Kind)

	rep2, err := provision.Apply(ctx, js, forceCfg)
	require.NoError(t, err)
	require.False(t, rep2.Aborted)
	require.Empty(t, rep2.Errors)
	require.Len(t, rep2.Executed, 1)
	require.Equal(t, provision.ActionRecreateKV, rep2.Executed[0].Kind)

	postStream, err := js.Stream(ctx, "KV_parti-partitions")
	require.NoError(t, err)
	postInfo, err := postStream.Info(ctx)
	require.NoError(t, err)
	require.Equal(t, jetstream.MemoryStorage, postInfo.Config.Storage,
		"bucket recreated with the desired storage")
	marker := provision.ParseMarker(postInfo.Config.Metadata)
	require.True(t, marker.IsManaged(), "recreated bucket carries the Parti marker")
	require.Equal(t, provision.ComponentPartitionSource, marker.Component)
}

// --- recreate-consumer end-to-end -------------------------------------------

// TestApplyRecreate_Consumer_EndToEnd precreates a per-partition consumer with
// a divergent immutable field (MemoryStorage), then runs PlanConsumers /
// ApplyConsumers under force + allowDeleteRecreate and asserts the consumer is
// delete/recreated at the desired (non-memory) config.
func TestApplyRecreate_Consumer_EndToEnd(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	createOrdersStream(t, js)

	// Precreate the partition-a consumer with MemoryStorage=true — an immutable
	// divergence from the builder default (false).
	durable := durableName()
	createConsumer(t, js, jetstream.ConsumerConfig{
		Durable:       durable,
		FilterSubject: "orders.a",
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		MemoryStorage: true,
	})

	parts := crPartitions("a")
	cfg := crConfig(parts...)
	cfg.Policy = provision.PolicyForce
	cfg.DynamicConsumers[0].AllowDeleteRecreate = true

	plan, err := provision.PlanConsumers(t.Context(), js, cfg)
	require.NoError(t, err)
	recreateCount := 0
	for _, a := range plan.Actions {
		if a.Kind == provision.ActionRecreateConsumer {
			recreateCount++
		}
	}
	require.Equal(t, 1, recreateCount, "one recreate-consumer for the drifted durable")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rep, err := provision.ApplyConsumers(ctx, js, plan)
	require.NoError(t, err)
	require.False(t, rep.Aborted)
	require.Empty(t, rep.Errors)
	require.Len(t, rep.Executed, 1)
	require.Equal(t, provision.ActionRecreateConsumer, rep.Executed[0].Kind)
	require.False(t, rep.Executed[0].Raced)

	// The recreated consumer no longer carries the divergent MemoryStorage flag.
	consumer, err := js.Consumer(ctx, "orders", durable)
	require.NoError(t, err)
	info, err := consumer.Info(ctx)
	require.NoError(t, err)
	require.False(t, info.Config.MemoryStorage, "consumer recreated at the desired config")
}

// TestApplyRecreate_Consumer_StalePlanNoOp verifies the stale-plan guard
// end-to-end: a recreate-consumer plan whose target was concurrently repaired
// records a raced no-op without deleting.
func TestApplyRecreate_Consumer_StalePlanNoOp(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	createOrdersStream(t, js)

	durable := durableName()
	// Precreate with the immutable divergence so PlanConsumers emits a recreate.
	createConsumer(t, js, jetstream.ConsumerConfig{
		Durable:       durable,
		FilterSubject: "orders.a",
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		MemoryStorage: true,
	})

	cfg := crConfig(crPartitions("a")...)
	cfg.Policy = provision.PolicyForce
	cfg.DynamicConsumers[0].AllowDeleteRecreate = true

	plan, err := provision.PlanConsumers(t.Context(), js, cfg)
	require.NoError(t, err)

	// Locate the recreate-consumer action and extract its desired After config:
	// the concurrent repair must converge the consumer to exactly that config so
	// the apply-time re-classify sees no immutable drift.
	var desired provision.PlannedConsumer
	for _, a := range plan.Actions {
		if a.Kind == provision.ActionRecreateConsumer {
			res, ok := a.Resource.(*provision.RecreateConsumerResource)
			require.True(t, ok)
			desired = res.After
		}
	}
	require.Equal(t, durable, desired.Durable, "plan must carry a recreate-consumer for the drifted durable")

	// Concurrent repair: delete and recreate the consumer at the desired config
	// (no immutable divergence) between plan and apply.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	require.NoError(t, js.DeleteConsumer(ctx, "orders", durable))
	_, err = js.CreateConsumer(ctx, "orders", desired.Config)
	require.NoError(t, err)

	repairedConsumer, err := js.Consumer(ctx, "orders", durable)
	require.NoError(t, err)
	repaired, err := repairedConsumer.Info(ctx)
	require.NoError(t, err)
	createdAt := repaired.Created

	// Apply the stale plan: the re-read live state has no immutable drift, so
	// the recreate is a raced no-op — the repaired consumer must survive.
	rep, err := provision.ApplyConsumers(ctx, js, plan)
	require.NoError(t, err)
	require.Len(t, rep.Executed, 1)
	require.True(t, rep.Executed[0].Raced, "stale-plan no-op records a raced success")

	survivor, err := js.Consumer(ctx, "orders", durable)
	require.NoError(t, err)
	survivorInfo, err := survivor.Info(ctx)
	require.NoError(t, err)
	require.Equal(t, createdAt, survivorInfo.Created,
		"the converged consumer was NOT deleted by the stale recreate plan")
}
