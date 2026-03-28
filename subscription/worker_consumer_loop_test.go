package subscription

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"text/template"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	partitesting "github.com/arloliu/parti/v2/testing"
	"github.com/arloliu/parti/v2/types"
)

func TestWorkerConsumer_UpdateAndPullLoop_ProcessesMessages(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	// Start embedded NATS with JetStream
	_, nc := partitesting.StartEmbeddedNATS(t)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create a stream for our subjects
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "TEST",
		Subjects:  []string{"events.*.*"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	// Prepare helper
	cfg := WorkerConsumerConfig{
		StreamName:      "TEST",
		ConsumerPrefix:  "wc2",
		SubjectTemplate: "events.{{.PartitionID}}",
		BatchSize:       2,
	}
	require.NoError(t, cfg.SetDefaults())

	// Parse template for direct construction path (avoid New helper dependencies)
	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)

	var handled int32
	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		atomic.AddInt32(&handled, 1)
		// returning nil causes helper to Ack in default mode
		return nil
	})

	wc := &WorkerConsumer{
		js:              js,
		config:          cfg,
		logger:          cfg.Logger,
		handler:         handler,
		subjects:        make(map[string]*partitionConsumer),
		iterFactory:     defaultIterFactory,
		subjectTemplate: tmpl,
	}

	// Exercise UpdateWorkerConsumer to add one subject and start loop
	parts := []types.Partition{{Keys: []string{"a", "1"}}}
	require.NoError(t, wc.UpdateWorkerConsumer(ctx, "worker-1", parts))
	t.Cleanup(func() { _ = wc.Close(ctx) })

	// Wait for the per-subject durable to exist before publishing to avoid race
	durable := wc.perSubjectDurableName(cfg.ConsumerPrefix, "events.a.1")
	waitDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(waitDeadline) {
		if _, err := js.Consumer(ctx, cfg.StreamName, durable); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Publish a few messages to the subject
	for i := 0; i < 3; i++ {
		require.NoError(t, nc.Publish("events.a.1", []byte("m")))
	}
	_ = nc.Flush()

	// Wait until all messages are handled or timeout
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&handled) >= 3 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	require.GreaterOrEqual(t, atomic.LoadInt32(&handled), int32(3), "expected handler to process at least 3 messages")
}

// errIter is a messages iterator that always returns an error.
type errIter struct{}

func (e *errIter) Next(opts ...jetstream.NextOpt) (jetstream.Msg, error) {
	return nil, errors.New("forced")
}
func (e *errIter) Stop()  {}
func (e *errIter) Drain() {}

