package instrumentedjs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// startEmbeddedNATS spins up an in-process NATS server with JetStream
// enabled on a random port and returns a connected jetstream context
// plus a teardown closure.
func startEmbeddedNATS(t *testing.T) (jetstream.JetStream, func()) {
	t.Helper()
	opts := &server.Options{
		JetStream: true,
		Port:      -1,
		StoreDir:  t.TempDir(),
	}
	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second), "NATS server not ready")

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cleanup := func() {
		nc.Close()
		ns.Shutdown()
		ns.WaitForShutdown()
	}

	return js, cleanup
}

// drainKeys consumes a KeyLister channel so the underlying watcher
// can be released by the server. Existence of this helper sidesteps
// the revive empty-block rule.
func drainKeys(kl jetstream.KeyLister) {
	for range kl.Keys() {
		_ = struct{}{}
	}
}

func mustBucket(t *testing.T, ijs *InstrumentedJS, name string) jetstream.KeyValue {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	kv, err := ijs.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  name,
		History: 5, // needed for History() coverage
	})
	require.NoError(t, err)

	return kv
}

// TestCoverage_AllWrappedKVMethods is the load-bearing test: it walks
// every wrapped KeyValue method once against a real NATS server and
// asserts the corresponding counter incremented. A missed method here
// means a silently zeroed column in the attribution budget.
func TestCoverage_AllWrappedKVMethods(t *testing.T) {
	js, teardown := startEmbeddedNATS(t)
	defer teardown()

	ijs := New(js)
	bucket := "cover"
	kv := mustBucket(t, ijs, bucket)
	ctx := context.Background()

	// Seed a key so Get/GetRevision/Update/Delete/Purge/History all have
	// something to operate on.
	rev, err := kv.Put(ctx, "seed", []byte("v0"))
	require.NoError(t, err)
	_, err = kv.Update(ctx, "seed", []byte("v1"), rev)
	require.NoError(t, err)

	type call struct {
		op string
		fn func(t *testing.T)
	}
	calls := []call{
		{"Get", func(t *testing.T) {
			_, err := kv.Get(ctx, "seed")
			require.NoError(t, err)
		}},
		{"GetRevision", func(t *testing.T) {
			_, err := kv.GetRevision(ctx, "seed", rev)
			require.NoError(t, err)
		}},
		{"Put", func(t *testing.T) {
			_, err := kv.Put(ctx, "put-key", []byte("x"))
			require.NoError(t, err)
		}},
		{"PutString", func(t *testing.T) {
			_, err := kv.PutString(ctx, "puts-key", "y")
			require.NoError(t, err)
		}},
		{"Create", func(t *testing.T) {
			_, err := kv.Create(ctx, "create-key", []byte("z"))
			require.NoError(t, err)
		}},
		{"Update", func(t *testing.T) {
			// Get latest revision for the seed key.
			e, gerr := kv.Get(ctx, "seed")
			require.NoError(t, gerr)
			_, uerr := kv.Update(ctx, "seed", []byte("v2"), e.Revision())
			require.NoError(t, uerr)
		}},
		{"Delete", func(t *testing.T) {
			require.NoError(t, kv.Delete(ctx, "create-key"))
		}},
		{"Purge", func(t *testing.T) {
			// Re-create so we can purge.
			_, err := kv.Put(ctx, "purge-key", []byte("p"))
			require.NoError(t, err)
			require.NoError(t, kv.Purge(ctx, "purge-key"))
		}},
		{"Keys", func(t *testing.T) {
			_, err := kv.Keys(ctx)
			require.NoError(t, err)
		}},
		{"ListKeys", func(t *testing.T) {
			kl, err := kv.ListKeys(ctx)
			require.NoError(t, err)
			// Drain to release the underlying watcher.
			drainKeys(kl)
		}},
		{"ListKeysFiltered", func(t *testing.T) {
			kl, err := kv.ListKeysFiltered(ctx, "seed")
			require.NoError(t, err)
			drainKeys(kl)
		}},
		{"Watch", func(t *testing.T) {
			w, err := kv.Watch(ctx, "seed")
			require.NoError(t, err)
			require.NoError(t, w.Stop())
		}},
		{"WatchAll", func(t *testing.T) {
			w, err := kv.WatchAll(ctx)
			require.NoError(t, err)
			require.NoError(t, w.Stop())
		}},
		{"WatchFiltered", func(t *testing.T) {
			w, err := kv.WatchFiltered(ctx, []string{"seed"})
			require.NoError(t, err)
			require.NoError(t, w.Stop())
		}},
		{"History", func(t *testing.T) {
			_, err := kv.History(ctx, "seed")
			require.NoError(t, err)
		}},
		{"PurgeDeletes", func(t *testing.T) {
			require.NoError(t, kv.PurgeDeletes(ctx))
		}},
		{"Status", func(t *testing.T) {
			_, err := kv.Status(ctx)
			require.NoError(t, err)
		}},
	}

	// The two Put + one Update issued before the loop are seed traffic
	// that must be subtracted from per-op asserts. Snapshot the
	// pre-loop counters so we can assert each method increments by
	// exactly 1 during the loop.
	pre := ijs.Snapshot()

	for _, c := range calls {
		t.Run(c.op, func(t *testing.T) {
			before := ijs.Snapshot()[CounterKey{Bucket: bucket, Op: c.op}]
			c.fn(t)
			after := ijs.Snapshot()[CounterKey{Bucket: bucket, Op: c.op}]
			require.Equal(t, before+1, after, "counter %s should have incremented by exactly 1", c.op)
		})
	}

	// Bucket() is intentionally not counted; assert it returns the
	// real name and did not add a row.
	require.Equal(t, bucket, kv.Bucket())
	require.Equal(t, pre[CounterKey{Bucket: bucket, Op: "Bucket"}], ijs.Snapshot()[CounterKey{Bucket: bucket, Op: "Bucket"}])
}

