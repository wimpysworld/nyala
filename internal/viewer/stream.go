package viewer

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	cterm "github.com/charmbracelet/x/term"

	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// Engine is the frame source the viewer draws. The viewer declares the methods
// it calls so it never imports the engine package, keeping the dependency one
// way (app wires the concrete engine to this interface). The viewer hands the
// engine the detected backend and cell px before Navigate, because the engine
// sizes its screencast capture from them. Frames carries decoded bitmaps with
// their crisp text already synced on. The viewer owns no scroll state; it
// forwards scroll keys by their code and wheel deltas in CSS pixels, and the
// engine owns the offset.
type Engine interface {
	// EnableCapture turns on engine-phase capture for this run and wires the
	// bundle's cdp-events.ndjson sink into the engine before Navigate. The caller
	// passes the open ndjson writer; the returned CaptureSession exposes the
	// cdp-session.json bytes the engine assembles during navigation, read after
	// Navigate returns. Stream never calls it, so the interactive path pays no
	// capture cost. The adapter constructs the engine recorder, so the viewer
	// never imports the engine package.
	EnableCapture(ndjson io.Writer) CaptureSession
	// SetCaptureSettleHold gates the engine's initial-layout settle hold on the
	// capture first-publish latch. The capture driver calls it with hold=true only for
	// a single-frame capture (n == 1), so the one bundled frame waits for the layout to
	// settle; a multi-frame capture passes false so the load-period frames it counts
	// are not drained by the wait. Stream never calls it, so the interactive path holds
	// nothing.
	SetCaptureSettleHold(hold bool)
	// SetBackend names the active backend (block, sixel, kitty) so the engine
	// sizes the capture: sub-cell for block, cell-pixel for the graphics
	// backends. When streaming it restarts the stream at the new size.
	SetBackend(name string)
	// SetScheme names the active block glyph scheme (quadrant, half, sextant) so
	// the engine sizes the block capture: quadrant samples 2x4 source pixels per
	// cell, half 1x2, sextant 2x3. It is set once before Navigate, so unlike
	// SetBackend it needs no live stream restart.
	SetScheme(name string)
	// SetCellPx sets the resolved terminal cell pixel size the engine uses to
	// size the graphics capture and to bin DOM text boxes to cells.
	SetCellPx(w, h int)
	// SetLayoutWidth sets the block-backend target layout width in CSS pixels the
	// engine uses to compute the block device scale.
	SetLayoutWidth(w int)
	// Resize sets the terminal grid in cells; the engine recomputes the capture
	// size and, when streaming, restarts the stream.
	Resize(cols, rows int)
	// Navigate loads url and starts emitting frames.
	Navigate(url string) error
	// Frames returns the channel of decoded frames, each with Text synced on.
	Frames() <-chan render.Frame
	// SendKey forwards a key by its code (for example ArrowDown) to the page.
	// Scroll keys route to the engine's wheel transport; the engine owns the
	// offset.
	SendKey(code string)
	// SendScroll scrolls the page down by dy CSS pixels through the engine's wheel
	// transport. The capture scroll hook calls it; the interactive path does not.
	SendScroll(dy int)
	// Close tears down the engine and the browser process.
	Close() error
}

// CaptureSession exposes the cdp-session.json bytes the engine assembles during
// navigation. The capture driver obtains it from Engine.EnableCapture and reads
// SessionJSON after Navigate returns to write cdp-session.json into the bundle.
// It is the one viewer-side seam onto the engine recorder, so the viewer never
// imports the engine package.
type CaptureSession interface {
	// SessionJSON returns the assembled cdp-session.json bytes, or nil before
	// navigation fills them.
	SessionJSON() []byte
	// Painted returns a channel the engine closes on the first contentful paint
	// (or load fallback). The capture driver waits on it before counting usable
	// frames, so a blank pre-paint frame is never one of the n. A nil channel reads
	// as never closed, so the driver's paint deadline bounds the wait.
	Painted() <-chan struct{}
	// TextReady returns a channel the engine closes on the first overlay publish.
	// The capture driver waits on it after paint, so the first counted frame carries
	// its decoded text runs rather than the pre-publish nil. A nil channel reads as
	// never closed, so the driver's deadline bounds the wait.
	TextReady() <-chan struct{}
	// ScrollSettle returns a channel the engine pokes after the settle snapshot
	// publishes the text at the resting scroll position. The capture scroll hook
	// drains it before injecting the scroll, then waits for a fresh poke so it draws
	// the scrolled frame with the settled overlay rather than the stale pre-scroll
	// text. A nil channel reads as never poked, so the hook's idle timer bounds the
	// wait.
	ScrollSettle() <-chan struct{}
	// LoadTimings returns a snapshot of the four load-path milestone spans the engine
	// recorded during launch (process spawn, screencast start, first frame, first
	// text), each the elapsed milliseconds since launch. The capture driver reads it at
	// finalise and writes it into performance.json's load_timings; a zero field means
	// that milestone did not fire.
	LoadTimings() render.LoadTimings
}

