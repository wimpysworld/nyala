package engine

import (
	"context"
	"fmt"
	"image"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/wimpysworld/nyala/internal/render"
)

// ChromiumEngine drives one headless-Chromium page over CDP and emits decoded
// frames with their crisp DOM text synced on. It holds the URL and scroll
// offset in its own state, not inside chromedp, so it can rebuild and
// re-navigate after a crash. Keep this struct thin: the screencast loop,
// snapshot decode, frame sync, and launch logic live in their own files.
type ChromiumEngine struct {
	// mu guards the session and geometry fields read or written from both the
	// viewer goroutine (Resize, SetBackend, SendKey, SendMouse, dispatchWheel)
	// and the supervisor goroutine (startSession, teardownSession, launch,
	// rebuild): ctx, cancel, slot, acks, text, frame, captureW, captureH, cols, rows, scrollX,
	// scrollY, scrollYTarget, backend, blockScheme, cellW, cellH, layoutWidth, windowID, inputFailures, fontForcingID. Lock, copy the fields into locals or write
	// the updates, unlock, then run the chromedp I/O on the locals. Never hold mu
	// across a chromedp.Run or a channel send, or it deadlocks with the supervisor.
	mu sync.Mutex

	// Browser context and its cancel, owned by chromedp.
	ctx    context.Context
	cancel context.CancelFunc

	// Page state held outside chromedp for crash recovery.
	url     string // current URL
	scrollX int    // current scroll offset X in CSS pixels
	scrollY int    // dispatched scroll offset Y in CSS pixels (block: whole-cell snapped)

	// scrollYTarget is the raw intended vertical scroll offset in CSS pixels before
	// whole-cell snapping. On the block backend dispatchWheel accumulates every raw
	// wheel delta here and snaps this target to the nearest whole cellCSSH to derive
	// scrollY, so repeated small scrolls track the true intent and do not drift. Off
	// the block backend it tracks scrollY exactly.
	scrollYTarget int

	// Resolved geometry from the Stage 1 cell px and the active backend.
	cellW int // resolved cell width in pixels
	cellH int // resolved cell height in pixels

	layoutWidth int    // block-backend target layout width in CSS pixels
	backend     string // active backend (block, sixel, kitty)
	// blockScheme is the block-backend cell rendering scheme ("quadrant" or
	// "half"); the zero value and any other value read as quadrant (see
	// captureSize). Set once by SetScheme before Navigate.
	blockScheme string
	browserFlag string // --browser value, empty for env/PATH discovery
	cols        int    // terminal grid width in cells
	rows        int    // terminal grid height in cells
	captureW    int    // capture width in pixels for the active backend
	captureH    int    // capture height in pixels for the active backend

	// fontForcingID is the ScriptIdentifier of the registered font-forcing
	// on-new-document script, empty when no forcing is active. Only the block backend
	// forces the page onto the monospace grid; syncFontForcing registers the script
	// and injects the live style when the active backend takes the overlay, and removes
	// both when it does not. It is reset per session by teardownSession so a stale id
	// never targets a fresh session's script list.
	fontForcingID page.ScriptIdentifier

	// layoutShiftID is the ScriptIdentifier of the registered layout-shift observer
	// on-new-document script, empty when no observer is active. It rides the block
	// backend's font-forcing lifecycle: syncFontForcing registers the observer script
	// and the Runtime binding when the block backend takes the overlay, and removes
	// both when it does not, so a layout shift on the live page re-decodes the text.
	// It is reset per session by teardownSession alongside fontForcingID.
	layoutShiftID page.ScriptIdentifier

	// windowID is the browser window id for the driven target, fetched once per
	// session via browser.GetWindowForTarget and used to size the captured surface
	// with Browser.SetContentsSize on launch and on every restart. It is reset per
	// session by teardownSession so a stale id never sizes a fresh window.
	windowID browser.WindowID

	// inputFailures counts consecutive input-dispatch failures. A successful
	// dispatch resets it; a run past inputDispatchFailureThreshold records one
	// capture event so a wedged browser is observable without spamming.
	inputFailures int

	// screencastCtl serialises screencast stop/start so a resize or backend switch
	// (restartStream) and an adaptive format switch (switchScreencastFormat) never run
	// StopScreencast/StartScreencast concurrently. It is a dedicated lock held only
	// across those CDP control calls, never nested with e.mu across a chromedp.Run, so
	// it cannot deadlock with the supervisor.
	screencastCtl sync.Mutex
	// screencastFmt is the current screencast format and JPEG quality, guarded by
	// e.mu. startScreencast reads it so every start (initial, resize, backend switch,
	// format switch) uses the current format. The block backend runs live JPEG, the
	// graphics backends run adaptive PNG-settled/JPEG-moving, and the capture path
	// forces PNG; initialScreencastFormat picks it from the backend and capture flag.
	screencastFmt screencastFmt
	// motion carries per-frame arrival instants from the listener goroutine to the
	// adaptive-format controller, which reads the frame-arrival rate as the page's
	// motion signal. It is created and the controller started only on the live
	// graphics-capable path; under capture it stays nil so pokeMotion is a no-op and
	// the format never switches. Buffered and latest-wins-drop, so the listener never
	// blocks. Reset per session by teardownSession.
	motion chan time.Time

	// Frame plumbing.
	frames chan render.Frame // emitted frames, created once and closed only on Close
	slot   *frameSlot        // capture-path latest-wins single-frame backpressure slot
	// rawQ is the live-path bounded, drop-oldest feed from the listener goroutine to
	// the parallel decode pool. It replaces the single-frame slot on the live path so
	// several workers decode concurrently; capture keeps the ordered slot and leaves
	// this nil. Either being non-nil means the session is streaming (see streaming).
	// Reset per session by teardownSession.
	rawQ *rawFrameQueue
	// acks carries screencast frame SessionID values from the listener goroutine
	// to the ack-pump goroutine, which runs page.ScreencastFrameAck off the event
	// goroutine. Running the ack inside the ListenTarget callback is the chromedp
	// #920 self-deadlock; the pump breaks it. It is buffered so the listener and
	// the slot's displaced-frame ack never block on a transient pump stall, and the
	// enqueue is select-with-default so a full channel or a torn-down session drops
	// the ack rather than parking the event goroutine.
	acks chan int64
	// text is the per-session overlay publisher: the snapshot goroutine stores the
	// latest decoded runs, the frame loop reads them. It decouples the DOM snapshot
	// from the frame-emit goroutine. Reset per session by teardownSession.
	text *textSync
	// reSnap pokes the snapshot goroutine for an immediate re-snapshot on a resize
	// or backend switch (restartStream). reflow pokes it for a debounced
	// networkAlmostIdle late reflow. scroll pokes it for a debounced re-snapshot
	// after a scroll-offset change, so the text overlay tracks the page as it
	// scrolls. All three are capacity-1 latest-wins, goroutine-scoped to one
	// session, so a burst coalesces to one wake and ctx cancellation stops the
	// goroutine without a channel reset. Reset per session by teardownSession.
	reSnap chan struct{}
	reflow chan struct{}
	scroll chan struct{}
	// reEmit pokes the frame loop to re-push its held bitmap recomposed with the
	// freshly published text, so a static page that delivers no new screencast frame
	// after the snapshot still repaints with text. The snapshot goroutine pokes it
	// after each publish; the frame loop folds the latest text onto the held frame.
	// It is capacity-1 latest-wins and session-scoped, reset per session by
	// teardownSession.
	reEmit chan struct{}
	// snapshot is the per-iteration snapshot function runTextSync calls, defaulting
	// to snapshotRuns. The unit test swaps in a counter to assert the snapshot is
	// gated to about one call per delivered frame without launching a browser; it is
	// nil in production, where runTextSync falls back to snapshotRuns. It returns the
	// decoded runs, the snapshot time, the DOM scroll offset the runs were decoded at,
	// the layout generation read in the same sequence, and the updated hold state.
	snapshot func(context.Context, overlayState) ([]render.GlyphRun, float64, scrollOffset, int64, overlayState)

	// decode turns a raw base64 screencast payload into an RGBA bitmap. It defaults
	// to decodeScreencastImage; the frame-loop unit test swaps in a fake so it drives
	// the raw slot without a real PNG. It is nil in production, where runFrameLoop
	// falls back to decodeScreencastImage, and read once at the top of runFrameLoop.
	decode func(string) (*image.RGBA, error)

	// switchFmt performs one adaptive screencast format switch. It defaults to
	// switchScreencastFormat (stop then start with the new format); the format-controller
	// unit test swaps in a fake that records the switch and updates e.screencastFmt, so
	// the state machine is exercised without a browser. It is nil in production, where
	// runScreencastFormat falls back to switchScreencastFormat.
	switchFmt func(context.Context, page.ScreencastFormat, int)

	// recvNano is the wall-clock nanoseconds of the last screencast frame received on
	// the listener goroutine, or zero before the first. lastSession is that frame's
	// SessionID, the id the watchdog re-acks to re-couple a wedged stream. stallPoked
	// marks that the watchdog already re-acked for the current silent gap, so it
	// re-couples at most once per gap and a fresh frame clears it. All three are
	// lock-free (set on the receive path, read on the watchdog goroutine) and always
	// on, independent of the NYALA_FPS_TIMING ingest counter. startSession resets them
	// so a rebuild does not treat a stale timestamp as a stall.
	recvNano    atomic.Int64
	lastSession atomic.Int64
	stallPoked  atomic.Bool

	// ingest counts delivered screencast frames at the true ingest point
	// (handleScreencastFrame), before the latest-wins slot drops any, for the
	// NYALA_FPS_TIMING In rate. It is armed once in New from the env switch and read
	// by the viewer over the ingestReporter seam (IngestFPS, IngestSummary). It
	// persists across sessions, so a resize or backend switch keeps the
	// engine-lifetime ingest rate. Off the switch it is a disabled no-op counter, so
	// the listener goroutine pays one bool check per frame.
	ingest *ingestCounter

	// timing traces the launch to first-frame to first-text load path to stderr when
	// NYALA_DEBUG_TIMING is set, and is a disabled no-op timer otherwise. startSession
	// arms it per session under mu before the frame and snapshot goroutines start, so
	// each reads it once at its top; a nil timer is a valid disabled timer, so an
	// unstarted session (a unit test) is safe.
	timing *loadTimer

	// Proxy config parsed once in New, read-only afterwards. Unlike the
	// mutex-guarded group above, these are set before any goroutine starts and
	// never written again, so they need no mu guard.
	proxy    ProxyConfig // parsed proxy settings for the launch path
	proxyErr error       // proxy parse error, returned later by launch

	// Capture wiring set once by EnableCapture before Navigate, read-only
	// afterwards. Like proxy above, the caller sets these before any goroutine
	// starts and never writes them again, so the listener and supervisor
	// goroutines read them without a mu guard. captureOn gates the phase timing
	// in composeFrame; recorder is the event sink the emit sites use behind a
	// nil check. Both stay unset on the production path, so it pays zero cost.
	captureOn bool // true once EnableCapture turned capture on
	// loadSpans is the capture-run load-timing recorder. EnableCapture allocates it
	// before any goroutine starts and hands it to the capture recorder; startSession
	// hands it to the per-session loadTimer, which records the four load-path
	// milestones into it. The capture driver reads it at bundle-finalise time via the
	// recorder's LoadTimings. It stays nil off the capture path, so the loadTimer
	// records nothing and production pays only its existing nil checks.
	loadSpans *sessionTimings
	// settleHold gates the initial-layout settle hold on textReady. The capture driver
	// sets it true only for a single-frame capture (n == 1), where the one bundled
	// frame must be the settled page, and leaves it false for a multi-frame capture,
	// where the extra frames come from the load period a settle wait would drain. It
	// takes the same set-once treatment as captureOn: written once before Navigate,
	// read-only on the snapshot goroutine afterwards. Off capture it stays false.
	settleHold  bool
	recorder    captureRecorder // event sink, nil off the capture path
	painted     chan struct{}   // closed on the first contentful paint (or load) under capture
	paintedOnce sync.Once       // guards painted against a double close
	// textReady is the capture-only first-publish latch. The snapshot goroutine
	// closes it once the overlay has published and the initial layout has settled (the
	// first debounced-reflow re-snapshot, or a bounded backstop), so the capture driver
	// waits for the settled overlay runs to land before it counts a usable frame.
	// Without it the driver and the snapshot goroutine both wake off painted and the
	// driver can latch the first frame before the (calibration-delayed) publish, so the
	// captured frame carries no text; without the settle hold it can latch a pre-settle
	// snapshot a post-paint reflow would still shift. It stays nil off the capture path.
	textReady     chan struct{}
	textReadyOnce sync.Once // guards textReady against a double close
	// scrollSettle is the capture-only post-scroll settle signal. The snapshot
	// goroutine pokes it (latest-wins, capacity 1) after the trailing settle
	// snapshot publishes the text at the resting scroll position, so the capture
	// scroll hook can wait for a publish that landed after its scroll injection
	// before it draws the scrolled frame. Without it the hook draws the first frame
	// whose bitmap shows the advanced scroll, which arrives before the debounced
	// settle re-snapshot, so the drawn frame carries the stale pre-scroll text. It
	// stays nil off the capture path.
	scrollSettle chan struct{}

	// Lifecycle. The supervisor goroutine owns the engine lifetime: it starts
	// each browser session, watches for a crash or a lost connection, rebuilds
	// from held state, and closes frames when Close stops it. closing signals a
	// real shutdown so the supervisor exits instead of rebuilding.
	closing    chan struct{} // closed by Close to stop the supervisor
	closeOnce  sync.Once     // guards closing against a double Close
	supDone    chan struct{} // closed when the supervisor goroutine has exited
	supervised bool          // true after the supervisor goroutine starts
	// navigated guards Navigate single-shot. The frame channel is created once by
	// the first Navigate and replaced only by Navigate; Resize and SetBackend never
	// touch it (the channel-stability invariant V3). A second Navigate is rejected
	// so two supervisors can never target the engine's session state at once.
	navigated bool
}

