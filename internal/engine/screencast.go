package engine

import (
	"context"
	"fmt"
	"image"
	"math"
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

// readyC returns the buffered wake channel a put signals, so a caller that must
// also select on another source (the re-emit poke) can park on the slot's wake
// alongside it rather than block in consume. A receive on it is only a hint that a
// frame may be waiting; the caller takes it with tryConsume, which re-checks under
// the lock.
func (s *frameSlot) readyC() <-chan struct{} {
	return s.ready
}

// tryConsume takes the held frame without blocking, acking it via ack, and returns
// ok=true when one was present. It returns ok=false on an empty or drained slot, so
// a caller woken by readyC that finds nothing waiting (a spurious or drained wake)
// loops back to its select rather than parking. It mirrors consume's take-and-ack
// under the lock, so the ack accounting stays exactly once per frame.
func (s *frameSlot) tryConsume(ack ackFunc) (pendingFrame, bool) {
	s.mu.Lock()
	if !s.full {
		s.mu.Unlock()
		return pendingFrame{}, false
	}
	frame := s.held
	s.held = pendingFrame{}
	s.full = false
	s.mu.Unlock()
	ack(frame.ack)
	return frame, true
}

// drained reports whether the slot has been drained, so a caller woken by readyC
// that found no frame can tell a teardown wake (exit) from a spurious wake (re-park).
func (s *frameSlot) drained() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
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
// iterations. It holds the last-matched runs, the wall-clock start of the current
// snapshot-failure run, and the wall-clock time of the last debounced re-snapshot.
// The snapshot goroutine owns it, so the hold is single-goroutine and lock-free;
// the snapshot read runs outside any lock.
type overlayState struct {
	lastText       []render.GlyphRun // last text matched to a bitmap
	lastScroll     scrollOffset      // DOM scroll offset lastText was decoded at, for the error-hold tag
	failingSince   time.Time         // start of the current snapshot-failure run, zero when not failing
	lastReSnap     time.Time         // wall-clock time of the last networkAlmostIdle re-snapshot, zero before the first
	lastScrollSnap time.Time         // wall-clock time of the last mid-scroll re-snapshot, zero before the first
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
	// scrollX/scrollY tag the runs with the DOM scroll offset (CSS pixels) they
	// were decoded at, so composeFrame can shift them to the bitmap's own scroll and
	// keep the overlay glued to the page while the two scrolls differ.
	scrollX float64
	scrollY float64
}

// store publishes the latest decoded runs, snapshot time, and the DOM scroll
// offset the runs were decoded at.
func (t *textSync) store(runs []render.GlyphRun, snapshotMS, scrollX, scrollY float64) {
	t.mu.Lock()
	t.runs = runs
	t.snapshotMS = snapshotMS
	t.scrollX = scrollX
	t.scrollY = scrollY
	t.mu.Unlock()
}

// latest returns the most recently published runs, snapshot time, and the DOM
// scroll offset the runs were decoded at, or nil and zeroes before the first
// snapshot lands.
func (t *textSync) latest() ([]render.GlyphRun, float64, float64, float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.runs, t.snapshotMS, t.scrollX, t.scrollY
}

// noSnapshotBackoff is the idle pause the snapshot goroutine takes between
// iterations when the active backend takes no overlay (the graphics backends), so
// it does not spin while there is no text to decode.
const noSnapshotBackoff = 50 * time.Millisecond

// reSnapshotMinInterval caps the debounced networkAlmostIdle re-snapshot to at
// most one extra snapshot per second per session. A late reflow that lands inside
// this window keeps the navigation and resize clocked text rather than forcing a
// fresh layout read, so a chatty page cannot drive the snapshot goroutine.
const reSnapshotMinInterval = time.Second

// scrollCadence is the minimum gap between mid-scroll re-snapshots, the leading
// throttle on the scroll trigger. During a continuous scroll the overlay
// re-snapshots at most once per scrollCadence (about 6-7 per second at 150 ms), so
// the crisp text tracks the page as it scrolls rather than freezing until the
// scroll settles. 150 ms balances overlay tracking against the per-snapshot cost: a
// captureSnapshot forces a synchronous layout read, so a tighter cadence would tax
// the page during a fast scroll for little extra legibility, and a looser one would
// let the text lag the bitmap visibly. The trailing holdStalenessLimit debounce
// lands one final snapshot on settle, so the overlay always ends on the resting
// scroll position regardless of the cadence phase.
const scrollCadence = 150 * time.Millisecond

// runTextSync runs the DOM snapshot loop on its own goroutine, off the frame-emit
// goroutine, so snapshot latency never stalls frame emission or withholds acks. It
// is clocked by the page, not by frame delivery: it takes the first snapshot when
// the painted latch closes (firstContentfulPaint), then re-snapshots only on a real
// trigger. So a static page that produces no further frames still publishes its
// text once after paint, and the frame loop folds the published runs onto every
// frame as sticky state. The re-snapshot triggers arrive on three channels: reSnap
// fires an immediate re-snapshot on resize or backend switch (restartStream),
// reflow carries the networkAlmostIdle late-reflow pokes, which this loop debounces
// to one trailing snapshot per holdStalenessLimit quiet window and caps to one per
// reSnapshotMinInterval, and scroll carries the scroll-offset-change pokes the frame
// loop sends, which this loop handles as a leading throttle plus a trailing
// debounce: each poke re-snapshots at once when at least scrollCadence has elapsed
// since the last scroll snapshot, so the overlay tracks the page during a continuous
// scroll (about 6-7 snapshots per second at 150 ms), and every poke also re-arms the
// holdStalenessLimit trailing debounce so one final snapshot lands on settle. The
// scroll path is not capped by the 1s reflow cap; scrollCadence is its only throttle.
// Each snapshot decodes the
// runs at the snapshot's own scroll, routes them through the bounded holdDecision,
// and publishes the emitted set to ts, then pokes reEmit so the frame loop re-paints
// its held bitmap with the new text on a static page that delivers no new frame. It
// owns the overlayState, so the hold and the re-snapshot timestamp are
// single-goroutine and lock-free. It runs only the block path's snapshot; on a
// graphics backend it publishes nil and backs off, since those backends ignore the
// overlay. It exits when ctx is cancelled (session teardown).
func (e *ChromiumEngine) runTextSync(ctx context.Context, ts *textSync, reSnap, reflow, scroll <-chan struct{}, reEmit chan<- struct{}) {
	if ctx == nil || ts == nil {
		return
	}
	snapshot := e.snapshot
	if snapshot == nil {
		snapshot = e.snapshotRuns
	}
	var state overlayState
	// lastCalCellW/H track the (cellW, cellH) pair the last calibration corrected
	// for. The reSnap arm skips recalibration when the pair is unchanged, so a
	// non-resize poke does not re-measure. Goroutine-local like state: the snapshot
	// goroutine owns it, so the guard is lock-free.
	var lastCalCellW, lastCalCellH float64
	if !e.waitForFirstSnapshot(ctx, ts) {
		return
	}
	// Reconcile font forcing to the active backend before the first calibration. On
	// the block path this is the no-op the launch injection already covered; on a
	// graphics-to-block Tab it registers the script and injects the live style.
	// waitForFirstSnapshot only returns for the block backend, so a graphics start
	// never reaches here.
	e.syncFontForcing(ctx)
	// Calibrate before the first publish so the first overlay maps against the
	// corrected font metrics. Program order on this single goroutine is the whole
	// ordering contract: no channel, no sync.Once, no new goroutine.
	e.calibrateFonts(ctx)
	e.mu.Lock()
	lastCalCellW, lastCalCellH = e.snapshotCellPx()
	e.mu.Unlock()
	// First snapshot on the painted latch: the navigation trigger, single-shot per
	// session, and the new reader of the painted latch replacing firstSnapshotAfterPaint.
	state = e.publishSnapshot(ctx, snapshot, ts, state)
	pokeReEmit(reEmit)
	// Signal the capture driver that the first overlay runs have published, so it
	// counts a usable frame only once the text has landed rather than racing the
	// (calibration-delayed) first publish. A no-op off the capture path.
	e.markTextReady()

	// debounce arms a trailing timer per reflow quiet window; scrollDebounce does the
	// same for scroll. A nil channel means none is pending; each timer is armed on its
	// poke and cleared on fire.
	var debounce *time.Timer
	var debounceC <-chan time.Time
	var scrollDebounce *time.Timer
	var scrollDebounceC <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-reSnap:
			// Resize or backend switch: reconcile font forcing first, then re-snapshot at
			// once, no debounce, no cap. A block-to-graphics Tab removes the forcing; a
			// graphics-to-block Tab re-registers it and injects the live style. A fresh
			// enable (disabled before, enabled after) resets the calibration guard to zero
			// so the recalibrate below reruns, since snapshotCellPx is backend-independent
			// and would otherwise wrongly skip after a Tab back to block at the same grid.
			wasForcing := e.fontForcingActive()
			if e.syncFontForcing(ctx) && !wasForcing {
				lastCalCellW, lastCalCellH = 0, 0
			}
			// Recalibrate first when the cell size changed, since the seed size is
			// derived from cellW; skip when the pair is unchanged so a backend switch
			// at the same grid does not re-measure. Only the block backend forces the
			// font, so a graphics backend takes no calibration.
			e.mu.Lock()
			cellW, cellH := e.snapshotCellPx()
			e.mu.Unlock()
			if e.textOverlayBackend() && (cellW != lastCalCellW || cellH != lastCalCellH) {
				e.calibrateFonts(ctx)
				lastCalCellW, lastCalCellH = cellW, cellH
			}
			state = e.publishSnapshot(ctx, snapshot, ts, state)
			pokeReEmit(reEmit)
		case <-reflow:
			// Late reflow: arm or reset the trailing debounce timer so a burst of
			// networkAlmostIdle events collapses to one trailing snapshot.
			debounce = armDebounce(debounce, holdStalenessLimit)
			debounceC = debounce.C
		case <-debounceC:
			debounceC = nil
			state = e.reSnapshotCapped(ctx, snapshot, ts, state)
			pokeReEmit(reEmit)
		case <-scroll:
			// Scroll: leading throttle plus trailing debounce. Snapshot now when the
			// cadence allows, so the overlay tracks mid-scroll, and always re-arm the
			// trailing timer so a final snapshot lands once the scroll settles.
			state = e.scrollSnap(ctx, snapshot, ts, state, reEmit)
			scrollDebounce = armDebounce(scrollDebounce, holdStalenessLimit)
			scrollDebounceC = scrollDebounce.C
		case <-scrollDebounceC:
			scrollDebounceC = nil
			// Settle snapshot: the scroll stopped, so land the final scroll position.
			// Uncapped by the 1s reflow cap; the cadence throttled the mid-scroll fires.
			state = e.publishSnapshot(ctx, snapshot, ts, state)
			state.lastScrollSnap = time.Now()
			pokeReEmit(reEmit)
			// Signal the capture scroll hook that the text at the resting scroll position
			// has published, so it draws the scrolled frame with the settled overlay rather
			// than the stale pre-scroll text. A no-op off the capture path.
			e.pokeScrollSettle()
		}
	}
}

