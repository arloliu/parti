// Package partition provides static partition-based NATS publishing and subscribing.
//
// It deterministically maps a key to a fixed partition index (0..N-1) and
// constructs subjects using a template pattern that includes {{partition}}
// and optionally {{key}}. Core NATS uses Publisher/Subscriber; JetStream
// publishing uses JSPublisher.
package partition
