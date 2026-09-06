package engine

import (
	"context"
	"errors"
	"fmt"
	"image"
	"math"
	"sync"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/wimpysworld/nyala/internal/render"
)

// ChromiumEngine drives one headless-Chromium page over CDP and emits native
// pixels or direct PNG frames with their crisp DOM text synced on. It holds the
// URL and scroll offset in its own state, not inside chromedp, so it can rebuild and
// re-navigate after a crash. Keep this struct thin: the screencast loop,
// snapshot decode, frame sync, and launch logic live in their own files.
type ChromiumEngine struct {
	// mu guards the session and geometry fields read or written from both the
	// viewer goroutine (Resize, SetBackend, SendKey, SendMouse, SendScroll,
	// dispatchWheel) and the supervisor goroutine (startSession,
	// teardownSession, launch, rebuild): ctx, cancel, acks, text, frame, cols,
	// rows, scrollX, scrollY, scrollYTarget, click, backend, cellW, cellH,
	// profileDir, remoteURL, profileGuard, connectSharedOp, windowID, inputFailures and
	// fontForcingID. Lock, copy the fields into locals or write the updates,
	// unlock, then run the chromedp I/O on the locals. Never hold mu across a
	// chromedp.Run or a channel send, or it deadlocks with the supervisor.
	mu sync.Mutex

	// Session context, bounded graceful close, and raw cancellation. For a
	// private engine ctx is chromedp's first context and closeBrowserOp closes
	// the browser. For a shared profile ctx is this instance's target context
	// and closeBrowserOp is sharedCloseOp, whose closures hold the websocket
	// cancel. teardownSession snapshots and clears all three together.
	ctx            context.Context
	closeBrowserOp func(context.Context) error
	cancel         context.CancelFunc
	// closeTimeout is a lifecycle-test override. Production uses
	// gracefulCloseTimeout.
	closeTimeout time.Duration

	// Page state held outside chromedp for crash recovery.
	url     string // launch URL, recorded once by Navigate and never rewritten
	scrollX int    // current scroll offset X in CSS pixels
	scrollY int    // dispatched scroll offset Y in CSS pixels (block: whole-cell snapped)

	// currentURL is the last committed main-frame URL, recorded by
	// watchLifecycle from Page.frameNavigated and kept fresh by
	// Page.navigatedWithinDocument for SPA and fragment navigation. It is
	// display state only: crash recovery and NavHome navigate to url. Empty
	// before the first commit, when CurrentURL falls back to the launch URL.
	currentURL string

	// currentTitle is the last recorded page title, read by watchLifecycle
	// through a bounded document.title evaluate in the isolated world on
	// main-frame navigation and kept fresh by Target.targetInfoChanged for SPA
	// title changes. It is display state only. Empty when the page has no
	// title; the viewer owns the host fallback.
	currentTitle string

	// browserProduct and browserUserAgent are the raw Browser.getVersion values.
	// userAgentMetadata is the matching native navigator.userAgentData value.
	// They are fetched before the first override and cached for the engine lifetime,
	// so a crash rebuild can reapply the same wire identity without refetching.
	browserProduct    string
	browserUserAgent  string
	userAgentMetadata *userAgentMetadata

	// webgl is the WebGL support snapshot probeWebGL records once per engine
	// and WebGL reports. It stays WebGLUnknown until a session probe answers,
	// and a crash rebuild reuses a known answer without probing again.
	webgl render.WebGLState

	// scrollYTarget is the raw intended vertical scroll offset in CSS pixels before
	// whole-cell snapping. On the block backend dispatchWheel accumulates every raw
	// wheel delta here and snaps this target to the nearest whole cellCSSH to derive
	// scrollY, so repeated small scrolls track the true intent and do not drift. Off
	// the block backend it tracks scrollY exactly.
	scrollYTarget int

	// find is the page-search state FindState reports: the active query, the
	// match counts, and whether the last dispatch is in flight or failed. It
	// sits beside the scroll state under mu, and resetFind drops it on every
	// main-frame commit because the new document carries no highlights.
	find FindState

	// loadState is the page-load progress snapshot LoadState reports. It sits
	// beside the scroll state under mu. watchLifecycle advances it from
	// main-frame CDP events through the pure transitionLoadState, a navigation
	// failure sets the error state, and teardownSession clears it so a crash
	// rebuild never carries a stale state.
	loadState render.LoadState

	// click is the press-chain state SendMouse's clickCount synthesis carries
	// between presses (double within clickChainWindow on the same cell). It
	// sits beside the scroll state under mu; a stale chain after a rebuild is
	// harmless because the window expires it.
	click clickState

	// Resolved geometry from terminal cell detection and the active backend.
	cellW int // resolved cell width in pixels
	cellH int // resolved cell height in pixels

	backend      render.Backend // active terminal rendering backend
	browserFlag  string         // --browser value, empty for env/PATH discovery
	profileDir   string         // resolved profile directory, empty until configured
	remoteURL    string         // running Chromium to attach to, empty for a managed launch
	profileGuard *profileGuard  // held for the engine lifetime, including recovery
	// disableFrameRateLimit passes Chromium's launch-time
	// --disable-frame-rate-limit switch. It is set before Navigate and remains
	// false by default, preserving Chromium's normal frame scheduling.
	disableFrameRateLimit bool
	// diagnosticTextPaint keeps ordinary DOM glyph ink in the forced Block page.
	// It is set only by the CDP source-paint correctness pass before Navigate.
	diagnosticTextPaint bool
	// diagnosticFrameMeta attaches CDP screencast metadata to emitted frames.
	// It is set only by diagnostics before Navigate, so ordinary frame publication
	// performs no metadata conversion, allocation, or copy.
	diagnosticFrameMeta bool
	// diagnosticCalibration fields are guarded by mu and copied only into the
	// opt-in frame diagnostic sidecar.
	diagnosticCalibrationSerial    int64
	diagnosticCalibrationSucceeded bool
	diagnosticCalibratedFontSize   float64
	cols                           int // terminal grid width in cells
	rows                           int // terminal grid height in cells

	// fontForcingID is the ScriptIdentifier of the registered font-forcing
	// on-new-document script, empty when no forcing is active. Only the block backend
	// forces the page onto the monospace grid; syncFontForcing registers the script
	// and injects the live style when the active backend takes the overlay, and removes
	// both when it does not. It is reset per session by teardownSession so a stale id
	// never targets a fresh session's script list.
	fontForcingID page.ScriptIdentifier

	// forcedCellW and forcedCellH record the fractional CSS-pixel cell pair the
	// registered font-forcing stylesheet was built with. The stylesheet bakes the
	// pair at build time (line-height cellH, seed font-size from cellW) and
	// calibration rewrites only the font-size, so syncFontForcing compares the
	// current snapshotCellPx pair against these to detect a stale sheet (a
	// terminal font change moves Z with cellW) and refresh it. Guarded by mu;
	// reset per session by teardownSession alongside fontForcingID.
	forcedCellW float64
	forcedCellH float64

	// layoutShiftID is the ScriptIdentifier of the registered layout-shift observer
	// on-new-document script, empty when no observer is active. It rides the block
	// backend's font-forcing lifecycle: syncFontForcing registers the observer script
	// and the Runtime binding when the block backend takes the overlay, and removes
	// both when it does not, so a layout shift on the live page re-decodes the text.
	// It is reset per session by teardownSession alongside fontForcingID.
	layoutShiftID page.ScriptIdentifier

	// layoutShiftCtxID is the Runtime execution context id of the layoutShiftWorld
	// isolated world on the main frame, 0 when no observer announced itself. The
	// observer script calls the binding once at install with the 'announce' payload
	// and the engine records the context id the call arrives on, so the snapshot
	// goroutine reads the generation counter in the same world the observer writes
	// it in. A navigation builds a fresh world, so the next announce replaces it.
	// It is reset per session by teardownSession alongside layoutShiftID.
	layoutShiftCtxID runtime.ExecutionContextID

	// layoutShiftActive reports whether the layout-shift observer is wanted, so a
	// binding callback that was already queued when cleanup ran cannot restore a dead
	// execution context id. enableLayoutShift sets it before the binding registration,
	// because runImmediately can deliver the install announce before the observer
	// script id comes back, and both disableLayoutShift and teardownSession clear it.
	// An announce is recorded only while it is set. Guarded by mu.
	layoutShiftActive bool

	// sessionActive reports whether a live session owns the load state, so a
	// lifecycle or navigation callback that was already queued when teardown ran
	// cannot repopulate the dead session's load indicator. startSession sets it
	// before the lifecycle listeners register, and teardownSession clears it
	// before it clears the load state, in the layoutShiftActive pattern. A load
	// event is applied only while it is set, except the startup navigation
	// error, which is recorded after teardown by design. Guarded by mu.
	sessionActive bool

	// cursorCtxID is the Runtime execution context id of the isolated world the
	// hover cursor read creates for itself, 0 until a hover creates it. It is
	// the fallback the read uses when layoutShiftCtxID is zero, which is every
	// hover on the Kitty and Sixel backends, where Block layout-shift forcing
	// never runs. The world reuses the layoutShiftWorld name, so a document
	// carries at most one extra world. A "Cannot find context" evaluate error
	// clears it, because a navigation invalidates isolated worlds, and the next
	// hover recreates it. It is reset per session by teardownSession alongside
	// layoutShiftCtxID. Guarded by mu.
	cursorCtxID runtime.ExecutionContextID

	// pointerCursor is the raw computed CSS cursor value read at the pointer on
	// the last hover move, empty before any read succeeds. A hover dispatch
	// evaluates the read in the layoutShiftWorld isolated world; a zero context
	// id or an evaluate error leaves the stored value unchanged. Guarded by mu.
	pointerCursor string

	// windowID is the browser window id for the driven target, fetched once per
	// session via browser.GetWindowForTarget and used to size the captured surface
	// with Browser.SetContentsSize on launch and on every restart. It is reset per
	// session by teardownSession so a stale id never sizes a fresh window.
	windowID browser.WindowID

	// inputFailures counts consecutive input-dispatch failures. A successful
	// dispatch resets it.
	inputFailures int

	// screencastCtl owns every screencast start and transition. After taking it, the
	// caller validates the session and backend before issuing CDP control calls. It is
	// never held together with e.mu across a CDP call, so the supervisor can cancel a
	// failing transition without deadlocking.
	screencastCtl sync.Mutex
	// screencastFmt is the last successfully started screencast format and JPEG
	// quality, guarded by e.mu. Block and Sixel use JPEG; direct Kitty uses PNG.
	screencastFmt screencastFmt
	// sizedSurface records the viewport sizing last applied to this session. The
	// launch path seeds it, so the post-commit initial capture does not repeat
	// Browser.SetContentsSize while the new document is becoming active.
	sizedSurfaceW int
	sizedSurfaceH int
	sizedScale    float64
	// screencastStarted distinguishes a published frame sink from a successful
	// StartScreencast, guarded by e.mu. A resize or backend change in that publication
	// window updates the held state but leaves the single initial start to apply it.
	screencastStarted bool
	// Frame plumbing.
	frames chan render.Frame // emitted frames, created once and closed only on Close
	// rawQ is the bounded, drop-oldest feed from the listener goroutine to the
	// parallel decode pool.
	// Reset per session by teardownSession.
	rawQ *rawFrameQueue
	// acks carries screencast frame SessionID values from the listener goroutine
	// to the ack-pump goroutine, which runs page.ScreencastFrameAck off the event
	// goroutine. Its unbounded FIFO keeps the listener independent of a slow CDP
	// command without dropping or reordering acknowledgements. ackPumpDone joins the
	// session pump during teardown. Both reset per session.
	acks        *ackQueue
	ackPumpDone chan struct{}
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
	// framePublisherDone closes when the live decode collector and all its workers
	// exit. teardownSession waits for it before draining the shared frame channel, so
	// a cancelled publisher cannot leave an old-session frame for recovery or race the
	// supervisor's channel close, and no in-flight decode failure can miss the summary.
	// Reset per session by teardownSession.
	framePublisherDone chan struct{}
	// snapshot is the per-iteration snapshot function runTextSync calls, defaulting
	// to snapshotRuns. The unit test swaps in a counter to assert the snapshot is
	// gated to about one call per delivered frame without launching a browser; it is
	// nil in production, where runTextSync falls back to snapshotRuns. It returns the
	// decoded runs, the DOM scroll offset the runs were decoded at,
	// the layout generation read in the same sequence, and the updated hold state.
	snapshot func(context.Context, overlayState) ([]render.GlyphRun, scrollOffset, int64, overlayState)

	// kittyDecode selects the kitty-shm decode mode, guarded by e.mu. When true
	// and the active backend takes direct payloads, decode workers decode each
	// PNG screencast frame into a pooled RGBA plane (pendingFrame.release owns
	// its return) instead of passing the encoded payload through. The viewer
	// sets it through SetKittyDecode when it selects the shm transport.
	kittyDecode bool
	// decode is the decoded-RGBA test override. Frame-loop and decode-pool tests swap
	// in a fake so they can drive queued frames without image payloads. It is nil in
	// production, where consumers keep decoder-native pixels or direct Kitty PNG.
	decode func(string) (*image.RGBA, error)

	// The screencast operation hooks are nil in production. Stream-control and
	// acknowledgement tests replace them with deterministic barriers and failures
	// without launching Chromium.
	stopScreencastOp  func(context.Context) error
	startScreencastOp func(context.Context, int, int, screencastFmt) error
	applySizingOp     func(context.Context, browser.WindowID, int, int, float64) error
	ackScreencastOp   func(context.Context, int64) error
	navigateOp        func(context.Context, string) (cdp.FrameID, cdp.LoaderID, bool, error)
	beforeInitialLock func()
	// startSessionOp is nil in production. Recovery tests replace it to prove
	// rebuild ordering without launching Chromium.
	startSessionOp func() (chan struct{}, error)
	// connectSharedOp is nil in production, where launchShared dials with
	// connectShared. Launch tests replace it to drive the adopt, spawn, and
	// retry decisions without a websocket.
	connectSharedOp connectFunc

	// lifecycle tracks navigation commits and main-frame first contentful paint for
	// one session. It is installed before navigation and cleared on teardown.
	lifecycle *sessionLifecycle

	// ingest counts delivered screencast frames at the true ingest point
	// before the live queue drops any, for the
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
	// touch it, preserving channel stability. A second Navigate is rejected
	// so two supervisors can never target the engine's session state at once.
	navigated bool
}