// waitForFirstSnapshot blocks until the active backend takes the overlay and the
// painted latch has closed, the gate the first post-paint snapshot fires on. A
// graphics backend publishes nil and backs off rather than reading layout it
// ignores; this runs before the painted gate so a non-block launch does not block
// on paint. Under capture painted closes on firstContentfulPaint; in production
// painted is nil so it proceeds at once. It returns false when ctx is cancelled, so
// the caller exits without snapshotting.
func (e *ChromiumEngine) waitForFirstSnapshot(ctx context.Context, ts *textSync) bool {
	for !e.textOverlayBackend() {
		ts.store(nil, 0, 0, 0)
		if sleepCtx(ctx, noSnapshotBackoff) {
			return false
		}
	}
	if e.painted != nil {
		select {
		case <-e.painted:
		case <-ctx.Done():
			return false
		}
	}
	return ctx.Err() == nil
}

// reSnapshotCapped runs one late-reflow re-snapshot if the rate cap allows, then
// records the time. The cap holds it to one per reSnapshotMinInterval, so a chatty
// page cannot drive the snapshot goroutine; a capped poke keeps the navigation and
// resize clocked text. It returns the updated hold state.
func (e *ChromiumEngine) reSnapshotCapped(ctx context.Context, snapshot func(context.Context, overlayState) ([]render.GlyphRun, float64, scrollOffset, overlayState), ts *textSync, state overlayState) overlayState {
	now := time.Now()
	if !state.lastReSnap.IsZero() && now.Sub(state.lastReSnap) < reSnapshotMinInterval {
		return state
	}
	state = e.publishSnapshot(ctx, snapshot, ts, state)
	state.lastReSnap = now
	return state
}

