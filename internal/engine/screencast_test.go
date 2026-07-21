package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAckFrameEnqueuesInsteadOfRunningInline(t *testing.T) {
	t.Parallel()
	acks := newAckQueue()
	e := &ChromiumEngine{acks: acks}
	e.ackFrame(42)
	got, ok := acks.pop()
	if !ok {
		t.Fatal("ackFrame did not enqueue the SessionID")
	}
	if got != 42 {
		t.Fatalf("pump queue carried %d, want 42", got)
	}
}

func TestAckPumpFailureCancelsSessionWithoutDuplicate(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	acks := newAckQueue()
	var calls int
	e := &ChromiumEngine{
		ctx:    ctx,
		cancel: cancel,
		acks:   acks,
		ackScreencastOp: func(context.Context, int64) error {
			calls++
			return errors.New("ack execution failed")
		},
	}
	done := make(chan struct{})
	go func() {
		e.ackPump(ctx, acks)
		close(done)
	}()
	acks.push(7)
	acks.push(7)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ack pump did not stop after execution failure")
	}
	if calls != 1 {
		t.Fatalf("ack execution calls = %d, want 1 with no retry or duplicate", calls)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("ack execution failure did not cancel the session for rebuild")
	}
}

// TestAckPumpRecordsAckRTTWhenArmed checks the pump times each acknowledgement
// round trip into the armed ingest counter, so the quit summary reports the mean
// and max ack RTT.
func TestAckPumpRecordsAckRTTWhenArmed(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	acks := newAckQueue()
	ran := make(chan struct{}, 2)
	e := &ChromiumEngine{
		acks:   acks,
		ingest: &ingestCounter{on: true, window: ingestWindow},
		ackScreencastOp: func(context.Context, int64) error {
			ran <- struct{}{}
			return nil
		},
	}
	done := make(chan struct{})
	go func() {
		e.ackPump(ctx, acks)
		close(done)
	}()
	acks.push(1)
	acks.push(2)
	for range 2 {
		select {
		case <-ran:
		case <-time.After(time.Second):
			t.Fatal("ack pump did not run the acknowledgement")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ack pump did not stop after cancel")
	}
	e.ingest.record(time.Unix(0, 0)) // one frame, so summary reports
	stats := e.ingest.summary()
	if got := ingestAckCount(e.ingest); got != 2 {
		t.Fatalf("pump recorded %d ack round trips, want 2", got)
	}
	if stats.AckRTTMaxMS < stats.AckRTTMeanMS {
		t.Fatalf("ack_rtt_max_ms %.3f below ack_rtt_mean_ms %.3f", stats.AckRTTMaxMS, stats.AckRTTMeanMS)
	}
}

func ingestAckCount(c *ingestCounter) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ackFrames
}

func TestAckFrameNilQueueDoesNotBlock(t *testing.T) {
	t.Parallel()
	e := &ChromiumEngine{}
	done := make(chan struct{})
	go func() { e.ackFrame(1); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ackFrame blocked on a nil queue")
	}
}

func TestAckPumpRetainsOrderBeyondFormerBufferDepth(t *testing.T) {
	t.Parallel()
	const ackCount = 32 // four times the former depth-eight channel

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	acks := newAckQueue()
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	observed := make(chan int64, ackCount)
	e := &ChromiumEngine{
		acks: acks,
		ackScreencastOp: func(_ context.Context, ack int64) error {
			if ack == 1 {
				close(firstStarted)
				<-releaseFirst
			}
			observed <- ack
			return nil
		},
	}
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		e.ackPump(ctx, acks)
	}()

	e.ackFrame(1)
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("ack pump did not start the first command")
	}
	enqueued := make(chan struct{})
	go func() {
		for ack := int64(2); ack <= ackCount; ack++ {
			e.ackFrame(ack)
		}
		close(enqueued)
	}()
	select {
	case <-enqueued:
	case <-time.After(time.Second):
		t.Fatal("listener enqueue blocked while the acknowledgement command stalled")
	}
	close(releaseFirst)

	for want := int64(1); want <= ackCount; want++ {
		select {
		case got := <-observed:
			if got != want {
				t.Fatalf("acknowledgement %d = SessionID %d, want %d", want, got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for acknowledgement %d", want)
		}
	}
	cancel()
	select {
	case <-pumpDone:
	case <-time.After(time.Second):
		t.Fatal("ack pump did not stop after cancellation")
	}
}

func TestAckPumpStopsOnCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	acks := newAckQueue()
	started := make(chan struct{})
	e := &ChromiumEngine{
		ackScreencastOp: func(ctx context.Context, _ int64) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.ackPump(ctx, acks)
	}()
	acks.push(1)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("ack pump did not start the queued command")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ack pump leaked after cancellation")
	}
}

func TestTeardownSessionStopsAndJoinsAckPump(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	acks := newAckQueue()
	ackPumpDone := make(chan struct{})
	observed := make(chan int64, 1)
	e := &ChromiumEngine{
		ctx:         ctx,
		cancel:      cancel,
		acks:        acks,
		ackPumpDone: ackPumpDone,
		ackScreencastOp: func(_ context.Context, ack int64) error {
			observed <- ack
			return nil
		},
	}
	go func() {
		defer close(ackPumpDone)
		e.ackPump(ctx, acks)
	}()
	e.ackFrame(7)
	select {
	case got := <-observed:
		if got != 7 {
			t.Fatalf("acknowledged SessionID %d, want 7", got)
		}
	case <-time.After(time.Second):
		t.Fatal("ack pump did not process the queued SessionID")
	}

	e.teardownSession()
	select {
	case <-ackPumpDone:
	default:
		t.Fatal("teardown returned before the ack pump stopped")
	}
	if acks.push(8) {
		t.Fatal("torn-down acknowledgement queue accepted a SessionID")
	}
}
