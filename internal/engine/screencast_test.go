package engine

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"
)

// ackRecorder is the fake ackFunc the slot tests drive instead of a chromedp
// ack. It records every acked id under a mutex, so a put on one goroutine and a
// consume on another can both ack without a race. No browser is involved.
type ackRecorder struct {
	mu  sync.Mutex
	ids []int64
}

func (r *ackRecorder) ack(id int64) {
	r.mu.Lock()
	r.ids = append(r.ids, id)
	r.mu.Unlock()
}

// counts returns how many times each id was acked, so a test can assert exactly
// once per id.
func (r *ackRecorder) counts() map[int64]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[int64]int, len(r.ids))
	for _, id := range r.ids {
		out[id]++
	}
	return out
}

// sortedIDs returns the acked ids in ascending order for set comparison.
func (r *ackRecorder) sortedIDs() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]int64(nil), r.ids...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// TestAckFrameEnqueuesInsteadOfRunningInline proves the production ack path never
// calls chromedp.Run on the listener goroutine: ackFrame enqueues the SessionID
// onto the pump channel, and a separate pump goroutine runs the real ack off the
// event goroutine. Running the ack inline is the chromedp #920 self-deadlock, so
// this guards the headline invariant. No browser is involved: the test reads the
// id straight off the channel.
func TestAckFrameEnqueuesInsteadOfRunningInline(t *testing.T) {
	t.Parallel()
	acks := make(chan int64, ackBufferDepth)
	e := &ChromiumEngine{acks: acks}

	// ackFrame must return at once (no chromedp.Run, no browser) and leave the id
	// on the pump channel for the pump to drain.
	e.ackFrame(42)

	select {
	case got := <-acks:
		if got != 42 {
			t.Fatalf("pump channel carried %d, want 42", got)
		}
	default:
		t.Fatal("ackFrame did not enqueue the SessionID onto the pump channel")
	}
}

// TestAckFrameNilChannelDrops checks ackFrame drops the ack without blocking when
// the session is torn down (a nil acks channel). The drain on Close acks a held
// frame through this path after teardownSession cleared e.acks, so it must not
// park the caller.
func TestAckFrameNilChannelDrops(t *testing.T) {
	t.Parallel()
	e := &ChromiumEngine{} // acks is nil

	done := make(chan struct{})
	go func() {
		e.ackFrame(1)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ackFrame on a nil channel blocked, want a silent drop")
	}
}

// TestAckFrameFullChannelDrops checks the select-with-default enqueue drops the
// ack rather than blocking when the pump channel is full, so a transient pump
// stall never parks the listener goroutine.
func TestAckFrameFullChannelDrops(t *testing.T) {
	t.Parallel()
	acks := make(chan int64, 1)
	acks <- 99 // fill it
	e := &ChromiumEngine{acks: acks}

	done := make(chan struct{})
	go func() {
		e.ackFrame(100)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ackFrame on a full channel blocked, want a silent drop")
	}
	// The pre-loaded id is still there; the dropped ack did not displace it.
	if got := <-acks; got != 99 {
		t.Fatalf("full channel carried %d, want the original 99", got)
	}
}

// TestFrameSlotPutOnEmptyDefersAck checks the first put stores the frame and
// does not ack it: an unconsumed frame stays unacked until consume or replace.
// A regression that acked on every put would fail here.
func TestFrameSlotPutOnEmptyDefersAck(t *testing.T) {
	t.Parallel()
	slot := newFrameSlot()
	var rec ackRecorder

	slot.put(pendingFrame{ack: 1}, rec.ack)

	if got := len(rec.sortedIDs()); got != 0 {
		t.Fatalf("put on empty slot acked %d frames, want 0", got)
	}
}

// TestFrameSlotPutReplaceAcksDisplaced checks a put onto a full slot acks the
// displaced frame and stores the newcomer. This is the latest-wins drop path: a
// regression that dropped a frame without acking it would stall the live stream,
// and is caught here.
func TestFrameSlotPutReplaceAcksDisplaced(t *testing.T) {
	t.Parallel()
	slot := newFrameSlot()
	var rec ackRecorder

	slot.put(pendingFrame{ack: 1}, rec.ack)
	slot.put(pendingFrame{ack: 2}, rec.ack)

	got := rec.sortedIDs()
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("after replace, acked = %v, want [1] (displaced frame only)", got)
	}

	// The slot now holds only the latest frame (id 2).
	frame, ok := slot.consume(context.Background(), rec.ack)
	if !ok {
		t.Fatal("consume returned ok=false, want the latest held frame")
	}
	if frame.ack != 2 {
		t.Fatalf("slot held frame %d, want 2 (latest wins)", frame.ack)
	}
}

// TestFrameSlotConsumeAcksTaken checks consume acks the frame it returns. The
// consume rate couples Chrome's production rate through this ack, so a missing
// consume ack stalls the stream.
func TestFrameSlotConsumeAcksTaken(t *testing.T) {
	t.Parallel()
	slot := newFrameSlot()
	var rec ackRecorder

	slot.put(pendingFrame{ack: 7}, rec.ack)
	frame, ok := slot.consume(context.Background(), rec.ack)
	if !ok || frame.ack != 7 {
		t.Fatalf("consume = (%d, %v), want (7, true)", frame.ack, ok)
	}

	if c := rec.counts()[7]; c != 1 {
		t.Fatalf("frame 7 acked %d times, want exactly 1", c)
	}
}

