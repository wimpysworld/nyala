package viewer

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

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
	// SetBackend names the active backend (block, sixel, kitty) so the engine
	// sizes the capture: sub-cell for block, cell-pixel for the graphics
	// backends. When streaming it restarts the stream at the new size.
	SetBackend(name string)
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
// detection did not confirm it. layoutWidth is the block-backend target layout
// width in CSS pixels, handed to the engine before Navigate.
func Stream(eng Engine, url string, tty *os.File, colourFlag, mode string, layoutWidth int) (err error) {
	fd, st, restore, err := setupViewer(eng, tty, colourFlag, layoutWidth)
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
	// (Decision 3). It stops when Stream returns and closes done.
	ctl := newControl(eng, done)
	defer ctl.close()

	// The cell differ drives the live block paint: each draw emits only the cells
	// that changed since the previous frame, with CUP and SGR coalescing. It is
	// reset on every full clear (first draw and resize) so a diff never skips a
	// cell the clear blanked. The graphics backends ignore it.
	diff := &block.GridDiff{}
	st.SetDiff(diff)

	s := &streamSession{eng: eng, st: st, tty: tty, fd: fd, keySender: keySender, ctl: ctl, diff: diff}
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
// closes. Both the control restart queue and the key sender route through it.
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
// enqueues restarts on. done stops the goroutine when Stream returns.
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

// close lets the goroutine drain through the shared done channel; it is a no-op
// placeholder so the caller can defer a symmetric close beside the other viewer
// goroutines. The goroutine exits on done.
func (c *control) close() {}

// setupViewer performs the shared viewer setup both Stream and the capture
// driver need: it checks tty is a terminal, reads the grid size, puts the
// terminal into raw mode, builds the State, runs backend and colour detection,
// and hands the resolved backend, cell px, and layout width to the engine before
// Navigate so the engine sizes its screencast capture and block device scale. It
// returns the tty fd, the State, and
// a restore closure the caller must defer to leave raw mode. It does not enter
// the full-screen mode or write the loading cue, because the capture driver must
// not mirror those bytes into its terminal-stream capture.
func setupViewer(eng Engine, tty *os.File, colourFlag string, layoutWidth int) (fd uintptr, st *State, restore func(), err error) {
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

	// Detection runs in raw mode but before the full-screen switch and the key
	// reader, so the handshake replies are not consumed by the key goroutine.
	configureBackends(&state, tty, cols, rows, colourFlag)

	// Hand the resolved backend, cell px, and layout width to the engine before
	// Navigate; the engine sizes the screencast capture and the block device scale
	// from them.
	eng.SetCellPx(state.W, state.H)
	eng.SetLayoutWidth(layoutWidth)
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
	if _, err := io.WriteString(s.tty, prefix); err != nil {
		return err
	}
	s.firstDrawn = true
	return s.st.RenderStreamFrame(frame, s.tty)
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
	act := s.st.HandleKey(ev.b)
	if act.Quit {
		return true
	}
	if act.Redraw && ev.b == keyTab {
		// Cycle the backend off the consumer loop: the engine restart runs on the
		// control goroutine so a slow restartStream cannot freeze draw or quit.
		name := backendName(s.st.Backend)
		s.ctl.submit(func(eng Engine) { eng.SetBackend(name) })
	}
	return false
}

// keySender serialises forwarded browser keys away from the input loop. The
// one-slot buffer keeps quit keys responsive if a browser scroll call stalls. It
// owns its done channel and closes it once on close, stopping the worker.
type keySender struct {
	worker *coalescingWorker[string]
	done   chan struct{}
	once   sync.Once
}

func newKeySender(eng Engine) *keySender {
	done := make(chan struct{})
	return &keySender{
		worker: startCoalescingWorker(done, eng.SendKey),
		done:   done,
	}
}

func (s *keySender) send(code string) {
	if code == "" {
		return
	}
	s.worker.submit(code)
}

func (s *keySender) close() {
	s.once.Do(func() {
		close(s.done)
	})
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
