package engine

import (
	"context"
	"fmt"
	"image"
	"sync"
	"time"

	"github.com/chromedp/cdproto/domsnapshot"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"github.com/wimpysworld/nyala/internal/render"
)

// msSince returns the milliseconds elapsed since start as a wall-clock duration.
// It is never negative because time.Since uses the monotonic clock. The capture
// phase timing uses it so the stats carry float milliseconds.
func msSince(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000
}

// pendingFrame is one decoded screencast frame waiting in the latest-wins slot,
// paired with the ack id Chrome expects so the frame can be acked on consume or
// on replace.
type pendingFrame struct {
	img      *image.RGBA                   // decoded bitmap in RGBA channel order
	meta     *page.ScreencastFrameMetadata // scroll offset and page scale for the sync
	ack      int64                         // SessionID to pass to page.ScreencastFrameAck
	bytes    int                           // screencast payload byte size, for capture stats
	decodeMS float64                       // wall-clock decode time in ms, set under capture only
}

// ackFunc acks one received screencast frame by its SessionID. The live loop
// passes a chromedp-backed ack; the unit test passes a fake that records the id,
// so the slot accounting is exercised without a browser.
type ackFunc func(ack int64)

// frameSlot is the latest-wins single-frame backpressure slot. It holds at most
// one unconsumed frame. A newly arrived frame replaces any unconsumed one, and
// the displaced frame is acked as it is dropped; the consumer acks the frame it
// takes. So every frame put into the slot is acked exactly once, on consume or
// on replace, and Chrome's production rate couples to the consume rate. The slot
// is the pure, browser-free core of the screencast loop: feed it frames with put
// and drain it with consume, both taking an ackFunc, and the ack count is
// assertable in a unit test (see screencast_test.go).
type frameSlot struct {
	mu      sync.Mutex
	held    pendingFrame
	full    bool
	ready   chan struct{} // signals consume that a frame is waiting; buffered size 1
	dropped bool          // set by drain so a late put acks instead of storing
}

// newFrameSlot returns an empty slot ready to accept frames.
func newFrameSlot() *frameSlot {
	return &frameSlot{ready: make(chan struct{}, 1)}
}

// put places frame in the slot. If the slot already holds an unconsumed frame,
// that displaced frame is acked via ack before the new one takes its place; the
// new frame stays unacked until it is consumed or itself displaced. After drain,
// put acks the frame immediately and stores nothing, so no goroutine blocks on a
// slot that will never be consumed.
func (s *frameSlot) put(frame pendingFrame, ack ackFunc) {
	s.mu.Lock()
	if s.dropped {
		s.mu.Unlock()
		ack(frame.ack)
		return
	}
	if s.full {
		displaced := s.held
		s.held = frame
		s.mu.Unlock()
		ack(displaced.ack)
	} else {
		s.held = frame
		s.full = true
		s.mu.Unlock()
	}
	select {
	case s.ready <- struct{}{}:
	default:
	}
}

// consume blocks until a frame is waiting or ctx is cancelled, then takes the
// held frame, acks it via ack, and returns it with ok=true. It returns ok=false
// when ctx is cancelled or the slot is drained with no frame waiting.
func (s *frameSlot) consume(ctx context.Context, ack ackFunc) (pendingFrame, bool) {
	for {
		s.mu.Lock()
		if s.full {
			frame := s.held
			s.held = pendingFrame{}
			s.full = false
			s.mu.Unlock()
			ack(frame.ack)
			return frame, true
		}
		dropped := s.dropped
		s.mu.Unlock()
		if dropped {
			return pendingFrame{}, false
		}
		select {
		case <-s.ready:
		case <-ctx.Done():
			return pendingFrame{}, false
		}
	}
}

// drain marks the slot dropped and acks any frame still held, so Close leaves no
// frame unacked and unblocks a consumer parked on ready. After drain, put acks
// and discards, and consume returns ok=false.
func (s *frameSlot) drain(ack ackFunc) {
	s.mu.Lock()
	s.dropped = true
	held, full := s.held, s.full
	s.held, s.full = pendingFrame{}, false
	s.mu.Unlock()
	if full {
		ack(held.ack)
	}
	select {
	case s.ready <- struct{}{}:
	default:
	}
}