// TestFrameSlotInterleavedAckedExactlyOnce is the acceptance-criterion-5 core:
// feed N puts interleaved with consumes through one slot and assert every
// received frame id is acked exactly once, never twice, never zero, whether it
// left via consume or via replace. A double-ack or a dropped ack fails here.
func TestFrameSlotInterleavedAckedExactlyOnce(t *testing.T) {
	t.Parallel()

	// A deterministic interleave: each "put" stores a fresh id, each "consume"
	// takes whatever is held. Some puts land on a full slot (replace), some
	// consumes find the slot empty (no-op via the empty branch below). Every id
	// that enters the slot must be acked once across the run.
	const n = 50
	slot := newFrameSlot()
	var rec ackRecorder
	put := 0

	// script mixes puts and consumes so both the replace path and the
	// consume path fire. A consume on an empty slot would block, so each
	// consume below runs only when the slot is known to hold a frame.
	doPut := func() {
		put++
		slot.put(pendingFrame{ack: int64(put)}, rec.ack)
	}
	doConsume := func() {
		_, ok := slot.consume(context.Background(), rec.ack)
		if !ok {
			t.Fatal("consume returned ok=false on a non-empty slot")
		}
	}

	for i := 0; i < n; i++ {
		doPut() // store
		doPut() // replace the one above (acks it)
		doPut() // replace again (acks it)
		doConsume()
	}

	// Drain the final held frame so its ack is accounted too.
	slot.drain(rec.ack)

	// All ids 1..3n entered the slot. Each must be acked exactly once.
	counts := rec.counts()
	total := 3 * n
	if len(counts) != total {
		t.Fatalf("acked %d distinct ids, want %d", len(counts), total)
	}
	for id := int64(1); id <= int64(total); id++ {
		switch counts[id] {
		case 1:
			// correct: acked exactly once
		case 0:
			t.Errorf("frame %d was never acked", id)
		default:
			t.Errorf("frame %d acked %d times, want exactly 1", id, counts[id])
		}
	}
}

// TestFrameSlotDrainAcksHeld checks drain acks the frame still in the slot, so
// Close leaves nothing unacked. After drain, consume returns ok=false and a
// later put acks immediately rather than storing into a slot no one will read.
func TestFrameSlotDrainAcksHeld(t *testing.T) {
	t.Parallel()
	slot := newFrameSlot()
	var rec ackRecorder

	slot.put(pendingFrame{ack: 9}, rec.ack)
	slot.drain(rec.ack)

	if c := rec.counts()[9]; c != 1 {
		t.Fatalf("held frame 9 acked %d times on drain, want 1", c)
	}

	// consume on a drained slot returns ok=false with no block.
	if _, ok := slot.consume(context.Background(), rec.ack); ok {
		t.Fatal("consume on a drained slot returned ok=true, want false")
	}

	// A late put acks and stores nothing, so no goroutine parks forever.
	slot.put(pendingFrame{ack: 10}, rec.ack)
	if c := rec.counts()[10]; c != 1 {
		t.Fatalf("late put of frame 10 acked %d times, want 1 (drained slot)", c)
	}
}

// TestFrameSlotDrainUnblocksParkedConsume checks a consumer blocked in consume
// returns ok=false when drain runs, so Close cannot leak the draw goroutine.
// Without the drain wake-up, this test times out.
func TestFrameSlotDrainUnblocksParkedConsume(t *testing.T) {
	t.Parallel()
	slot := newFrameSlot()
	var rec ackRecorder

	done := make(chan bool, 1)
	go func() {
		_, ok := slot.consume(context.Background(), rec.ack)
		done <- ok
	}()

	// Give the goroutine time to park on the empty slot, then drain.
	time.Sleep(20 * time.Millisecond)
	slot.drain(rec.ack)

	select {
	case ok := <-done:
		if ok {
			t.Fatal("parked consume returned ok=true after drain, want false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("parked consume did not unblock after drain")
	}
}

// TestFrameSlotConsumeCancelUnblocks checks a parked consume returns ok=false
// when its context is cancelled, the leak-free teardown path Close relies on.
func TestFrameSlotConsumeCancelUnblocks(t *testing.T) {
	t.Parallel()
	slot := newFrameSlot()
	var rec ackRecorder

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		_, ok := slot.consume(ctx, rec.ack)
		done <- ok
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("parked consume returned ok=true after cancel, want false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("parked consume did not unblock after context cancel")
	}

	if got := len(rec.sortedIDs()); got != 0 {
		t.Fatalf("cancel acked %d frames, want 0 (no frame was held)", got)
	}
}

// TestFrameSlotConcurrentPutsAckedExactlyOnce checks the accounting holds under
// concurrent producers and a draining consumer: every id put from many
// goroutines is acked exactly once across consume, replace, and drain. A race
// in the full/held handover would surface as a missing or doubled ack.
func TestFrameSlotConcurrentPutsAckedExactlyOnce(t *testing.T) {
	t.Parallel()
	slot := newFrameSlot()
	var rec ackRecorder

	const producers = 8
	const perProducer = 100
	total := producers * perProducer

	ctx, cancel := context.WithCancel(context.Background())
	consumed := make(chan struct{})
	go func() {
		defer close(consumed)
		for {
			if _, ok := slot.consume(ctx, rec.ack); !ok {
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				slot.put(pendingFrame{ack: int64(base + i + 1)}, rec.ack)
			}
		}(p * perProducer)
	}
	wg.Wait()

	// Drain after producers finish, then stop the consumer.
	slot.drain(rec.ack)
	cancel()
	<-consumed

	counts := rec.counts()
	if len(counts) != total {
		t.Fatalf("acked %d distinct ids, want %d", len(counts), total)
	}
	for id := int64(1); id <= int64(total); id++ {
		if counts[id] != 1 {
			t.Errorf("frame %d acked %d times, want exactly 1", id, counts[id])
		}
	}
}