// New returns a ChromiumEngine configured with the browser preference and proxy
// environment. Executable discovery is deferred until Navigate starts a browser
// session. The caller sets the backend, cell px, and grid before Navigate because
// the engine derives the screencast capture size from them.
func New(browserFlag string) *ChromiumEngine {
	proxy, proxyErr := parseProxyEnv()
	return &ChromiumEngine{
		backend:     render.BackendBlock,
		browserFlag: browserFlag,
		closing:     make(chan struct{}),
		proxy:       proxy,
		proxyErr:    proxyErr,
		ingest:      newIngestCounter(),
	}
}

// SetProfileDir resolves and records the browser profile directory. Call it
// before Navigate. It does not create, inspect, or lock the directory.
func (e *ChromiumEngine) SetProfileDir(flag string) error {
	profileDir, err := resolveProfileDir(flag)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.profileDir = profileDir
	e.mu.Unlock()
	return nil
}

// SetRemoteDebuggingURL records the debugging endpoint of a Chromium that is
// already running, either http://host:port or ws://host:port/devtools/browser/<id>.
// A set URL excludes the managed launch: launch attaches to that browser instead
// of resolving an executable or a profile. Call it before Navigate.
func (e *ChromiumEngine) SetRemoteDebuggingURL(url string) {
	e.mu.Lock()
	e.remoteURL = url
	e.mu.Unlock()
}