// handleScreencastFrame decodes one screencast frame and feeds it into the slot,
// acking and dropping a corrupt frame so the stream does not stall. It is the
// screencast-frame arm of the listener type switch. The recorder carries metadata
// only, never the frame image bytes, so the event stream holds no payload.
func (e *ChromiumEngine) handleScreencastFrame(slot *frameSlot, f *page.EventScreencastFrame) {
	if e.recorder != nil {
		e.recorder.Record(captureEvent{
			Type:      captureEventScreencastFrame,
			Meta:      f.Metadata,
			SessionID: f.SessionID,
		})
	}
	// Time the decode only under capture; off the capture path the wall-clock read
	// is skipped and decodeMS stays zero.
	start := time.Now()
	img, err := decodeScreencastImage(f.Data)
	if err != nil {
		// A corrupt frame is acked and dropped so the stream does not stall. The
		// ack enqueues onto the pump rather than running here, because this is the
		// ListenTarget event goroutine and a chromedp.Run on it is the #920 deadlock.
		e.ackFrame(f.SessionID)
		return
	}
	pending := pendingFrame{img: img, meta: f.Metadata, ack: f.SessionID}
	if e.captureOn {
		pending.bytes = len(f.Data)
		pending.decodeMS = msSince(start)
	}
	slot.put(pending, e.ackFrame)
}

// startScreencast starts a JPEG screencast capped at the engine's active capture
// size and registers a ListenTarget handler that decodes each EventScreencastFrame
// and feeds it into the slot, acking displaced frames as they are replaced. The
// stream couples to the consume rate because Chrome withholds frames until acked.
// The capture paint latch is no longer fired here: the navigate waiter's matched
// load signal closes it (markPainted from navigateAsync), so this listener handles
// only screencast frames.
func (e *ChromiumEngine) startScreencast(ctx context.Context, slot *frameSlot, capW, capH int) error {
	if ctx == nil {
		return fmt.Errorf("engine: start screencast: no browser context")
	}
	if slot == nil {
		return fmt.Errorf("engine: start screencast: no frame slot")
	}
	chromedp.ListenTarget(ctx, func(ev any) {
		if f, ok := ev.(*page.EventScreencastFrame); ok {
			e.handleScreencastFrame(slot, f)
		}
	})
	if e.recorder != nil {
		e.recorder.Record(captureEvent{Type: captureEventScreencastControl, Action: "start"})
	}
	return chromedp.Run(ctx,
		page.StartScreencast().
			WithFormat(page.ScreencastFormatJpeg).
			WithQuality(screencastQuality).
			WithMaxWidth(int64(capW)).
			WithMaxHeight(int64(capH)),
	)
}

// ackFrame is the production ackFunc the slot calls on consume and on replace,
// and the listener calls for a corrupt frame. It enqueues the SessionID onto the
// pump channel instead of running page.ScreencastFrameAck inline: the slot's
// displaced-frame ack and the corrupt-frame ack both run on the ListenTarget
// event goroutine, and a chromedp.Run there is the chromedp #920 self-deadlock.
// The enqueue is select-with-default so a full channel or a torn-down session (a
// nil acks channel) drops the ack rather than parking the caller. A dropped ack
// only delays one frame's credit; the next consumed frame's ack re-couples the
// stream.
func (e *ChromiumEngine) ackFrame(ack int64) {
	e.mu.Lock()
	acks := e.acks
	e.mu.Unlock()
	if e.recorder != nil {
		e.recorder.Record(captureEvent{Type: captureEventFrameAck, SessionID: ack})
	}
	if acks == nil {
		return
	}
	select {
	case acks <- ack:
	default:
	}
}

// ackPump drains the ack channel and runs page.ScreencastFrameAck on the live
// context, off the ListenTarget event goroutine. It is the chromedp #920 fix:
// the listener enqueues SessionID values and this single goroutine runs the
// chromedp.Run that would deadlock if it ran on the event goroutine. It exits
// when ctx is cancelled (session teardown), so it never leaks past a session.
func (e *ChromiumEngine) ackPump(ctx context.Context, acks <-chan int64) {
	if ctx == nil || acks == nil {
		return
	}
	for {
		select {
		case ack := <-acks:
			_ = chromedp.Run(ctx, page.ScreencastFrameAck(ack))
		case <-ctx.Done():
			return
		}
	}
}

// overlayState is the text-sync state the snapshot goroutine threads across its
// iterations. It holds the last-matched runs and the wall-clock start of the
// current snapshot-failure run. The snapshot goroutine owns it, so the hold is
// single-goroutine and lock-free; the snapshot read runs outside any lock.
type overlayState struct {
	lastText     []render.GlyphRun // last text matched to a bitmap
	failingSince time.Time         // start of the current snapshot-failure run, zero when not failing
}

// textSync publishes the latest decoded overlay runs from the snapshot goroutine
// to the frame-emit goroutine. The snapshot goroutine stores the freshly decoded
// runs; composeFrame reads the latest set and folds it onto the frame. Decoupling
// the DOM snapshot from the frame loop this way keeps snapshot latency from
// withholding acks or stalling frame emission (the snapshot no longer runs on the
// frame-emit goroutine). The held runs are read-only once published, so the
// reader shares the slice without copying.
type textSync struct {
	mu         sync.Mutex
	runs       []render.GlyphRun
	snapshotMS float64
}

