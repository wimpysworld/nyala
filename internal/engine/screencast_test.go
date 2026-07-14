package engine

import (
	"context"
	"fmt"
	"image"
	"image/png"
	"io"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"

	"github.com/wimpysworld/nyala/internal/render"
)

// TestRunTextSyncSnapshotsOncePerPaint proves the snapshot goroutine takes its
// first snapshot off the painted latch, not off frame delivery, and then holds: a
// static page that produces no further frames and no re-snapshot triggers publishes
// its text and stops (AC 1). Paint publishes twice, the uncalibrated first publish
// then the calibrated re-publish, so the count settles at 2 and holds. It injects a
// counting snapshot fn that returns a real run and a closed painted latch, so no
// browser is involved (calibrateFonts finds no executor and no-ops). It asserts the
// published run reaches textSync, so a later frame can fold it on, and that a
// free-spinning loop (which would run far past two) does not occur.
func TestRunTextSyncSnapshotsOncePerPaint(t *testing.T) {
	t.Parallel()
	var snaps int64
	want := []render.GlyphRun{{Col: 2, Row: 1, Text: "hi"}}
	painted := make(chan struct{})
	close(painted) // paint already happened
	e := &ChromiumEngine{backend: "block", painted: painted}
	e.snapshot = func(context.Context, overlayState) ([]render.GlyphRun, float64, scrollOffset, int64, overlayState) {
		atomic.AddInt64(&snaps, 1)
		return want, 0, scrollOffset{}, 0, overlayState{}
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

	// Paint fires the uncalibrated first publish then the calibrated re-publish, two
	// snapshots, with no frame ever delivered.
	waitForCount(t, &snaps, 2)

	// No triggers: the goroutine must park, not spin. Give it time to misbehave,
	// then confirm the count is unchanged.
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt64(&snaps); got != 2 {
		t.Fatalf("snapshots = %d on a static page after paint, want 2 (loop is free-spinning)", got)
	}

	// The post-paint snapshots published their runs to textSync as sticky state, so a
	// later frame folds them on without re-snapshotting.
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
	e := &ChromiumEngine{backend: "block", decode: staticDecode(bitmap)}
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

	// One raw screencast frame arrives before any text is published. The worker
	// decodes it, holds it, and emits it with a nil text layer.
	slot.put(rawFrame{data: "f1"})
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

// TestRunFrameLoopCaptureUsesNativeDecoder drives the routine capture worker with
// a real PNG and no decode hook. It guards capture against entering the live Kitty
// pass-through path and proves the decoder-native image remains usable by capture.
func TestRunFrameLoopCaptureUsesNativeDecoder(t *testing.T) {
	t.Parallel()
	payload := encodeBase64PNG(t)
	wantNative, err := decodeScreencastNative(payload)
	if err != nil {
		t.Fatalf("decode reference PNG: %v", err)
	}
	slot := newFrameSlot()
	e := &ChromiumEngine{backend: "kitty", captureOn: true}

	ctx, cancel := context.WithCancel(context.Background())
	frames := make(chan render.Frame, 2)
	done := make(chan struct{})
	go func() {
		e.runFrameLoop(ctx, slot, frames, &textSync{}, make(chan struct{}, 1), make(chan struct{}, 1))
		close(done)
	}()

	for i, scrollY := range []float64{10, 20} {
		slot.put(rawFrame{
			data: payload,
			meta: &page.ScreencastFrameMetadata{ScrollOffsetY: scrollY},
		})
		got := recvFrame(t, frames)
		if got.Img == nil || got.EncodedPNG != "" {
			t.Fatalf("capture frame image = %T, encoded PNG length = %d, want decoded pixels only", got.Img, len(got.EncodedPNG))
		}
		if reflect.TypeOf(got.Img) != reflect.TypeOf(wantNative) {
			t.Fatalf("capture frame type = %T, want decoder-native %T", got.Img, wantNative)
		}
		if got.Stats == nil || got.Stats.ScreencastPayloadBytes != int64(len(payload)) || got.Stats.ScreencastDecodeMS < 0 {
			t.Fatalf("capture frame stats = %+v, want payload bytes %d and a non-negative decode time", got.Stats, len(payload))
		}
		if got.Meta == nil || got.Meta.ScrollOffsetY != scrollY {
			t.Fatalf("capture frame metadata = %+v, want ordered scroll %.0f for frame %d", got.Meta, scrollY, i+1)
		}
		if err := png.Encode(io.Discard, got.Img); err != nil {
			t.Fatalf("capture consumer rejected native image: %v", err)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runFrameLoop did not return after capture cancellation")
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
	e.snapshot = func(context.Context, overlayState) ([]render.GlyphRun, float64, scrollOffset, int64, overlayState) {
		atomic.AddInt64(&snaps, 1)
		return nil, 0, scrollOffset{}, 0, overlayState{}
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

	// Paint publishes twice, the uncalibrated first publish then the calibrated re-publish.
	waitForCount(t, &snaps, 2)
	// A reSnap poke drives a further snapshot.
	reSnap <- struct{}{}
	waitForCount(t, &snaps, 3)

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runTextSync did not return after context cancel")
	}
}

// TestRunTextSyncHoldsTextReadyUntilSettle proves the capture first-publish latch is
// held past the first publish until the initial layout settles: with textReady wired
// (the capture path), the latch stays open after the first snapshot and closes only
// once the first debounced-reflow re-snapshot lands. So the first counted capture
// frame carries the settled overlay, not a pre-settle snapshot. No browser is involved.
func TestRunTextSyncHoldsTextReadyUntilSettle(t *testing.T) {
	var snaps int64
	want := []render.GlyphRun{{Col: 2, Row: 1, Text: "hi"}}
	painted := make(chan struct{})
	close(painted)
	e := &ChromiumEngine{backend: "block", painted: painted}
	// Wire the capture first-publish latch and the single-frame settle hold, the gate
	// the hold runs behind.
	e.textReady = make(chan struct{})
	e.settleHold = true
	e.snapshot = func(context.Context, overlayState) ([]render.GlyphRun, float64, scrollOffset, int64, overlayState) {
		atomic.AddInt64(&snaps, 1)
		return want, 0, scrollOffset{}, 0, overlayState{}
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

	// Paint publishes twice (uncalibrated then calibrated), but the latch is held for
	// the settle.
	waitForCount(t, &snaps, 2)
	time.Sleep(50 * time.Millisecond)
	if isClosed(e.textReady) {
		t.Fatal("textReady closed on the first publish, want it held until the layout settles")
	}

	// A reflow poke drives the debounced load-settle re-snapshot, which releases the latch.
	reflow <- struct{}{}
	if !waitClosed(e.textReady, time.Second) {
		t.Fatal("textReady did not close after the load-settle re-snapshot")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runTextSync did not return after context cancel")
	}
}

// TestRunTextSyncTextReadyBackstop proves a page that fires no load-settle reflow
// still releases the capture first-publish latch, on the initialSettleTimeout
// backstop, so the capture never stalls. It shrinks the backstop so the test runs
// instantly. No browser is involved.
func TestRunTextSyncTextReadyBackstop(t *testing.T) {
	old := initialSettleTimeout
	initialSettleTimeout = 40 * time.Millisecond
	defer func() { initialSettleTimeout = old }()

	var snaps int64
	painted := make(chan struct{})
	close(painted)
	e := &ChromiumEngine{backend: "block", painted: painted}
	e.textReady = make(chan struct{})
	e.settleHold = true
	e.snapshot = func(context.Context, overlayState) ([]render.GlyphRun, float64, scrollOffset, int64, overlayState) {
		atomic.AddInt64(&snaps, 1)
		return nil, 0, scrollOffset{}, 0, overlayState{}
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

	// Paint publishes twice, then no reflow: the backstop releases the latch on its own.
	waitForCount(t, &snaps, 2)
	if !waitClosed(e.textReady, time.Second) {
		t.Fatal("textReady did not close on the settle backstop with no reflow")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runTextSync did not return after context cancel")
	}
}

// waitClosed reports whether ch closes within d, polling so a settle that trails the
// first publish is observed without a fixed sleep.
func waitClosed(ch <-chan struct{}, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if isClosed(ch) {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return isClosed(ch)
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
	e.snapshot = func(context.Context, overlayState) ([]render.GlyphRun, float64, scrollOffset, int64, overlayState) {
		atomic.AddInt64(&snaps, 1)
		return nil, 0, scrollOffset{}, 0, overlayState{}
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

	// Paint publishes twice (uncalibrated then calibrated).
	waitForCount(t, &snaps, 2)
	// A scroll poke fires the leading throttle at once (no prior scroll snapshot).
	scroll <- struct{}{}
	waitForCount(t, &snaps, 3)
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
	e.snapshot = func(context.Context, overlayState) ([]render.GlyphRun, float64, scrollOffset, int64, overlayState) {
		atomic.AddInt64(&snaps, 1)
		return nil, 0, scrollOffset{}, 0, overlayState{}
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

	// Paint publishes twice (uncalibrated then calibrated).
	waitForCount(t, &snaps, 2)

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
	// At least two mid-scroll snapshots past the two-publish paint.
	waitForCount(t, &snaps, 4)

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
	e := &ChromiumEngine{backend: "block", decode: staticDecode(bitmap)}
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

	raw := func(y float64) rawFrame {
		return rawFrame{data: "f", meta: &page.ScreencastFrameMetadata{ScrollOffsetY: y}}
	}

	// First frame establishes the baseline offset; it must not poke (no prior offset).
	slot.put(raw(0))
	recvFrame(t, frames)
	select {
	case <-scroll:
		t.Fatal("first frame poked scroll, want no poke on the baseline offset")
	case <-time.After(50 * time.Millisecond):
	}

	// A frame at a new offset pokes scroll.
	slot.put(raw(120))
	recvFrame(t, frames)
	select {
	case <-scroll:
	case <-time.After(time.Second):
		t.Fatal("scroll-offset change did not poke scroll")
	}

	// A frame at the same offset does not poke again.
	slot.put(raw(120))
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

// staticDecode returns a decode hook that always yields img, so a frame-loop test
// drives the raw slot and the worker decode path without encoding a real PNG.
func staticDecode(img *image.RGBA) func(string) (*image.RGBA, error) {
	return func(string) (*image.RGBA, error) { return img, nil }
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

// blockingConsume drives the slot's real take path, the one runFrameLoop uses: it
// parks on readyC, takes the held raw frame with tryConsume, and treats an empty
// wake as a drain (ok=false) or a spurious re-park. It blocks until a frame is
// taken, ctx is cancelled, or the slot drains, so the slot tests exercise the
// production take rather than a bespoke one. The slot no longer acks: the ack
// happens on receive, before a frame reaches the slot.
func blockingConsume(ctx context.Context, slot *frameSlot) (rawFrame, bool) {
	for {
		if frame, ok := slot.tryConsume(); ok {
			return frame, true
		}
		if slot.drained() {
			return rawFrame{}, false
		}
		select {
		case <-slot.readyC():
		case <-ctx.Done():
			return rawFrame{}, false
		}
	}
}

// TestFrameSlotPutStoresLatest checks a single put stores the raw frame and consume
// returns it. The slot performs no ack; ack accounting lives on the receive path.
func TestFrameSlotPutStoresLatest(t *testing.T) {
	t.Parallel()
	slot := newFrameSlot()

	slot.put(rawFrame{data: "a"})
	frame, ok := blockingConsume(context.Background(), slot)
	if !ok || frame.data != "a" {
		t.Fatalf("consume = (%q, %v), want (\"a\", true)", frame.data, ok)
	}
}

// TestFrameSlotPutReplaceCoalesces checks a put onto a full slot replaces the
// unconsumed raw frame (latest-wins coalescing), so the worker only ever decodes
// the newest and the older undecoded frame is dropped. A regression that queued
// rather than coalesced would return the stale frame here.
func TestFrameSlotPutReplaceCoalesces(t *testing.T) {
	t.Parallel()
	slot := newFrameSlot()

	slot.put(rawFrame{data: "old"})
	slot.put(rawFrame{data: "new"})

	frame, ok := blockingConsume(context.Background(), slot)
	if !ok || frame.data != "new" {
		t.Fatalf("consume after replace = (%q, %v), want (\"new\", true) (latest wins)", frame.data, ok)
	}

	// Nothing else is held: the coalesced older frame left no residue.
	if _, ok := slot.tryConsume(); ok {
		t.Fatal("slot held a second frame after coalescing, want only the latest")
	}
}

// TestFrameSlotDrainStopsStore checks drain marks the slot dropped: consume returns
// ok=false and a late put stores nothing, so no goroutine parks on a slot no one
// reads. drain performs no ack; the held raw frame was already acked on receive.
func TestFrameSlotDrainStopsStore(t *testing.T) {
	t.Parallel()
	slot := newFrameSlot()

	slot.put(rawFrame{data: "x"})
	slot.drain()

	if _, ok := blockingConsume(context.Background(), slot); ok {
		t.Fatal("consume on a drained slot returned ok=true, want false")
	}

	// A late put stores nothing on a drained slot.
	slot.put(rawFrame{data: "late"})
	if _, ok := slot.tryConsume(); ok {
		t.Fatal("late put stored a frame in a drained slot, want nothing stored")
	}
}

// TestFrameSlotDrainUnblocksParkedConsume checks a consumer blocked in consume
// returns ok=false when drain runs, so Close cannot leak the draw goroutine.
// Without the drain wake-up, this test times out.
func TestFrameSlotDrainUnblocksParkedConsume(t *testing.T) {
	t.Parallel()
	slot := newFrameSlot()

	done := make(chan bool, 1)
	go func() {
		_, ok := blockingConsume(context.Background(), slot)
		done <- ok
	}()

	// Give the goroutine time to park on the empty slot, then drain.
	time.Sleep(20 * time.Millisecond)
	slot.drain()

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

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		_, ok := blockingConsume(ctx, slot)
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
}

// TestFrameSlotConcurrentPutConsumeNoRace stresses the slot handoff under many
// concurrent producers and a draining consumer: every frame the consumer takes
// must be a distinct frame that was put (no duplicate delivery, no phantom), and
// the run must be race-clean under -race. Coalescing means some puts are dropped,
// which is correct; the invariant is that no frame is delivered twice.
func TestFrameSlotConcurrentPutConsumeNoRace(t *testing.T) {
	t.Parallel()
	slot := newFrameSlot()

	const producers = 8
	const perProducer = 100

	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	seen := map[string]int{}
	consumed := make(chan struct{})
	go func() {
		defer close(consumed)
		for {
			frame, ok := blockingConsume(ctx, slot)
			if !ok {
				return
			}
			mu.Lock()
			seen[frame.data]++
			mu.Unlock()
		}
	}()

	var wg sync.WaitGroup
	for p := range producers {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := range perProducer {
				slot.put(rawFrame{data: fmt.Sprintf("f%d", base+i+1)})
			}
		}(p * perProducer)
	}
	wg.Wait()

	slot.drain()
	cancel()
	<-consumed

	mu.Lock()
	defer mu.Unlock()
	for data, n := range seen {
		if n != 1 {
			t.Errorf("frame %q delivered %d times, want at most once", data, n)
		}
	}
}

// TestHandleScreencastFrameAcksOnReceiveBeforeDecode is the headline of the
// ack-on-receive fix: the listener callback acks the frame and stores it raw and
// undecoded, doing no decode itself. It wires a decode hook that would record a
// call, calls handleScreencastFrame, then asserts the ack landed on the pump
// channel, the raw frame sits in the slot, and the decode hook was never invoked on
// the listener goroutine. So Chrome's credit is released before any pixel work.
func TestHandleScreencastFrameAcksOnReceiveBeforeDecode(t *testing.T) {
	t.Parallel()
	acks := make(chan int64, ackBufferDepth)
	var decoded atomic.Int64
	e := &ChromiumEngine{acks: acks, decode: func(string) (*image.RGBA, error) {
		decoded.Add(1)
		return image.NewRGBA(image.Rect(0, 0, 1, 1)), nil
	}}
	slot := newFrameSlot()

	e.handleScreencastFrame(slot, &page.EventScreencastFrame{Data: "payload", SessionID: 5})

	// Acked on receive.
	select {
	case got := <-acks:
		if got != 5 {
			t.Fatalf("acked SessionID %d, want 5", got)
		}
	default:
		t.Fatal("handleScreencastFrame did not ack the frame on receive")
	}
	// Stored raw and undecoded: the listener ran no decode.
	if n := decoded.Load(); n != 0 {
		t.Fatalf("decode ran %d times on the listener goroutine, want 0 (decode is the worker's job)", n)
	}
	raw, ok := slot.tryConsume()
	if !ok || raw.data != "payload" {
		t.Fatalf("slot held (%q, %v), want the raw payload undecoded", raw.data, ok)
	}
}

// TestHandleScreencastFrameAcksEveryDeliveredFrameIncludingDropped feeds several
// frames through the listener with no consumer, so the slot coalesces all but the
// last. Each delivered frame must still be acked exactly once, on receive, whether
// or not it is later decoded. This is the ack-exactly-once invariant under the raw
// coalescing slot.
func TestHandleScreencastFrameAcksEveryDeliveredFrameIncludingDropped(t *testing.T) {
	t.Parallel()
	const n = 6
	acks := make(chan int64, ackBufferDepth)
	e := &ChromiumEngine{acks: acks}
	slot := newFrameSlot()

	for i := 1; i <= n; i++ {
		e.handleScreencastFrame(slot, &page.EventScreencastFrame{Data: "d", SessionID: int64(i)})
	}

	// Every delivered frame acked once, even though the slot holds only the latest.
	got := map[int64]int{}
	for len(got) < n {
		select {
		case id := <-acks:
			got[id]++
		default:
			t.Fatalf("acked %d distinct frames, want %d (a delivered frame went unacked)", len(got), n)
		}
	}
	for id := int64(1); id <= n; id++ {
		if got[id] != 1 {
			t.Errorf("SessionID %d acked %d times, want exactly 1", id, got[id])
		}
	}
	// The slot coalesced to the latest raw frame only.
	if _, ok := slot.tryConsume(); !ok {
		t.Fatal("slot held no frame after a burst, want the latest")
	}
	if _, ok := slot.tryConsume(); ok {
		t.Fatal("slot held more than the latest frame after coalescing")
	}
}

// TestWorkerDecodesOffReceiver ties the pieces together: a frame delivered through
// handleScreencastFrame (which does not decode) is decoded by the runFrameLoop
// worker and emitted. It asserts the emitted frame carries the worker-decoded
// bitmap and the decode hook ran exactly once, on the worker, after receive.
func TestWorkerDecodesOffReceiver(t *testing.T) {
	t.Parallel()
	bitmap := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var decoded atomic.Int64
	e := &ChromiumEngine{backend: "block", acks: make(chan int64, ackBufferDepth), decode: func(string) (*image.RGBA, error) {
		decoded.Add(1)
		return bitmap, nil
	}}
	slot := newFrameSlot()
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

	e.handleScreencastFrame(slot, &page.EventScreencastFrame{Data: "p", SessionID: 1})

	frame := recvFrame(t, frames)
	if frame.Img != bitmap {
		t.Fatalf("emitted frame bitmap = %p, want the worker-decoded bitmap %p", frame.Img, bitmap)
	}
	if n := decoded.Load(); n != 1 {
		t.Fatalf("decode ran %d times, want exactly 1 (on the worker)", n)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runFrameLoop did not return after context cancel")
	}
}

// TestWatchdogRecoverIfStalled proves the stuck-state watchdog re-couples a wedged
// stream once per silent gap without disturbing a healthy or idle stream. It drives
// watchdogRecoverIfStalled directly, with no ticker, over the four states: never
// streamed, recently streamed (healthy), silent past the threshold (stall), and a
// second check in the same gap (already poked).
func TestWatchdogRecoverIfStalled(t *testing.T) {
	t.Parallel()
	acks := make(chan int64, ackBufferDepth)
	e := &ChromiumEngine{acks: acks}
	now := time.Now()

	// Never streamed: no recovery.
	if e.watchdogRecoverIfStalled(now) {
		t.Fatal("re-acked before any frame streamed, want no recovery")
	}

	// A frame arrives, then a check inside the threshold: healthy, no recovery.
	e.markFrameReceived(now, 42)
	if e.watchdogRecoverIfStalled(now.Add(screencastStallThreshold - time.Millisecond)) {
		t.Fatal("re-acked a healthy stream inside the stall threshold, want no recovery")
	}

	// Silent past the threshold: recover once, re-acking the last SessionID.
	if !e.watchdogRecoverIfStalled(now.Add(screencastStallThreshold + time.Second)) {
		t.Fatal("did not re-ack a stalled stream past the threshold")
	}
	select {
	case got := <-acks:
		if got != 42 {
			t.Fatalf("watchdog re-acked SessionID %d, want the last frame's 42", got)
		}
	default:
		t.Fatal("watchdog reported recovery but enqueued no ack")
	}

	// A second check in the same silent gap does not re-ack again.
	if e.watchdogRecoverIfStalled(now.Add(screencastStallThreshold + 2*time.Second)) {
		t.Fatal("re-acked twice in one silent gap, want at most once per gap")
	}

	// A fresh frame clears the gap, so a later stall re-couples again.
	later := now.Add(time.Minute)
	e.markFrameReceived(later, 43)
	if !e.watchdogRecoverIfStalled(later.Add(screencastStallThreshold + time.Second)) {
		t.Fatal("did not re-ack after a fresh frame re-armed the watchdog")
	}
}
