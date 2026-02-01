package jsutil_test

import (
	"context"
	"sync"
	"testing"

	"github.com/arloliu/parti/jsutil"
	partitesting "github.com/arloliu/parti/testing"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureStream_CreatesNewStream(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	stream, err := jsutil.EnsureStream(ctx, js, jetstream.StreamConfig{
		Name:     "TEST-CREATE",
		Subjects: []string{"test.create.>"},
	})

	require.NoError(t, err)
	require.NotNil(t, stream)

	info, err := stream.Info(ctx)
	require.NoError(t, err)
	assert.Equal(t, "TEST-CREATE", info.Config.Name)
	assert.Equal(t, []string{"test.create.>"}, info.Config.Subjects)
}

func TestEnsureStream_OpensExistingStream(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Pre-create the stream
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "TEST-EXISTING",
		Subjects: []string{"test.existing.>"},
	})
	require.NoError(t, err)

	// EnsureStream should open the existing stream without error
	stream, err := jsutil.EnsureStream(ctx, js, jetstream.StreamConfig{
		Name:     "TEST-EXISTING",
		Subjects: []string{"test.existing.>"},
	})

	require.NoError(t, err)
	require.NotNil(t, stream)

	info, err := stream.Info(ctx)
	require.NoError(t, err)
	assert.Equal(t, "TEST-EXISTING", info.Config.Name)
}

func TestEnsureStream_EmptyNameReturnsError(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	stream, err := jsutil.EnsureStream(ctx, js, jetstream.StreamConfig{
		Name:     "",
		Subjects: []string{"test.>"},
	})

	require.Error(t, err)
	assert.Nil(t, stream)
	assert.Contains(t, err.Error(), "stream name is required")
}

func TestEnsureStream_ConcurrentCreation(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	const numGoroutines = 10
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	results := make(chan error, numGoroutines)

	// Simulate race condition: multiple goroutines trying to create the same stream
	for range numGoroutines {
		go func() {
			defer wg.Done()
			_, err := jsutil.EnsureStream(ctx, js, jetstream.StreamConfig{
				Name:     "TEST-CONCURRENT",
				Subjects: []string{"test.concurrent.>"},
			})
			results <- err
		}()
	}

	wg.Wait()
	close(results)

	// All goroutines should succeed (either create or open existing)
	for err := range results {
		assert.NoError(t, err, "all concurrent EnsureStream calls should succeed")
	}

	// Verify stream exists
	stream, err := js.Stream(ctx, "TEST-CONCURRENT")
	require.NoError(t, err)
	require.NotNil(t, stream)
}

func TestEnsureStream_ContextCancellation(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create already-cancelled context
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	stream, err := jsutil.EnsureStream(ctx, js, jetstream.StreamConfig{
		Name:     "TEST-CANCELLED",
		Subjects: []string{"test.cancelled.>"},
	})

	require.Error(t, err)
	assert.Nil(t, stream)
}