// Stream runs the interactive viewer on tty, drawing the live frames eng pushes,
// until the user quits. It puts the terminal into raw mode, detects which
// backends the terminal supports, hands the resolved backend and cell px to the
// engine so it sizes the screencast capture, then navigates to url. It writes a
// plain-text loading cue before the first frame and clears it on the first draw.
// It re-fits and restarts the stream on resize, forwards scroll keys and the
// wheel to the engine, cycles the backend on Tab, and restores the terminal on
// exit. It returns an error only on a setup or I/O failure; a clean quit returns
// nil. colourFlag is the --color intent; configureBackends resolves it against
// NO_COLOR and detection. mode is the --backend mode: auto and block always
// pass, and a forced graphics backend is rejected before navigation when
// detection did not confirm it. blocks is the --blocks mode selecting the block
// glyph scheme; Stream resolves it with resolveScheme against os.Environ(), so
// auto and sextant render sextant on a supported terminal and quadrant otherwise,
// quadrant forces quadrant, and half renders half-block.
// layoutWidth is the block-backend target layout
// width in CSS pixels, handed to the engine before Navigate.
func Stream(eng Engine, url string, tty *os.File, colourFlag, mode, blocks string, layoutWidth int) (err error) {
	scheme := resolveScheme(blocks, os.Environ())
	// The live fps meter counts every drawn frame for the badge and the quit
	// summary. Its printSummary defer is registered before setupViewer's restore
	// below, so LIFO runs it last: after raw mode and the full-screen escapes are
	// undone, so the summary line lands readable on the normal screen. The meter is
	// nil unless NYALA_FPS_TIMING is set, so an off run pays only nil checks.
	fps := newFPSMeter(os.Environ())
	defer fps.printSummary(os.Stdout)
	fd, st, restore, err := setupViewer(eng, tty, colourFlag, scheme, layoutWidth)
	if err != nil {
		return err
	}
	defer restore()

	if verr := ValidateForcedBackend(mode, st.Enabled); verr != nil {
		return verr
	}
	// Pin the active backend to a named --backend mode now that validation
	// passed, so a forced block overrides a higher-fidelity detected backend.
	// setupViewer handed the engine the detected backend; re-notify it so the
	// engine sizes its capture for the forced backend, not the detected one.
	applyForcedBackend(st, mode)
	eng.SetBackend(backendName(st.Backend))

	if _, err := io.WriteString(tty, enterFullScreen); err != nil {
		return err
	}
	// Registered after the Restore defer, so LIFO runs it first: re-enable
	// auto-wrap and show the cursor while still in raw mode, then Restore.
	defer func() { _, _ = io.WriteString(tty, exitFullScreen) }()

	// Close the engine on exit so the browser process and the frame loop tear
	// down with the terminal.
	defer func() { _ = eng.Close() }()

	if _, werr := io.WriteString(tty, fmt.Sprintf("loading %s\r\n", url)); werr != nil {
		return werr
	}
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	// done stops the key-reader goroutine when Stream returns, so a goroutine
	// blocked on a send after the consumer stops selecting cannot leak.
	done := make(chan struct{})
	defer close(done)

	keySender := newKeySender(eng)
	defer keySender.close()

	// The control goroutine runs the engine restarts (Resize, SetBackend) off the
	// consumer loop, so a slow restartStream cannot freeze draw, keys, and quit
	// (Decision 3). It stops when Stream returns and the deferred close(done) fires.
	ctl := newControl(eng, done)

	// The cell differ drives the live block paint: each draw emits only the cells
	// that changed since the previous frame, with CUP and SGR coalescing. It is
	// reset on every full clear (first draw and resize) so a diff never skips a
	// cell the clear blanked. The graphics backends ignore it.
	diff := &block.GridDiff{}
	st.SetDiff(diff)

	s := &streamSession{eng: eng, st: st, tty: tty, fd: fd, keySender: keySender, ctl: ctl, diff: diff, fps: fps}
	keys := readKeys(tty, done)
	navigated := make(chan error, 1)
	go func() {
		navigated <- eng.Navigate(url)
	}()
	if nerr := s.waitForNavigate(winch, keys, navigated); nerr != nil {
		return nerr
	}
	return s.loop(winch, keys)
}

