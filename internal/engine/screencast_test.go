package engine

import (
	"context"
	"image"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"

	"github.com/wimpysworld/nyala/internal/render"
)

// TestRunTextSyncSnapshotsOncePerPaint proves the snapshot goroutine takes its
// first snapshot off the painted latch, not off frame delivery, and then holds: a
// static page that produces no further frames and no re-snapshot triggers
// publishes its text once and stops (AC 1). It injects a counting snapshot fn that
// returns a real run and a closed painted latch, so no browser is involved. It
// asserts the published run reaches textSync, so a later frame can fold it on, and
// that a free-spinning loop (which would run far past one) does not occur.
func TestRunTextSyncSnapshotsOncePerPaint(t *testing.T) {
	t.Parallel()
	var snaps int64
	want := []render.GlyphRun{{Col: 2, Row: 1, Text: "hi"}}
	painted := make(chan struct{})
	close(painted) // paint already happened
	e := &ChromiumEngine{backend: "block", painted: painted}
	e.snapshot = func(context.Context, overlayState) ([]render.GlyphRun, float64, scrollOffset, overlayState) {
		atomic.AddInt64(&snaps, 1)
		return want, 0, scrollOffset{}, overlayState{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reSnap := make(chan struct{}, 1)
	reflow := make(chan struct{}, 1)
	scroll := make(chan struct{}, 1)
	reEmit := make(chan struct{}, 1)
	ts := &textSync{}
	done := make(chan struct{})
	go func() {
		e.runTextSync(ctx, ts, reSnap, reflow, scroll, reEmit)
		close(done)
	}()

	// The first snapshot fires off paint with no frame ever delivered.
	waitForCount(t, &snaps, 1)

	// No triggers: the goroutine must park, not spin. Give it time to misbehave,
	// then confirm the count is unchanged.
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt64(&snaps); got != 1 {
		t.Fatalf("snapshots = %d on a static page after paint, want 1 (loop is free-spinning)", got)
	}

	// The single post-paint snapshot published its runs to textSync as sticky state,
	// so a later frame folds them on without re-snapshotting.
	if got, _, _, _ := ts.latest(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("published runs = %v, want %v", got, want)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runTextSync did not return after context cancel")
	}
}

// TestRunFrameLoopReEmitsHeldFrameWithText proves AC 2 at the engine boundary: once
// the snapshot goroutine has published text and no new screencast frame is pending,
// runFrameLoop re-pushes its held bitmap recomposed with the new text layer. It
// feeds one frame through the slot, drains it, then publishes runs into textSync and
// pokes reEmit. The re-emitted frame must carry the same non-nil bitmap and the
// folded runs. No browser is involved: the slot and channels are driven directly.
func TestRunFrameLoopReEmitsHeldFrameWithText(t *testing.T) {
	t.Parallel()
	bitmap := image.NewRGBA(image.Rect(0, 0, 2, 2))
	slot := newFrameSlot()
	var rec ackRecorder
	e := &ChromiumEngine{backend: "block"}
	ts := &textSync{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames := make(chan render.Frame, 1)
	scroll := make(chan struct{}, 1)
	reEmit := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		e.runFrameLoop(ctx, slot, frames, ts, scroll, reEmit)
		close(done)
	}()

	// One screencast frame arrives before any text is published. The loop holds it
	// and emits it with a nil text layer.
	slot.put(pendingFrame{img: bitmap, ack: 1}, rec.ack)
	first := recvFrame(t, frames)
	if first.Img != bitmap {
		t.Fatalf("first frame bitmap = %p, want the put bitmap %p", first.Img, bitmap)
	}
	if len(first.Text) != 0 {
		t.Fatalf("first frame folded %d runs before any publish, want 0", len(first.Text))
	}

	// Text lands with no new screencast frame pending. The poke must re-emit the held
	// bitmap recomposed with the published runs.
	runs := []render.GlyphRun{{Col: 1, Row: 0, Text: "ok"}}
	ts.store(runs, 0, 0, 0)
	reEmit <- struct{}{}

	reframe := recvFrame(t, frames)
	if reframe.Img != bitmap {
		t.Fatalf("re-emitted frame bitmap = %p, want the held bitmap %p", reframe.Img, bitmap)
	}
	if len(reframe.Text) != 1 || reframe.Text[0] != runs[0] {
		t.Fatalf("re-emitted frame text = %v, want %v folded in", reframe.Text, runs)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runFrameLoop did not return after context cancel")
	}
}

// recvFrame takes one frame off the channel or fails on a deadline, so a re-emit
// assertion does not race the frame loop.
func recvFrame(t *testing.T, frames <-chan render.Frame) render.Frame {
	t.Helper()
	select {
	case f := <-frames:
		return f
	case <-time.After(time.Second):
		t.Fatal("no frame delivered before deadline")
		return render.Frame{}
	}
}

// TestRunTextSyncReSnapTriggers proves an immediate reSnap poke (resize or backend
// switch) drives one more snapshot past the first-after-paint snapshot. It uses a
// closed painted latch and a counting snapshot fn, so no browser is involved.
func TestRunTextSyncReSnapTriggers(t *testing.T) {
	t.Parallel()
	var snaps int64
	painted := make(chan struct{})
	close(painted)
	e := &ChromiumEngine{backend: "block", painted: painted}
	e.snapshot = func(context.Context, overlayState) ([]render.GlyphRun, float64, scrollOffset, overlayState) {
		atomic.AddInt64(&snaps, 1)
		return nil, 0, scrollOffset{}, overlayState{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reSnap := make(chan struct{}, 1)
	reflow := make(chan struct{}, 1)
	scroll := make(chan struct{}, 1)
	reEmit := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		e.runTextSync(ctx, &textSync{}, reSnap, reflow, scroll, reEmit)
		close(done)
	}()

	// First snapshot off paint.
	waitForCount(t, &snaps, 1)
	// A reSnap poke drives a second snapshot.
	reSnap <- struct{}{}
	waitForCount(t, &snaps, 2)

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runTextSync did not return after context cancel")
	}
}

// TestRunTextSyncScrollTriggers proves a scroll poke drives a re-snapshot past the
// first-after-paint snapshot, so the overlay tracks the page as it scrolls. The
// first poke fires the leading throttle at once (lastScrollSnap is zero). It uses a
// closed painted latch and a counting snapshot fn, so no browser is involved.
func TestRunTextSyncScrollTriggers(t *testing.T) {
	t.Parallel()
	var snaps int64
	painted := make(chan struct{})
	close(painted)
	e := &ChromiumEngine{backend: "block", painted: painted}
	e.snapshot = func(context.Context, overlayState) ([]render.GlyphRun, float64, scrollOffset, overlayState) {
		atomic.AddInt64(&snaps, 1)
		return nil, 0, scrollOffset{}, overlayState{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reSnap := make(chan struct{}, 1)
	reflow := make(chan struct{}, 1)
	scroll := make(chan struct{}, 1)
	reEmit := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		e.runTextSync(ctx, &textSync{}, reSnap, reflow, scroll, reEmit)
		close(done)
	}()

	// First snapshot off paint.
	waitForCount(t, &snaps, 1)
	// A scroll poke fires the leading throttle at once (no prior scroll snapshot).
	scroll <- struct{}{}
	waitForCount(t, &snaps, 2)
	// The re-snapshot pokes reEmit so the held frame repaints with the new text.
	select {
	case <-reEmit:
	case <-time.After(time.Second):
		t.Fatal("scroll re-snapshot did not poke reEmit")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runTextSync did not return after context cancel")
	}
}

// TestScrollSnapDue proves the pure leading-throttle decision: the first poke
// (zero last) always fires, a poke inside the cadence is suppressed, and a poke at
// or past the cadence fires. This is the deterministic core of the mid-scroll
// throttle, tested without a wall clock.
func TestScrollSnapDue(t *testing.T) {
	t.Parallel()
	cadence := 150 * time.Millisecond
	base := time.Now()
	cases := []struct {
		name string
		last time.Time
		now  time.Time
		want bool
	}{
		{"first poke fires", time.Time{}, base, true},
		{"inside cadence suppressed", base, base.Add(100 * time.Millisecond), false},
		{"at cadence fires", base, base.Add(cadence), true},
		{"past cadence fires", base, base.Add(300 * time.Millisecond), true},
	}
	for _, c := range cases {
		if got := scrollSnapDue(c.last, c.now, cadence); got != c.want {
			t.Errorf("%s: scrollSnapDue = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestRunTextSyncScrollCadence proves the mid-scroll cadence: a sustained burst of
// scroll pokes spaced under scrollCadence drives more than one snapshot before the
// scroll settles, so the overlay tracks mid-scroll rather than waiting for the
// trailing debounce. It overrides scrollCadence-equivalent timing by driving real
// pokes spaced just over the cadence, then asserts the burst alone (before any
// settle) produced at least two snapshots past the first-after-paint one. The
// counting snapshot fn keeps it browser-free.
func TestRunTextSyncScrollCadence(t *testing.T) {
	t.Parallel()
	var snaps int64
	painted := make(chan struct{})
	close(painted)
	e := &ChromiumEngine{backend: "block", painted: painted}
	e.snapshot = func(context.Context, overlayState) ([]render.GlyphRun, float64, scrollOffset, overlayState) {
		atomic.AddInt64(&snaps, 1)
		return nil, 0, scrollOffset{}, overlayState{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reSnap := make(chan struct{}, 1)
	reflow := make(chan struct{}, 1)
	scroll := make(chan struct{}, 1)
	reEmit := make(chan struct{}, 16)
	done := make(chan struct{})
	go func() {
		e.runTextSync(ctx, &textSync{}, reSnap, reflow, scroll, reEmit)
		close(done)
	}()

	// First snapshot off paint.
	waitForCount(t, &snaps, 1)

	// A sustained scroll: pokes spaced just over scrollCadence keep the leading
	// throttle firing, so the count climbs mid-scroll without waiting for settle. The
	// first poke fires at once; each later poke past the cadence fires again. Drive a
	// few cadence windows, staying under the holdStalenessLimit settle so these are
	// mid-scroll fires, not the trailing one.
	step := scrollCadence + 20*time.Millisecond
	for range 3 {
		scroll <- struct{}{}
		time.Sleep(step)
	}
	// At least two mid-scroll snapshots past the first-after-paint one.
	waitForCount(t, &snaps, 3)

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runTextSync did not return after context cancel")
	}
}

// TestRunFrameLoopPokesScrollOnOffsetChange proves the frame loop pokes the scroll
// channel when a consumed frame's scroll offset differs from the last, and does not
// poke when it is unchanged. No browser is involved: the slot is driven directly.
func TestRunFrameLoopPokesScrollOnOffsetChange(t *testing.T) {
	t.Parallel()
	bitmap := image.NewRGBA(image.Rect(0, 0, 2, 2))
	slot := newFrameSlot()
	var rec ackRecorder
	e := &ChromiumEngine{backend: "block"}
	ts := &textSync{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames := make(chan render.Frame, 4)
	scroll := make(chan struct{}, 1)
	reEmit := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		e.runFrameLoop(ctx, slot, frames, ts, scroll, reEmit)
		close(done)
	}()

	meta := func(y float64) *page.ScreencastFrameMetadata {
		return &page.ScreencastFrameMetadata{ScrollOffsetY: y}
	}

	// First frame establishes the baseline offset; it must not poke (no prior offset).
	slot.put(pendingFrame{img: bitmap, meta: meta(0), ack: 1}, rec.ack)
	recvFrame(t, frames)
	select {
	case <-scroll:
		t.Fatal("first frame poked scroll, want no poke on the baseline offset")
	case <-time.After(50 * time.Millisecond):
	}

	// A frame at a new offset pokes scroll.
	slot.put(pendingFrame{img: bitmap, meta: meta(120), ack: 2}, rec.ack)
	recvFrame(t, frames)
	select {
	case <-scroll:
	case <-time.After(time.Second):
		t.Fatal("scroll-offset change did not poke scroll")
	}

	// A frame at the same offset does not poke again.
	slot.put(pendingFrame{img: bitmap, meta: meta(120), ack: 3}, rec.ack)
	recvFrame(t, frames)
	select {
	case <-scroll:
		t.Fatal("unchanged offset poked scroll, want no poke")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runFrameLoop did not return after context cancel")
	}
}

// waitForCount spins on an atomic counter until it reaches want or the deadline
// passes, so a gated-snapshot assertion does not race the snapshot goroutine.
func waitForCount(t *testing.T, n *int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(n) >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("counter reached %d, want %d before deadline", atomic.LoadInt64(n), want)
}

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
	slices.Sort(out)
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

	for range n {
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
	for p := range producers {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := range perProducer {
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