func TestWorkerConsumer_PullGating_SuppressesUntilOwnerStable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// stream for subjects
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "PG",
		Subjects:  []string{"pg.*.*"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	// configure with gate + pull gating
	cfg := WorkerConsumerConfig{
		StreamName:        "PG",
		ConsumerPrefix:    "wc2",
		SubjectTemplate:   "pg.{{.PartitionID}}",
		PullGatingEnabled: true,
		ProcessingGate:    &ProcessingGateConfig{Enabled: true},
		Resolver: ResolverConfig{
			HandoffBucketName:   "pg-handoff",
			HandoffClaimsPrefix: "claims/",
			BatchWindow:         5 * time.Millisecond,
			BatchMaxItems:       64,
		},
	}
	require.NoError(t, cfg.SetDefaults())
	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)

	var handled int32
	h := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		atomic.AddInt32(&handled, 1)
		return nil
	})

	wc := &WorkerConsumer{
		js:              js,
		config:          cfg,
		logger:          cfg.Logger,
		handler:         h,
		subjects:        make(map[string]*partitionConsumer),
		iterFactory:     defaultIterFactory,
		subjectTemplate: tmpl,
	}
	// Configure partition prefix/suffix for extraction (since we bypassed constructor)
	wc.partitionPrefix = "pg."
	wc.partitionSuffix = ""

	// init resolver
	require.NoError(t, wc.ensureGateResolver(ctx))

	// add subject with worker w1; gating should suppress since no claim exists
	parts := []types.Partition{{Keys: []string{"a", "1"}}}
	require.NoError(t, wc.UpdateWorkerConsumer(ctx, "w1", parts))
	t.Cleanup(func() { _ = wc.Close(context.Background()) })

	// Wait for the per-subject durable to exist before publishing to avoid race
	durable := wc.perSubjectDurableName(cfg.ConsumerPrefix, "pg.a.1")
	waitDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(waitDeadline) {
		if _, err := js.Consumer(ctx, cfg.StreamName, durable); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Sanity: extraction should produce correct partition id
	require.Equal(t, "a.1", wc.extractPartitionID("pg.a.1"))

	// Publish 3 messages pre-claim; expect none handled yet (gated by processing gate)
	for i := 0; i < 3; i++ {
		require.NoError(t, nc.Publish("pg.a.1", []byte("m")))
	}
	_ = nc.Flush()
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, int32(0), atomic.LoadInt32(&handled))

	// Put claim for owner w1 in Stable; resolver should pick it up and allow pulls
	kv, err := js.KeyValue(ctx, cfg.Resolver.HandoffBucketName)
	require.NoError(t, err)
	cl := handoff.NewInitialClaim("a.1", "w1", time.Now(), 10*time.Minute)
	b, err := cl.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, cfg.Resolver.HandoffClaimsPrefix+cl.PartitionID, b)
	require.NoError(t, err)

	// Publish 3 more messages after claim becomes visible to ensure delivery post-claim
	for i := 0; i < 3; i++ {
		require.NoError(t, nc.Publish("pg.a.1", []byte("m2")))
	}
	_ = nc.Flush()

	// Wait for handling to progress after claim becomes visible
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&handled) >= 3 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	require.GreaterOrEqual(t, atomic.LoadInt32(&handled), int32(3))
}

func TestWorkerConsumer_IteratorEscalation_BurstTriggersMetric(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "ESC", Subjects: []string{"esc.*"}, Retention: jetstream.LimitsPolicy, Storage: jetstream.MemoryStorage, MaxMsgs: -1})
	require.NoError(t, err)

	// Iterator always errors
	iterFactory := func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error) {
		return &errIter{}, nil
	}

	var m captureEscalationMetrics
	cfg := WorkerConsumerConfig{StreamName: "ESC", ConsumerPrefix: "wc2", SubjectTemplate: "esc.{{.PartitionID}}", Retry: RetryConfig{Max: 50 * time.Millisecond}}
	cfg.IteratorEscalationWindow = 200 * time.Millisecond
	cfg.IteratorEscalationThreshold = 3
	cfg.Metrics = &m
	require.NoError(t, cfg.SetDefaults())
	tmpl, _ := template.New("subject").Parse(cfg.SubjectTemplate)
	wc := &WorkerConsumer{js: js, config: cfg, logger: cfg.Logger, handler: MessageHandlerFunc(func(context.Context, jetstream.Msg) error { return nil }), subjects: make(map[string]*partitionConsumer), iterFactory: iterFactory, subjectTemplate: tmpl}

	require.NoError(t, wc.UpdateWorkerConsumer(ctx, "w1", []types.Partition{{Keys: []string{"x"}}}))
	t.Cleanup(func() { _ = wc.Close(context.Background()) })

	// Wait for escalation metric to trigger
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&m.escalations) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.GreaterOrEqual(t, atomic.LoadInt32(&m.escalations), int32(1))
}

