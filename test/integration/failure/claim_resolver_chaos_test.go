package failure_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/subscription"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestClaimBasedResolver_Chaos_ConcurrentUpdates(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping chaos test in short mode")
	}

	nc, cleanup := testutil.StartExternalNATS(t)
	defer cleanup()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{
		Bucket: "claims_chaos",
	})
	require.NoError(t, err)

	resolver := subscription.NewClaimBasedResolver(kv, "claims/", nil)
	err = resolver.Start(context.Background())
	require.NoError(t, err)
	defer resolver.Stop()

	const (
		numPartitions = 10
		numWorkers    = 5
		duration      = 5 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	var wg sync.WaitGroup

	// Writers
	for i := 0; i < numWorkers; i++ {
		workerID := fmt.Sprintf("w%d", i)
		wg.Add(1) //nolint:revive
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					pid := fmt.Sprintf("p%d", rand.IntN(numPartitions))
					key := "claims/" + pid

					entry, err := kv.Get(context.Background(), key)
					var epoch int64
					if err == nil {
						cl, _ := handoff.UnmarshalClaim(entry.Value())
						epoch = cl.Epoch + 1
					} else {
						epoch = 1
					}

					if rand.Float64() < 0.05 {
						_ = kv.Delete(context.Background(), key)
					} else {
						cl := handoff.Claim{
							PartitionID: pid,
							Owner:       workerID,
							State:       handoff.ClaimStateStable,
							Epoch:       epoch,
							LastUpdated: time.Now(),
						}
						data, _ := cl.Marshal()
						_, _ = kv.Put(context.Background(), key, data)
					}

					time.Sleep(time.Duration(rand.IntN(10)) * time.Millisecond)
				}
			}
		}()
	}

	// Readers
	for i := 0; i < numWorkers; i++ {
		wg.Add(1) //nolint:revive
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					pid := fmt.Sprintf("p%d", rand.IntN(numPartitions))

					if rand.Float64() < 0.1 {
						_ = resolver.ForceRefreshPartition(context.Background(), pid)
					} else {
						_, _, _, _ = resolver.GetOwner(pid)
					}

					time.Sleep(time.Duration(rand.IntN(5)) * time.Millisecond)
				}
			}
		}()
	}

	wg.Wait()

	time.Sleep(200 * time.Millisecond)

	for i := 0; i < numPartitions; i++ {
		pid := fmt.Sprintf("p%d", i)
		key := "claims/" + pid

		entry, err := kv.Get(context.Background(), key)

		owner, state, epoch, ok := resolver.GetOwner(pid)

		if errors.Is(err, jetstream.ErrKeyNotFound) {
			if ok {
				t.Errorf("Partition %s: KV has no key, but Resolver has owner=%s epoch=%d", pid, owner, epoch)
			}
		} else {
			require.NoError(t, err)
			cl, _ := handoff.UnmarshalClaim(entry.Value())

			if !ok {
				t.Errorf("Partition %s: KV has owner=%s epoch=%d, but Resolver has no entry", pid, cl.Owner, cl.Epoch)
			} else {
				if owner != cl.Owner {
					t.Errorf("Partition %s: Owner mismatch. KV=%s, Resolver=%s", pid, cl.Owner, owner)
				}
				if epoch != cl.Epoch {
					t.Errorf("Partition %s: Epoch mismatch. KV=%d, Resolver=%d", pid, cl.Epoch, epoch)
				}
				expectedState := types.HandoffStateStable
				if state != expectedState {
					t.Errorf("Partition %s: State mismatch. KV=%v, Resolver=%v", pid, expectedState, state)
				}
			}
		}
	}
}
