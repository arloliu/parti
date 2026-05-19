package provision_test

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/nats-io/nats.go/jetstream"
)

// failAccountInfoJS wraps a JetStream and always returns an error from
// AccountInfo, causing ValidateLive to fail at step 1.B (reachability).
// Used to verify Apply returns Report{} on pre-mutation ValidateLive failure.
type failAccountInfoJS struct {
	jetstream.JetStream
	err error
}

func newFailAccountInfoJS(js jetstream.JetStream) *failAccountInfoJS {
	return &failAccountInfoJS{
		JetStream: js,
		err:       errors.New("simulated reachability failure"),
	}
}

func (f *failAccountInfoJS) AccountInfo(ctx context.Context) (*jetstream.AccountInfo, error) {
	return nil, f.err
}

// raceOnceJS wraps a jetstream.JetStream so that on the first
// CreateKeyValue whose bucket maps to targetStream, the wrapped JS
// pre-creates the bucket out-of-band before forwarding the call. This
// guarantees js.CreateKeyValue returns ErrBucketExists, exercising
// Apply's Plan→Apply race branch (sub-spec §2.E).
type raceOnceJS struct {
	jetstream.JetStream
	targetStream string // e.g. "KV_parti-election"
	fired        atomic.Bool
}

func newRaceOnceJS(js jetstream.JetStream, targetStream string) *raceOnceJS {
	return &raceOnceJS{JetStream: js, targetStream: targetStream}
}

func (r *raceOnceJS) CreateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) {
	if "KV_"+cfg.Bucket == r.targetStream && r.fired.CompareAndSwap(false, true) {
		// Pre-create the bucket out-of-band with a DELIBERATELY
		// DIFFERENT config so nats.go's "silent update on
		// Discard/AllowDirect-only diff" fallback path
		// (jetstream/kv.go:539-558) does NOT trigger. We toggle the
		// History to force a real ErrStreamNameAlreadyInUse return on
		// the forwarded call, which is exactly the Plan→Apply race
		// surface (sub-spec §2.E).
		raceCfg := cfg
		// A different TTL leaves the underlying StreamConfig.MaxAge
		// different from the desired config, so the forwarded call
		// triggers ErrBucketExists / ErrStreamNameAlreadyInUse and
		// the nats.go silent-update branch (which only fires when the
		// only diffs are Discard and AllowDirect) is bypassed.
		if raceCfg.TTL == 0 {
			raceCfg.TTL = 1 * 1000 * 1000 * 1000 // 1s
		} else {
			raceCfg.TTL += 7 // a few ns of drift
		}
		if _, err := r.JetStream.CreateKeyValue(ctx, raceCfg); err != nil {
			return nil, err
		}
		// Fall through to forward the original call; the server now
		// returns ErrBucketExists / ErrStreamNameAlreadyInUse.
	}

	return r.JetStream.CreateKeyValue(ctx, cfg)
}

// cancelOnFirstCreateJS cancels the supplied context.CancelFunc the
// moment the underlying CreateKeyValue would be invoked for the first
// time. The wrapped JetStream returns ctx.Err() without performing the
// create, which Apply must treat as a mid-mutation cancellation. To
// model a pre-mutation cancel (no actions executed), the first call
// must observe the cancellation BEFORE its CreateKeyValue runs — we
// achieve that by cancelling and returning ctx.Err() before calling
// through.
type cancelOnFirstCreateJS struct {
	jetstream.JetStream
	cancel context.CancelFunc
	fired  atomic.Bool
}

func newCancelOnFirstCreateJS(js jetstream.JetStream, cancel context.CancelFunc) *cancelOnFirstCreateJS {
	return &cancelOnFirstCreateJS{JetStream: js, cancel: cancel}
}

func (c *cancelOnFirstCreateJS) CreateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) {
	if c.fired.CompareAndSwap(false, true) {
		c.cancel()
		// Return the cancellation directly so Apply observes
		// context.Canceled from CreateKeyValue itself. (Apply also
		// re-checks ctx.Err() at the top of each iteration, so either
		// path lands in the cancellation branch — but returning
		// ctx.Err() here keeps the model simple: zero actions executed.)
		return nil, context.Canceled
	}

	return c.JetStream.CreateKeyValue(ctx, cfg)
}

// cancelAfterNthCreateJS cancels the supplied context.CancelFunc once
// the underlying CreateKeyValue has completed N times successfully. The
// (N+1)th and subsequent calls return ctx.Err() without invoking the
// real CreateKeyValue. This models a mid-mutation cancellation that
// leaves the first N actions in Apply's Executed slice.
type cancelAfterNthCreateJS struct {
	jetstream.JetStream
	n      int
	cancel context.CancelFunc
	count  atomic.Int64
}

func newCancelAfterNthCreateJS(js jetstream.JetStream, n int, cancel context.CancelFunc) *cancelAfterNthCreateJS {
	return &cancelAfterNthCreateJS{JetStream: js, n: n, cancel: cancel}
}

func (c *cancelAfterNthCreateJS) CreateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) {
	current := c.count.Add(1)
	if current > int64(c.n) {
		c.cancel()
		return nil, context.Canceled
	}

	return c.JetStream.CreateKeyValue(ctx, cfg)
}