func TestWorkerConsumer_SubjectRemoval_InactiveGCGarbageCollects(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "GC", Subjects: []string{"gc.*.*"}, Retention: jetstream.LimitsPolicy, Storage: jetstream.MemoryStorage, MaxMsgs: -1})
	require.NoError(t, err)

	cfg := WorkerConsumerConfig{StreamName: "GC", ConsumerPrefix: "wc2", SubjectTemplate: "gc.{{.PartitionID}}"}
	cfg.InactiveThreshold = 300 * time.Millisecond
	require.NoError(t, cfg.SetDefaults())
	tmpl, _ := template.New("subject").Parse(cfg.SubjectTemplate)
	wc := &WorkerConsumer{js: js, config: cfg, logger: cfg.Logger, handler: MessageHandlerFunc(func(context.Context, jetstream.Msg) error { return nil }), subjects: make(map[string]*partitionConsumer), iterFactory: defaultIterFactory, subjectTemplate: tmpl}

	subj := "gc.a.1"
	part := []types.Partition{{Keys: []string{"a", "1"}}}
	require.NoError(t, wc.UpdateWorkerConsumer(ctx, "w1", part))
	// Wait for durable creation
	durable := wc.perSubjectDurableName(cfg.ConsumerPrefix, subj)
	waitDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(waitDeadline) {
		if _, err := js.Consumer(ctx, cfg.StreamName, durable); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Remove subject (stop loop)
	require.NoError(t, wc.UpdateWorkerConsumer(ctx, "w1", nil))

	// Wait for InactiveThreshold * ~6 and assert consumer is GC'd (allow headroom)
	time.Sleep(2 * time.Second)
	_, err = js.Consumer(ctx, cfg.StreamName, durable)
	require.Error(t, err, "expected consumer to be garbage collected")
	_ = wc.Close(context.Background())
}

func TestWorkerConsumer_DualWorkerPullGating_OnlyOwnerPulls(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Stream and subjects
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "PG2", Subjects: []string{"pg2.*.*"}, Retention: jetstream.LimitsPolicy, Storage: jetstream.MemoryStorage, MaxMsgs: -1})
	require.NoError(t, err)

	cfg := WorkerConsumerConfig{
		StreamName:        "PG2",
		ConsumerPrefix:    "wc2",
		SubjectTemplate:   "pg2.{{.PartitionID}}",
		PullGatingEnabled: true,
		ProcessingGate:    &ProcessingGateConfig{Enabled: true},
		Resolver: ResolverConfig{
			HandoffBucketName:   "pg2-handoff",
			HandoffClaimsPrefix: "claims/",
			BatchWindow:         5 * time.Millisecond,
			BatchMaxItems:       64,
		},
	}
	require.NoError(t, cfg.SetDefaults())

	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)

	var a1w1, a1w2, b1w1, b1w2 int32
	h1 := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		if msg.Subject() == "pg2.a.1" {
			atomic.AddInt32(&a1w1, 1)
		}
		if msg.Subject() == "pg2.b.1" {
			atomic.AddInt32(&b1w1, 1)
		}

		return nil
	})
	h2 := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		if msg.Subject() == "pg2.a.1" {
			atomic.AddInt32(&a1w2, 1)
		}
		if msg.Subject() == "pg2.b.1" {
			atomic.AddInt32(&b1w2, 1)
		}

		return nil
	})

	wc1 := &WorkerConsumer{js: js, config: cfg, logger: cfg.Logger, handler: h1, subjects: make(map[string]*partitionConsumer), iterFactory: defaultIterFactory, subjectTemplate: tmpl}
	wc2 := &WorkerConsumer{js: js, config: cfg, logger: cfg.Logger, handler: h2, subjects: make(map[string]*partitionConsumer), iterFactory: defaultIterFactory, subjectTemplate: tmpl}
	wc1.partitionPrefix = "pg2."
	wc2.partitionPrefix = "pg2."

	// Initialize resolvers
	require.NoError(t, wc1.ensureGateResolver(ctx))
	require.NoError(t, wc2.ensureGateResolver(ctx))

	// Claims: a.1 -> w1, b.1 -> w2
	kv, err := js.KeyValue(ctx, cfg.Resolver.HandoffBucketName)
	require.NoError(t, err)
	clA := handoff.NewInitialClaim("a.1", "w1", time.Now(), 10*time.Minute)
	b, err := clA.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, cfg.Resolver.HandoffClaimsPrefix+clA.PartitionID, b)
	require.NoError(t, err)
	clB := handoff.NewInitialClaim("b.1", "w2", time.Now(), 10*time.Minute)
	b, err = clB.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, cfg.Resolver.HandoffClaimsPrefix+clB.PartitionID, b)
	require.NoError(t, err)

	// Assign both partitions to both workers
	parts := []types.Partition{{Keys: []string{"a", "1"}}, {Keys: []string{"b", "1"}}}
	require.NoError(t, wc1.UpdateWorkerConsumer(ctx, "w1", parts))
	t.Cleanup(func() { _ = wc1.Close(context.Background()) })
	require.NoError(t, wc2.UpdateWorkerConsumer(ctx, "w2", parts))
	t.Cleanup(func() { _ = wc2.Close(context.Background()) })

	// Wait for consumers to exist to avoid race
	for _, subj := range []string{"pg2.a.1", "pg2.b.1"} {
		durable := wc1.perSubjectDurableName(cfg.ConsumerPrefix, subj)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := js.Consumer(ctx, cfg.StreamName, durable); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Publish to both subjects
	require.NoError(t, nc.Publish("pg2.a.1", []byte("1")))
	require.NoError(t, nc.Publish("pg2.b.1", []byte("1")))
	_ = nc.Flush()

	// Expect a.1 handled by w1 only, b.1 by w2 only
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&a1w1) >= 1 && atomic.LoadInt32(&b1w2) >= 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	require.GreaterOrEqual(t, atomic.LoadInt32(&a1w1), int32(1))
	require.Equal(t, int32(0), atomic.LoadInt32(&a1w2), "non-owner should be gated")
	require.GreaterOrEqual(t, atomic.LoadInt32(&b1w2), int32(1))
	require.Equal(t, int32(0), atomic.LoadInt32(&b1w1), "non-owner should be gated")
}

