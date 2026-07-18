package engine

import (
	"testing"
	"time"
)

func TestAckFrameEnqueuesInsteadOfRunningInline(t *testing.T) {
	t.Parallel()
	acks := make(chan int64, ackBufferDepth)
	e := &ChromiumEngine{acks: acks}
	e.ackFrame(42)
	select {
	case got := <-acks:
		if got != 42 {
			t.Fatalf("pump channel carried %d, want 42", got)
		}
	default:
		t.Fatal("ackFrame did not enqueue the SessionID")
	}
}

func TestAckFrameNilChannelDrops(t *testing.T) {
	t.Parallel()
	e := &ChromiumEngine{}
	done := make(chan struct{})
	go func() { e.ackFrame(1); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ackFrame blocked on a nil channel")
	}
}

func TestAckFrameFullChannelDrops(t *testing.T) {
	t.Parallel()
	acks := make(chan int64, 1)
	acks <- 99
	e := &ChromiumEngine{acks: acks}
	done := make(chan struct{})
	go func() { e.ackFrame(100); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ackFrame blocked on a full channel")
	}
	if got := <-acks; got != 99 {
		t.Fatalf("full channel carried %d, want original 99", got)
	}
}

func TestWatchdogRecoverIfStalled(t *testing.T) {
	t.Parallel()
	acks := make(chan int64, ackBufferDepth)
	e := &ChromiumEngine{acks: acks}
	now := time.Now()
	if e.watchdogRecoverIfStalled(now) {
		t.Fatal("watchdog recovered before any frame arrived")
	}
	e.markFrameReceived(now, 42)
	if e.watchdogRecoverIfStalled(now.Add(screencastStallThreshold - time.Millisecond)) {
		t.Fatal("watchdog recovered a healthy stream")
	}
	if !e.watchdogRecoverIfStalled(now.Add(screencastStallThreshold + time.Second)) {
		t.Fatal("watchdog did not recover a stalled stream")
	}
	if got := <-acks; got != 42 {
		t.Fatalf("watchdog acknowledged %d, want 42", got)
	}
	if e.watchdogRecoverIfStalled(now.Add(screencastStallThreshold + 2*time.Second)) {
		t.Fatal("watchdog recovered twice in one silent gap")
	}
	later := now.Add(time.Minute)
	e.markFrameReceived(later, 43)
	if !e.watchdogRecoverIfStalled(later.Add(screencastStallThreshold + time.Second)) {
		t.Fatal("fresh frame did not re-arm watchdog")
	}
}