// SetDisableFrameRateLimit controls Chromium's launch-time
// --disable-frame-rate-limit switch. Call it before Navigate.
func (e *ChromiumEngine) SetDisableFrameRateLimit(disable bool) {
	e.mu.Lock()
	e.disableFrameRateLimit = disable
	e.mu.Unlock()
}

// SetDiagnosticFrameMeta controls whether emitted frames carry CDP screencast
// metadata. Diagnostics call it before Navigate; production leaves it disabled.
func (e *ChromiumEngine) SetDiagnosticFrameMeta(enabled bool) {
	e.mu.Lock()
	e.diagnosticFrameMeta = enabled
	e.mu.Unlock()
}

// IngestFPS returns the rolling screencast-ingest rate over the trailing window,
// pruned to now, for the fps badge's In figure. It counts frames at the true ingest
// point before the live queue drops any, so it reports the pre-drop rate the
// paint meter cannot see; an idle page decays it to 0. It returns 0 when
// NYALA_FPS_TIMING is unset. It is the ingestReporter capability the viewer fps
// meter type-asserts, so the viewer's required engine contract stays unchanged.
func (e *ChromiumEngine) IngestFPS(now time.Time) float64 {
	return e.ingest.fps(now)
}

// IngestSummary returns the session ingest statistics for the session-end report:
// the frame count, mean/min/max/median ingest fps, worst inter-arrival gap, the
// worker decode mean and max, decode failures, and the acknowledgement round-trip
// mean and max. When NYALA_FPS_TIMING is unset, only decode-failure fields can be
// non-zero so the viewer can warn once without arming the timing counters. It is
// the ingestReporter capability the viewer reads.
func (e *ChromiumEngine) IngestSummary() render.IngestStats {
	return e.ingest.summary()
}