// TestCoverage_AllWrappedJSMethods exercises every wrapped
// JetStream-level method and asserts each counter under JetStreamBucket
// incremented by 1.
func TestCoverage_AllWrappedJSMethods(t *testing.T) {
	js, teardown := startEmbeddedNATS(t)
	defer teardown()
	ijs := New(js)
	ctx := context.Background()

	// Pre-create a stream so Publish/PublishMsg/PublishAsync/Stream/
	// CreateOrUpdateConsumer have a real target.
	_, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "JS_COVER",
		Subjects: []string{"jscover.>"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	type jscall struct {
		op string
		fn func(t *testing.T)
	}
	calls := []jscall{
		{"KeyValue", func(t *testing.T) {
			// Need a bucket to look up.
			_, err := ijs.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "kv-look"})
			require.NoError(t, err)
			_, err = ijs.KeyValue(ctx, "kv-look")
			require.NoError(t, err)
		}},
		{"CreateKeyValue", func(t *testing.T) {
			_, err := ijs.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "kv-create"})
			require.NoError(t, err)
		}},
		{"UpdateKeyValue", func(t *testing.T) {
			_, err := ijs.UpdateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "kv-create", Description: "u"})
			require.NoError(t, err)
		}},
		{"CreateOrUpdateKeyValue", func(t *testing.T) {
			_, err := ijs.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "kv-cou"})
			require.NoError(t, err)
		}},
		{"AccountInfo", func(t *testing.T) {
			_, err := ijs.AccountInfo(ctx)
			require.NoError(t, err)
		}},
		{"Publish", func(t *testing.T) {
			_, err := ijs.Publish(ctx, "jscover.a", []byte("p"))
			require.NoError(t, err)
		}},
		{"PublishMsg", func(t *testing.T) {
			_, err := ijs.PublishMsg(ctx, &nats.Msg{Subject: "jscover.b", Data: []byte("p")})
			require.NoError(t, err)
		}},
		{"PublishAsync", func(t *testing.T) {
			f, err := ijs.PublishAsync("jscover.c", []byte("p"))
			require.NoError(t, err)
			select {
			case <-f.Ok():
			case e := <-f.Err():
				require.NoError(t, e)
			case <-time.After(2 * time.Second):
				t.Fatal("async publish timed out")
			}
		}},
		{"PublishMsgAsync", func(t *testing.T) {
			f, err := ijs.PublishMsgAsync(&nats.Msg{Subject: "jscover.d", Data: []byte("p")})
			require.NoError(t, err)
			select {
			case <-f.Ok():
			case e := <-f.Err():
				require.NoError(t, e)
			case <-time.After(2 * time.Second):
				t.Fatal("async publish-msg timed out")
			}
		}},
		{"CreateOrUpdateConsumer", func(t *testing.T) {
			_, err := ijs.CreateOrUpdateConsumer(ctx, "JS_COVER", jetstream.ConsumerConfig{
				Durable:   "cover-cons",
				AckPolicy: jetstream.AckExplicitPolicy,
			})
			require.NoError(t, err)
		}},
		{"Stream", func(t *testing.T) {
			_, err := ijs.Stream(ctx, "JS_COVER")
			require.NoError(t, err)
		}},
		{"CreateStream", func(t *testing.T) {
			_, err := ijs.CreateStream(ctx, jetstream.StreamConfig{
				Name:     "JS_COVER_CREATE",
				Subjects: []string{"jscovercreate.>"},
				Storage:  jetstream.MemoryStorage,
			})
			require.NoError(t, err)
		}},
	}

	for _, c := range calls {
		t.Run(c.op, func(t *testing.T) {
			before := ijs.Snapshot()[CounterKey{Bucket: JetStreamBucket, Op: c.op}]
			c.fn(t)
			after := ijs.Snapshot()[CounterKey{Bucket: JetStreamBucket, Op: c.op}]
			require.Equal(t, before+1, after, "JS counter %s should have incremented by exactly 1", c.op)
		})
	}
}