// store publishes the latest decoded runs and snapshot time.
func (t *textSync) store(runs []render.GlyphRun, snapshotMS float64) {
	t.mu.Lock()
	t.runs = runs
	t.snapshotMS = snapshotMS
	t.mu.Unlock()
}

// latest returns the most recently published runs and snapshot time, or nil and
// zero before the first snapshot lands.
func (t *textSync) latest() ([]render.GlyphRun, float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.runs, t.snapshotMS
}

// noSnapshotBackoff is the idle pause the snapshot goroutine takes between
// iterations when the active backend takes no overlay (the graphics backends), so
// it does not spin while there is no text to decode.
const noSnapshotBackoff = 50 * time.Millisecond

// runTextSync runs the DOM snapshot loop on its own goroutine, off the frame-emit
// goroutine, so snapshot latency never stalls frame emission or withholds acks. It
// repeatedly captures a snapshot, decodes the runs at the snapshot's own scroll,
// routes them through the bounded holdDecision, and publishes the emitted set to
// ts. It owns the overlayState, so the hold is single-goroutine and lock-free. It
// runs only the block path's snapshot; on a graphics backend it publishes nil and
// backs off, since those backends ignore the overlay. It exits when ctx is
// cancelled (session teardown).
func (e *ChromiumEngine) runTextSync(ctx context.Context, ts *textSync) {
	if ctx == nil || ts == nil {
		return
	}
	var state overlayState
	for {
		if ctx.Err() != nil {
			return
		}
		if !e.textOverlayBackend() {
			ts.store(nil, 0)
			if sleepCtx(ctx, noSnapshotBackoff) {
				return
			}
			continue
		}
		var runs []render.GlyphRun
		var snapshotMS float64
		runs, snapshotMS, state = e.snapshotRuns(ctx, state)
		ts.store(runs, snapshotMS)
		if ctx.Err() != nil {
			return
		}
	}
}

// sleepCtx sleeps for d or returns true early when ctx is cancelled, so the
// snapshot goroutine's idle back-off never outlives the session.
func sleepCtx(ctx context.Context, d time.Duration) (cancelled bool) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return false
	case <-ctx.Done():
		return true
	}
}

// runFrameLoop consumes frames from the slot until the context is cancelled or
// the slot is drained, builds a synced render.Frame for each, and pushes it on
// the frames channel. It runs in its own goroutine started per session; a crash
// or Close drains the slot and cancels the context to stop it. The DOM snapshot
// runs on a separate goroutine (runTextSync), so this loop reads the latest
// published overlay runs without ever blocking on a snapshot read. It does not
// close frames: the channel outlives one session so consumers keep reading across
// a crash recovery, and only the supervisor closes it on Close. The send selects
// on the context so the goroutine cannot leak when the consumer stops.
func (e *ChromiumEngine) runFrameLoop(ctx context.Context, slot *frameSlot, frames chan render.Frame, ts *textSync) {
	if ctx == nil || slot == nil || frames == nil {
		return
	}
	for {
		pending, ok := slot.consume(ctx, e.ackFrame)
		if !ok {
			return
		}
		frame := e.composeFrame(pending, ts)
		select {
		case frames <- frame:
		case <-ctx.Done():
			return
		}
	}
}

// composeFrame turns one consumed screencast bitmap into a synced render.Frame.
// It reads the latest overlay runs the snapshot goroutine published (textSync),
// rather than running the DOM snapshot inline, so snapshot latency never stalls
// the frame loop. The graphics backends draw the bitmap alone and ignore
// Frame.Text; runTextSync publishes nil for them, so this folds nil. The overlay
// text may lag the bitmap by a frame, which the place-by-scroll sync tolerates.
func (e *ChromiumEngine) composeFrame(pending pendingFrame, ts *textSync) render.Frame {
	var runs []render.GlyphRun
	var snapshotMS float64
	if ts != nil {
		runs, snapshotMS = ts.latest()
	}
	frame := render.Frame{Img: pending.img, Text: runs}
	if e.captureOn {
		frame.Stats = &render.FrameStats{
			ScreencastPayloadBytes: int64(pending.bytes),
			ScreencastDecodeMS:     pending.decodeMS,
			DOMSnapshotMS:          snapshotMS,
		}
		// Copy the screencast metadata onto a neutral render.ScreencastMeta so the
		// bundle records it without the render seam importing the page type. Off the
		// capture path frame.Meta stays nil, so production pays zero cost.
		if pending.meta != nil {
			frame.Meta = screencastMeta(pending.meta)
		}
	}
	return frame
}