// SetCellPx records the resolved terminal cell pixel size. The engine uses it to
// size the graphics-backend capture and to bin DOM text boxes to cells. The
// caller sets it before Navigate, with the backend and the grid.
func (e *ChromiumEngine) SetCellPx(w, h int) {
	e.mu.Lock()
	e.cellW, e.cellH = w, h
	e.mu.Unlock()
}

// SetDiagnosticTextPaint controls whether Block font forcing keeps ordinary DOM
// glyph ink in the screencast. Diagnostics set it before Navigate to measure
// paint displacement. Production leaves the zero value false for separate layers.
func (e *ChromiumEngine) SetDiagnosticTextPaint(enabled bool) {
	e.mu.Lock()
	e.diagnosticTextPaint = enabled
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
	// parking the producer while the viewer's control goroutine handles a resize or
	// backend change.
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
	e.screencastCtl.Lock()
	defer e.screencastCtl.Unlock()
	e.mu.Lock()
	e.cols, e.rows = cols, rows
	streaming := e.screencastStartedLocked()
	e.mu.Unlock()
	if streaming {
		_ = e.restartStreamLocked()
	}
}

// streamingLocked reports whether a session has published a frame sink, read under
// the caller's e.mu hold. The initial transition uses it before StartScreencast.
func (e *ChromiumEngine) streamingLocked() bool {
	return e.rawQ != nil
}