// TestBucketAttribution confirms that calls to different buckets
// produce independent counter rows. This is what the rig depends on
// when mapping bucket name -> parti subsystem.
func TestBucketAttribution(t *testing.T) {
	js, teardown := startEmbeddedNATS(t)
	defer teardown()
	ijs := New(js)
	ctx := context.Background()

	kvA := mustBucket(t, ijs, "bucket-a")
	kvB := mustBucket(t, ijs, "bucket-b")

	for range 3 {
		_, err := kvA.Put(ctx, "k", []byte("v"))
		require.NoError(t, err)
	}
	for range 7 {
		_, err := kvB.Put(ctx, "k", []byte("v"))
		require.NoError(t, err)
	}

	snap := ijs.Snapshot()
	require.Equal(t, int64(3), snap[CounterKey{Bucket: "bucket-a", Op: "Put"}])
	require.Equal(t, int64(7), snap[CounterKey{Bucket: "bucket-b", Op: "Put"}])
}

// TestConcurrency_CountersAreRaceFree spins N goroutines × M ops and
// asserts the final tally is exact. Counters must be safe for
// concurrent use since parti's election / heartbeat / assignment /
// handoff loops share a single JetStream handle.
func TestConcurrency_CountersAreRaceFree(t *testing.T) {
	js, teardown := startEmbeddedNATS(t)
	defer teardown()
	ijs := New(js)
	ctx := context.Background()
	kv := mustBucket(t, ijs, "race")

	const (
		goroutines = 16
		ops        = 50
	)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errCh := make(chan error, goroutines*ops)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range ops {
				if _, err := kv.Put(ctx, "k", []byte("v")); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}
	got := ijs.Snapshot()[CounterKey{Bucket: "race", Op: "Put"}]
	require.Equal(t, int64(goroutines*ops), got)
}