// coalescingWorker runs one handler serially off a producer loop. It holds a
// one-slot work channel; submit sends non-blocking, so a fresh item supersedes
// an unprocessed older one and the producer never blocks (latest-wins-drop). The
// goroutine selects on the work channel and a done channel and exits when done
// closes. The control restart queue routes through it; the scroll key sender
// accumulates instead of dropping, so it does not.
type coalescingWorker[T any] struct {
	work chan T
	done <-chan struct{}
	fn   func(T)
}

// startCoalescingWorker starts the goroutine and returns the worker handle the
// producer submits on. done stops the goroutine; fn runs each submitted item
// serially. The work channel is buffered one, so submit drops on full.
func startCoalescingWorker[T any](done <-chan struct{}, fn func(T)) *coalescingWorker[T] {
	w := &coalescingWorker[T]{work: make(chan T, 1), done: done, fn: fn}
	go w.run()
	return w
}

// submit enqueues one item with a non-blocking send. If an item is already
// queued the new one is dropped, so the producer loop never blocks on a wedged
// handler. The drop is safe wherever a fresh item supersedes the queued one.
func (w *coalescingWorker[T]) submit(item T) {
	select {
	case w.work <- item:
	default:
	}
}

// run processes items serially until done closes. Running them here, not on the
// producer loop, keeps that loop responsive while a handler blocks.
func (w *coalescingWorker[T]) run() {
	for {
		select {
		case item := <-w.work:
			w.fn(item)
		case <-w.done:
			return
		}
	}
}

// control runs engine restarts off the viewer consumer loop. The loop enqueues a
// resize or a backend switch with a non-blocking send; this goroutine runs the
// engine call (which may block in restartStream) serially, so a slow restart
// never freezes draw, keys, or quit. It selects on done and exits when Stream
// returns. The one-slot buffer plus the drop-on-full send keeps the loop
// responsive: a coalesced restart is fine because each request re-reads the
// current size and backend from the engine state.
type control struct {
	eng    Engine
	worker *coalescingWorker[func(Engine)]
}

// newControl starts the control goroutine and returns the handle the loop
// enqueues restarts on. done stops the goroutine when Stream returns; the caller
// owns done and closes it, so control needs no close of its own.
func newControl(eng Engine, done <-chan struct{}) *control {
	return &control{eng: eng, worker: startCoalescingWorker(done, func(fn func(Engine)) { fn(eng) })}
}

// submit enqueues one engine restart for the control goroutine. The send is
// non-blocking: if a restart is already queued the request is dropped, because
// the queued one will re-read the latest size and backend when it runs. So the
// consumer loop never blocks on a wedged restart.
func (c *control) submit(fn func(Engine)) {
	c.worker.submit(fn)
}

// setupViewer performs the shared viewer setup both Stream and the capture
// driver need: it checks tty is a terminal, reads the grid size, puts the
// terminal into raw mode, builds the State, runs backend and colour detection,
// and hands the resolved backend, cell px, and layout width to the engine before
// Navigate so the engine sizes its screencast capture and block device scale. It
// returns the tty fd, the State, and
// a restore closure the caller must defer to leave raw mode. It does not enter
// the full-screen mode or write the loading cue, because the capture driver must
// not mirror those bytes into its terminal-stream capture. scheme is the resolved
// block glyph scheme; it sets the scheme on the State and hands its engine name to
// the engine before Navigate. The caller resolves it: Stream via resolveScheme
// with detection, Capture via resolveCaptureScheme without.
func setupViewer(eng Engine, tty *os.File, colourFlag string, scheme block.Scheme, layoutWidth int) (fd uintptr, st *State, restore func(), err error) {
	fd = tty.Fd()
	if !cterm.IsTerminal(fd) {
		return 0, nil, nil, fmt.Errorf("viewer: %s is not a terminal", tty.Name())
	}
	cols, rows, err := cterm.GetSize(fd)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("viewer: terminal size: %w", err)
	}

	oldState, err := cterm.MakeRaw(fd)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("viewer: raw mode: %w", err)
	}

	// The image size is unknown until the first frame; NewState seeds a square
	// and each drawn frame updates ImgW/ImgH before Fit and the block compose.
	state := NewState(cols, rows, 1, 1)
	// Set the resolved block glyph scheme, overriding the NewState quadrant default
	// for this run. The caller resolves the --blocks mode; auto keeps quadrant on the
	// capture path, so capture and corpus are unchanged.
	state.Scheme = scheme

	// Detection runs in raw mode but before the full-screen switch and the key
	// reader, so the handshake replies are not consumed by the key goroutine.
	if err := configureBackends(&state, tty, cols, rows, colourFlag); err != nil {
		_ = cterm.Restore(fd, oldState)
		return 0, nil, nil, err
	}

	// Hand the resolved backend, cell px, and layout width to the engine before
	// Navigate; the engine sizes the screencast capture and the block device scale
	// from them.
	eng.SetCellPx(state.W, state.H)
	eng.SetLayoutWidth(layoutWidth)
	eng.SetScheme(schemeEngineName(state.Scheme))
	eng.SetBackend(backendName(state.Backend))
	eng.Resize(cols, rows)

	return fd, &state, func() { _ = cterm.Restore(fd, oldState) }, nil
}

