package engine

import (
	"context"
	"fmt"
	"image"
	"math"
	"sync"
	"time"

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

// pendingFrame is one decoded screencast frame the worker produces off the
// listener goroutine and hands to compose. It carries no ack id: the frame was
// already acked on receive (handleScreencastFrame), before decode.
type pendingFrame struct {
	img      *image.RGBA                   // decoded bitmap in RGBA channel order
	meta     *page.ScreencastFrameMetadata // scroll offset and page scale for the sync
	bytes    int                           // screencast payload byte size, for capture stats
	decodeMS float64                       // wall-clock decode time in ms, set under capture only
}

// rawFrame is one undecoded screencast frame waiting in the coalescing slot: the
// base64 payload and the screencast metadata. The frame is acked on receive,
// before it enters the slot, so it carries no ack id. The slot exists only to hand
// the latest raw frame to the decode worker, dropping older undecoded frames.
type rawFrame struct {
	data string                        // base64 screencast payload, decoded by the worker
	meta *page.ScreencastFrameMetadata // scroll offset and page scale for the sync
}

// frameSlot is the latest-wins coalescing slot for undecoded screencast frames. It
// holds at most one raw frame; a newly arrived frame replaces any unconsumed one,
// so the decode worker always takes the newest and older undecoded frames are
// dropped without decode. It carries no ack: every delivered frame is acked once on
// receive (handleScreencastFrame), before it reaches the slot, so a dropped raw
// frame is already acked. The slot is the pure, browser-free coalescing core: feed
// it raw frames with put and take the latest with tryConsume, both browser-free and
// assertable in a unit test (see screencast_test.go).
type frameSlot struct {
	mu      sync.Mutex
	held    rawFrame
	full    bool
	ready   chan struct{} // signals consume that a frame is waiting; buffered size 1
	dropped bool          // set by drain so a late put stores nothing
}

// newFrameSlot returns an empty slot ready to accept frames.
func newFrameSlot() *frameSlot {
	return &frameSlot{ready: make(chan struct{}, 1)}
}

// put places the raw frame in the slot, replacing any unconsumed one (latest-wins
// coalescing). After drain it stores nothing, so no goroutine feeds a slot that
// will never be consumed. It never acks: the frame is acked on receive before it
// reaches here, so a replaced frame is already acked.
func (s *frameSlot) put(frame rawFrame) {
	s.mu.Lock()
	if s.dropped {
		s.mu.Unlock()
		return
	}
	s.held = frame
	s.full = true
	s.mu.Unlock()
	select {
	case s.ready <- struct{}{}:
	default:
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

// tryConsume takes the held raw frame without blocking, returning ok=true when one
// was present. It returns ok=false on an empty or drained slot, so a caller woken by
// readyC that finds nothing waiting (a spurious or drained wake) loops back to its
// select rather than parking. The caller decodes the taken frame off the listener
// goroutine.
func (s *frameSlot) tryConsume() (rawFrame, bool) {
	s.mu.Lock()
	if !s.full {
		s.mu.Unlock()
		return rawFrame{}, false
	}
	frame := s.held
	s.held = rawFrame{}
	s.full = false
	s.mu.Unlock()
	return frame, true
}

// drained reports whether the slot has been drained, so a caller woken by readyC
// that found no frame can tell a teardown wake (exit) from a spurious wake (re-park).
func (s *frameSlot) drained() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// drain marks the slot dropped and unblocks a consumer parked on ready, so Close
// releases the frame loop. After drain, put stores nothing and tryConsume returns
// ok=false. It needs no ack: the held raw frame was already acked on receive.
func (s *frameSlot) drain() {
	s.mu.Lock()
	s.dropped = true
	s.held, s.full = rawFrame{}, false
	s.mu.Unlock()
	select {
	case s.ready <- struct{}{}:
	default:
	}
}

// handleScreencastFrame acks one delivered screencast frame immediately, then
// hands the raw, undecoded frame to the coalescing slot for the worker to decode
// off this goroutine. It is the screencast-frame arm of the listener type switch,
// running on the chromedp ListenTarget event goroutine, so it must do no PNG decode
// and no chromedp.Run here: it counts ingest, records the metadata event, acks via
// the pump channel (the #920-safe off-goroutine ack), stamps the watchdog, and puts
// the raw frame. Acking on receive, before decode, releases Chrome's in-flight
// credit at once and decouples the ack from decode and any downstream stall. The
// recorder carries metadata only, never the frame image bytes, so the event stream
// holds no payload.
func (e *ChromiumEngine) handleScreencastFrame(slot *frameSlot, f *page.EventScreencastFrame) {
	now := time.Now()
	// Count true ingest for every delivered frame, before the slot coalesces any.
	// Gated behind the armed check so an off run pays one bool check and no clock read.
	if e.ingest.armed() {
		e.ingest.record(now)
	}
	// Stamp the receive for the stuck-state watchdog (always on, lock-free).
	e.markFrameReceived(now, f.SessionID)
	if e.recorder != nil {
		e.recorder.Record(captureEvent{
			Type:      captureEventScreencastFrame,
			Meta:      f.Metadata,
			SessionID: f.SessionID,
		})
	}
	// Ack on receive, before any decode. The enqueue is #920-safe: ackPump runs the
	// chromedp.Run off this event goroutine. Whether this frame is later decoded or
	// dropped from the slot, it is acked exactly once, here.
	e.ackFrame(f.SessionID)
	// Hand the raw frame to the coalescing slot; the worker decodes it. An older
	// undecoded frame is dropped without decode, and it is already acked.
	slot.put(rawFrame{data: f.Data, meta: f.Metadata})
}

// registerScreencastListener registers the one ListenTarget handler that decodes
// each EventScreencastFrame and feeds it into the slot, acking displaced frames as
// they are replaced. It is called once per session, on the fresh session context,
// so a stream restart (restartStream) does not add a second handler. chromedp
// appends listeners and drops them only when the context is done, so re-registering
// on every backend switch or resize would pile up handlers that each decode and ack
// every frame, over-delivering into the slot. The capture paint latch is not fired
// here: the navigate waiter's matched load signal closes it (markPainted from
// navigateAsync), so this listener handles only screencast frames.
func (e *ChromiumEngine) registerScreencastListener(ctx context.Context, slot *frameSlot) {
	if ctx == nil || slot == nil {
		return
	}
	chromedp.ListenTarget(ctx, func(ev any) {
		if f, ok := ev.(*page.EventScreencastFrame); ok {
			e.handleScreencastFrame(slot, f)
		}
	})
}

// startScreencast starts (or restarts) a lossless PNG screencast capped at the
// engine's active capture size. The frame listener is registered once per session
// by registerScreencastListener, so this issues only the CDP StartScreencast
// command; restartStream re-issues it after a resize or backend switch without
// leaking a second listener. The stream couples to the consume rate because Chrome
// withholds frames until acked.
func (e *ChromiumEngine) startScreencast(ctx context.Context, capW, capH int) error {
	if ctx == nil {
		return fmt.Errorf("engine: start screencast: no browser context")
	}
	if e.recorder != nil {
		e.recorder.Record(captureEvent{Type: captureEventScreencastControl, Action: "start"})
	}
	return chromedp.Run(ctx,
		page.StartScreencast().
			WithFormat(page.ScreencastFormatPng).
			WithMaxWidth(int64(capW)).
			WithMaxHeight(int64(capH)),
	)
}

// ackFrame enqueues one frame's SessionID onto the pump channel for ackPump to run
// as page.ScreencastFrameAck off the ListenTarget event goroutine. handleScreencastFrame
// calls it once per delivered frame, on receive; the watchdog calls it to re-couple
// a stalled stream. It never runs the ack inline, because handleScreencastFrame is
// on the event goroutine and a chromedp.Run there is the chromedp #920 self-deadlock.
// The enqueue is select-with-default so a full channel or a torn-down session (a nil
// acks channel) drops the ack rather than parking the caller. Chrome self-throttles
// the screencast to at most a few frames in flight, so the buffered pump keeps pace
// and the drop branch stays unreached under normal cadence.
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

// screencastWatchdogInterval is how often the stuck-state watchdog checks the
// stream. It matches casty's 5s cadence: long enough to leave a healthy idle page
// alone (at most one re-couple per silent gap), short enough to recover a wedge
// quickly.
const screencastWatchdogInterval = 5 * time.Second

// screencastStallThreshold is how long the stream must be silent (no frame
// received) before the watchdog treats the silence as a possible wedge and
// re-couples once. It equals the check interval, so a genuine stall is recovered
// within about two intervals.
const screencastStallThreshold = 5 * time.Second

// markFrameReceived stamps the wall time and SessionID of a delivered screencast
// frame for the stuck-state watchdog, and clears the per-gap poke guard so a fresh
// frame re-arms recovery. handleScreencastFrame calls it on the listener goroutine
// for every delivered frame, before any decode. It is lock-free (atomics), so it
// adds only three atomic stores to the hot receive path and is always on,
// independent of the NYALA_FPS_TIMING ingest counter.
func (e *ChromiumEngine) markFrameReceived(now time.Time, session int64) {
	e.recvNano.Store(now.UnixNano())
	e.lastSession.Store(session)
	e.stallPoked.Store(false)
}

// runScreencastWatchdog recovers a wedged screencast without disturbing a healthy
// idle page. It ticks every screencastWatchdogInterval and, when the stream has
// received at least one frame but none within screencastStallThreshold, re-acks the
// last frame's SessionID once per silent gap. The re-ack restores Chrome's in-flight
// credit if an ack was ever lost (the wedge) and is a no-op on a genuinely idle
// page, which has no pending compositor frame to send regardless of credit. Silence
// alone cannot tell idle from wedge, so the watchdog uses only this conservative
// re-couple, never a stream reset. It runs per session and exits when ctx is
// cancelled.
func (e *ChromiumEngine) runScreencastWatchdog(ctx context.Context) {
	if ctx == nil {
		return
	}
	t := time.NewTicker(screencastWatchdogInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			e.watchdogRecoverIfStalled(now)
		}
	}
}

// watchdogRecoverIfStalled re-acks once when the stream looks stalled: it received a
// frame before (recvNano set) but none within screencastStallThreshold, and it has
// not already re-acked for this silent gap (stallPoked). It is factored out so the
// decision is unit-tested without a ticker, and returns true when it re-acked.
func (e *ChromiumEngine) watchdogRecoverIfStalled(now time.Time) bool {
	recv := e.recvNano.Load()
	if recv == 0 {
		return false // never streamed: nothing to recover
	}
	if now.Sub(time.Unix(0, recv)) < screencastStallThreshold {
		return false // a frame arrived recently: healthy
	}
	if e.stallPoked.Swap(true) {
		return false // already re-acked for this silent gap
	}
	e.ackFrame(e.lastSession.Load())
	return true
}

// overlayState is the text-sync state the snapshot goroutine threads across its
// iterations. It holds the last-matched runs, the wall-clock start of the current
// snapshot-failure run, and the wall-clock time of the last debounced re-snapshot.
// The snapshot goroutine owns it, so the hold is single-goroutine and lock-free;
// the snapshot read runs outside any lock.
type overlayState struct {
	lastText       []render.GlyphRun // last text matched to a bitmap
	lastScroll     scrollOffset      // DOM scroll offset lastText was decoded at, for the error-hold tag
	lastGen        int64             // layout generation lastText was decoded at, for the error-hold tag
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
	// scrollY tags the runs with the DOM vertical scroll offset (CSS pixels) they were
	// decoded at, so composeFrame pairs a bitmap to this set only when the bitmap's own
	// scroll matches. Vertical scroll is the only axis that reflows rows, so the pair
	// gates on Y alone.
	scrollY float64
	// gen is the layout generation the runs were decoded at, read in the same
	// sequence as the snapshot. composeFrame pairs a bitmap to this set only when the
	// bitmap's inferred generation matches gen, so a run set never composes over a
	// reflowed bitmap of a different generation.
	gen int64
}

// store publishes the latest decoded runs, snapshot time, the DOM vertical scroll
// offset the runs were decoded at, and the layout generation they were decoded at.
func (t *textSync) store(runs []render.GlyphRun, snapshotMS, scrollY float64, gen int64) {
	t.mu.Lock()
	t.runs = runs
	t.snapshotMS = snapshotMS
	t.scrollY = scrollY
	t.gen = gen
	t.mu.Unlock()
}

// latest returns the most recently published runs, snapshot time, the DOM vertical
// scroll offset, and the layout generation the runs were decoded at, or nil and
// zeroes before the first snapshot lands.
func (t *textSync) latest() ([]render.GlyphRun, float64, float64, int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.runs, t.snapshotMS, t.scrollY, t.gen
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
// scroll settles. 150 ms balances overlay tracking against the per-snapshot cost: an
// in-page text walk forces a synchronous layout read, so a tighter cadence would tax
// the page during a fast scroll for little extra legibility, and a looser one would
// let the text lag the bitmap visibly. The trailing holdStalenessLimit debounce
// lands one final snapshot on settle, so the overlay always ends on the resting
// scroll position regardless of the cadence phase.
const scrollCadence = 150 * time.Millisecond

// initialSettleTimeout bounds how long the capture first-publish latch (textReady)
// waits for the initial layout to settle before it releases anyway. Under capture
// the snapshot goroutine holds textReady past the first publish until the first
// debounced-reflow re-snapshot lands (networkAlmostIdle, the load-settle signal), so
// the first counted capture frame carries the settled overlay rather than a
// pre-settle snapshot a post-paint reflow would still shift. A page that never fires
// that signal releases the latch on this backstop, so the capture never stalls. It
// is a var, not a const, so a test shrinks it to run instantly; production reads it
// only under capture, where textReady is non-nil, so the live path never touches it.
var initialSettleTimeout = 2 * time.Second

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
	// Read the per-session load timer once here, on the goroutine start ordering, so
	// no rebuild race with startSession's write. A nil timer is a valid disabled timer.
	e.mu.Lock()
	timer := e.timing
	e.mu.Unlock()
	var state overlayState
	// lastCalCellW/H track the (cellW, cellH) pair the last calibration corrected
	// for. The reSnap arm skips recalibration when the pair is unchanged, so a
	// non-resize poke does not re-measure. Goroutine-local like state: the snapshot
	// goroutine owns it, so the guard is lock-free.
	var lastCalCellW, lastCalCellH float64
	if !e.waitForFirstSnapshot(ctx, ts) {
		return
	}
	// Reconcile font forcing to the active backend before the first publish. On the
	// block path this is the no-op the launch injection already covered; on a
	// graphics-to-block Tab it registers the script and injects the live style.
	// waitForFirstSnapshot only returns for the block backend, so a graphics start
	// never reaches here.
	e.syncFontForcing(ctx)
	// First snapshot on the painted latch: the navigation trigger, single-shot per
	// session, and the new reader of the painted latch replacing firstSnapshotAfterPaint.
	// Publish before calibrating so first paint shows text without waiting the
	// calibration base (document.fonts.load plus probe and reinject evals). The seed
	// advance is already ~cellW (advanceRatio measured from the shipped TTF), so seed
	// placement is near-correct; the calibrated re-publish below trims the residual.
	// markText fires here, the first-text milestone. Program order on this single
	// goroutine is the whole ordering contract: no channel, no sync.Once, no new goroutine.
	state = e.publishSnapshot(ctx, snapshot, ts, state)
	timer.markText()
	pokeReEmit(reEmit)
	// Calibrate against the forced font metrics, then re-publish so the overlay maps
	// against the corrected size. This trails the first publish, so production first
	// paint shows near-calibrated text for one reflow cycle before this re-publish
	// lands the final placement.
	e.calibrateFonts(ctx)
	e.mu.Lock()
	lastCalCellW, lastCalCellH = e.snapshotCellPx()
	e.mu.Unlock()
	state = e.publishSnapshot(ctx, snapshot, ts, state)
	pokeReEmit(reEmit)
	// Signal the capture driver that the overlay runs have published, so it counts a
	// usable frame only once the calibrated text has landed: this runs after the
	// calibrated re-publish above, so a multi-frame capture never counts the
	// uncalibrated first publish. Off the capture path textReady is nil, so
	// markTextReady is a no-op and this arms no timer, keeping production at a nil
	// check. For a single-frame capture (settleHold) hold the latch past the first
	// publish until the initial layout settles: the first debounced-reflow re-snapshot
	// (networkAlmostIdle, the load-settle signal) releases it on the settled overlay,
	// and initialSettleTimeout is the backstop for a page that never fires that signal.
	// Without the hold the one bundled frame can carry a pre-settle snapshot a
	// post-paint reflow (a late font swap, a CJK vertical/ruby relayout) would still
	// shift, making the single-frame capture non-deterministic. A multi-frame capture
	// leaves settleHold false and fires the latch now, so the extra frames it counts
	// come from the load period a settle wait would otherwise drain.
	textReadyPending := e.settleHold
	if !textReadyPending {
		e.markTextReady()
	}
	var settleTimer *time.Timer
	var settleTimerC <-chan time.Time
	if textReadyPending {
		settleTimer = time.NewTimer(initialSettleTimeout)
		settleTimerC = settleTimer.C
	}
	// releaseTextReady closes the capture first-publish latch once and stops the
	// settle backstop timer, so the load-settle and backstop arms below both funnel
	// through one place and the latch fires exactly once.
	releaseTextReady := func() {
		if !textReadyPending {
			return
		}
		e.markTextReady()
		textReadyPending = false
		settleTimerC = nil
		if settleTimer != nil {
			settleTimer.Stop()
		}
	}

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
			// The load settled: the first debounced-reflow re-snapshot published the
			// resting-layout overlay and re-emitted the held bitmap with it, so release
			// the capture first-publish latch now, on the settled text.
			releaseTextReady()
		case <-settleTimerC:
			// Backstop: the page fired no load-settle reflow within initialSettleTimeout,
			// so release the capture first-publish latch on the first-publish text rather
			// than stall the capture.
			releaseTextReady()
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
// resize clocked text. It recalibrates the font before the publish, because a late
// web-font swap raises a layout shift and is the reflow this path fires on: the
// one-shot launch calibration measured the fallback face, so a font that lands after
// paint would leave a stale font-size and drift the origins the walk snaps. The
// recalibrate rides the same rate cap, so a chatty page cannot re-measure per reflow.
// It returns the updated hold state.
func (e *ChromiumEngine) reSnapshotCapped(ctx context.Context, snapshot func(context.Context, overlayState) ([]render.GlyphRun, float64, scrollOffset, int64, overlayState), ts *textSync, state overlayState) overlayState {
	now := time.Now()
	if !state.lastReSnap.IsZero() && now.Sub(state.lastReSnap) < reSnapshotMinInterval {
		return state
	}
	if e.textOverlayBackend() {
		e.calibrateFonts(ctx)
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
// reEmit on a fire so the held frame repaints with the new scroll's text. Each
// snapshot decodes runs at its own whole-cell scroll, so the overlay tracks the page
// as it scrolls.
func (e *ChromiumEngine) scrollSnap(ctx context.Context, snapshot func(context.Context, overlayState) ([]render.GlyphRun, float64, scrollOffset, int64, overlayState), ts *textSync, state overlayState, reEmit chan<- struct{}) overlayState {
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

// publishSnapshot runs one snapshot and publishes its runs, time, scroll, and
// layout generation to ts. It returns the updated hold state. It is the single
// snapshot-and-publish step the first-after-paint and re-snapshot triggers share, so
// the publish stays in one place. A graphics backend takes no overlay, so it
// publishes nil and skips the layout read rather than snapshot the full viewport it
// ignores.
func (e *ChromiumEngine) publishSnapshot(ctx context.Context, snapshot func(context.Context, overlayState) ([]render.GlyphRun, float64, scrollOffset, int64, overlayState), ts *textSync, state overlayState) overlayState {
	if !e.textOverlayBackend() {
		ts.store(nil, 0, 0, 0)
		return state
	}
	runs, snapshotMS, scroll, gen, next := snapshot(ctx, state)
	// snapshotRuns rebuilds the hold fields and drops the cadence timestamps, so carry
	// them forward here: the re-snapshot caps live in runTextSync, not the snapshot.
	next.lastReSnap = state.lastReSnap
	next.lastScrollSnap = state.lastScrollSnap
	// Tag the published runs with the DOM scroll and the layout generation they were
	// decoded at, so composeFrame can pair them to a same-generation bitmap at the same
	// scroll.
	ts.store(runs, snapshotMS, scroll.Y, gen)
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

// runFrameLoop takes the latest raw frame from the slot, decodes it off the
// listener goroutine, builds a synced render.Frame, and pushes it on the frames
// channel, until the context is cancelled or the slot is drained. The decode runs
// here, not in handleScreencastFrame, so the chromedp receiver goroutine never
// stalls on a PNG decode and acks are never queued behind one. A corrupt frame
// fails to decode and is dropped; it is already acked on receive, so no ack is
// needed here. It runs in its own goroutine started per session; a crash
// or Close drains the slot and cancels the context to stop it. The DOM snapshot
// runs on a separate goroutine (runTextSync) clocked by the page, not by frame
// delivery, so this loop reads the latest published overlay runs as sticky state
// and folds them onto every frame without ever poking the snapshot or blocking on
// a snapshot read. It also parks on the re-emit poke the snapshot goroutine sends
// after each publish: on a poke it re-pushes its last consumed bitmap recomposed
// with the freshly published text, so a static page that delivers no new screencast
// frame still repaints with text. The re-emit reuses the held decoded bitmap and
// never re-feeds the slot. On
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
	// Read the per-session load timer once here, on the goroutine start ordering, so
	// no per-frame lock and no rebuild race with startSession's write. A nil timer is
	// a valid disabled timer.
	e.mu.Lock()
	timer := e.timing
	decode := e.decode
	e.mu.Unlock()
	if decode == nil {
		decode = decodeScreencastImage
	}
	var held pendingFrame
	var haveHeld bool
	// lastScroll tracks the scroll offset of the last consumed frame so a change
	// pokes a re-snapshot. It lives on this goroutine, so the tracking is lock-free.
	var lastScroll scrollOffset
	var haveScroll bool
	// lastComposed is the overlay the previous frame composed. When a bitmap's
	// inferred generation or scroll does not match the latest published run set,
	// composeFrame holds this set for one frame rather than composing a stale set onto
	// a fresh bitmap. It lives on this goroutine, so the hold is lock-free.
	var lastComposed []render.GlyphRun
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
			if !e.emitFrame(ctx, frames, ts, held, &lastComposed) {
				return
			}
			timer.markFrame()
		case <-slot.readyC():
			raw, ok := slot.tryConsume()
			if !ok {
				// A drained slot wakes readyC with no frame; exit on drain, otherwise the
				// wake was spurious and the select re-parks.
				if slot.drained() {
					return
				}
				continue
			}
			// Decode off the listener goroutine. Time it only under capture. A corrupt
			// frame fails here and is dropped; it was already acked on receive.
			start := time.Now()
			img, derr := decode(raw.data)
			if derr != nil {
				continue
			}
			pending := pendingFrame{img: img, meta: raw.meta}
			if e.captureOn {
				pending.bytes = len(raw.data)
				pending.decodeMS = msSince(start)
			}
			held, haveHeld = pending, true
			off, offOK := frameScrollOffset(pending)
			if offOK {
				if !haveScroll || off != lastScroll {
					if haveScroll {
						pokeScroll(scroll)
					}
					lastScroll, haveScroll = off, true
				}
			}
			if !e.emitFrame(ctx, frames, ts, pending, &lastComposed) {
				return
			}
			timer.markFrame()
		}
	}
}

// inferFrameGen infers the layout generation of a consumed screencast bitmap. The
// screencast metadata carries no generation token, so it is inferred from the latest
// published snapshot. Scroll never bumps the layout-generation counter (only a reflow
// does), so a bitmap shares the latest published snapshot's generation whether or not
// its scroll matches; composeFrame gates on scroll separately, so a scroll-only
// difference shifts the runs onto the bitmap's scroll rather than holding the overlay.
// A reflow bumps the counter and the snapshot goroutine re-snapshots
// on the same layout-shift signal, so the latest published generation tracks the
// current layout. It returns the latest published generation, and whether a snapshot
// has published (haveTS): before the first publish there is no generation to pair, so
// ok is false and the compose draws the bitmap alone. tsGen is the latest published
// set's generation.
//
// Bound: the inference lags a reflow by at most one re-snapshot. In the window between
// a reflow bumping the page counter and the snapshot goroutine publishing the
// re-decoded runs at the new generation, a bitmap already painted at the new layout is
// inferred at the old published generation. The runs published at the old generation
// then compose over it for that window. It closes on the next publish, which the
// layout-shift trigger fires promptly, so the drift is bounded to the re-snapshot
// latency rather than persisting.
func inferFrameGen(tsGen int64, haveTS bool) (gen int64, ok bool) {
	if !haveTS {
		return 0, false
	}
	return tsGen, true
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
func (e *ChromiumEngine) emitFrame(ctx context.Context, frames chan render.Frame, ts *textSync, pending pendingFrame, lastComposed *[]render.GlyphRun) bool {
	frame := e.composeFrame(pending, ts, lastComposed)
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
// Frame.Text; runTextSync publishes nil for them, so this folds nil.
//
// It pairs the run set to the bitmap by layout generation and scroll. It infers the
// bitmap's generation from the latest published snapshot (inferFrameGen). When the
// bitmap's inferred generation matches the run set's generation and the bitmap's own
// scroll equals the scroll the runs were decoded at, it composes the set as-is: the
// walk placed each run on-screen at that scroll, so no shift is needed. When the
// generation matches but the bitmap has scrolled past the snapshot, it shifts the runs
// onto the bitmap's whole-cell scroll (shiftRunsToBitmapScroll) so the overlay tracks
// the moving page. Only a genuinely different generation (a reflow) or an
// uninferable generation holds the previous frame's composed overlay for one frame
// rather than compose a stale set onto a freshly reflowed bitmap. lastComposed carries
// the held set across frames; the frame loop owns it. Before the first publish (nil
// run set) it composes nil, the bitmap alone.
func (e *ChromiumEngine) composeFrame(pending pendingFrame, ts *textSync, lastComposed *[]render.GlyphRun) render.Frame {
	var runs []render.GlyphRun
	var snapshotMS, snapScrollY float64
	var tsGen int64
	var haveTS bool
	if ts != nil {
		runs, snapshotMS, snapScrollY, tsGen = ts.latest()
		haveTS = runs != nil
	}
	e.mu.Lock()
	_, cellCSSH := e.snapshotCellPx()
	rows := e.rows
	scale := e.layoutScale()
	e.mu.Unlock()
	bitmapScrollY := snapScrollY
	if pending.meta != nil {
		bitmapScrollY = pending.meta.ScrollOffsetY
	}
	frameGen, frameGenOK := inferFrameGen(tsGen, haveTS)
	var composed []render.GlyphRun
	switch {
	case !haveTS:
		// No valid run set to compose: none has published yet, or the snapshot error
		// hold expired to nil (holdDecision dropped the stale text past its staleness
		// bound). Draw the bitmap alone, so the staleness bound keeps its authority and
		// the overlay never shows text the page no longer matches.
		composed = nil
	case frameGenOK && frameGen == tsGen && bitmapScrollY == snapScrollY:
		// The bitmap and the run set share a generation and scroll: compose the runs
		// as-is, since the walk placed each on-screen at that scroll. The generation and
		// scroll guards pair the two so a run set never composes onto a bitmap of a
		// different generation or scroll.
		composed = runs
	case frameGenOK && frameGen == tsGen:
		// Same generation, but the bitmap scrolled past the snapshot: shift the runs onto
		// the bitmap's scroll so the overlay stays glued to the moving page rather than
		// freezing at the snapshot's scroll until the settle re-snapshot lands. Scroll is
		// whole-cell (dispatchWheel snaps the DOM scroll to a multiple of cellH), so the
		// row shift is an exact integer and the copy never aliases fractional rows.
		composed = shiftRunsToBitmapScroll(runs, bitmapScrollY, snapScrollY, cellCSSH, rows)
	default:
		// A genuinely different generation (a reflow bumped the counter): hold the
		// previous composed overlay one frame rather than compose a stale set onto a
		// freshly reflowed bitmap. The re-snapshot on the layout-shift signal converges
		// the two so the pairing lands on the next frame.
		if lastComposed != nil {
			composed = *lastComposed
		}
	}
	if lastComposed != nil {
		*lastComposed = composed
	}
	frame := render.Frame{Img: pending.img, Text: composed}
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

// shiftRunsToBitmapScroll shifts the published runs vertically onto the bitmap's
// own scroll, so the overlay tracks the moving page while the bitmap scrolls past
// the snapshot the runs were decoded at. A run's box Y is the on-screen top with the
// snapshot scroll already subtracted, so a bitmap scrolled dy past the snapshot moves
// the run up by dy: the box Y shifts by -dy and the cell row by -round(dy/cellCSSH).
// Scroll is whole-cell, so dy/cellCSSH is an integer and the row shift is exact.
// Columns are untouched: this is the vertical-scroll axis only. A run whose shifted
// row falls outside the grid is dropped. It returns the runs untouched (no copy) on a
// zero delta, the production fast path; otherwise it returns a shifted copy and never
// mutates runs, which textSync shares across composes.
func shiftRunsToBitmapScroll(runs []render.GlyphRun, bitmapScrollY, snapScrollY, cellCSSH float64, rows int) []render.GlyphRun {
	dy := bitmapScrollY - snapScrollY
	if len(runs) == 0 || dy == 0 {
		return runs
	}
	var rowShift int
	if cellCSSH > 0 {
		rowShift = int(math.Round(dy / cellCSSH))
	}
	shifted := make([]render.GlyphRun, 0, len(runs))
	for _, run := range runs {
		run.Row -= rowShift
		run.Y -= dy
		if run.Row < 0 || (rows > 0 && run.Row >= rows) {
			continue
		}
		shifted = append(shifted, run)
	}
	return shifted
}

// snapshotRuns runs one in-page text walk, which returns per-line runs already
// snapped to integer cells against the forced cell, the DOM scroll offset the walk
// read at, and the layout generation, all in one Runtime.evaluate. It decodes the
// walk result to glyph runs (trusting the page's integer origins, keeping only the
// grid right-edge clip), then routes the result through the bounded holdDecision: a
// successful read emits the fresh runs, a failing read holds the last-matched runs
// for at most holdStalenessLimit before dropping to nil. It returns the emitted
// runs, the snapshot time, the scroll offset, the layout generation, and the updated
// state. It runs on the snapshot goroutine; the walk chromedp.Run stays outside any
// lock.
func (e *ChromiumEngine) snapshotRuns(ctx context.Context, state overlayState) ([]render.GlyphRun, float64, scrollOffset, int64, overlayState) {
	if ctx == nil {
		emit, last, failing := holdDecision(context.Canceled, nil, state.lastText, state.failingSince, time.Now())
		return emit, 0, state.lastScroll, state.lastGen, overlayState{lastText: last, lastScroll: state.lastScroll, lastGen: state.lastGen, failingSince: failing}
	}
	e.mu.Lock()
	cellW, cellH := e.snapshotCellPx()
	cols := e.cols
	e.mu.Unlock()
	start := time.Now()
	result, err := e.textWalk(ctx, cellW, cellH)
	var snapshotMS float64
	if e.captureOn {
		snapshotMS = msSince(start)
	}
	if e.recorder != nil {
		e.recorder.Record(captureEvent{Type: captureEventDOMSnapshot})
	}
	var decoded []render.GlyphRun
	gen := state.lastGen
	var scroll scrollOffset
	if err == nil {
		// The walk returns runs already placed on-screen (getClientRects subtracts the
		// scroll) and snapped to integer cells against the forced cell in the page, so
		// the origin snap and the layout advance share one unit and a long run cannot
		// drift across x. The tagged scroll and generation come from the same evaluate,
		// so they are internally consistent with the runs.
		gen = result.Gen
		scroll = scrollOffset{X: result.ScrollX, Y: result.ScrollY}
		decoded = decodeWalkRuns(result, cellW, cols)
	}
	emit, last, failing := holdDecision(err, decoded, state.lastText, state.failingSince, time.Now())
	// Tag the emitted runs with the scroll and generation they were decoded at: a
	// fresh success carries the new snapshot scroll and generation, an error hold
	// re-emits the last-matched runs so it carries the scroll and generation those
	// were decoded at (state.lastScroll, state.lastGen). The runs and their scroll and
	// generation travel together so composeFrame shifts and pairs the right way.
	emitScroll := scroll
	emitGen := gen
	if err != nil {
		emitScroll = state.lastScroll
		emitGen = state.lastGen
	}
	return emit, snapshotMS, emitScroll, emitGen, overlayState{lastText: last, lastScroll: emitScroll, lastGen: emitGen, failingSince: failing}
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

// restartStream stops the current screencast and starts a fresh one at the
// engine's recomputed surface size. The viewer calls it after a resize or a
// backend switch, so frames arrive at the new grid rather than the old one. It
// moves the three sizing values together: the captured surface and the layout
// viewport via applySizing, and the screencast cap (the painted device surface
// from screencastCap) via startScreencast. Moving them as one triple keeps a
// backend switch from leaving the surface pinned at the launch size with a moved
// cap. A stop error is ignored because the next start re-establishes the stream.
func (e *ChromiumEngine) restartStream() error {
	e.mu.Lock()
	ctx := e.ctx
	slot := e.slot
	windowID := e.windowID
	reSnap := e.reSnap
	surfW, surfH := e.surfaceSize(e.cols, e.rows)
	capW, capH := e.screencastCap(e.cols, e.rows)
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
	return e.startScreencast(ctx, capW, capH)
}