// Ensure ChromiumEngine satisfies Engine.
var _ Engine = (*ChromiumEngine)(nil)

// New returns a ChromiumEngine that discovers its executable from browserFlag,
// then the NYALA_BROWSER and CHROME_PATH environment variables, then PATH. It
// parses the proxy environment once and stores the config and any parse error
// for the launch path. The caller sets the backend and cell px (SetBackend,
// SetCellPx) and the grid (Resize) before Navigate, since the engine sizes the
// screencast capture from them.
func New(browserFlag string) *ChromiumEngine {
	proxy, proxyErr := parseProxyEnv()
	return &ChromiumEngine{
		backend:     "block",
		browserFlag: browserFlag,
		closing:     make(chan struct{}),
		proxy:       proxy,
		proxyErr:    proxyErr,
		ingest:      newIngestCounter(),
	}
}

// IngestFPS returns the rolling screencast-ingest rate over the trailing window,
// pruned to now, for the fps badge's In figure. It counts frames at the true ingest
// point before the latest-wins slot drops any, so it reports the pre-drop rate the
// paint meter cannot see; an idle page decays it to 0. It returns 0 when
// NYALA_FPS_TIMING is unset. It is the ingestReporter capability the viewer fps
// meter type-asserts, so the Engine seam stays unchanged.
func (e *ChromiumEngine) IngestFPS(now time.Time) float64 {
	return e.ingest.fps(now)
}