// streamSession bundles the per-run viewer state the push loop mutates: the
// engine it forwards to, the State it draws from, the tty it writes to, and the
// tty fd it reads the resized geometry from. firstDrawn gates the resize clear
// so no clear lands before the loading cue is replaced by a real frame.
type streamSession struct {
	eng          Engine
	st           *State
	tty          *os.File
	fd           uintptr
	keySender    *keySender
	ctl          *control
	diff         *block.GridDiff
	firstDrawn   bool
	nilImageOnce sync.Once // guards the one-shot nil-image diagnostic
	resizeOnce   sync.Once // guards the one-shot dropped-resize diagnostic
	// fps is the live fps counter, non-nil only when NYALA_FPS_TIMING is set. When
	// set, draw stamps each frame and repaints a rolling usable-fps badge after the
	// grid (at most once per second); when nil the draw path pays only a nil check,
	// so production is unchanged.
	fps *fpsMeter
}

// loop runs the push consumer until the frames or keys channel closes, a quit
// key arrives, or an I/O error occurs. It draws each pushed frame, restarts the
// stream on SIGWINCH at the recomputed grid, and forwards keys. It caches the
// frame channel once: the engine keeps the channel stable across Resize and
// SetBackend and replaces it only by Navigate, which is single-shot (invariant
// V3), so a backend switch or a resize never strands this cached handle.
func (s *streamSession) loop(winch <-chan os.Signal, keys <-chan keyEvent) error {
	frames := s.eng.Frames()
	for {
		select {
		case <-winch:
			if err := s.resize(); err != nil {
				return err
			}
		case frame, ok := <-frames:
			if !ok {
				return nil
			}
			// Coalesce a queued burst to one paint: drain any frames already waiting
			// and keep only the newest, so a scroll burst draws once with the latest
			// frame instead of painting every stale frame. Channel close mid-drain
			// stops the loop, matching the main receive. The differ stays correct
			// because it compares the drawn frame against the previous drawn grid, so
			// skipped frames do not corrupt its state.
			frame, ok = drainFrames(frames, frame)
			if !ok {
				return nil
			}
			if err := s.draw(frame); err != nil {
				return err
			}
		case ev, ok := <-keys:
			if !ok {
				return nil
			}
			if s.handleKey(ev) {
				return nil
			}
		}
	}
}

// drainFrames keeps only the newest immediately-available frame, given the one
// already received. It pulls any further queued frames non-blocking and returns
// the last, coalescing a burst to one paint. The bool is false when the channel
// closed mid-drain, so the caller stops the loop the same way the main receive
// does; otherwise it is true with the newest frame.
func drainFrames(frames <-chan render.Frame, latest render.Frame) (render.Frame, bool) {
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				return latest, false
			}
			latest = f
		default:
			return latest, true
		}
	}
}

// waitForNavigate keeps the raw-mode controls live while engine startup runs.
// Browser launch and CDP setup can block before frames exist, so Ctrl-C and
// resize handling must not wait for Navigate to return.
func (s *streamSession) waitForNavigate(winch <-chan os.Signal, keys <-chan keyEvent, navigated <-chan error) error {
	for {
		select {
		case <-winch:
			if err := s.resize(); err != nil {
				return err
			}
		case ev, ok := <-keys:
			if !ok {
				return nil
			}
			if startupQuit(ev) {
				return nil
			}
		case err := <-navigated:
			return err
		}
	}
}

// startupQuit reports whether a pre-navigation key requests exit. Browser input
// and backend cycling are ignored until Navigate finishes and the live loop owns
// the page.
func startupQuit(ev keyEvent) bool {
	return ev.scroll == ScrollNone && (ev.b == 'q' || ev.b == 'Q' || ev.b == keyQuitCtrlC)
}