func TestWorkerConsumer_SubscribesAllPartitions_AfterAssignment(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "SUBALL", Subjects: []string{"suball.*.*"}, Retention: jetstream.LimitsPolicy, Storage: jetstream.MemoryStorage, MaxMsgs: -1})
	require.NoError(t, err)

	cfg := WorkerConsumerConfig{StreamName: "SUBALL", ConsumerPrefix: "wc2", SubjectTemplate: "suball.{{.PartitionID}}", BatchSize: 2}
	require.NoError(t, cfg.SetDefaults())
	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)

	var p1, p2, p3 int32
	h := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		switch msg.Subject() {
		case "suball.a.1":
			atomic.AddInt32(&p1, 1)
		case "suball.a.2":
			atomic.AddInt32(&p2, 1)
		case "suball.b.1":
			atomic.AddInt32(&p3, 1)
		}

		return nil
	})

	wc := &WorkerConsumer{js: js, config: cfg, logger: cfg.Logger, handler: h, subjects: make(map[string]*partitionConsumer), iterFactory: defaultIterFactory, subjectTemplate: tmpl}
	parts := []types.Partition{{Keys: []string{"a", "1"}}, {Keys: []string{"a", "2"}}, {Keys: []string{"b", "1"}}}
	require.NoError(t, wc.UpdateWorkerConsumer(ctx, "w1", parts))
	t.Cleanup(func() { _ = wc.Close(context.Background()) })

	// Ensure per-subject consumers are created for all partitions
	subjects := []string{"suball.a.1", "suball.a.2", "suball.b.1"}
	for _, subj := range subjects {
		durable := wc.perSubjectDurableName(cfg.ConsumerPrefix, subj)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := js.Consumer(ctx, cfg.StreamName, durable); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Publish one message to each subject and expect one handled each
	for _, subj := range subjects {
		require.NoError(t, nc.Publish(subj, []byte("x")))
	}
	_ = nc.Flush()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&p1) >= 1 && atomic.LoadInt32(&p2) >= 1 && atomic.LoadInt32(&p3) >= 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	require.GreaterOrEqual(t, atomic.LoadInt32(&p1), int32(1))
	require.GreaterOrEqual(t, atomic.LoadInt32(&p2), int32(1))
	require.GreaterOrEqual(t, atomic.LoadInt32(&p3), int32(1))
}