// IngestSummary returns the session ingest statistics for the fps quit summary: the
// frame count, mean/min/max/median ingest fps, worst inter-arrival gap, and the
// worker decode mean and max. It returns the zero value when NYALA_FPS_TIMING is
// unset. It is the ingestReporter capability the viewer fps meter reads.
func (e *ChromiumEngine) IngestSummary() render.IngestStats {
	return e.ingest.summary()
}

// EnableCapture turns on engine-phase timing and event recording, setting rec
// as the event sink. The caller sets it before Navigate, so the capture flag
// and recorder take the same set-once treatment as the proxy config: written
// once before any goroutine starts and read-only afterwards, so the listener
// and supervisor goroutines need no mu guard. Off the capture path EnableCapture
// is never called, so captureOn stays false, the recorder stays nil, and
// Frame.Stats stays nil. A nil rec leaves the recorder unset.
func (e *ChromiumEngine) EnableCapture(rec captureRecorder) {
	e.captureOn = true
	e.recorder = rec
	// The painted latch lets the capture driver wait for first contentful paint
	// before it counts usable frames, so a blank pre-paint frame is never one of
	// the n. It is created once here, before any goroutine starts, like the recorder
	// above. markPainted closes it on the first paint signal; a late reader still
	// sees the closed channel. The exported recorder relays it to the viewer's
	// CaptureSession, so the viewer never imports the engine package.
	e.painted = make(chan struct{})
	// The textReady latch lets the capture driver wait for the first overlay publish
	// before it counts a usable frame, so the first captured frame carries its text
	// runs rather than the pre-publish nil. It is created here alongside painted,
	// before any goroutine starts; markTextReady closes it on the first publish.
	e.textReady = make(chan struct{})
	// The scrollSettle channel lets the capture scroll hook wait for a text publish
	// that landed after its scroll injection. It is latest-wins, capacity 1, so a
	// burst of settle pokes collapses to one and the snapshot goroutine never blocks.
	e.scrollSettle = make(chan struct{}, 1)
	// The load-timing recorder holds the four load-path spans the per-session
	// loadTimer records. It is allocated here, before any goroutine starts, and shared
	// with the recorder so the capture driver reads the spans at finalise; startSession
	// hands the same holder to newLoadTimer.
	e.loadSpans = &sessionTimings{}
	if r, ok := rec.(*Recorder); ok {
		r.setPainted(e.painted)
		r.setTextReady(e.textReady)
		r.setScrollSettle(e.scrollSettle)
		r.setLoadSpans(e.loadSpans)
	}
}