// draw renders one pushed frame. It updates the image size from the frame, then
// draws with the crisp DOM-text overlay folded on. The first draw emits the full
// clearScreen to wipe the loading cue; steady draws emit cursorHome only and
// overwrite every cell in place, because auto-wrap is off and each cell is
// rewritten, so nothing stale remains. Resize re-emits the full clear. A nil-image
// frame is skipped with a one-shot diagnostic so the dropped frame is observable.
//
// The whole frame, prefix and render bytes both, is bracketed in a DEC 2026
// synchronised update so the terminal presents it atomically and never shows a
// torn partial frame during a full refresh or a scroll. It wraps the block and the
// graphics paths alike, since both flow through RenderStreamFrame here.
func (s *streamSession) draw(frame render.Frame) error {
	if frame.Img == nil {
		s.nilImageOnce.Do(func() {
			fmt.Fprintln(os.Stderr, "nyala: dropped a frame with no image")
		})
		return nil
	}
	b := frame.Img.Bounds()
	s.st.ImgW, s.st.ImgH = b.Dx(), b.Dy()
	prefix := framePrefix(s.firstDrawn, s.diff)
	if _, err := io.WriteString(s.tty, beginSyncUpdate); err != nil {
		return err
	}
	if _, err := io.WriteString(s.tty, prefix); err != nil {
		return err
	}
	s.firstDrawn = true
	if err := s.st.RenderStreamFrame(frame, s.tty); err != nil {
		return err
	}
	if _, err := io.WriteString(s.tty, endSyncUpdate); err != nil {
		return err
	}
	// Paint the fps badge after the grid, gated behind NYALA_FPS_TIMING. It is nil
	// off the timing path, so production pays only this nil check. The meter counts
	// every draw but rebuilds the badge string at most once per second; the cached
	// escape-wrapped string is repainted each frame (cheap, no per-frame alloc) so
	// the grid draw does not leave it overwritten. The badge is a cursor-saved
	// overlay, so it never enters the block grid the differ tracks and leaves the
	// golden-gated compose bytes untouched.
	if s.fps != nil {
		now := time.Now()
		// A frame carrying text runs is the first-text-published milestone reaching
		// the viewer directly, no engine plumbing: it opens the warm-up gate so the
		// page-load stall stays out of the rate stats.
		s.fps.tick(now, len(frame.Text) > 0)
		s.paintFPS(s.fps.badge(now))
	}
	return nil
}

// paintFPS writes the cached badge string (cursor save, home, reverse-video rate,
// cursor restore) to the terminal. The save/restore brackets keep the badge off
// the grid cells the differ diffs, so the block differ's next CUP lands where it
// expects. The write is best-effort like the teardown escapes: a failure surfaces
// on the next draw.
func (s *streamSession) paintFPS(badge string) {
	_, _ = io.WriteString(s.tty, badge)
}

// resize re-fits to the new terminal size and restarts the stream at the
// recomputed capture size, so frames arrive at the new grid, not the old one. It
// clears the screen only once a frame has drawn, so the loading cue is not wiped
// before the first frame replaces it.
func (s *streamSession) resize() error {
	nc, nr, err := cterm.GetSize(s.fd)
	if err != nil {
		// A GetSize failure drops this resize: the engine keeps the old grid, so the
		// view desyncs until the next resize. A one-shot diagnostic makes it observable.
		s.resizeOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "nyala: dropped a resize (terminal size unavailable): %v\n", err)
		})
		return nil
	}
	s.st.Resize(nc, nr)
	// The engine restart runs on the control goroutine so a slow restartStream
	// never freezes the consumer loop; the loop keeps drawing and handling quit.
	s.ctl.submit(func(eng Engine) { eng.Resize(nc, nr) })
	if !s.firstDrawn {
		return nil
	}
	// The resize clears the screen, so the differ full-repaints the next frame
	// even when the grid dimensions are unchanged.
	s.diff.Reset()
	_, err = io.WriteString(s.tty, clearScreen)
	return err
}

// handleKey forwards one key to the engine or the state machine and reports
// whether the user asked to quit. A scroll key forwards its code; the engine
// owns the offset. Tab cycles the backend and restarts the stream at the new
// capture size.
func (s *streamSession) handleKey(ev keyEvent) (quit bool) {
	if ev.scroll != ScrollNone {
		code := scrollKeyCode(ev.scroll)
		if s.keySender != nil {
			s.keySender.send(code)
		} else {
			s.eng.SendKey(code)
		}
		return false
	}
	wasKitty := s.st.Backend == BackendKitty
	act := s.st.HandleKey(ev.b)
	if act.Quit {
		return true
	}
	if act.Redraw && ev.b == keyTab {
		if wasKitty && s.st.Backend != BackendKitty {
			// Free both double-buffer images when leaving kitty, so the terminal keeps
			// no stale image and a later return rebuilds from id 1. Best-effort like the
			// teardown escapes: a write failure surfaces on the next draw.
			_ = s.st.resetKitty(s.tty)
		}
		// Cycle the backend off the consumer loop: the engine restart runs on the
		// control goroutine so a slow restartStream cannot freeze draw or quit.
		name := backendName(s.st.Backend)
		s.ctl.submit(func(eng Engine) { eng.SetBackend(name) })
	}
	return false
}

