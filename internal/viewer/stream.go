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
// detection did not confirm it.
func Stream(eng Engine, url string, tty *os.File, colourFlag, mode string) (err error) {
	fd, st, restore, err := setupViewer(eng, tty, colourFlag)
	if err != nil {
		return err
	}
	defer restore()

	if verr := ValidateForcedBackend(mode, st.Enabled); verr != nil {
		return verr
	}

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

	s := &streamSession{eng: eng, st: st, tty: tty, fd: fd, keySender: keySender}
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

// setupViewer performs the shared viewer setup both Stream and the capture
// driver need: it checks tty is a terminal, reads the grid size, puts the
// terminal into raw mode, builds the State, runs backend and colour detection,
// and hands the resolved backend and cell px to the engine before Navigate so
// the engine sizes its screencast capture. It returns the tty fd, the State, and
// a restore closure the caller must defer to leave raw mode. It does not enter
// the full-screen mode or write the loading cue, because the capture driver must
// not mirror those bytes into its terminal-stream capture.
func setupViewer(eng Engine, tty *os.File, colourFlag string) (fd uintptr, st *State, restore func(), err error) {
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

	// Hand the resolved backend and cell px to the engine before Navigate; the
	// engine sizes the screencast capture from them.
	eng.SetCellPx(state.W, state.H)
	eng.SetBackend(backendName(state.Backend))
	eng.Resize(cols, rows)

	return fd, &state, func() { _ = cterm.Restore(fd, oldState) }, nil
}

// streamSession bundles the per-run viewer state the push loop mutates: the
// engine it forwards to, the State it draws from, the tty it writes to, and the
// tty fd it reads the resized geometry from. firstDrawn gates the resize clear
// so no clear lands before the loading cue is replaced by a real frame.
type streamSession struct {
	eng        Engine
	st         *State
	tty        *os.File
	fd         uintptr
	keySender  *keySender
	firstDrawn bool
}

// loop runs the push consumer until the frames or keys channel closes, a quit
// key arrives, or an I/O error occurs. It draws each pushed frame, restarts the
// stream on SIGWINCH at the recomputed grid, and forwards keys.
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

// draw renders one pushed frame. It updates the image size from the frame, clears
// the loading cue (or the prior frame) so a resized or smaller frame leaves no
// stale cells, then draws with the crisp DOM-text overlay folded on.
func (s *streamSession) draw(frame render.Frame) error {
	if frame.Img == nil {
		return nil
	}
	b := frame.Img.Bounds()
	s.st.ImgW, s.st.ImgH = b.Dx(), b.Dy()
	if _, err := io.WriteString(s.tty, clearScreen); err != nil {
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
		return nil
	}
	s.st.Resize(nc, nr)
	s.eng.Resize(nc, nr)
	if !s.firstDrawn {
		return nil
	}
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
		s.eng.SetBackend(backendName(s.st.Backend))
	}
	return false
}

// keySender serialises forwarded browser keys away from the input loop. The
// one-slot buffer keeps quit keys responsive if a browser scroll call stalls.
type keySender struct {
	eng  Engine
	keys chan string
	done chan struct{}
	once sync.Once
}

func newKeySender(eng Engine) *keySender {
	s := &keySender{
		eng:  eng,
		keys: make(chan string, 1),
		done: make(chan struct{}),
	}
	go s.run()
	return s
}

func (s *keySender) send(code string) {
	if code == "" {
		return
	}
	select {
	case s.keys <- code:
	default:
	}
}

func (s *keySender) close() {
	s.once.Do(func() {
		close(s.done)
	})
}

func (s *keySender) run() {
	for {
		select {
		case code := <-s.keys:
			s.eng.SendKey(code)
		case <-s.done:
			return
		}
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
