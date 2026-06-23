package engine

import (
	"context"
	"sync"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
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
	// rebuild): ctx, cancel, slot, captureW, captureH, cols, rows, scrollX,
	// scrollY, backend, cellW, cellH. Lock, copy the fields into locals or write
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

	// Frame plumbing.
	frames   chan render.Frame // emitted frames, created once and closed only on Close
	slot     *frameSlot        // latest-wins single-frame backpressure slot
	lastText []render.GlyphRun // last text matched to a bitmap

	// Lifecycle. The supervisor goroutine owns the engine lifetime: it starts
	// each browser session, watches for a crash or a lost connection, rebuilds
	// from held state, and closes frames when Close stops it. closing signals a
	// real shutdown so the supervisor exits instead of rebuilding.
	closing   chan struct{} // closed by Close to stop the supervisor
	closeOnce sync.Once     // guards closing against a double Close
	supDone   chan struct{} // closed when the supervisor goroutine has exited
}

// Ensure ChromiumEngine satisfies Engine.
var _ Engine = (*ChromiumEngine)(nil)

// New returns a ChromiumEngine that discovers its executable from browserFlag,
// then the NYALA_BROWSER and CHROME_PATH environment variables, then PATH. The
// caller sets the backend and cell px (SetBackend, SetCellPx) and the grid
// (Resize) before Navigate, since the engine sizes the screencast capture from
// them.
func New(browserFlag string) *ChromiumEngine {
	return &ChromiumEngine{backend: "block", browserFlag: browserFlag}
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
// capture size, runs the navigation, then starts the latest-wins screencast and
// the frame loop that pushes synced frames on Frames().
func (e *ChromiumEngine) Navigate(url string) error {
	e.url = url
	e.scrollX, e.scrollY = 0, 0
	e.frames = make(chan render.Frame)
	e.closing = make(chan struct{})
	e.supDone = make(chan struct{})
	crashed, err := e.startSession()
	if err != nil {
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

// Frames returns the emitted-frame channel.
func (e *ChromiumEngine) Frames() <-chan render.Frame {
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
	_ = chromedp.Run(ctx, ev)
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
	_ = chromedp.Run(ctx, ev)
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
	if e.closing == nil { // Navigate was never called.
		e.teardownSession()
		return nil
	}
	e.closeOnce.Do(func() { close(e.closing) })
	<-e.supDone
	return nil
}