// screencastStartedLocked reports whether the published session has completed its
// first StartScreencast, read under the caller's e.mu hold. Resize and SetBackend use
// it so a pre-start change updates held state without issuing StopScreencast.
func (e *ChromiumEngine) screencastStartedLocked() bool {
	return e.streamingLocked() && e.screencastStarted
}

// Frames returns the emitted-frame channel. The channel is stable across Resize
// and SetBackend (which restart the stream but keep the same channel) and is
// replaced only by Navigate, which is single-shot. A consumer
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
// The viewer push consumer calls it through its local interface on a backend cycle.
func (e *ChromiumEngine) SetBackend(backend render.Backend) {
	e.screencastCtl.Lock()
	defer e.screencastCtl.Unlock()
	e.mu.Lock()
	e.backend = backend
	streaming := e.screencastStartedLocked()
	e.mu.Unlock()
	if streaming {
		_ = e.restartStreamLocked()
	}
}

// SendKey forwards a key event to the page. Stroke keys (Tab, Enter,
// Backspace, Escape, the arrows, Home, End, and the Page keys) dispatch a full
// rawKeyDown/char/keyUp sequence so the page gets the browser-native default
// action: focus traversal with the focus ring, activation of the focused
// element, editing of the focused field, dismissal of page state, and, for the
// caret keys, Chromium's own choice between moving the caret in a focused field
// and scrolling the page. A caret key's dispatch then adopts the page's settled
// scroll offset, because that default action moves the page under the offset
// the engine holds. Any other key routes to DispatchKeyEvent for a focused
// element to receive; typed text takes SendText instead.
func (e *ChromiumEngine) SendKey(k Key) {
	ctx := e.inputCtx()
	if ctx == nil {
		return
	}
	if params := keyStrokeParams(k.Code, k.Mod); params != nil {
		tasks := keyStrokeTasks(params)
		if strokeScrolls(k.Code) {
			e.dispatchScrollingStroke(ctx, tasks)
			return
		}
		e.dispatchInput(ctx, tasks)
		return
	}
	ev := input.DispatchKeyEvent(input.KeyChar).
		WithText(k.Text).
		WithCode(k.Code).
		WithModifiers(input.Modifier(k.Mod))
	e.dispatchInput(ctx, ev)
}