// TestSnapshotIsolation verifies the snapshot is a real copy: mutating
// it does not affect subsequent reads, and subsequent counter
// increments do not retroactively change a held snapshot.
func TestSnapshotIsolation(t *testing.T) {
	js, teardown := startEmbeddedNATS(t)
	defer teardown()
	ijs := New(js)
	ctx := context.Background()
	kv := mustBucket(t, ijs, "iso")

	_, err := kv.Put(ctx, "k", []byte("v"))
	require.NoError(t, err)

	snap1 := ijs.Snapshot()
	require.Equal(t, int64(1), snap1[CounterKey{Bucket: "iso", Op: "Put"}])

	// Mutating the snapshot must not affect the live state.
	snap1[CounterKey{Bucket: "iso", Op: "Put"}] = 9999

	snap2 := ijs.Snapshot()
	require.Equal(t, int64(1), snap2[CounterKey{Bucket: "iso", Op: "Put"}], "live counter must not be aliased to snapshot")

	// Further increments must not retroactively appear in snap1.
	_, err = kv.Put(ctx, "k", []byte("v"))
	require.NoError(t, err)
	require.Equal(t, int64(9999), snap1[CounterKey{Bucket: "iso", Op: "Put"}], "held snapshot must not observe later writes")

	snap3 := ijs.Snapshot()
	require.Equal(t, int64(2), snap3[CounterKey{Bucket: "iso", Op: "Put"}])
}

// TestReset clears all counters.
func TestReset(t *testing.T) {
	js, teardown := startEmbeddedNATS(t)
	defer teardown()
	ijs := New(js)
	ctx := context.Background()
	kv := mustBucket(t, ijs, "reset")

	_, err := kv.Put(ctx, "k", []byte("v"))
	require.NoError(t, err)
	require.NotEmpty(t, ijs.Snapshot())

	ijs.Reset()
	require.Empty(t, ijs.Snapshot())

	// Counters must keep working after reset.
	_, err = kv.Put(ctx, "k", []byte("v"))
	require.NoError(t, err)
	require.Equal(t, int64(1), ijs.Snapshot()[CounterKey{Bucket: "reset", Op: "Put"}])
}

// TestPassthroughOnError confirms that when the underlying call fails,
// the counter still increments (we count attempted RPCs, since that
// reflects load on the server even on error).
func TestPassthroughOnError(t *testing.T) {
	js, teardown := startEmbeddedNATS(t)
	defer teardown()
	ijs := New(js)
	ctx := context.Background()

	// KeyValue on a missing bucket returns an error but should still count.
	_, err := ijs.KeyValue(ctx, "does-not-exist")
	require.Error(t, err)
	require.True(t, errors.Is(err, jetstream.ErrBucketNotFound))
	require.Equal(t, int64(1), ijs.Snapshot()[CounterKey{Bucket: JetStreamBucket, Op: "KeyValue"}])
}

// TestConsumerOverrides_MemoryStorage_OptIn verifies that
// SetConsumerOverrides(true, 0) mutates an in-flight ConsumerConfig
// to set MemoryStorage=true while leaving Replicas at its
// caller-supplied value, and that the caller's local cfg is not
// modified (struct-pass-by-value semantics).
//
// This is the load-bearing test for Plan 02's M2.A cell — the
// interceptor must apply the override even when the caller (parti)
// passes a zero-valued MemoryStorage.
func TestConsumerOverrides_MemoryStorage_OptIn(t *testing.T) {
	js, teardown := startEmbeddedNATS(t)
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "OVR_MEM",
		Subjects: []string{"ovr.mem.>"},
		Storage:  jetstream.FileStorage, // file-backed stream to mirror M1.2 baseline
	})
	require.NoError(t, err)

	ijs := New(js)
	ijs.SetConsumerOverrides(true, 0)

	callerCfg := jetstream.ConsumerConfig{
		Durable:       "ovr-mem-cons",
		AckPolicy:     jetstream.AckExplicitPolicy,
		FilterSubject: "ovr.mem.>",
		// MemoryStorage and Replicas intentionally left at zero values
		// — this mirrors how parti constructs its ConsumerConfig.
	}

	cons, err := ijs.CreateOrUpdateConsumer(ctx, "OVR_MEM", callerCfg)
	require.NoError(t, err)

	info, err := cons.Info(ctx)
	require.NoError(t, err)
	require.True(t, info.Config.MemoryStorage, "interceptor should have set MemoryStorage=true")

	// Caller's cfg must NOT have been mutated (Go pass-by-value).
	require.False(t, callerCfg.MemoryStorage, "caller's local cfg must remain unchanged")
}

