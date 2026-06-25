package engine

import (
	"context"
	"fmt"
	"sync"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/input"
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
	// rebuild): ctx, cancel, slot, acks, text, captureW, captureH, cols, rows, scrollX,
	// scrollY, backend, cellW, cellH, windowID, inputFailures. Lock, copy the fields into locals or write
	// the updates, unlock, then run the chromedp I/O on the locals. Never hold mu
	// across a chromedp.Run or a channel send, or it deadlocks with the supervisor.
	mu sync.Mutex

	// Browser context and its cancel, owned by chromedp.
	ctx    context.Context
	cancel context.CancelFunc

	// Page state held outside chromedp for crash recovery.
	url     string // current URL
	scrollX int    // current scroll offset X in CSS pixels
	scrollY int    // current scroll offset Y in CSS pixels

	// Resolved geometry from the Stage 1 cell px and the active backend.
	cellW       int    // resolved cell width in pixels
	cellH       int    // resolved cell height in pixels
	backend     string // active backend (block, sixel, kitty)
	browserFlag string // --browser value, empty for env/PATH discovery
	cols        int    // terminal grid width in cells
	rows        int    // terminal grid height in cells
	captureW    int    // capture width in pixels for the active backend
	captureH    int    // capture height in pixels for the active backend

	// windowID is the browser window id for the driven target, fetched once per
	// session via browser.GetWindowForTarget and used to size the captured surface
	// with Browser.SetContentsSize on launch and on every restart. It is reset per
	// session by teardownSession so a stale id never sizes a fresh window.
	windowID browser.WindowID

	// inputFailures counts consecutive input-dispatch failures. A successful
	// dispatch resets it; a run past inputDispatchFailureThreshold records one
	// capture event so a wedged browser is observable without spamming.
	inputFailures int

	// Frame plumbing.
	frames chan render.Frame // emitted frames, created once and closed only on Close
	slot   *frameSlot        // latest-wins single-frame backpressure slot
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
	// networkAlmostIdle late reflow. Both are capacity-1 latest-wins, goroutine-
	// scoped to one session, so a burst coalesces to one wake and ctx cancellation
	// stops the goroutine without a channel reset. Reset per session by
	// teardownSession.
	reSnap chan struct{}
	reflow chan struct{}
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
	// nil in production, where runTextSync falls back to snapshotRuns.
	snapshot func(context.Context, overlayState) ([]render.GlyphRun, float64, overlayState)

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
	captureOn   bool            // true once EnableCapture turned capture on
	recorder    captureRecorder // event sink, nil off the capture path
	painted     chan struct{}   // closed on the first contentful paint (or load) under capture
	paintedOnce sync.Once       // guards painted against a double close

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
	}
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
	if r, ok := rec.(*Recorder); ok {
		r.setPainted(e.painted)
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
	e.scrollX, e.scrollY = 0, 0
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
	streaming := e.slot != nil
	e.mu.Unlock()
	if streaming {
		_ = e.restartStream()
	}
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
	streaming := e.slot != nil
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
	pageStepPx := e.rows * e.cellH
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

// dispatchWheel scrolls the page by dx, dy CSS pixels through the one scroll
// transport: a mouseWheel DispatchMouseEvent at the viewport centre. It advances
// the engine's held scroll offset by the dispatched deltas, which crash recovery
// reads to restore the page position after a rebuild.
// synthesizeScrollGesture is deliberately not used.
func (e *ChromiumEngine) dispatchWheel(dx, dy int) {
	e.mu.Lock()
	e.scrollX += dx
	e.scrollY += dy
	ctx := e.ctx
	cx := float64(e.cols*e.cellW) / 2
	cy := float64(e.rows*e.cellH) / 2
	e.mu.Unlock()
	if ctx == nil {
		return
	}
	ev := input.DispatchMouseEvent(input.MouseWheel, cx, cy).
		WithDeltaX(float64(dx)).
		WithDeltaY(float64(dy))
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