// SendScroll scrolls the page down by dy CSS pixels through the same wheel
// transport a user's pointer wheel takes. Diagnostic tests use it to
// exercise the full input chain; a non-positive dy is a no-op.
func (e *ChromiumEngine) SendScroll(dy int) {
	if dy <= 0 {
		return
	}
	e.dispatchWheel(0, dy)
}

// dispatchWheel scrolls the page by dx, dy CSS pixels at the viewport centre.
// Keyboard scrolling and crash-recovery replay take this path, so their
// behaviour is position-independent and unchanged by pointer wheels. Neither
// carries a modifier, so it dispatches none.
func (e *ChromiumEngine) dispatchWheel(dx, dy int) {
	e.dispatchWheelFrom(dx, dy, 0, false, 0, 0)
}

// dispatchWheelAt scrolls the page by dx, dy CSS pixels through the same
// transport with the event positioned at the given terminal cell and carrying
// the report's CDP modifier bits, so a pointer wheel scrolls the scrollable
// element under the pointer rather than the viewport centre and a page reading
// shiftKey or ctrlKey on the wheel event sees what the user held.
func (e *ChromiumEngine) dispatchWheelAt(dx, dy, mod, col, row int) {
	e.dispatchWheelFrom(dx, dy, mod, true, col, row)
}