// keySender serialises forwarded scroll keys away from the input loop, so a
// browser scroll call that stalls never freezes quit or draw. Unlike the frame
// path (latest-wins-drop), scrolling is cumulative: a rapid burst of scroll keys
// must all reach the engine, or the page barely moves. So it accumulates every
// submitted code and the worker forwards them all in order, letting the engine's
// wheel transport sum the burst (dispatchWheel adds each delta into its held
// offset) rather than dropping to the last key. send never blocks the input loop.
// It owns its done channel and closes it once on close, stopping the worker.
type keySender struct {
	fn      func(string)
	wake    chan struct{}
	done    chan struct{}
	mu      sync.Mutex
	pending []string
	once    sync.Once
}

func newKeySender(eng Engine) *keySender {
	s := &keySender{
		fn:   eng.SendKey,
		wake: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
	go s.run()
	return s
}

// send queues one scroll-key code for the worker with a non-blocking append, so
// the input loop never blocks on a stalled browser. The empty code is ignored.
// The queued code is never dropped: a burst accumulates and the worker forwards
// every code, so the engine sums the scroll instead of taking a single step.
func (s *keySender) send(code string) {
	if code == "" {
		return
	}
	s.mu.Lock()
	s.pending = append(s.pending, code)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// drain removes and returns all queued codes in submission order, resetting the
// buffer. The worker forwards the whole batch, so codes queued while a dispatch
// was in flight are summed by the engine, not lost.
func (s *keySender) drain() []string {
	s.mu.Lock()
	codes := s.pending
	s.pending = nil
	s.mu.Unlock()
	return codes
}

// run forwards queued codes serially off the input loop until done closes. A
// single wake may cover several appends because drain takes the whole batch, so
// no scroll key is dropped between the append and the drain.
func (s *keySender) run() {
	for {
		select {
		case <-s.wake:
			for _, code := range s.drain() {
				s.fn(code)
			}
		case <-s.done:
			return
		}
	}
}

func (s *keySender) close() {
	s.once.Do(func() {
		close(s.done)
	})
}

// fpsTimingEnv is the environment variable that turns on the live fps instrument:
// the rolling badge and the quit summary. Set to any non-empty value it arms the
// meter; unset it stays off. It follows the NYALA_* diagnostic-switch convention
// and is independent of NYALA_DEBUG_TIMING (the load-milestone stderr trace).
const fpsTimingEnv = "NYALA_FPS_TIMING="

// fpsWindow is the trailing span the badge averages usable-fps over, so it tracks
// the last second of draws rather than the whole session.
const fpsWindow = time.Second

// fpsBadgeInterval bounds how often the badge string is rebuilt, so the fps text
// repaints at most once per second: the meter counts every draw, but the badge
// string is regenerated no more than once per interval and the cached string is
// reused between rebuilds.
const fpsBadgeInterval = time.Second

// fpsWarmupFallback bounds the wait for the first-text-published milestone. A
// text-free page never trips the warm-up gate, so once this span has elapsed since
// Stream start the gate opens at the current frame anyway, so a text-free page
// still measures. It is generous enough that a normal text page gates on its text,
// not on this fallback.
const fpsWarmupFallback = 5 * time.Second

// fpsMeter tracks the live usable-frames-per-second figure for the NYALA_FPS_TIMING
// instrument: a rolling window feeds the badge and running accumulators feed the
// quit summary. The rate stats are gated to steady state: they accumulate only from
// the first text-bearing frame (the first-text-published milestone), so the page
// load stall stays out of the mean, median, min, max, and worst gap. It is nil
// unless the switch is set, so the draw path pays only a nil check when the switch
// is off. All access is on the single draw goroutine, so it needs no lock.
type fpsMeter struct {
	window time.Duration
	start  time.Time   // Stream construction, the warm-up span measures from here
	times  []time.Time // draw instants inside the trailing window, for the badge rate

	gated  bool          // set once the warm-up gate opened, so rate stats accumulate
	warmup time.Duration // start to the gate frame, the time-to-first-usable-frame
	gateAt time.Time     // the gate frame, the rate-window wall-clock start

	frames  int             // usable draws counted from the gate frame onward
	last    time.Time       // most recent counted draw, the wall-clock end
	prev    time.Time       // previous counted draw, to measure the inter-frame gap
	haveGap bool            // set once two counted draws exist, so min/max gaps are real
	minGap  time.Duration   // smallest inter-frame gap seen, the fastest frame
	maxGap  time.Duration   // largest inter-frame gap seen, the slowest frame
	gaps    []time.Duration // post-gate inter-frame gaps, sorted once at quit for the median

	badgeStr  string    // cached escape-wrapped badge, rebuilt at most once per interval
	badgeAt   time.Time // instant badgeStr was last rebuilt
	haveBadge bool      // set once badgeStr holds a real string
}

// newFPSMeter returns a meter when NYALA_FPS_TIMING is set in env, or nil
// otherwise. The env read happens once per Stream, so an off run does no per-frame
// work beyond the draw path's nil check. start is stamped now, so the warm-up span
// measures from Stream construction.
func newFPSMeter(env []string) *fpsMeter {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, fpsTimingEnv); ok && v != "" {
			return &fpsMeter{window: fpsWindow, start: time.Now()}
		}
	}
	return nil
}