// scrollSnapDue reports whether a mid-scroll re-snapshot is due: the first scroll
// poke (lastScrollSnap zero) always fires, and a later poke fires once cadence has
// elapsed since the last scroll snapshot. It is the pure leading-throttle decision,
// factored out so it is unit-tested without a wall-clock sleep.
func scrollSnapDue(last, now time.Time, cadence time.Duration) bool {
	return last.IsZero() || now.Sub(last) >= cadence
}

// scrollSnap runs one mid-scroll re-snapshot when the leading throttle allows, then
// records the time, so a continuous scroll re-snapshots at most once per
// scrollCadence. A throttled poke leaves the state unchanged; the trailing debounce
// still lands the settle snapshot. It returns the updated hold state and pokes
// reEmit on a fire so the held frame repaints with the new scroll's text.
func (e *ChromiumEngine) scrollSnap(ctx context.Context, snapshot func(context.Context, overlayState) ([]render.GlyphRun, float64, scrollOffset, overlayState), ts *textSync, state overlayState, reEmit chan<- struct{}) overlayState {
	now := time.Now()
	if !scrollSnapDue(state.lastScrollSnap, now, scrollCadence) {
		return state
	}
	state = e.publishSnapshot(ctx, snapshot, ts, state)
	state.lastScrollSnap = now
	pokeReEmit(reEmit)
	return state
}