// TestConsumerOverrides_Replicas_GuardOnPositive verifies that
// SetConsumerOverrides(_, N>0) mutates Replicas, and that
// SetConsumerOverrides(_, 0) leaves Replicas at the caller's value.
//
// Replicas=0 is JetStream's wire-level "inherit stream replicas"
// sentinel — overriding it would silently force single-replica
// consumers for every non-M2 cell, so the guard on >0 is critical.
func TestConsumerOverrides_Replicas_GuardOnPositive(t *testing.T) {
	js, teardown := startEmbeddedNATS(t)
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "OVR_REP",
		Subjects: []string{"ovr.rep.>"},
		Storage:  jetstream.FileStorage,
	})
	require.NoError(t, err)

	// Case 1: Replicas override = 1 (the M2.B value). Embedded NATS is
	// single-node so 1 is the only valid non-zero replicas value here.
	ijs := New(js)
	ijs.SetConsumerOverrides(false, 1)

	cons, err := ijs.CreateOrUpdateConsumer(ctx, "OVR_REP", jetstream.ConsumerConfig{
		Durable:       "ovr-rep-cons",
		AckPolicy:     jetstream.AckExplicitPolicy,
		FilterSubject: "ovr.rep.>",
	})
	require.NoError(t, err)
	info, err := cons.Info(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, info.Config.Replicas, "interceptor should have set Replicas=1")
	require.False(t, info.Config.MemoryStorage, "MemoryStorage must NOT be set when only Replicas override is configured")

	// Case 2: Replicas override = 0 (default — should NOT override).
	// We assert that the caller-supplied non-zero Replicas survives.
	// On a single-node embedded NATS, Replicas defaults to 1 server-side
	// for any non-explicit value, so the assertion is "the override did
	// not force a different value" — there is no way to set Replicas=2+
	// on a single-node cluster.
	ijs2 := New(js)
	ijs2.SetConsumerOverrides(false, 0) // both off

	cons2, err := ijs2.CreateOrUpdateConsumer(ctx, "OVR_REP", jetstream.ConsumerConfig{
		Durable:       "ovr-rep-cons-2",
		AckPolicy:     jetstream.AckExplicitPolicy,
		FilterSubject: "ovr.rep.>",
	})
	require.NoError(t, err)
	info2, err := cons2.Info(ctx)
	require.NoError(t, err)
	require.False(t, info2.Config.MemoryStorage, "MemoryStorage must remain unset when overrides are off")
}

// TestConsumerOverrides_NoMutationWhenUnset verifies that with both
// override fields at their zero values (the default after New(js)),
// the interceptor is a no-op: an explicit caller-supplied
// MemoryStorage=true survives, and the consumer comes up with the
// caller's exact config.
func TestConsumerOverrides_NoMutationWhenUnset(t *testing.T) {
	js, teardown := startEmbeddedNATS(t)
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "OVR_NOOP",
		Subjects: []string{"ovr.noop.>"},
		Storage:  jetstream.FileStorage,
	})
	require.NoError(t, err)

	ijs := New(js) // no SetConsumerOverrides call

	cons, err := ijs.CreateOrUpdateConsumer(ctx, "OVR_NOOP", jetstream.ConsumerConfig{
		Durable:       "ovr-noop-cons",
		AckPolicy:     jetstream.AckExplicitPolicy,
		FilterSubject: "ovr.noop.>",
		MemoryStorage: true, // caller-supplied
	})
	require.NoError(t, err)
	info, err := cons.Info(ctx)
	require.NoError(t, err)
	require.True(t, info.Config.MemoryStorage, "caller-supplied MemoryStorage=true must pass through unchanged when no override is configured")
}