// snapshotRuns runs one captureSnapshot, decodes its inline text boxes to glyph
// runs at the resolved cell px and the snapshot's own scroll offset, then routes
// the result through the bounded holdDecision: a successful read emits the fresh
// runs, a failing read holds the last-matched runs for at most holdStalenessLimit
// before dropping to nil. It returns the emitted runs, the snapshot time, and the
// updated state. It runs on the snapshot goroutine; the snapshot chromedp.Run
// stays outside any lock.
func (e *ChromiumEngine) snapshotRuns(ctx context.Context, state overlayState) ([]render.GlyphRun, float64, overlayState) {
	if ctx == nil {
		emit, last, failing := holdDecision(context.Canceled, nil, state.lastText, state.failingSince, time.Now())
		return emit, 0, overlayState{lastText: last, failingSince: failing}
	}
	e.mu.Lock()
	cellW, cellH := e.snapshotCellPx()
	e.mu.Unlock()
	start := time.Now()
	documents, strs, err := captureSnapshot(ctx)
	var snapshotMS float64
	if e.captureOn {
		snapshotMS = msSince(start)
	}
	if e.recorder != nil {
		e.recorder.Record(captureEvent{Type: captureEventDOMSnapshot})
	}
	var decoded []render.GlyphRun
	if err == nil {
		// Place runs by the snapshot's own scroll offset, not the bitmap's: the
		// snapshot bounds and its scroll come from one DOM read, so they are
		// internally consistent and a sustained scroll shows the current text.
		// deviceScaleFactor is pinned to 1 (DSF 1 plus a small viewport), so capture
		// pixels equal CSS pixels and the DPR divide inside decodeGlyphRuns is a no-op
		// in production. The pure decode stays general so tests can pass other ratios.
		scroll := snapshotScrollOffset(documents)
		decoded = decodeGlyphRuns(documents, strs, deviceScaleFactor, scroll.X, scroll.Y, cellW, cellH)
	}
	emit, last, failing := holdDecision(err, decoded, state.lastText, state.failingSince, time.Now())
	return emit, snapshotMS, overlayState{lastText: last, failingSince: failing}
}

// screencastMeta copies the CDP screencast frame metadata into a neutral
// render.ScreencastMeta, so the bundle records the scroll offset, page scale, and
// device size without the render seam importing the page type. The timestamp is
// the frame swap time as float64 seconds since the Unix epoch, or 0 when absent.
func screencastMeta(meta *page.ScreencastFrameMetadata) *render.ScreencastMeta {
	var ts float64
	if meta.Timestamp != nil {
		t := time.Time(*meta.Timestamp)
		ts = float64(t.UnixNano()) / float64(time.Second)
	}
	return &render.ScreencastMeta{
		OffsetTop:       meta.OffsetTop,
		PageScaleFactor: meta.PageScaleFactor,
		DeviceWidth:     meta.DeviceWidth,
		DeviceHeight:    meta.DeviceHeight,
		ScrollOffsetX:   meta.ScrollOffsetX,
		ScrollOffsetY:   meta.ScrollOffsetY,
		Timestamp:       ts,
	}
}

// snapshotScrollOffset reads the root document's CSS-pixel scroll offset from a
// DOM snapshot, or the zero offset when no document is present. The first
// document is the root, whose ScrollOffsetX/Y is the page scroll the bitmap
// offset is matched against.
func snapshotScrollOffset(documents []*domsnapshot.DocumentSnapshot) scrollOffset {
	if len(documents) == 0 || documents[0] == nil {
		return scrollOffset{}
	}
	root := documents[0]
	return scrollOffset{X: root.ScrollOffsetX, Y: root.ScrollOffsetY}
}

// restartStream stops the current screencast and starts a fresh one at the
// engine's recomputed surface size. The viewer calls it after a resize or a
// backend switch, so frames arrive at the new grid rather than the old one. It
// moves the three sizing values together: the captured surface and the layout
// viewport via applySizing, and the screencast cap via startScreencast, all at
// the real pixel surface. Moving them as one triple keeps a backend switch from
// leaving the surface pinned at the launch size with a moved cap. A stop error is
// ignored because the next start re-establishes the stream.
func (e *ChromiumEngine) restartStream() error {
	e.mu.Lock()
	ctx := e.ctx
	slot := e.slot
	windowID := e.windowID
	surfW, surfH := e.surfaceSize(e.cols, e.rows)
	e.mu.Unlock()
	if ctx == nil || slot == nil {
		return nil
	}
	if e.recorder != nil {
		e.recorder.Record(captureEvent{Type: captureEventScreencastControl, Action: "restart"})
	}
	_ = chromedp.Run(ctx, page.StopScreencast())
	if err := applySizing(ctx, windowID, surfW, surfH); err != nil {
		return fmt.Errorf("engine: resize viewport: %w", err)
	}
	return e.startScreencast(ctx, slot, surfW, surfH)
}