func TestWorkerConsumer_ManualAck_MaxAckPending_ThrottlesDelivery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "MACK",
		Subjects:  []string{"mack.*.*"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	cfg := WorkerConsumerConfig{
		StreamName:            "MACK",
		ConsumerPrefix:        "wc2",
		SubjectTemplate:       "mack.{{.PartitionID}}",
		ManualAck:             true,
		MaxAckPending:         2,
		MaxConcurrentSubjects: 10,
		BatchSize:             1,
		FetchTimeout:          1 * time.Second,
	}
	require.NoError(t, cfg.SetDefaults())

	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)

	msgCh := make(chan jetstream.Msg, 16)
	var delivered int32
	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		atomic.AddInt32(&delivered, 1)
		// don't ack yet; push to channel for the test to control
		select {
		case msgCh <- msg:
		default:
		}
		return nil
	})

	wc := &WorkerConsumer{
		js:              js,
		config:          cfg,
		logger:          cfg.Logger,
		handler:         handler,
		subjects:        make(map[string]*partitionConsumer),
		iterFactory:     defaultIterFactory,
		subjectTemplate: tmpl,
	}
	wc.partitionPrefix = "mack."
	require.NoError(t, wc.UpdateWorkerConsumer(ctx, "worker-1", []types.Partition{{Keys: []string{"a", "1"}}}))
	t.Cleanup(func() { _ = wc.Close(context.Background()) })

	// Sanity check: loop was started for the subject
	require.Eventually(t, func() bool { return len(wc.subjects) == 1 }, 500*time.Millisecond, 20*time.Millisecond)

	// Wait for durable to exist before publishing
	durable := wc.perSubjectDurableName(cfg.ConsumerPrefix, "mack.a.1")
	waitDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(waitDeadline) {
		if _, err := js.Consumer(ctx, cfg.StreamName, durable); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Give the pull loop a brief moment to start before publishing
	time.Sleep(200 * time.Millisecond)

	// publish 5 messages
	for i := 0; i < 5; i++ {
		require.NoError(t, nc.Publish("mack.a.1", []byte(fmt.Sprintf("m%d", i))))
	}
	_ = nc.Flush()

	// Inspect consumer info for visibility during debugging
	if ci, err := js.Consumer(ctx, cfg.StreamName, durable); err == nil {
		if info, err := ci.Info(ctx); err == nil {
			t.Logf("consumer num_ack_pending=%d, num_pending=%d, num_waiting=%d", info.NumAckPending, info.NumPending, info.NumWaiting)
		}
	}

	// Expect delivery to stop at MaxAckPending (2)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&delivered) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// If nothing delivered yet, try a direct pull to validate server delivery works
	if atomic.LoadInt32(&delivered) == 0 {
		if ci, err := js.Consumer(ctx, cfg.StreamName, durable); err == nil {
			it, ierr := ci.Messages(jetstream.PullMaxMessages(1), jetstream.PullExpiry(1*time.Second))
			if ierr == nil {
				if m, merr := it.Next(); merr == nil && m != nil {
					t.Logf("direct pull received a message: subject=%s", m.Subject())
					_ = m.Ack()
				} else {
					t.Logf("direct pull received no message: err=%v", merr)
				}
			}
		}
	}
	require.Equal(t, int32(2), atomic.LoadInt32(&delivered), "should deliver up to MaxAckPending before acks")

	// Ensure it doesn't exceed while we don't ack
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, int32(2), atomic.LoadInt32(&delivered))

	// Ack one; expect one more to arrive soon (now 3 delivered)
	select {
	case m := <-msgCh:
		require.NoError(t, m.Ack())
	case <-time.After(1 * time.Second):
		t.Fatal("did not receive message to ack")
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&delivered) >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.GreaterOrEqual(t, atomic.LoadInt32(&delivered), int32(3))
}

