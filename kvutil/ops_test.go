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
