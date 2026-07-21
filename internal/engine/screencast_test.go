package engine

import (
	"context"
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
