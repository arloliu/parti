package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func main() {
	// Start embedded NATS server
	opts := &server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  "",
		NoLog:     true,
	}

	ns, err := server.NewServer(opts)
	if err != nil {
		log.Fatal("Failed to create NATS server:", err)
	}

	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		log.Fatal("NATS server not ready")
	}
	defer ns.Shutdown()

	// Connect client
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		log.Fatal("Failed to connect:", err)
	}
	defer nc.Close()

	// Create JetStream context
	js, err := jetstream.New(nc)
	if err != nil {
		log.Fatal("Failed to create JetStream:", err)
	}

	ctx := context.Background()

	// Create KV bucket
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  "test-bucket",
		Storage: jetstream.MemoryStorage,
	})
	if err != nil {
		log.Fatal("Failed to create KV bucket:", err)
	}

	fmt.Println("=== Test 1: Watch() on specific key ===")
	testWatchSpecificKey(ctx, kv)

	fmt.Println("\n=== Test 2: WatchAll() without options ===")
	testWatchAll(ctx, kv)

	fmt.Println("\n=== Test 3: WatchAll() with UpdatesOnly ===")
	testWatchAllUpdatesOnly(ctx, kv)

	fmt.Println("\n=== Test 4: Watch() with UpdatesOnly ===")
	testWatchSpecificKeyUpdatesOnly(ctx, kv)
}

func testWatchSpecificKey(ctx context.Context, kv jetstream.KeyValue) {
	// Put some initial values
	kv.Put(ctx, "key1", []byte("initial-value-1"))
	kv.Put(ctx, "key2", []byte("initial-value-2"))

	// Create watcher for specific key
	watcher, err := kv.Watch(ctx, "key1")
	if err != nil {
		log.Fatal("Failed to create watcher:", err)
	}
	defer watcher.Stop()

	fmt.Println("Watcher created for 'key1'")

	// Read initial values
	timeout := time.After(2 * time.Second)
	updateCount := 0

	go func() {
		time.Sleep(500 * time.Millisecond)
		fmt.Println("  [Publisher] Updating key1 to 'updated-1'")
		kv.Put(ctx, "key1", []byte("updated-1"))

		time.Sleep(500 * time.Millisecond)
		fmt.Println("  [Publisher] Updating key1 to 'updated-2'")
		kv.Put(ctx, "key1", []byte("updated-2"))
	}()

	for {
		select {
		case entry := <-watcher.Updates():
			if entry == nil {
				fmt.Println("  ⚠️  Received nil entry (end of initial values marker)")
				continue
			}
			updateCount++
			fmt.Printf("  ✓ Update #%d: key=%s, value=%s, op=%s\n",
				updateCount, entry.Key(), string(entry.Value()), entry.Operation())

		case <-timeout:
			fmt.Printf("  Summary: Received %d updates total\n", updateCount)
			return
		}
	}
}

func testWatchAll(ctx context.Context, kv jetstream.KeyValue) {
	// Put some initial values
	kv.Put(ctx, "key3", []byte("initial-value-3"))
	kv.Put(ctx, "key4", []byte("initial-value-4"))

	// Create watcher for all keys
	watcher, err := kv.WatchAll(ctx)
	if err != nil {
		log.Fatal("Failed to create watcher:", err)
	}
	defer watcher.Stop()

	fmt.Println("WatchAll created (default behavior)")

	timeout := time.After(2 * time.Second)
	updateCount := 0
	nilCount := 0

	go func() {
		time.Sleep(500 * time.Millisecond)
		fmt.Println("  [Publisher] Updating key3 to 'updated-3'")
		kv.Put(ctx, "key3", []byte("updated-3"))

		time.Sleep(500 * time.Millisecond)
		fmt.Println("  [Publisher] Updating key4 to 'updated-4'")
		kv.Put(ctx, "key4", []byte("updated-4"))
	}()

	for {
		select {
		case entry := <-watcher.Updates():
			if entry == nil {
				nilCount++
				fmt.Println("  ⚠️  Received nil entry (end of initial values marker)")
				continue
			}
			updateCount++
			fmt.Printf("  ✓ Update #%d: key=%s, value=%s, op=%s\n",
				updateCount, entry.Key(), string(entry.Value()), entry.Operation())

		case <-timeout:
			fmt.Printf("  Summary: Received %d updates total, %d nil entries\n", updateCount, nilCount)
			return
		}
	}
}

func testWatchAllUpdatesOnly(ctx context.Context, kv jetstream.KeyValue) {
	// Put some initial values
	kv.Put(ctx, "key5", []byte("initial-value-5"))
	kv.Put(ctx, "key6", []byte("initial-value-6"))

	// Create watcher with UpdatesOnly
	watcher, err := kv.WatchAll(ctx, jetstream.UpdatesOnly())
	if err != nil {
		log.Fatal("Failed to create watcher:", err)
	}
	defer watcher.Stop()

	fmt.Println("WatchAll created with UpdatesOnly()")

	timeout := time.After(2 * time.Second)
	updateCount := 0
	nilCount := 0

	go func() {
		time.Sleep(500 * time.Millisecond)
		fmt.Println("  [Publisher] Updating key5 to 'updated-5'")
		kv.Put(ctx, "key5", []byte("updated-5"))

		time.Sleep(500 * time.Millisecond)
		fmt.Println("  [Publisher] Updating key6 to 'updated-6'")
		kv.Put(ctx, "key6", []byte("updated-6"))
	}()

	for {
		select {
		case entry := <-watcher.Updates():
			if entry == nil {
				nilCount++
				fmt.Println("  ⚠️  Received nil entry")
				continue
			}
			updateCount++
			fmt.Printf("  ✓ Update #%d: key=%s, value=%s, op=%s\n",
				updateCount, entry.Key(), string(entry.Value()), entry.Operation())

		case <-timeout:
			fmt.Printf("  Summary: Received %d updates total, %d nil entries\n", updateCount, nilCount)
			return
		}
	}
}

func testWatchSpecificKeyUpdatesOnly(ctx context.Context, kv jetstream.KeyValue) {
	// Put initial value
	kv.Put(ctx, "key7", []byte("initial-value-7"))

	// Create watcher with UpdatesOnly
	watcher, err := kv.Watch(ctx, "key7", jetstream.UpdatesOnly())
	if err != nil {
		log.Fatal("Failed to create watcher:", err)
	}
	defer watcher.Stop()

	fmt.Println("Watch('key7') created with UpdatesOnly()")

	timeout := time.After(2 * time.Second)
	updateCount := 0
	nilCount := 0

	go func() {
		time.Sleep(500 * time.Millisecond)
		fmt.Println("  [Publisher] Updating key7 to 'updated-7a'")
		kv.Put(ctx, "key7", []byte("updated-7a"))

		time.Sleep(500 * time.Millisecond)
		fmt.Println("  [Publisher] Updating key7 to 'updated-7b'")
		kv.Put(ctx, "key7", []byte("updated-7b"))
	}()

	for {
		select {
		case entry := <-watcher.Updates():
			if entry == nil {
				nilCount++
				fmt.Println("  ⚠️  Received nil entry")
				continue
			}
			updateCount++
			fmt.Printf("  ✓ Update #%d: key=%s, value=%s, op=%s\n",
				updateCount, entry.Key(), string(entry.Value()), entry.Operation())

		case <-timeout:
			fmt.Printf("  Summary: Received %d updates total, %d nil entries\n", updateCount, nilCount)
			return
		}
	}
}