func TestWorkerConsumer_UpdateRemovesSubject_StopsLoopKeepsDurable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx := context.Background()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "STOP",
		Subjects:  []string{"stop.*.*"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	cfg := WorkerConsumerConfig{StreamName: "STOP", ConsumerPrefix: "wc2", SubjectTemplate: "stop.{{.PartitionID}}"}
	require.NoError(t, cfg.SetDefaults())
	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)

	var handled int32
	h := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		atomic.AddInt32(&handled, 1)
		return nil
	})

	wc := &WorkerConsumer{
		js:              js,
		config:          cfg,
		logger:          cfg.Logger,
		handler:         h,
		subjects:        make(map[string]*partitionConsumer),
		iterFactory:     defaultIterFactory,
		subjectTemplate: tmpl,
	}

	part := []types.Partition{{Keys: []string{"a", "1"}}}
	require.NoError(t, wc.UpdateWorkerConsumer(ctx, "worker-2", part))
	// publish one and wait handled
	require.NoError(t, nc.Publish("stop.a.1", []byte("x")))
	_ = nc.Flush()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&handled) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.Equal(t, int32(1), atomic.LoadInt32(&handled))

	// Now remove the subject
	require.NoError(t, wc.UpdateWorkerConsumer(ctx, "worker-2", nil))

	// Durable should still exist
	durable := wc.perSubjectDurableName(cfg.ConsumerPrefix, "stop.a.1")
	_, err = js.Consumer(ctx, cfg.StreamName, durable)
	require.NoError(t, err)

	// Publish more; loop is stopped so handler should not increment
	for i := 0; i < 3; i++ {
		require.NoError(t, nc.Publish("stop.a.1", []byte("y")))
	}
	_ = nc.Flush()
	// wait a bit; handled should stay the same
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, int32(1), atomic.LoadInt32(&handled))
}

func TestWorkerConsumer_Close_RespectsTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx := context.Background()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "CLOSE",
		Subjects:  []string{"close.*.*"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	cfg := WorkerConsumerConfig{
		StreamName:           "CLOSE",
		ConsumerPrefix:       "wc2",
		SubjectTemplate:      "close.{{.PartitionID}}",
		DrainOnRemoveTimeout: 500 * time.Millisecond, // Short timeout for test
	}
	require.NoError(t, cfg.SetDefaults())
	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)

	// Handler that blocks until signaled
	blockCh := make(chan struct{})
	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		<-blockCh
		return nil
	})

	wc := &WorkerConsumer{
		js:              js,
		config:          cfg,
		logger:          cfg.Logger,
		handler:         handler,
		subjects:        make(map[string]*partitionConsumer),
		iterFactory:     defaultIterFactory,
		subjectTemplate: tmpl,
	}

	// Start consumer
	part := []types.Partition{{Keys: []string{"a", "1"}}}
	require.NoError(t, wc.UpdateWorkerConsumer(ctx, "worker-1", part))

	// Publish message to trigger blocking handler
	require.NoError(t, nc.Publish("close.a.1", []byte("msg")))
	_ = nc.Flush()

	// Give it a moment to pick up the message and block
	time.Sleep(100 * time.Millisecond)

	// Attempt to close with a timeout shorter than the block
	closeCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	err = wc.Close(closeCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded, "Close should return DeadlineExceeded when handler blocks")

	// Unblock handler to allow clean teardown
	close(blockCh)
}