// SetCaptureSettleHold gates the initial-layout settle hold on the capture
// first-publish latch. The capture driver calls it once before Navigate with
// hold=true only for a single-frame capture (n == 1), so that one bundled frame
// waits for the layout to settle; a multi-frame capture passes false, so the load
// period frames it counts are not drained by the wait. It is a set-once flag read on
// the snapshot goroutine, matching captureOn, so it needs no lock. Off the capture
// path the viewer never calls it, so settleHold stays false and production holds
// nothing.
func (e *ChromiumEngine) SetCaptureSettleHold(hold bool) {
	e.settleHold = hold
}

// markTextReady closes the textReady latch once the overlay has published and the
// initial layout has settled, so a late reader still sees the closed channel. The
// snapshot goroutine (runTextSync) holds it past the first publishSnapshot until the
// first debounced-reflow re-snapshot (networkAlmostIdle) lands, or a bounded backstop
// fires, so the first counted capture frame carries the settled overlay rather than a
// pre-settle snapshot. textReadyOnce guards the close because a crash rebuild restarts
// the snapshot goroutine. It is a no-op off the capture path, where textReady stays
// nil.
func (e *ChromiumEngine) markTextReady() {
	if e.textReady == nil {
		return
	}
	e.textReadyOnce.Do(func() { close(e.textReady) })
}

