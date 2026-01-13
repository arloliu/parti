package kvutil

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	partitest "github.com/arloliu/parti/testing"
)

type testStruct struct {
	Name  string `json:"name"`
	Value int    `json:"value"`
}

func TestJSONHelpers(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	ctx := context.Background()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := EnsureKVBucket(ctx, js, "test-helpers", time.Minute)
	require.NoError(t, err)

	t.Run("PutJSON and GetJSON", func(t *testing.T) {
		obj := testStruct{Name: "foo", Value: 42}
		key := "obj1"

		// Put
		rev, err := PutJSON(ctx, kv, key, obj)
		require.NoError(t, err)
		require.Greater(t, rev, uint64(0))

		// Get
		got, rev2, err := GetJSON[testStruct](ctx, kv, key)
		require.NoError(t, err)
		require.Equal(t, rev, rev2)
		require.Equal(t, obj, *got)

		// Get Non-existent
		gotNil, revNil, err := GetJSON[testStruct](ctx, kv, "non-existent")
		require.NoError(t, err)
		require.Nil(t, gotNil)
		require.Equal(t, uint64(0), revNil)
	})

	t.Run("UpdateJSON CAS success", func(t *testing.T) {
		obj := testStruct{Name: "cas-test", Value: 1}
		key := "cas-key"

		// Put initial value
		rev1, err := PutJSON(ctx, kv, key, obj)
		require.NoError(t, err)

		// Update with correct revision
		obj.Value = 2
		rev2, err := UpdateJSON(ctx, kv, key, obj, rev1)
		require.NoError(t, err)
		require.Greater(t, rev2, rev1)

		// Verify update
		got, _, err := GetJSON[testStruct](ctx, kv, key)
		require.NoError(t, err)
		require.Equal(t, 2, got.Value)
	})

	t.Run("UpdateJSON CAS conflict", func(t *testing.T) {
		obj := testStruct{Name: "conflict-test", Value: 1}
		key := "conflict-key"

		// Put initial value
		rev1, err := PutJSON(ctx, kv, key, obj)
		require.NoError(t, err)

		// Simulate concurrent update (put with new value)
		obj.Value = 100
		_, err = PutJSON(ctx, kv, key, obj)
		require.NoError(t, err)

		// Try to update with stale revision - should fail
		obj.Value = 2
		_, err = UpdateJSON(ctx, kv, key, obj, rev1)
		require.Error(t, err)
		// NATS returns jetstream.ErrKeyExists when revision doesn't match
		require.ErrorIs(t, err, jetstream.ErrKeyExists)
	})
}

func TestListKeys(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	ctx := context.Background()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := EnsureKVBucket(ctx, js, "test-list", time.Minute)
	require.NoError(t, err)

	keys := []string{"prefix/1", "prefix/2", "other/1"}
	for _, k := range keys {
		_, err := kv.PutString(ctx, k, "val")
		require.NoError(t, err)
	}

	t.Run("List with prefix", func(t *testing.T) {
		list, err := ListKeys(ctx, kv, "prefix/", false)
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"prefix/1", "prefix/2"}, list)
	})

	t.Run("List with prefix stripped", func(t *testing.T) {
		list, err := ListKeys(ctx, kv, "prefix/", true)
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"1", "2"}, list)
	})

	t.Run("List all", func(t *testing.T) {
		list, err := ListKeys(ctx, kv, "", false)
		require.NoError(t, err)
		require.ElementsMatch(t, keys, list)
	})

	t.Run("List empty bucket", func(t *testing.T) {
		kvEmpty, err := EnsureKVBucket(ctx, js, "test-list-empty", time.Minute)
		require.NoError(t, err)
		list, err := ListKeys(ctx, kvEmpty, "", false)
		require.NoError(t, err)
		require.Empty(t, list)
	})
}

func TestDeleteKey(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	ctx := context.Background()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := EnsureKVBucket(ctx, js, "test-delete", time.Minute)
	require.NoError(t, err)

	key := "del-key"
	_, err = kv.PutString(ctx, key, "val")
	require.NoError(t, err)

	// Delete existing
	err = DeleteKey(ctx, kv, key)
	require.NoError(t, err)

	// Verify deleted
	_, err = kv.Get(ctx, key)
	require.ErrorIs(t, err, jetstream.ErrKeyNotFound)

	// Delete non-existent (idempotent)
	err = DeleteKey(ctx, kv, key)
	require.NoError(t, err)
}