// dispatchWheelFrom scrolls the page by dx, dy CSS pixels through the one scroll
// transport: a mouseWheel DispatchMouseEvent, positioned at the cell centre when
// atCell is set and at the viewport centre otherwise. It advances
// the engine's held scroll offset by the dispatched deltas, which crash recovery
// reads to restore the page position after a rebuild.
//
// On the block backend the vertical scroll is quantised to whole cells: every raw
// delta accumulates into scrollYTarget, that target snaps to the nearest whole
// cellCSSH, and the actual dispatched vertical delta closes the gap to the snapped
// offset. So the DOM scroll offset the screencast metadata and the snapshot report
// always advances in whole cells, which removes fractional-cell row aliasing
// and lets composeFrame pair a run set to the bitmap on scroll equality once the
// settle re-snapshot lands. Snapping the accumulated target rather than each raw delta
// keeps repeated small scrolls from drifting. The quantisation applies to keyboard
// and pointer wheels alike. Horizontal scroll and graphics backends are unchanged.
// synthesizeScrollGesture is deliberately not used.
func (e *ChromiumEngine) dispatchWheelFrom(dx, dy, mod int, atCell bool, col, row int) {
	e.mu.Lock()
	e.scrollX += dx
	e.scrollYTarget += dy
	dyDispatch := dy
	if e.backend.QuantisesScroll() {
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
	cx, cy := e.wheelPositionLocked(atCell, col, row)
	e.mu.Unlock()
	if ctx == nil {
		return
	}
	if dx == 0 && dyDispatch == 0 {
		return
	}
	e.dispatchInput(ctx, wheelParams(cx, cy, dx, dyDispatch, mod))
}

// adoptScrollLocked adopts the page's own settled scroll offset as the engine's
// held offset, called with e.mu held. A find reveal and a caret key both move
// the page without a wheel delta the engine sent, so the held offset has to
// follow the page; otherwise the wheel transport, the whole-cell snap, and the
// crash-recovery replay all work from an offset the page never had.
func (e *ChromiumEngine) adoptScrollLocked(sx, sy float64) {
	e.scrollX = int(math.Round(sx))
	e.scrollY = int(math.Round(sy))
	e.scrollYTarget = e.scrollY
}

// snapCellHeightLocked returns the CSS-pixel cell height a page-side scroll
// snaps to so the crisp text overlay stays on the grid, read under the caller's
// e.mu hold. It is 0 when the backend does not quantise scroll or no cell
// height is resolved yet, which leaves a page-side scroll unsnapped.
func (e *ChromiumEngine) snapCellHeightLocked() float64 {
	if !e.backend.QuantisesScroll() {
		return 0
	}
	if _, cellH := e.snapshotCellPx(); cellH > 0 {
		return cellH
	}
	return 0
}

// wheelPositionLocked returns the CSS-pixel wheel dispatch position, read
// under the caller's e.mu hold: the pointer cell centre when atCell is set,
// the viewport centre otherwise.
func (e *ChromiumEngine) wheelPositionLocked(atCell bool, col, row int) (cx, cy float64) {
	if atCell {
		cellW, cellH := e.snapshotCellPx()
		return cellCentreCSS(col, row, e.cols, e.rows, cellW, cellH)
	}
	return wheelCentre(e.cols, e.rows, e.cellW, e.cellH, e.layoutScale())
}

// Close tears down the engine and what it owns of the browser. A private engine
// owns the Chromium process. On a shared profile the engine owns one target and
// one websocket, and it terminates the process only when it is the last client
// (sharedCloseOp). It then releases the profile guard last, after the session is
// torn down, so the last-client decision reads a still-held lock. It signals the
// supervisor to stop instead of rebuilding, then waits for the supervisor to tear
// down the live session and close the frames channel. The supervisor is the single owner
// of the session fields, so Close never touches them concurrently with a rebuild.
// Teardown drains the slot before cancelling the context, mirroring the readKeys
// done-channel pattern, so no consumer goroutine blocks on a slot or a send that
// will never complete. closeOnce makes a second Close a no-op rather than a double
// channel close.
func (e *ChromiumEngine) Close() (err error) {
	defer func() {
		err = errors.Join(err, e.releaseProfileGuard())
	}()
	e.mu.Lock()
	closing := e.closing
	supDone := e.supDone
	supervised := e.supervised
	e.mu.Unlock()
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