// tick records a usable draw at now. hasText is true when the frame carries text
// runs. It always updates the trailing badge window (so the live badge tracks the
// load period too), but the rate stats accumulate only from the warm-up gate: the
// gate opens on the first text-bearing frame, or, for a text-free page, once
// fpsWarmupFallback has elapsed since start. From the gate frame onward it counts
// frames, the wall span, the inter-frame gaps (min, max, and the slice the median
// sorts at quit). It allocates nothing per call beyond the amortised window and gap
// slices, so an armed run stays cheap.
func (m *fpsMeter) tick(now time.Time, hasText bool) {
	cutoff := now.Add(-m.window)
	m.times = append(m.times, now)
	drop := 0
	for drop < len(m.times) && m.times[drop].Before(cutoff) {
		drop++
	}
	m.times = m.times[drop:]

	if !m.gated {
		if !hasText && now.Sub(m.start) < fpsWarmupFallback {
			return // still warming up: badge window updated, rate stats held
		}
		m.gated = true
		m.warmup = now.Sub(m.start)
		m.gateAt = now
	}

	if m.frames == 0 {
		m.gateAt = now
	} else {
		gap := now.Sub(m.prev)
		if !m.haveGap || gap < m.minGap {
			m.minGap = gap
		}
		if !m.haveGap || gap > m.maxGap {
			m.maxGap = gap
		}
		m.haveGap = true
		m.gaps = append(m.gaps, gap)
	}
	m.frames++
	m.prev = now
	m.last = now
}

// windowRate returns the rolling usable-fps over the trailing window. F draws in
// the window leave F-1 intervals over their span, so the rate is (F-1)/span. Fewer
// than two frames, or a non-positive span, report 0.
func (m *fpsMeter) windowRate() float64 {
	if len(m.times) < 2 {
		return 0
	}
	span := m.times[len(m.times)-1].Sub(m.times[0]).Seconds()
	if span <= 0 {
		return 0
	}
	return float64(len(m.times)-1) / span
}

// badge returns the escape-wrapped badge string to write this draw. It rebuilds
// the string (the only per-call allocation) at most once per fpsBadgeInterval and
// returns the cached string between rebuilds, so the fps text refreshes about once
// per second while the badge stays painted over every frame.
func (m *fpsMeter) badge(now time.Time) string {
	if !m.haveBadge || now.Sub(m.badgeAt) >= fpsBadgeInterval {
		m.badgeStr = fmt.Sprintf("\x1b7\x1b[1;1H\x1b[7m %.1f fps \x1b[0m\x1b8", m.windowRate())
		m.badgeAt = now
		m.haveBadge = true
	}
	return m.badgeStr
}

// printSummary writes one compact fps summary line to w after the session ends. It
// reports warmup_ms (time to the first counted frame, so the "loads slowly" signal
// stays visible), then the steady-state rate stats measured from that gate frame
// onward: frames drawn, wall seconds, mean and median fps, the min and max
// instantaneous fps (the slowest and fastest single frames), and the worst
// inter-frame gap in ms, which exposes a post-load stall a mean hides. A nil meter
// or a session whose gate never opened prints nothing, so an off run and an empty
// run are both silent.
func (m *fpsMeter) printSummary(w io.Writer) {
	if m == nil || m.frames == 0 {
		return
	}
	warmupMS := float64(m.warmup) / float64(time.Millisecond)
	wall := m.last.Sub(m.gateAt).Seconds()
	mean := 0.0
	if wall > 0 {
		mean = float64(m.frames-1) / wall
	}
	minInst, maxInst := 0.0, 0.0
	if m.haveGap {
		if s := m.maxGap.Seconds(); s > 0 {
			minInst = 1 / s
		}
		if s := m.minGap.Seconds(); s > 0 {
			maxInst = 1 / s
		}
	}
	worstGapMS := float64(m.maxGap) / float64(time.Millisecond)
	fmt.Fprintf(w, "nyala fps: warmup_ms=%.0f frames=%d wall=%.2fs mean=%.1f median=%.1f min=%.1f max=%.1f worst_gap_ms=%.1f\n",
		warmupMS, m.frames, wall, mean, medianFPS(m.gaps), minInst, maxInst, worstGapMS)
}

