package durable

import (
	"context"
	"testing"
	"time"
)

func TestSimpleTracker_BasicFlow(t *testing.T) {
	tr := NewSimpleMessageTracker()
	part := "work.partition.1"
	now := time.Now()

	tr.OnDelivery(part, 1, now)
	tr.OnAck(part, 1)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res, err := tr.DrainAudit(ctx, part)
	if err != nil {
		t.Fatalf("DrainAudit error: %v", err)
	}
	if res.TimedOut {
		t.Fatalf("unexpected timeout; holes=%d lost=%d", res.UnresolvedHoles, res.LostMessages)
	}
	if res.UnresolvedHoles != 0 || res.LostMessages != 0 {
		t.Fatalf("expected no holes/loss, got holes=%d lost=%d", res.UnresolvedHoles, res.LostMessages)
	}
}

func TestSimpleTracker_GapAndFill(t *testing.T) {
	tr := NewSimpleMessageTracker()
	part := "work.partition.2"
	now := time.Now()

	tr.OnDelivery(part, 2, now)
	tr.OnDelivery(part, 4, now)
	tr.OnAck(part, 4)
	// Introduce gap; Drain should timeout if not filled
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	res, _ := tr.DrainAudit(ctx, part)
	if !res.TimedOut || res.UnresolvedHoles == 0 {
		t.Fatalf("expected timeout with unresolved holes, got %+v", res)
	}

	// Fill missing sequences
	tr.OnDelivery(part, 1, now)
	tr.OnAck(part, 1)
	tr.OnDelivery(part, 3, now)
	tr.OnAck(part, 3)
	tr.OnAck(part, 2)

	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	res2, err := tr.DrainAudit(ctx2, part)
	if err != nil {
		t.Fatalf("DrainAudit error: %v", err)
	}
	if res2.TimedOut || res2.UnresolvedHoles != 0 || res2.LostMessages != 0 {
		t.Fatalf("expected clean audit, got %+v", res2)
	}
}

func TestSimpleTracker_LatencyClassification_Strict(t *testing.T) {
	tr := NewSimpleMessageTracker()
	tr.ConfigurePolicy(50*time.Millisecond, true, 0)
	part := "work.partition.latency"
	past := time.Now().Add(-100 * time.Millisecond)

	tr.OnDelivery(part, 1, past)
	tr.OnAck(part, 1)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res, err := tr.DrainAudit(ctx, part)
	if err != nil {
		t.Fatalf("DrainAudit error: %v", err)
	}
	if res.LateMessages != 1 {
		t.Fatalf("expected 1 late message, got %d", res.LateMessages)
	}
}

func TestSimpleTracker_LateTailAllowance(t *testing.T) {
	tr := NewSimpleMessageTracker()
	tr.ConfigurePolicy(50*time.Millisecond, false, 2)
	part := "work.partition.tail"
	past := time.Now().Add(-100 * time.Millisecond)

	tr.OnDelivery(part, 1, past)
	tr.OnAck(part, 1)
	tr.OnDelivery(part, 2, past)
	tr.OnAck(part, 2)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res, err := tr.DrainAudit(ctx, part)
	if err != nil {
		t.Fatalf("DrainAudit error: %v", err)
	}
	if res.LateMessages != 2 {
		t.Fatalf("expected 2 late messages, got %d", res.LateMessages)
	}
}

// TestDrainAudit_ClassifiesLossOnTimeout induces a gap and ensures DrainAudit classifies lost messages on timeout.
func TestDrainAudit_ClassifiesLossOnTimeout(t *testing.T) {
	tr := NewSimpleMessageTracker()
	part := "work.partition.loss"
	now := time.Now()

	// Deliver out-of-order to create a hole that never gets filled.
	tr.OnDelivery(part, 2, now)
	tr.OnDelivery(part, 4, now)
	tr.OnAck(part, 4)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	res, err := tr.DrainAudit(ctx, part)
	if err != nil {
		t.Fatalf("DrainAudit error: %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("expected audit timeout, got %+v", res)
	}
	if res.LostMessages == 0 || res.UnresolvedHoles == 0 {
		t.Fatalf("expected unresolved holes and lost classification, got %+v", res)
	}
}
