package engine

import (
	"context"
	"fmt"
	"image"
	"sync"

	"github.com/chromedp/cdproto/domsnapshot"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"github.com/wimpysworld/nyala/internal/render"
)

// pendingFrame is one decoded screencast frame waiting in the latest-wins slot,
// paired with the ack id Chrome expects so the frame can be acked on consume or
// on replace.
type pendingFrame struct {
	img  *image.RGBA                   // decoded bitmap in RGBA channel order
	meta *page.ScreencastFrameMetadata // scroll offset and page scale for the sync
	ack  int64                         // SessionID to pass to page.ScreencastFrameAck
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

// startScreencast starts a JPEG screencast capped at the engine's active capture
// size and registers a ListenTarget handler that decodes each EventScreencastFrame
// and feeds it into the slot, acking displaced frames as they are replaced. The
// stream couples to the consume rate because Chrome withholds frames until acked.
func (e *ChromiumEngine) startScreencast() error {
	e.mu.Lock()
	ctx := e.ctx
	slot := e.slot
	capW, capH := e.captureW, e.captureH
	e.mu.Unlock()
	chromedp.ListenTarget(ctx, func(ev any) {
		f, ok := ev.(*page.EventScreencastFrame)
		if !ok {
			return
		}
		img, err := decodeScreencastImage(f.Data)
		if err != nil {
			// A corrupt frame is acked and dropped so the stream does not stall.
			_ = chromedp.Run(ctx, page.ScreencastFrameAck(f.SessionID))
			return
		}
		slot.put(pendingFrame{img: img, meta: f.Metadata, ack: f.SessionID}, e.ackFrame)
	})
	return chromedp.Run(ctx,
		page.StartScreencast().
			WithFormat(page.ScreencastFormatJpeg).
			WithQuality(screencastQuality).
			WithMaxWidth(int64(capW)).
			WithMaxHeight(int64(capH)),
	)
}

// ackFrame acks one received screencast frame on the live context. It is the
// production ackFunc the slot calls on consume and on replace.
func (e *ChromiumEngine) ackFrame(ack int64) {
	e.mu.Lock()
	ctx := e.ctx
	e.mu.Unlock()
	if ctx == nil {
		return
	}
	_ = chromedp.Run(ctx, page.ScreencastFrameAck(ack))
}

// runFrameLoop consumes frames from the slot until the context is cancelled or
// the slot is drained, builds a synced render.Frame for each, and pushes it on
// the frames channel. It runs in its own goroutine started per session; a crash
// or Close drains the slot and cancels the context to stop it. It does not close
// frames: the channel outlives one session so consumers keep reading across a
// crash recovery, and only the supervisor closes it on Close. The send selects on
// the context so the goroutine cannot leak when the consumer stops.
func (e *ChromiumEngine) runFrameLoop() {
	e.mu.Lock()
	ctx := e.ctx
	slot := e.slot
	e.mu.Unlock()
	for {
		pending, ok := slot.consume(ctx, e.ackFrame)
		if !ok {
			return
		}
		frame := e.composeFrame(pending)
		select {
		case e.frames <- frame:
		case <-ctx.Done():
			return
		}
	}
}

// composeFrame turns one consumed screencast bitmap into a synced render.Frame.
// It captures the DOM snapshot synchronously so the text reflects the page state
// of the frame in hand, decodes the inline text boxes to glyph runs, and folds
// the synced runs onto Frame.Text. The sync seam routes the bitmap and snapshot
// scroll offsets through the pure syncDecision: within tolerance it folds the
// fresh runs on, on divergence it holds the last-matched text, and on a snapshot
// read error it also holds the last-matched text so a frame is never emitted with
// stale-but-wrong text.
func (e *ChromiumEngine) composeFrame(pending pendingFrame) render.Frame {
	return render.Frame{Img: pending.img, Text: e.syncedRuns(pending.meta)}
}

// syncedRuns runs one captureSnapshot, decodes its inline text boxes to glyph
// runs at the resolved cell px and the bitmap's scroll offset, then routes the
// bitmap and snapshot scroll offsets through syncDecision to decide whether to
// fold the fresh runs or hold the last-matched set. It records the emitted set as
// the last-matched. On a snapshot error it holds the last-matched runs so the
// overlay holds rather than flickers.
func (e *ChromiumEngine) syncedRuns(meta *page.ScreencastFrameMetadata) []render.GlyphRun {
	e.mu.Lock()
	ctx := e.ctx
	cellW, cellH := e.snapshotCellPx()
	e.mu.Unlock()
	documents, strs, err := captureSnapshot(ctx)
	if err != nil {
		emit := errorHoldRuns(err, nil, e.lastText)
		e.lastText = emit
		return emit
	}
	bitmap := bitmapScrollOffset(meta)
	// deviceScaleFactor is pinned to 1 (DSF 1 plus a small viewport), so capture
	// pixels equal CSS pixels and the DPR divide inside decodeGlyphRuns is a no-op
	// in production. The pure decode stays general so tests can pass other ratios.
	decoded := decodeGlyphRuns(documents, strs, deviceScaleFactor, bitmap.X, bitmap.Y, cellW, cellH)
	emit, lastMatched := syncDecision(bitmap, snapshotScrollOffset(documents), metaPageScaleFactor(meta), decoded, e.lastText)
	e.lastText = lastMatched
	return emit
}

// bitmapScrollOffset reads the bitmap's CSS-pixel scroll offset from the
// screencast frame metadata, or the zero offset when the metadata is absent.
func bitmapScrollOffset(meta *page.ScreencastFrameMetadata) scrollOffset {
	if meta == nil {
		return scrollOffset{}
	}
	return scrollOffset{X: meta.ScrollOffsetX, Y: meta.ScrollOffsetY}
}

// metaPageScaleFactor reads the page scale factor from the screencast frame
// metadata, or 0 when the metadata is absent; syncDecision treats 0 as no skew.
func metaPageScaleFactor(meta *page.ScreencastFrameMetadata) float64 {
	if meta == nil {
		return 0
	}
	return meta.PageScaleFactor
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
// engine's recomputed capture size. The viewer calls it after a resize or a
// backend switch, so frames arrive at the new grid rather than the old one. A
// stop error is ignored because the next start re-establishes the stream.
func (e *ChromiumEngine) restartStream() error {
	e.mu.Lock()
	ctx := e.ctx
	capW, capH := e.captureW, e.captureH
	e.mu.Unlock()
	if ctx == nil {
		return nil
	}
	_ = chromedp.Run(ctx, page.StopScreencast())
	if err := applyDeviceMetrics(ctx, capW, capH); err != nil {
		return fmt.Errorf("engine: resize viewport: %w", err)
	}
	return e.startScreencast()
}