// medianFPS returns the median instantaneous fps from the collected inter-frame
// gaps: it sorts the gaps (once, at quit, off the draw path), takes the middle gap
// (averaging the two middle gaps for an even count), and inverts it to fps. The
// median ignores a one-off stall, so it reads the typical frame rate. An empty or
// non-positive-median slice reports 0.
func medianFPS(gaps []time.Duration) float64 {
	if len(gaps) == 0 {
		return 0
	}
	slices.Sort(gaps)
	n := len(gaps)
	med := gaps[n/2]
	if n%2 == 0 {
		med = (gaps[n/2-1] + gaps[n/2]) / 2
	}
	s := med.Seconds()
	if s <= 0 {
		return 0
	}
	return 1 / s
}

// resolveScheme maps the --blocks mode to a block.Scheme for the interactive
// path, which may detect terminal support. "half" selects half-block; "quadrant"
// selects quadrant; "auto" and "sextant" select sextant when SextantSupported(env)
// reports the terminal renders sextant blocks, and degrade to quadrant otherwise.
// Any other value, including the empty string, resolves to quadrant, the default
// that keeps behaviour unchanged.
func resolveScheme(blocks string, env []string) block.Scheme {
	switch blocks {
	case "half":
		return block.SchemeHalfBlock
	case "quadrant":
		return block.SchemeQuadrant
	case "auto", "sextant":
		if _, supported := SextantSupported(env); supported {
			return block.SchemeSextant
		}
		return block.SchemeQuadrant
	default:
		return block.SchemeQuadrant
	}
}

// resolveCaptureScheme maps the --blocks mode to a block.Scheme for the capture
// path, which is deterministic and runs no terminal detection. "sextant" forces
// sextant; "half" selects half-block; everything else, including "auto" and the
// empty string, resolves to quadrant, so the default capture and corpus sweeps
// are unchanged.
func resolveCaptureScheme(blocks string) block.Scheme {
	switch blocks {
	case "sextant":
		return block.SchemeSextant
	case "half":
		return block.SchemeHalfBlock
	default:
		return block.SchemeQuadrant
	}
}

// schemeEngineName maps a block.Scheme to the engine's scheme string, the name
// SetScheme accepts. It returns "half" for half-block, "sextant" for sextant, and
// "quadrant" otherwise, matching the engine's set-once scheme seam.
func schemeEngineName(s block.Scheme) string {
	switch s {
	case block.SchemeHalfBlock:
		return "half"
	case block.SchemeSextant:
		return "sextant"
	default:
		return "quadrant"
	}
}

// renderScheme maps a block.Scheme to the render package's BlockScheme, the enum
// render.BlockCaptureSize takes. The render package cannot import block, so this
// viewer-side helper bridges the two enums for the three schemes.
func renderScheme(s block.Scheme) render.BlockScheme {
	switch s {
	case block.SchemeHalfBlock:
		return render.BlockHalf
	case block.SchemeSextant:
		return render.BlockSextant
	default:
		return render.BlockQuadrant
	}
}

// backendName maps a Backend to the engine's backend string. The engine sizes
// its capture from this name: sub-cell for block, cell-pixel for the graphics
// backends.
func backendName(b Backend) string {
	switch b {
	case BackendSixel:
		return "sixel"
	case BackendKitty:
		return "kitty"
	default:
		return "block"
	}
}

// scrollKeyCode maps a recognised scroll key to the CDP key code the engine
// routes to its wheel transport. The viewer forwards the code; the engine owns
// the scroll offset.
func scrollKeyCode(k ScrollKey) string {
	switch k {
	case ScrollUp:
		return "ArrowUp"
	case ScrollDown:
		return "ArrowDown"
	case ScrollRight:
		return "ArrowRight"
	case ScrollLeft:
		return "ArrowLeft"
	case ScrollPageUp:
		return "PageUp"
	case ScrollPageDown:
		return "PageDown"
	default:
		return ""
	}
}