// pokeScrollSettle signals the capture scroll hook that a settle snapshot has
// published the text at the resting scroll position. The send is latest-wins on a
// capacity-1 channel, so it never blocks and a burst of settle snapshots collapses
// to one signal. It is a no-op off the capture path, where scrollSettle stays nil.
func (e *ChromiumEngine) pokeScrollSettle() {
	if e.scrollSettle == nil {
		return
	}
	select {
	case e.scrollSettle <- struct{}{}:
	default:
	}
}

// markPainted closes the painted latch, so a late reader still sees it closed. The
// primary fire source is the watchPaint listener on the Page.lifecycleEvent whose
// Name is "firstContentfulPaint"; navigateAsync calls it again on navigate-return
// as the bounded timeout backstop. paintedOnce guards the close because both
// sources may fire and a crash rebuild re-registers the listener. It is a no-op
// off the capture path, where painted stays nil.
func (e *ChromiumEngine) markPainted() {
	if e.painted == nil {
		return
	}
	e.paintedOnce.Do(func() { close(e.painted) })
}

// SetCellPx records the resolved terminal cell pixel size. The engine uses it to
// size the graphics-backend capture and to bin DOM text boxes to cells. The
// caller sets it before Navigate, with the backend and the grid.
func (e *ChromiumEngine) SetCellPx(w, h int) {
	e.mu.Lock()
	e.cellW, e.cellH = w, h
	e.mu.Unlock()
}