// pokeReEmit signals the frame loop to re-push its held bitmap recomposed with the
// freshly published text. The send is latest-wins on a capacity-1 channel, so it
// never blocks and a burst of publishes collapses to one re-emit. A nil channel (no
// session) drops the poke.
func pokeReEmit(reEmit chan<- struct{}) {
	if reEmit == nil {
		return
	}
	select {
	case reEmit <- struct{}{}:
	default:
	}
}

// armDebounce starts or resets a trailing timer to fire after d, draining a stale
// fire so a reset never leaves a spurious tick on the channel. It returns the live
// timer so the caller threads it across iterations.
func armDebounce(t *time.Timer, d time.Duration) *time.Timer {
	if t == nil {
		return time.NewTimer(d)
	}
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
	return t
}

// publishSnapshot runs one snapshot and publishes its runs and time to ts. It
// returns the updated hold state. It is the single snapshot-and-publish step the
// first-after-paint and re-snapshot triggers share, so the publish stays in one
// place. A graphics backend takes no overlay, so it publishes nil and skips the
// layout read rather than snapshot the full viewport it ignores.
func (e *ChromiumEngine) publishSnapshot(ctx context.Context, snapshot func(context.Context, overlayState) ([]render.GlyphRun, float64, scrollOffset, overlayState), ts *textSync, state overlayState) overlayState {
	if !e.textOverlayBackend() {
		ts.store(nil, 0, 0, 0)
		return state
	}
	runs, snapshotMS, scroll, next := snapshot(ctx, state)
	// snapshotRuns rebuilds the hold fields and drops the cadence timestamps, so carry
	// them forward here: the re-snapshot caps live in runTextSync, not the snapshot.
	next.lastReSnap = state.lastReSnap
	next.lastScrollSnap = state.lastScrollSnap
	// Tag the published runs with the DOM scroll they were decoded at, so
	// composeFrame can shift them to the bitmap's own scroll.
	ts.store(runs, snapshotMS, scroll.X, scroll.Y)
	return next
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
// runs on a separate goroutine (runTextSync) clocked by the page, not by frame
// delivery, so this loop reads the latest published overlay runs as sticky state
// and folds them onto every frame without ever poking the snapshot or blocking on
// a snapshot read. It also parks on the re-emit poke the snapshot goroutine sends
// after each publish: on a poke it re-pushes its last consumed bitmap recomposed
// with the freshly published text, so a static page that delivers no new screencast
// frame still repaints with text. The re-emit reuses the held bitmap and never
// re-feeds the slot, so the slot's exactly-once ack accounting is untouched. On
// each consumed frame it compares the frame's scroll offset against the last seen
// one and pokes the scroll channel on any change, so the snapshot goroutine
// re-snapshots the text at the new scroll once a scroll settles. The latest-wins
// slot may drop intermediate frames during a fast scroll, but a scroll always
// delivers a final settled frame, so the offset change is seen and the overlay
// catches up. It does not close frames: the channel outlives one session so
// consumers keep reading across a crash recovery, and only the supervisor closes
// it on Close. The send selects on the context so the goroutine cannot leak when
// the consumer stops.
func (e *ChromiumEngine) runFrameLoop(ctx context.Context, slot *frameSlot, frames chan render.Frame, ts *textSync, scroll chan<- struct{}, reEmit <-chan struct{}) {
	if ctx == nil || slot == nil || frames == nil {
		return
	}
	var held pendingFrame
	var haveHeld bool
	// lastScroll tracks the scroll offset of the last consumed frame so a change
	// pokes a re-snapshot. It lives on this goroutine, so the tracking is lock-free.
	var lastScroll scrollOffset
	var haveScroll bool
	for {
		select {
		case <-ctx.Done():
			return
		case <-reEmit:
			// Text published with no new frame in flight: re-push the held bitmap so
			// the viewer repaints with the fresh text. Before the first frame there is
			// nothing to re-push, so the poke is a no-op; the first frame carries it.
			if !haveHeld {
				continue
			}
			if !e.emitFrame(ctx, frames, ts, held) {
				return
			}
		case <-slot.readyC():
			pending, ok := slot.tryConsume(e.ackFrame)
			if !ok {
				// A drained slot wakes readyC with no frame; exit on drain, otherwise the
				// wake was spurious and the select re-parks.
				if slot.drained() {
					return
				}
				continue
			}
			held, haveHeld = pending, true
			if off, ok := frameScrollOffset(pending); ok {
				if !haveScroll || off != lastScroll {
					if haveScroll {
						pokeScroll(scroll)
					}
					lastScroll, haveScroll = off, true
				}
			}
			if !e.emitFrame(ctx, frames, ts, pending) {
				return
			}
		}
	}
}

// frameScrollOffset reads the page scroll offset from a consumed frame's
// screencast metadata in CSS pixels, returning ok=false when the frame carries no
// metadata. The listener sets meta on every frame, so ok is true in production;
// the false branch only guards a metadata-less test frame.
func frameScrollOffset(p pendingFrame) (scrollOffset, bool) {
	if p.meta == nil {
		return scrollOffset{}, false
	}
	return scrollOffset{X: p.meta.ScrollOffsetX, Y: p.meta.ScrollOffsetY}, true
}

// pokeScroll signals the snapshot goroutine that the page scroll offset changed,
// so it re-snapshots the text at the new scroll once the scroll settles. The send
// is latest-wins on a capacity-1 channel, so a burst of scroll frames collapses to
// one wake and the frame loop never blocks. A nil channel drops the poke.
func pokeScroll(scroll chan<- struct{}) {
	if scroll == nil {
		return
	}
	select {
	case scroll <- struct{}{}:
	default:
	}
}

// emitFrame composes one pending bitmap with the latest published text and pushes
// it on the frames channel, selecting on ctx so the send cannot leak when the
// consumer stops. It returns false when ctx is cancelled mid-send, so the caller
// exits the loop.
func (e *ChromiumEngine) emitFrame(ctx context.Context, frames chan render.Frame, ts *textSync, pending pendingFrame) bool {
	frame := e.composeFrame(pending, ts)
	select {
	case frames <- frame:
		return true
	case <-ctx.Done():
		return false
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
	var snapshotMS, snapScrollX, snapScrollY float64
	if ts != nil {
		runs, snapshotMS, snapScrollX, snapScrollY = ts.latest()
	}
	e.mu.Lock()
	cellCSSW, cellCSSH := e.snapshotCellPx()
	scale := e.layoutScale()
	e.mu.Unlock()
	// Shift the published runs onto the bitmap's own scroll, so the overlay stays
	// glued to the page when the snapshot and the bitmap scrolls differ.
	runs = shiftRunsToBitmapScroll(runs, pending.meta, snapScrollX, snapScrollY, cellCSSW, cellCSSH)
	frame := render.Frame{Img: pending.img, Text: runs}
	if e.captureOn {
		frame.Stats = &render.FrameStats{
			ScreencastPayloadBytes: int64(pending.bytes),
			ScreencastDecodeMS:     pending.decodeMS,
			DOMSnapshotMS:          snapshotMS,
			LayoutScale:            scale,
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

// shiftRunsToBitmapScroll compensates the compose-time skew between the runs and
// the bitmap. The runs were decoded at the DOM scroll of snapshot time
// (snapScrollX/Y, CSS pixels); the bitmap carries its own scroll in the screencast
// metadata. When the two differ, every composed frame is skewed by the difference,
// so this shifts the runs onto the bitmap's scroll. A run's box Y is the on-screen
// top with the snapshot scroll already subtracted, so a bitmap scrolled dy past the
// snapshot moves the run up by dy: the box shifts by -dy and the cell row by
// -round(dy/cellCSSH), keeping the row and the box in step. The horizontal axis
// shifts the same way by dx. cellCSSW/H are the CSS-pixel cell dimensions the runs
// were snapped with. It returns the runs untouched (no copy) when there is no delta,
// the production fast path. Otherwise it returns a shifted copy and never mutates
// runs, which textSync shares across composes, so a shift never accumulates. Runs
// shifted off the grid are dropped by applyOverlay's bounds guards.
func shiftRunsToBitmapScroll(runs []render.GlyphRun, meta *page.ScreencastFrameMetadata, snapScrollX, snapScrollY, cellCSSW, cellCSSH float64) []render.GlyphRun {
	if len(runs) == 0 || meta == nil {
		return runs
	}
	dx := meta.ScrollOffsetX - snapScrollX
	dy := meta.ScrollOffsetY - snapScrollY
	if dx == 0 && dy == 0 {
		return runs
	}
	var dCol, dRow int
	if cellCSSW > 0 {
		dCol = int(math.Round(dx / cellCSSW))
	}
	if cellCSSH > 0 {
		dRow = int(math.Round(dy / cellCSSH))
	}
	shifted := make([]render.GlyphRun, len(runs))
	for i, run := range runs {
		run.Col -= dCol
		run.Row -= dRow
		run.X -= dx
		run.Y -= dy
		shifted[i] = run
	}
	return shifted
}

// snapshotRuns runs one captureSnapshot, decodes its inline text boxes to glyph
// runs at the resolved cell px and the snapshot's own scroll offset, then routes
// the result through the bounded holdDecision: a successful read emits the fresh
// runs, a failing read holds the last-matched runs for at most holdStalenessLimit
// before dropping to nil. It returns the emitted runs, the snapshot time, and the
// updated state. It runs on the snapshot goroutine; the snapshot chromedp.Run
// stays outside any lock.
func (e *ChromiumEngine) snapshotRuns(ctx context.Context, state overlayState) ([]render.GlyphRun, float64, scrollOffset, overlayState) {
	if ctx == nil {
		emit, last, failing := holdDecision(context.Canceled, nil, state.lastText, state.failingSince, time.Now())
		return emit, 0, state.lastScroll, overlayState{lastText: last, lastScroll: state.lastScroll, failingSince: failing}
	}
	e.mu.Lock()
	cellW, cellH := e.snapshotCellPx()
	cols := e.cols
	dpr := e.layoutScale()
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
	var scroll scrollOffset
	if err == nil {
		// Place runs by the snapshot's own scroll offset, not the bitmap's: the
		// snapshot bounds and its scroll come from one DOM read, so they are
		// internally consistent and a sustained scroll shows the current text.
		// The decode divides the device-pixel box bounds by the layout scale Z to
		// reach CSS pixels: at Z == 1 (a grid at or under the target width) the divide
		// is a no-op, and under Z > 1 the block backend launches at DSF Z so the bounds
		// are Z device pixels per CSS pixel. The pure decode stays general so tests can
		// pass other ratios.
		scroll = snapshotScrollOffset(documents)
		decoded = decodeGlyphRuns(documents, strs, dpr, scroll.X, scroll.Y, cellW, cellH, cols)
	}
	emit, last, failing := holdDecision(err, decoded, state.lastText, state.failingSince, time.Now())
	// Tag the emitted runs with the scroll they were decoded at: a fresh success
	// carries the new snapshot scroll, an error hold re-emits the last-matched runs
	// so it carries the scroll those were decoded at (state.lastScroll). The runs and
	// their scroll travel together so composeFrame shifts the right amount.
	emitScroll := scroll
	if err != nil {
		emitScroll = state.lastScroll
	}
	return emit, snapshotMS, emitScroll, overlayState{lastText: last, lastScroll: emitScroll, failingSince: failing}
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
	reSnap := e.reSnap
	surfW, surfH := e.surfaceSize(e.cols, e.rows)
	z := e.layoutScale()
	e.mu.Unlock()
	if ctx == nil || slot == nil {
		return nil
	}
	if e.recorder != nil {
		e.recorder.Record(captureEvent{Type: captureEventScreencastControl, Action: "restart"})
	}
	// Poke the snapshot goroutine for an immediate re-snapshot: a resize or backend
	// switch changes the layout viewport, so the prior text no longer maps to the
	// new grid. The send is latest-wins so it never blocks. A graphics backend
	// publishes nil through the same poke.
	if reSnap != nil {
		select {
		case reSnap <- struct{}{}:
		default:
		}
	}
	_ = chromedp.Run(ctx, page.StopScreencast())
	if err := applySizing(ctx, windowID, surfW, surfH, z); err != nil {
		return fmt.Errorf("engine: resize viewport: %w", err)
	}
	return e.startScreencast(ctx, slot, surfW, surfH)
}
