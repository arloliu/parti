package testutil

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// IterStep is one result of a ScriptedIter's Next call.
type IterStep struct {
	Msg jetstream.Msg
	Err error
	// Before runs just before the step is returned.
	Before func()
}

// IterData returns a step that delivers a message with the given payload.
func IterData(payload string) IterStep { return IterStep{Msg: &scriptedMsg{data: []byte(payload)}} }

// IterMiss returns a step that reports a missed pull heartbeat.
func IterMiss() IterStep { return IterStep{Err: jetstream.ErrNoHeartbeat} }

// ScriptedIter is a jetstream.MessagesContext that returns scripted steps, for pull loop tests.
// Once the script is used up, Next blocks until Stop, as an idle iterator does.
type ScriptedIter struct {
	mu       sync.Mutex
	steps    []IterStep
	idx      int
	done     chan struct{}
	stopOnce sync.Once
	stops    atomic.Int32
	idle     chan struct{}
	idleOnce sync.Once
}

// NewScriptedIter returns an iterator that plays steps in order.
func NewScriptedIter(steps ...IterStep) *ScriptedIter {
	return &ScriptedIter{steps: steps, done: make(chan struct{}), idle: make(chan struct{})}
}

// Next returns the next scripted step.
func (it *ScriptedIter) Next(_ ...jetstream.NextOpt) (jetstream.Msg, error) {
	select {
	case <-it.done:
		return nil, jetstream.ErrMsgIteratorClosed
	default:
	}

	it.mu.Lock()
	if it.idx >= len(it.steps) {
		it.mu.Unlock()
		it.idleOnce.Do(func() { close(it.idle) })
		<-it.done

		return nil, jetstream.ErrMsgIteratorClosed
	}
	step := it.steps[it.idx]
	it.idx++
	it.mu.Unlock()

	if step.Before != nil {
		step.Before()
	}

	return step.Msg, step.Err
}

// Stop closes the iterator and counts the call.
func (it *ScriptedIter) Stop() {
	it.stops.Add(1)
	it.stopOnce.Do(func() { close(it.done) })
}

// Drain does nothing.
func (it *ScriptedIter) Drain() {}

// Stops returns how many times Stop was called.
func (it *ScriptedIter) Stops() int { return int(it.stops.Load()) }

// Idle is closed when Next is entered with the script used up:
// the loop came back to this iterator and is blocked in it.
func (it *ScriptedIter) Idle() <-chan struct{} { return it.idle }

// scriptedMsg is a jetstream.Msg carrying only a payload; acknowledgements succeed and do nothing.
type scriptedMsg struct {
	data []byte
}

func (m *scriptedMsg) Data() []byte                     { return m.data }
func (m *scriptedMsg) Ack() error                       { return nil }
func (m *scriptedMsg) DoubleAck(context.Context) error  { return nil }
func (m *scriptedMsg) Nak() error                       { return nil }
func (m *scriptedMsg) NakWithDelay(time.Duration) error { return nil }
func (m *scriptedMsg) Term() error                      { return nil }
func (m *scriptedMsg) TermWithReason(string) error      { return nil }
func (m *scriptedMsg) InProgress() error                { return nil }
func (m *scriptedMsg) Subject() string                  { return "" }
func (m *scriptedMsg) Reply() string                    { return "" }
func (m *scriptedMsg) Headers() nats.Header             { return nil }
func (m *scriptedMsg) Metadata() (*jetstream.MsgMetadata, error) {
	return nil, errors.New("not implemented")
}