// SetLayoutWidth records the block-backend target layout width in CSS pixels. The
// engine uses it to compute the block device scale so a terminal wider than the
// target lays out at the target width. The caller sets it before Navigate,
// alongside the cell px and the grid.
func (e *ChromiumEngine) SetLayoutWidth(w int) {
	e.mu.Lock()
	e.layoutWidth = w
	e.mu.Unlock()
}

// Navigate launches the browser, loads url, and records it in engine state so a
// later crash recovery can re-navigate. It resolves the executable from the
// --browser flag held in browserFlag, launches a context sized to the current
// capture size, starts the latest-wins screencast and frame loop, then dispatches
// navigation without waiting for every page resource to finish loading. It is
// single-shot: the frame channel is created here once and replaced only here, so
// Resize and SetBackend (which call restartStream, never Navigate) keep a stable
// channel handle. A second Navigate is rejected so two supervisors cannot target
// the engine session state at once.
func (e *ChromiumEngine) Navigate(url string) error {
	e.mu.Lock()
	if channelClosed(e.closing) {
		e.mu.Unlock()
		return context.Canceled
	}
	if e.navigated {
		e.mu.Unlock()
		return fmt.Errorf("engine: Navigate already called; it is single-shot")
	}
	e.navigated = true
	e.url = url
	e.scrollX, e.scrollY, e.scrollYTarget = 0, 0, 0
	// A small frame-channel buffer keeps a transient restartStream stall from
	// parking the producer on an unbuffered frames <- frame send (Decision 3); the
	// control goroutine on the viewer side is the primary interactive-hang fix.
	e.frames = make(chan render.Frame, frameChannelBuffer)
	e.closing = make(chan struct{})
	e.supDone = make(chan struct{})
	e.closeOnce = sync.Once{}
	e.supervised = false
	e.mu.Unlock()

	crashed, err := e.startSession()
	if err != nil {
		e.teardownSession()
		e.mu.Lock()
		e.frames = nil
		e.closing = nil
		e.supDone = nil
		e.supervised = false
		e.navigated = false
		e.mu.Unlock()
		return err
	}
	if err := e.finishStartup(); err != nil {
		return err
	}
	go e.supervise(crashed)
	return nil
}

// Resize sets the terminal grid in cells and derives the capture size from the
// resolved cell px and the active backend. When the engine is already streaming
// it restarts the stream at the new capture size, so frames arrive at the new
// grid rather than the old one.
func (e *ChromiumEngine) Resize(cols, rows int) {
	e.mu.Lock()
	e.cols, e.rows = cols, rows
	e.captureW, e.captureH = e.captureSize(cols, rows)
	streaming := e.streamingLocked()
	e.mu.Unlock()
	if streaming {
		_ = e.restartStream()
	}
}

// streamingLocked reports whether a session is streaming, read under the caller's
// e.mu hold. It is true when either frame sink is set: the capture-path slot or the
// live-path decode-pool queue. Resize, SetBackend, and restartStream gate on it, so a
// live session (queue, no slot) restarts on a resize or backend switch as a capture
// session (slot) does.
func (e *ChromiumEngine) streamingLocked() bool {
	return e.slot != nil || e.rawQ != nil
}

// Frames returns the emitted-frame channel. The channel is stable across Resize
// and SetBackend (which restart the stream but keep the same channel) and is
// replaced only by Navigate, which is single-shot (invariant V3). So a consumer
// that caches the handle keeps reading across a resize, a backend switch, and a
// crash rebuild.
func (e *ChromiumEngine) Frames() <-chan render.Frame {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.frames
}

// SetBackend switches the active backend and recomputes the capture size, since
// the block backend captures a sub-cell grid and the graphics backends capture a
// cell-pixel bitmap. When the engine is already streaming it restarts the stream
// at the new capture size, so the next frames arrive sized for the new backend.
// It is not part of the Engine interface; the viewer push consumer (Phase 7)
// calls it on a Tab cycle.
func (e *ChromiumEngine) SetBackend(backend string) {
	e.mu.Lock()
	e.backend = backend
	e.captureW, e.captureH = e.captureSize(e.cols, e.rows)
	streaming := e.streamingLocked()
	e.mu.Unlock()
	if streaming {
		_ = e.restartStream()
	}
}

// SendKey forwards a key event to the page. Scroll keys (arrows, PageUp,
// PageDown) route to the wheel transport so scrolling does not depend on which
// page element holds focus. Any other key routes to DispatchKeyEvent for a
// focused element to receive, such as text typed into a form field.
func (e *ChromiumEngine) SendKey(k Key) {
	e.mu.Lock()
	// The wheel transport speaks CSS pixels, but rows*cellH is device pixels. Divide
	// by the layout scale Z so a Page key steps one CSS-pixel viewport height rather
	// than over-scrolling by Z on a wide block grid.
	pageStepPx := int(math.Round(float64(e.rows*e.cellH) / e.layoutScale()))
	ctx := e.ctx
	e.mu.Unlock()
	if dx, dy, ok := scrollKeyDelta(k.Code, pageStepPx); ok {
		e.dispatchWheel(dx, dy)
		return
	}
	if ctx == nil {
		return
	}
	ev := input.DispatchKeyEvent(input.KeyChar).
		WithText(k.Text).
		WithCode(k.Code).
		WithModifiers(input.Modifier(k.Mod))
	e.dispatchInput(ctx, ev)
}

// SendMouse forwards a mouse event to the page. A wheel event routes to the
// wheel transport with its CSS-pixel deltas; the engine updates its owned scroll
// offset there.
func (e *ChromiumEngine) SendMouse(m MouseEvent) {
	if m.DeltaX != 0 || m.DeltaY != 0 {
		e.dispatchWheel(m.DeltaX, m.DeltaY)
	}
}

// SendScroll scrolls the page down by dy CSS pixels through the same wheel
// transport a user wheel or scroll key takes, so the capture scroll hook
// exercises the full input chain. It is the capture-only vertical scroll
// injection; a non-positive dy is a no-op.
func (e *ChromiumEngine) SendScroll(dy int) {
	if dy <= 0 {
		return
	}
	e.dispatchWheel(0, dy)
}

// dispatchWheel scrolls the page by dx, dy CSS pixels through the one scroll
// transport: a mouseWheel DispatchMouseEvent at the viewport centre. It advances
// the engine's held scroll offset by the dispatched deltas, which crash recovery
// reads to restore the page position after a rebuild.
//
// On the block backend the vertical scroll is quantised to whole cells: every raw
// delta accumulates into scrollYTarget, that target snaps to the nearest whole
// cellCSSH, and the actual dispatched vertical delta closes the gap to the snapped
// offset. So the DOM scroll offset the screencast metadata and the snapshot report
// always advances in whole cells, which removes the fractional-cell row aliasing (R2)
// and lets composeFrame pair a run set to the bitmap on scroll equality once the
// settle re-snapshot lands. Snapping the accumulated target rather than each raw delta
// keeps repeated small scrolls from drifting. Horizontal scroll and graphics backends
// are unchanged.
// synthesizeScrollGesture is deliberately not used.
func (e *ChromiumEngine) dispatchWheel(dx, dy int) {
	e.mu.Lock()
	e.scrollX += dx
	e.scrollYTarget += dy
	dyDispatch := dy
	if e.backend == "block" {
		if _, cellH := e.snapshotCellPx(); cellH > 0 {
			cells := math.Round(float64(e.scrollYTarget) / cellH)
			snapped := int(math.Round(cells * cellH))
			dyDispatch = snapped - e.scrollY
			e.scrollY = snapped
		} else {
			// No cell height to snap against: track scrollY exactly and realign the
			// target, so a later block scroll that can snap starts from the current
			// offset with no stale snap residue to correct.
			e.scrollY += dy
			e.scrollYTarget = e.scrollY
		}
	} else {
		// Off the block backend the target tracks scrollY exactly, so a return to
		// block accumulates from the current offset and dispatches no corrective jump.
		e.scrollY += dy
		e.scrollYTarget = e.scrollY
	}
	ctx := e.ctx
	cx := float64(e.cols*e.cellW) / 2
	cy := float64(e.rows*e.cellH) / 2
	e.mu.Unlock()
	if ctx == nil {
		return
	}
	if dx == 0 && dyDispatch == 0 {
		return
	}
	ev := input.DispatchMouseEvent(input.MouseWheel, cx, cy).
		WithDeltaX(float64(dx)).
		WithDeltaY(float64(dyDispatch))
	e.dispatchInput(ctx, ev)
}

// Close tears down the engine and the browser process. It signals the supervisor
// to stop instead of rebuilding, then waits for the supervisor to tear down the
// live session and close the frames channel. The supervisor is the single owner
// of the session fields, so Close never touches them concurrently with a rebuild.
// Teardown drains the slot before cancelling the context, mirroring the readKeys
// done-channel pattern, so no consumer goroutine blocks on a slot or a send that
// will never complete. closeOnce makes a second Close a no-op rather than a double
// channel close.
func (e *ChromiumEngine) Close() error {
	e.mu.Lock()
	closing := e.closing
	supDone := e.supDone
	supervised := e.supervised
	e.mu.Unlock()
	// Emit one stop at the clean session end so the event stream shows a tidy
	// close. Crash rebuilds tear down through teardownSession without this, so
	// only the real shutdown records a stop.
	if e.recorder != nil {
		e.recorder.Record(captureEvent{Type: captureEventScreencastControl, Action: "stop"})
	}
	if closing != nil {
		e.closeOnce.Do(func() { close(closing) })
	}
	if closing == nil || supDone == nil { // Navigate was never called or startup failed.
		e.teardownSession()
		return nil
	}
	if !supervised {
		e.teardownSession()
		return nil
	}
	<-supDone
	return nil
}
