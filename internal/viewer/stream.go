package viewer

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
// sizes its screencast capture from them. Frames carries native pixels or a
// direct Kitty PNG with crisp text already synced on. The viewer owns no scroll state; it
// forwards scroll keys by their code and wheel deltas in CSS pixels, and the
// engine owns the offset.
type Engine interface {
	// SetBackend sets the active backend so the engine
	// sizes the capture: sub-cell for block, cell-pixel for the graphics
	// backends. When streaming it restarts the stream at the new size.
	SetBackend(backend render.Backend)
	// SetCellPx sets the resolved terminal cell pixel size the engine uses to
	// size the graphics capture and to bin DOM text boxes to cells.
	SetCellPx(w, h int)
	// Resize sets the terminal grid in cells; the engine recomputes the capture
	// size and, when streaming, restarts the stream.
	Resize(cols, rows int)
	// Navigate loads url and starts emitting frames.
	Navigate(url string) error
	// Frames returns the channel of pixel or direct-PNG frames, each with Text synced on.
	Frames() <-chan render.Frame
	// SendKey forwards a key by its code (for example ArrowDown) to the page.
	// The codes Tab, ShiftTab, Enter, Backspace, Escape, SelectAll, and the
	// caret keys dispatch full key strokes for native focus traversal,
	// activation, field editing, dismissal, selection, and caret movement.
	// Chromium decides whether a caret key moves the caret or scrolls the page;
	// the engine then adopts the offset the page settled at.
	SendKey(code string)
	// SendText forwards typed text to the focused page element. The engine
	// picks the CDP transport: a single ASCII character goes as a full key
	// stroke, so the page sees the key press and Chromium keeps its default
	// action for it (a space types in a focused field and scrolls the page
	// otherwise), and multi-byte text inserts atomically.
	SendText(text string)
	// SendPaste inserts one pasted unit at the caret in the focused page
	// element. It is separate from SendText because a paste is not a key
	// press: the engine always inserts it atomically, so a one-character
	// paste reports no key press and runs no key default action. It reports
	// discarded only when the page proved the insert had no editable target,
	// so the bar can say the paste reached nothing; a dropped dispatch or a
	// frame the engine cannot read reports false, because neither proves
	// where the text went.
	SendPaste(text string) (discarded bool)
	// SendNav forwards one navigation request: reload, a history step, or the
	// launch page. The engine owns the launch URL and the navigation path.
	SendNav(action NavAction)
	// CurrentURL returns the last committed main-frame URL, falling back to
	// the launch URL before the first commit. The engine keeps it fresh
	// across SPA and fragment navigation; the viewer reads it on repaint.
	CurrentURL() string
	// OpenURL navigates to url as a normal navigation, pushing a history
	// entry. The engine resets its held scroll offset and never rewrites the
	// launch URL NavHome returns to.
	OpenURL(url string)
	// BrowserVersion returns the driven browser's product normalised for
	// display, for example "Chrome 138.0.7204.49", or an empty string when
	// unavailable.
	BrowserVersion() string
	// Find searches the page for query, highlighting every match and selecting
	// the first. Chromium owns the search and the highlights, which arrive as
	// ordinary frames; the page's own focus survives it, so the bar can hold
	// the text field while the page stays the search target. An empty query
	// clears the search.
	Find(query string)
	// FindNext selects the next match, wrapping past the last.
	FindNext()
	// FindPrev selects the previous match, wrapping past the first.
	FindPrev()
	// FindClear removes the search highlights and state from the page.
	FindClear()
	// FindState returns the search-state snapshot to draw. It never waits on
	// the browser, so a repaint always has a value: a search in flight reports
	// Pending with the previous counts, and a dropped dispatch reports Failed.
	FindState() FindState
	// Selection returns the text the page has selected, for an explicit copy to
	// the terminal clipboard, and whether that selection was over the copy
	// bound. An over-bound selection is refused rather than truncated, so it
	// carries no text and the viewer reports the refusal. Empty text with over
	// false means the page holds no selection or the read did not land, which
	// both mean nothing to copy. It reads the page, never the terminal
	// clipboard.
	Selection() (text string, over bool)
	// SendMouse forwards one decoded mouse event. A wheel step scrolls at the
	// pointer cell through the engine's wheel transport; a press or release
	// dispatches a mouse event at the cell centre. The engine owns the
	// cell-to-CSS conversion and clamps the cell to its grid.
	SendMouse(ev MouseEvent)
	// Close tears down the engine and the browser process.
	Close() error
}

// FindState is the page-search state the viewer draws, mirroring the engine's
// snapshot so the viewer never imports the engine package. Total is the match
// count and Current the 1-based selected match, 0 when the page holds none.
// Pending marks a dispatch in flight, with the previous counts still readable;
// Failed marks the last dispatch as dropped by a wedged browser, with the last
// known counts kept, because the highlights it painted are still on the page.
type FindState struct {
	Query   string
	Total   int
	Current int
	Pending bool
	Failed  bool
}

// NavAction identifies one page navigation request the viewer forwards over
// the Engine seam. The viewer holds no URL state: the engine recorded the
// launch URL at Navigate and owns the navigation path.
type NavAction int

const (
	// NavReload reloads the page (Ctrl+R or F5).
	NavReload NavAction = iota
	// NavBack goes one history entry back (Alt+Left).
	NavBack
	// NavForward goes one history entry forward (Alt+Right).
	NavForward
	// NavHome opens the launch URL as a normal navigation (Alt+Home), pushing
	// a history entry so NavBack returns to the prior page.
	NavHome
)

// kittyDecodeSetter is the optional engine capability the viewer toggles when
// the Kitty shm transport is in play. ChromiumEngine implements it; a fake
// engine that does not simply keeps sending direct PNG frames. The viewer
// type-asserts the Engine against it, so the Engine seam and all fakes stay
// unchanged.
type kittyDecodeSetter interface {
	// SetKittyDecode selects the kitty-shm decode mode: on, frames arrive as
	// pooled RGBA planes with a Release hook; off, as direct PNG payloads.
	SetKittyDecode(on bool)
}

// syncEngineKittyDecode aligns the engine's kitty-shm decode mode with the
// viewer state: on only while the active backend is Kitty and the shm
// transport is resolved. It runs at startup after the backend settles and on
// every backend cycle.
func syncEngineKittyDecode(eng Engine, st *State) {
	d, ok := eng.(kittyDecodeSetter)
	if !ok {
		return
	}
	d.SetKittyDecode(st.Backend == BackendKitty && st.kittySHMLive())
}

// fatalSignals are the terminating signals Stream's watcher catches so the
// terminal is restored before the process dies. SIGHUP arrives when the
// terminal window closes or an SSH connection drops, SIGINT and SIGQUIT from a
// kill in another shell, SIGTERM from a supervisor or a plain kill. Catching
// SIGINT cannot change interactive Ctrl+C: raw mode clears ISIG, so Ctrl+C
// arrives as byte 0x03 and quits through byteKeyAction, and only an external
// kill -INT ever reaches this list. SIGKILL is absent because no process can
// catch it, and a dropped connection defeats in-process cleanup either way.
var fatalSignals = []os.Signal{syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM}

// signalExitStatus returns the conventional shell exit status for a process a
// signal ended: 128 plus the signal number. So SIGTERM keeps 143 and SIGHUP,
// SIGINT, and SIGQUIT report 129, 130, and 131. A value carrying no signal
// number reports the 128 base, which still reads as signalled rather than as a
// clean exit.
func signalExitStatus(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 128
}

func handleFatalSignal(sig os.Signal, restore func(), closeEngine func() error, exit func(int)) {
	restore()
	_ = closeEngine()
	exit(signalExitStatus(sig))
}

// Stream runs the interactive viewer on tty, drawing the live frames eng pushes,
// until the user quits. It puts the terminal into raw mode, detects which
// backends the terminal supports, hands the resolved backend and cell px to the
// engine so it sizes the screencast capture, then navigates to url. It clears
// the screen and draws the bottom bar immediately, with a braille spinner in
// the indicator slot until the first frame replaces it with the backend glyph.
// It re-fits and restarts the stream on resize, forwards scroll keys, page keys,
// and the wheel to the engine, cycles the backend on Alt+R, and restores the
// terminal on exit. It returns an error only on a setup or I/O failure; a clean
// quit (Ctrl+Q, or the Ctrl+C interrupt fallback) returns nil. colourFlag is the
// --color intent; configureBackends resolves it against
// NO_COLOR and detection. mode is the --backend mode: a named mode picks the
// starting backend and Alt+R still cycles every supported backend; auto and
// block always pass, and a graphics start is rejected before navigation when
// detection did not confirm it. blocks is the --blocks mode selecting the block
// glyph scheme; Stream resolves it with resolveScheme against os.Environ(), so
// auto and sextant render sextant on a supported terminal and quadrant otherwise,
// quadrant forces quadrant, and half renders half-block.
// kittyTransport is the --kitty-transport intent (auto, direct, or shm);
// configureBackends resolves it against the probe result and the allow and
// deny lists. version is the Nyala build version (main.version, ldflags-set)
// the bar's About mode shows.
func Stream(eng Engine, url string, tty *os.File, colourFlag, mode, blocks, kittyTransport, version string) (err error) {
	scheme := resolveScheme(blocks, os.Environ())
	// The live fps meter counts every drawn frame for the badge and the quit
	// summary. The session summary defer is registered before setupViewer's restore
	// below, so LIFO runs it last: after raw mode and the full-screen escapes are
	// undone, so its line lands readable on the normal screen. The meter is nil
	// unless NYALA_FPS_TIMING is set.
	fps := newFPSMeter(os.Environ())
	// Read the optional ingest reporter on every session so an off run can report
	// decode failures at quit. When the meter is armed, the same reporter supplies
	// its live In rate and detailed summary. ChromiumEngine implements it; a fake
	// engine that does not leaves ingest nil.
	ingest, _ := eng.(ingestReporter)
	if fps != nil {
		fps.ingest = ingest
	}
	defer printStreamSummary(os.Stdout, fps, ingest)
	fd, st, restore, err := setupViewer(eng, tty, colourFlag, kittyTransport, scheme)
	if err != nil {
		return err
	}
	// restoreTerminal is the one terminal-restore path: the exit escapes
	// (mouse reporting off, auto-wrap and cursor back, and, once a frame
	// drew, the cursor moved below the frame), then the termios restore. The
	// deferred teardown and the fatal-signal watcher below share it through the
	// Once, so the restore runs exactly once however the session ends. It
	// reads firstDrawn through the session's atomic, so the watcher goroutine
	// never races the producer's write.
	var restoreOnce sync.Once
	var session *streamSession
	restoreTerminal := func() {
		restoreOnce.Do(func() {
			drawn := session != nil && session.firstDrawn.Load()
			_, _ = io.WriteString(tty, exitTerminal(st.Rows, drawn, st.kittyKeyboard))
			restore()
		})
	}
	defer restoreTerminal()

	if verr := st.validateForcedBackend(mode); verr != nil {
		return verr
	}
	if cerr := st.validateColourCompatibility(colourFlag); cerr != nil {
		return cerr
	}
	// Start on the named --backend mode now that validation passed, so a forced
	// block start overrides a higher-fidelity detected nomination while Alt+R
	// keeps the full detected cycle. setupViewer handed the engine the detected backend;
	// re-notify it so the engine sizes its capture for the starting backend, not
	// the detected one. The bottom-row bar reservation applies to every
	// backend, so a forced start never moves the engine grid setupViewer sized.
	applyForcedBackend(st, mode)
	eng.SetBackend(st.Backend)
	// The backend is final now, so arm the engine's kitty-shm decode when the
	// resolved transport is shm and the session starts on Kitty. Unlink any
	// unconsumed shm objects when the session ends, however it ends.
	syncEngineKittyDecode(eng, st)
	defer st.closeKittySHM()

	// Mouse reporting, bracketed paste, and the kitty keyboard protocol enable
	// here, beside the full-screen escapes and after the setupViewer handshake,
	// so none of them races the startup probe replies. Bracketed paste is on for
	// the whole session, not just the URL editor: with it off, a paste into the
	// page would arrive as loose bytes and type character by character, so a
	// pasted newline would submit a form. The keyboard protocol is pushed only
	// where the handshake found it, so every other terminal keeps the legacy
	// parser. restoreTerminal's exitTerminal write undoes all three on every
	// teardown, including the signal path; the defer is already registered
	// above, so an enable that fails half-way is still cleaned up.
	if _, err := io.WriteString(tty, enterTerminalModes(st.kittyKeyboard)); err != nil {
		return err
	}

	// Close the engine on exit so the browser process and the frame loop tear
	// down with the terminal.
	defer func() { _ = eng.Close() }()

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	// done stops the key-reader goroutine when Stream returns, so a goroutine
	// blocked on a send after the consumer stops selecting cannot leak.
	done := make(chan struct{})
	defer close(done)

	keySender := newKeySender(eng)
	defer keySender.close()

	// outcomes carries a copy or paste result from the input worker, which read
	// it from the page, back to the consumer loop, which owns the terminal
	// writes. Cap 1 is enough: each is one user action, and the worker's send
	// waits on the loop rather than dropping the result.
	outcomes := make(chan inputOutcome, 1)

	// The control goroutine runs the engine restarts (Resize, SetBackend) off the
	// consumer loop, so a slow restartStream cannot freeze draw, keys, and quit
	// handling. It stops when Stream returns and the deferred close(done) fires.
	ctl := newControl(eng, done)

	// The cell differ drives the live block paint: each draw emits only the cells
	// that changed since the previous frame, with CUP and SGR coalescing. It is
	// reset on every full clear (first draw and resize) so a diff never skips a
	// cell the clear blanked. The graphics backends ignore it.
	diff := &block.GridDiff{}
	st.SetDiff(diff)

	s := &streamSession{eng: eng, st: st, tty: tty, fd: fd, keySender: keySender, outcomes: outcomes, ctl: ctl, diff: diff, fps: fps, launchURL: url, version: version}
	session = s
	// Clear the screen and draw the bar immediately, with a blank page region
	// and the launch URL: the first user feedback is the bar, not a text cue.
	// The writer does not exist yet, so emitControl writes the tty directly.
	var startup bytes.Buffer
	startup.WriteString(clearScreen)
	s.appendBar(&startup)
	if werr := s.emitControl(startup.Bytes()); werr != nil {
		return werr
	}
	// A caught signal restores the terminal before bounded browser teardown can
	// wait, or raw mode and mouse reporting could remain active during that wait.
	// The watcher shares restoreTerminal's Once with the deferred teardown, so the
	// restore runs exactly once, then the engine closes before the process exits
	// with the unchanged 128-plus-signal status. It starts after session is set,
	// so the drawn-state read never races this goroutine's start.
	fatal := make(chan os.Signal, 1)
	signal.Notify(fatal, fatalSignals...)
	defer signal.Stop(fatal)
	go func() {
		select {
		case sig := <-fatal:
			handleFatalSignal(sig, restoreTerminal, eng.Close, os.Exit)
		case <-done:
		}
	}()
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

// control runs engine restarts off the viewer consumer loop. The loop enqueues a
// resize or a backend switch with a non-blocking send; this goroutine runs the
// engine call (which may block in restartStream) serially, so a slow restart
// never freezes draw, keys, or quit. It selects on done and exits when Stream
// returns. The one-slot buffer plus the drop-on-full send keeps the loop
// responsive: a coalesced restart is fine because each request re-reads the
// current size and backend from the engine state.
type control struct {
	eng  Engine
	work chan func(Engine)
	done <-chan struct{}
}

// newControl starts the control goroutine and returns the handle the loop
// enqueues restarts on. done stops the goroutine when Stream returns; the caller
// owns done and closes it, so control needs no close of its own.
func newControl(eng Engine, done <-chan struct{}) *control {
	c := &control{eng: eng, work: make(chan func(Engine), 1), done: done}
	go c.run()
	return c
}

// submit enqueues one engine restart for the control goroutine. The send is
// non-blocking: if a restart is already queued the request is dropped, because
// the queued one will re-read the latest size and backend when it runs. So the
// consumer loop never blocks on a wedged restart.
func (c *control) submit(fn func(Engine)) {
	select {
	case c.work <- fn:
	default:
	}
}

// run processes restart requests serially until done closes. Running them here,
// not on the consumer loop, keeps that loop responsive while an engine call blocks.
func (c *control) run() {
	for {
		select {
		case fn := <-c.work:
			fn(c.eng)
		case <-c.done:
			return
		}
	}
}

// setupViewer performs Stream's terminal setup: it checks tty is a terminal, reads the grid size, puts the
// terminal into raw mode, builds the State, runs backend and colour detection,
// and hands the resolved backend and cell px to the engine before Navigate so
// the engine sizes its screencast capture and derives its layout scale. It
// returns the tty fd, the State, and a restore closure the caller must defer to
// leave raw mode. Scheme is the resolved block glyph scheme; it is set directly
// on State. The caller resolves it through detection. kittyTransport is the
// --kitty-transport intent, resolved inside configureBackends.
func setupViewer(eng Engine, tty *os.File, colourFlag, kittyTransport string, scheme block.Scheme) (fd uintptr, st *State, restore func(), err error) {
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

	state := NewState(cols, rows)
	// Set the resolved block glyph scheme, overriding the NewState quadrant default
	// for this run. The caller resolves the --blocks mode; auto keeps quadrant as
	// the production and corpus default.
	state.Scheme = scheme

	// Detection runs in raw mode but before the full-screen switch and the key
	// reader, so the handshake replies are not consumed by the key goroutine.
	if err := configureBackends(&state, tty, cols, rows, colourFlag, kittyTransport); err != nil {
		_ = cterm.Restore(fd, oldState)
		return 0, nil, nil, err
	}

	// Hand the resolved backend and cell px to the engine before Navigate; the
	// engine sizes the screencast capture and derives its layout scale from them.
	// The engine grid reserves the bottom bar row, so the engine gets
	// engineRows while State keeps the full terminal grid.
	eng.SetCellPx(state.W, state.H)
	eng.SetBackend(state.Backend)
	eng.Resize(cols, engineRows(rows))

	return fd, &state, func() { _ = cterm.Restore(fd, oldState) }, nil
}

// streamSession bundles the per-run viewer state the push loop mutates: the
// engine it forwards to, the State it draws from, the tty it writes to, and the
// tty fd it reads the resized geometry from. firstDrawn selects the first
// frame's full clear and keeps the bar's indicator on the startup spinner
// until a real frame draws.
type streamSession struct {
	eng Engine
	st  *State
	// tty is the terminal sink. It is an io.Writer (satisfied by the *os.File the
	// production path passes) so the writer goroutine and a test can drive it; the
	// resized geometry is read from fd, not tty.
	tty       io.Writer
	fd        uintptr
	keySender *keySender
	// outcomes delivers a copy or paste result from the input worker to the
	// consumer loop, which writes the clipboard sequence and the bar signal
	// through the ordered control path. It is nil on a session with no worker,
	// which reads and reports inline instead.
	outcomes chan inputOutcome
	ctl      *control
	diff     *block.GridDiff
	// firstDrawn is atomic because the producer goroutine writes it per frame
	// while Stream's fatal-signal watcher reads it for the teardown escapes.
	firstDrawn   atomic.Bool
	nilImageOnce sync.Once // guards the one-shot nil-image diagnostic
	resizeOnce   sync.Once // guards the one-shot dropped-resize diagnostic
	// writer owns the terminal writes on its own goroutine, so the producer's next
	// frame encode overlaps the current frame's write. It is nil until loop starts
	// the pipeline and nil again after loop stops it, so the startup and teardown
	// paths write the tty directly (no writer to race). Only the producer goroutine
	// reads or sets it.
	writer *streamWriter
	// assembled holds one encoded-but-unsent frame buffer between the producer's
	// assemble and the handoff to the writer, so a control write or teardown can
	// flush it first and keep frame order. It is one of the two ping-pong buffers;
	// nil when nothing waits. assembledEncodeMS is that frame's RenderStreamFrame
	// time, carried to the writer so recordPaint keeps the encode/write split. Both
	// are producer-goroutine only.
	assembled         *bytes.Buffer
	assembledEncodeMS float64
	assembledTimed    bool // the assembled frame passed the FCP measurement gate
	// assembledSHM identifies the assembled Kitty shm transmit and the reset
	// generation that created its object. The handoff records it in the reply
	// ledger and arms the one-in-flight pacing gate when it is still valid.
	// Producer only.
	assembledSHM kittySHMFrame
	// assembledTmuxSixel marks the assembled frame as a tmux Sixel frame, so
	// the handoff to the writer stamps the emit-rate pacing gate. Producer only.
	assembledTmuxSixel bool
	// lastTmuxSixel is the instant of the last tmux Sixel frame handoff; the
	// pacing gate holds the next encode until the effective emit interval
	// (tmuxSixelInterval) has passed. Producer only. The zero value leaves the
	// first frame ungated.
	lastTmuxSixel time.Time
	// tmuxSixelWriteDur is the duration of the last tmux Sixel frame write,
	// measured by the writer goroutine and returned on free. It stretches the
	// next emit interval past a saturated pipe's consumption time. Producer
	// only; the zero value keeps the interval at the floor.
	tmuxSixelWriteDur time.Duration
	// kittyShmAwait is true while one shm transmit awaits the terminal's
	// acknowledgement; kittyShmAwaitSeq ties that gate to its reply-ledger
	// entry. kittyShmDeadline bounds the wait so a silent terminal degrades to
	// a conservative self-pace instead of stalling. The generation advances
	// before Nyala unlinks objects on reset. All fields are producer-only.
	kittyShmAwait      bool
	kittyShmAwaitSeq   uint64
	kittyShmDeadline   time.Time
	kittySHMGeneration uint64
	kittySHMLedger     kittySHMReplyLedger
	// fps is the live fps counter, armed by NYALA_FPS_TIMING at startup or by
	// the menu's fps command mid-session. When set, assemble stamps each frame
	// and the meter accumulates; when nil the draw path pays only a nil check,
	// so a session that never shows the badge is unchanged.
	fps *fpsMeter
	// fpsHidden is the menu's fps command state. The zero value draws the badge
	// whenever a meter is armed, so NYALA_FPS_TIMING keeps its exact effect and
	// hiding is the explicit act. It is separate from fps because hiding must not
	// discard a meter the switch armed: the quit summary reads that one.
	fpsHidden bool
	// fpsTicker and fpsBeat are the once-per-second bar refresh that decays the
	// badge on an idle page. They are armed with the meter, lazily, so a session
	// that never shows the badge starts no ticker.
	fpsTicker *time.Ticker
	fpsBeat   <-chan time.Time
	// launchURL is the URL Stream navigated to. The bar's URL segment shows
	// it until the engine records a committed URL. Set once before the loops
	// start and read-only after.
	launchURL string
	// spinner is the startup spinner step. Only waitForNavigate's ticker
	// advances it, on the producer goroutine.
	spinner int
	// version is the Nyala build version the bar's About mode shows. Set
	// once by Stream and read-only after.
	version string
	// barMode selects the bar row's content and input dispatch: chrome,
	// edit, menu, about, or find. All bar-mode state below is producer-goroutine
	// only: the consumer loop dispatches keys, mouse, and repaints on the
	// one goroutine.
	barMode barMode
	// barLay is the segment layout of the last chrome bar repaint, the spans
	// bar-row mouse hits test against.
	barLay barLayout
	// modeSpans is the click-span list of the last menu, About, or find repaint.
	modeSpans []barSpan
	// editor is the bar's text field; non-nil exactly while barMode is edit
	// (the URL line) or find (the search query).
	editor *urlEditor
	// editPromptW and editFieldW are the cells the field prompt and the field
	// itself took on the last text-mode repaint, so a click maps to a text cell
	// against the row as drawn. A row too narrow for the prompt records a zero
	// prompt width.
	editPromptW int
	editFieldW  int
	// findQuery is the query the engine was last asked to search for, so Enter
	// tells a new search from a step through the matches of the current one.
	// Empty while no search has been submitted.
	findQuery string
	// menuSel is the highlighted menu command index.
	menuSel int
	// signal is the transient copy or paste report the chrome row shows in the
	// URL region, and signalUntil the instant it expires. Empty signal means the
	// row shows the URL. The loop wakes at signalUntil to clear it, so it never
	// needs a key press or a frame to go away.
	signal      string
	signalUntil time.Time
	// pressed and pressedSet carry the chrome press feedback: the pressed
	// button renders bold until its release.
	pressed    barSegmentKind
	pressedSet bool
	// dragOnPage is the pointer capture: true from a chrome press on the page
	// region until the matching release, so the drag's motion keeps reaching
	// the page even over the bar row, and button-held motion from a bar press
	// or with no press at all reaches nothing. It gates drags only: buttonless
	// hover motion off the bar row forwards without any capture. A release
	// clears it, including a release for a button never pressed, which finds it
	// already false.
	dragOnPage bool
	// heldMotion is the motion report the coalescer holds back, latest-wins:
	// the newest hover or drag move of the current burst, waiting for the
	// window edge to send it. motionHeld says one waits, because the zero
	// MouseEvent is itself a valid report (cell 0,0 with the left button).
	// motionWindow is the instant the current coalescing window closes; the
	// zero value is in the past, so the first move of a session goes out at
	// once. All three are consumer-goroutine only, like the rest of the input
	// state, so they need no lock.
	heldMotion   MouseEvent
	motionHeld   bool
	motionWindow time.Time
}

// appendBar folds the bottom-bar bytes into buf, cursor-addressed to the
// bottom terminal row in the style barStyle resolved: the base-16 palette
// only, so the bar follows the user's theme. The bar exists only when the terminal is at
// least two rows tall (one bar row plus one page row); a one-row terminal
// appends nothing, matching the engineRows reservation. A non-chrome bar
// mode (edit, menu, about, find) swaps the row content through appendBarMode. In
// chrome, the URL comes from Engine.CurrentURL at each repaint, falling back
// to the launch URL, a live copy or paste signal takes the URL region for its
// hold, the indicator shows the startup spinner until the first frame draws, a
// pressed button renders bold until its release, and the layout is kept for
// bar-row mouse hit-testing.
func (s *streamSession) appendBar(buf *bytes.Buffer) {
	if s.st.Rows < 2 {
		return
	}
	if s.barMode != barModeChrome {
		s.appendBarMode(buf)
		return
	}
	indicator := backendGlyph(s.st.Backend, s.st.Scheme)
	if !s.firstDrawn.Load() {
		indicator = spinnerGlyph(s.spinner)
	}
	fps := ""
	if s.fpsBadgeShown() {
		fps = s.fps.barText(time.Now())
	}
	content, layout := renderBar(s.st.Cols, barContent{
		indicator: indicator,
		url:       s.currentURL(),
		fps:       fps,
		signal:    s.liveSignal(time.Now()),
	})
	s.barLay = layout
	// A pressed button holds bold until its release.
	if s.pressedSet {
		if seg, ok := layout.segment(s.pressed); ok {
			content = boldSpan(content, seg.col, seg.width)
		}
	}
	fmt.Fprintf(buf, "\x1b[%d;1H%s%s\x1b[0m", s.st.Rows, s.st.barStyle(), content)
}

// armFPSHeartbeat starts the once-per-second badge refresh, if it is not
// already running. It runs on the draw goroutine, which is also the goroutine
// selecting on fpsBeat, so a mid-session arm never races the loop.
func (s *streamSession) armFPSHeartbeat() {
	if s.fpsTicker != nil {
		return
	}
	s.fpsTicker = time.NewTicker(fpsBadgeInterval)
	s.fpsBeat = s.fpsTicker.C
}

// stopFPSHeartbeat stops the badge refresh ticker when the loop ends. A session
// that never armed one does nothing.
func (s *streamSession) stopFPSHeartbeat() {
	if s.fpsTicker != nil {
		s.fpsTicker.Stop()
	}
}

// fpsBadgeShown reports whether the bar draws the fps segment: a meter must be
// armed and the menu must not have hidden it.
func (s *streamSession) fpsBadgeShown() bool {
	return s.fps != nil && !s.fpsHidden
}

// toggleFPS shows or hides the bar's fps segment for the rest of the session,
// the menu's fps command. Showing it on a session NYALA_FPS_TIMING left off arms
// a meter and its heartbeat, so the badge has live rates to report; hiding it
// keeps the meter, because the switch's quit summary reads the same one. It
// closes the menu, which repaints chrome, so the row and its hit spans are
// rebuilt together for the new state.
func (s *streamSession) toggleFPS() {
	if s.fpsBadgeShown() {
		s.fpsHidden = true
		s.leaveMode()
		return
	}
	s.fpsHidden = false
	if s.fps == nil {
		s.fps = armFPSMeter()
		if r, ok := s.eng.(ingestReporter); ok {
			s.fps.ingest = r
		}
	}
	s.armFPSHeartbeat()
	s.leaveMode()
}

// barBytes returns one standalone bar repaint (the spinner tick, the fps
// heartbeat, and the post-clear repaints route it through emitControl), or
// nil when the terminal has no bar row.
func (s *streamSession) barBytes() []byte {
	var buf bytes.Buffer
	s.appendBar(&buf)
	if buf.Len() == 0 {
		return nil
	}
	return buf.Bytes()
}

// loop runs the push consumer until the frames or keys channel closes, a quit
// key arrives, or an I/O error occurs. It draws each pushed frame, restarts the
// stream on SIGWINCH at the recomputed grid, and forwards keys. It caches the
// frame channel once: the engine keeps the channel stable across Resize and
// SetBackend and replaces it only by Navigate, which is single-shot, so a backend
// switch or a resize never strands this cached handle.
func (s *streamSession) loop(winch <-chan os.Signal, keys <-chan keyEvent) error {
	frames := s.eng.Frames()
	// Heartbeat the bar's fps segment so an idle page decays it to a live 0
	// rather than freezing it at the last frame's rate. The block backend
	// pushes no frames while static, so without this a scroll that settles
	// leaves the segment wedged. A nil channel blocks forever in the select, so
	// an off run starts no ticker and the case never fires; the menu's fps
	// command arms it mid-session on the same goroutine as this loop.
	if s.fps != nil {
		s.armFPSHeartbeat()
	}
	defer s.stopFPSHeartbeat()

	// Start the producer/writer pipeline. The producer (this goroutine) assembles
	// each frame into an owned buffer; the writer goroutine does the single tty
	// write, so encode N+1 overlaps write N. Stopping the writer here, before loop
	// returns, guarantees no goroutine writes the terminal while Stream runs its
	// teardown escapes (exitFullScreen, restore). flushAssembled presents the last
	// assembled frame first, so the final image lands before teardown.
	w := newStreamWriter(s.tty)
	s.writer = w
	defer func() {
		s.flushAssembled()
		w.stop()
		s.writer = nil
	}()

	// The two ping-pong buffers live here on the producer. avail holds the buffers
	// free to encode into; the writer returns each drained buffer over w.free, which
	// the producer folds back into avail. pending holds the newest un-encoded frame,
	// so a slow writer (no free buffer) keeps coalescing and drops stale frames
	// (latest-wins) rather than blocking ingest.
	avail := []*bytes.Buffer{{}, {}}
	var (
		pending     render.Frame
		havePending bool
	)
	// Return an undrawn pending frame's pooled plane to the engine at loop
	// exit, so a quit mid-stream cannot strand a kitty-shm plane.
	defer func() {
		if havePending {
			releaseFrame(pending)
		}
	}()

	for {
		// Encode the newest pending frame into a spare buffer when one is free and
		// nothing already waits to hand off. Holding only one assembled frame at a
		// time bounds the in-flight buffers to two. The kitty-shm ack gate holds
		// the encode back while one shm transmit is unacknowledged, so at most
		// one shm frame is in flight and pty latency cannot build.
		if s.assembled == nil && havePending && len(avail) > 0 && !s.kittySHMGated() && !s.tmuxSixelGated() {
			buf := avail[len(avail)-1]
			avail = avail[:len(avail)-1]
			// Transfer ownership before assembly. renderStreamFrame releases the
			// frame on every exit, including an encode failure, so the loop's
			// pending-frame defer must no longer own it when assemble returns.
			frame := pending
			pending = render.Frame{}
			havePending = false
			encodeMS, err := s.assemble(buf, frame)
			if err != nil {
				return err
			}
			s.assembled = buf
			s.assembledEncodeMS = encodeMS
			s.assembledTimed = s.fps != nil && s.fps.gated
		}

		// Enable the handoff case only when a buffer waits. A nil channel send never
		// fires, so the select ignores it while nothing is assembled.
		var readyC chan<- writeItem
		var item writeItem
		if s.assembled != nil {
			readyC = w.ready
			item = writeItem{
				buf: s.assembled, encodeMS: s.assembledEncodeMS, timed: s.assembledTimed,
				tmuxSixel: s.assembledTmuxSixel, kittySHM: s.assembledSHM.id != 0,
			}
		}

		// While a pacing gate alone holds back a pending frame, wake at the
		// gate deadline so a lost acknowledgement (kitty shm) or an elapsed
		// emit interval (tmux Sixel) releases the frame without needing
		// another event. A nil channel never fires.
		var gateC <-chan time.Time
		if s.assembled == nil && havePending && len(avail) > 0 {
			switch {
			case s.kittyShmAwait:
				gateC = time.After(time.Until(s.kittyShmDeadline))
			case s.tmuxSixelGated():
				gateC = time.After(s.tmuxSixelInterval() - time.Since(s.lastTmuxSixel))
			}
		}

		// While the tmux Sixel stream is idle with the last emit blurred, wake
		// once the idle threshold passes to re-emit that frame at full
		// quality. The settle encode resets the ladder and drops the retained
		// source, so the timer disarms until a new budgeted frame goes out
		// and cannot loop. A nil channel never fires.
		var settleC <-chan time.Time
		if s.assembled == nil && !havePending && len(avail) > 0 && s.tmuxSixelSettlePending() {
			settleC = time.After(s.tmuxSixelSettleDelay() - time.Since(s.lastTmuxSixel))
		}

		// While a copy or paste signal holds the bar's URL region, wake at its
		// expiry so the row falls back to the URL on its own. An idle page
		// pushes no frames, so without this the signal would sit there until
		// the next key or frame. A nil channel never fires.
		var signalC <-chan time.Time
		if s.signal != "" {
			signalC = time.After(time.Until(s.signalUntil))
		}

		// While a coalesced motion report waits out its window, wake at the
		// window edge, so the last move of a burst reaches the page without
		// needing another event to push it out. A nil channel never fires.
		var motionC <-chan time.Time
		if s.motionHeld {
			motionC = time.After(time.Until(s.motionWindow))
		}

		select {
		case readyC <- item:
			s.assembled = nil
			s.markKittySHMHandoff()
			s.markTmuxSixelHandoff()
		case <-gateC:
			// The deadline passed; the next kittySHMGated call clears the gate.
		case <-settleC:
			// The idle threshold passed with the last emit blurred: assemble
			// the full-quality settle frame into a spare buffer. The next
			// iteration hands it off through the normal tmux Sixel path, so
			// it stamps the pacing gate like any frame; firing two pacing
			// intervals after the last handoff keeps the gate already open.
			buf := avail[len(avail)-1]
			avail = avail[:len(avail)-1]
			if err := s.assembleSettle(buf); err != nil {
				return err
			}
			s.assembled = buf
			s.assembledEncodeMS = 0
			s.assembledTimed = false
			s.assembledTmuxSixel = true
		case <-winch:
			if err := s.resize(); err != nil {
				return err
			}
		case now := <-s.fpsBeat:
			// Refresh the fps text and repaint the bar. The repaint routes
			// through the writer so it never races an in-flight frame write. A
			// hidden badge needs neither, so the beat costs a flag check.
			if !s.fpsBadgeShown() {
				continue
			}
			s.fps.refresh(now)
			if b := s.barBytes(); b != nil {
				_ = s.emitControl(b)
			}
		case frame, ok := <-frames:
			if !ok {
				return nil
			}
			// Coalesce a queued burst to the newest frame, so a scroll burst presents
			// once with the latest frame instead of every stale one. Channel close
			// mid-drain stops the loop, matching the main receive. The differ stays
			// correct because it compares the assembled frame against the previous
			// assembled grid, so skipped frames do not corrupt its state.
			var drained int
			frame, drained, ok = drainFrames(frames, frame)
			// Count every frame the engine delivered, including the coalesced ones,
			// so the badge total distinguishes a stalled stream from an idle one.
			if s.fps != nil {
				s.fps.countReceived(1 + drained)
			}
			if !ok {
				return nil
			}
			if !frame.HasImage() {
				s.nilImageOnce.Do(func() {
					fmt.Fprintln(os.Stderr, "nyala: dropped a frame with no image")
				})
				continue
			}
			// Latest-wins: the overwritten pending frame is dropped, so its
			// pooled plane goes back to the engine now.
			if havePending {
				releaseFrame(pending)
			}
			pending = frame
			havePending = true
		case fi := <-w.free:
			// A drained buffer came back: record its paint split on this goroutine
			// (the meter is single-goroutine) and return the buffer to the free set.
			if s.fps != nil && fi.timed {
				s.fps.recordPaint(fi.encodeMS, fi.writeMS)
			}
			if fi.tmuxSixel {
				s.recordTmuxSixelWrite(fi.writeDur)
			}
			avail = append(avail, fi.buf)
		case ev, ok := <-keys:
			if !ok {
				return nil
			}
			if ev.apc != "" {
				if err := s.handleKittyResponse(ev.apc); err != nil {
					return err
				}
				continue
			}
			// An OSC or DCS terminal reply is consumed here so it never
			// types into the page. A future handler (WW-88) reads it here.
			if ev.osc != "" || ev.dcs != "" {
				continue
			}
			if ev.mouse != nil {
				if s.handleMouse(*ev.mouse) {
					return nil
				}
				continue
			}
			if s.handleKey(ev) {
				return nil
			}
		case <-signalC:
			// The signal's hold elapsed: the row falls back to the URL.
			s.clearSignal()
		case <-motionC:
			// The coalescing window closed: the newest move of the burst goes
			// to the page.
			s.flushMotionAt(time.Now())
		case o := <-s.outcomes:
			// A copy read the page's selection on the input worker, or a paste
			// found no field there; the clipboard write and the bar signal
			// belong here, on the goroutine that owns the terminal, so both
			// stay ordered against the frames and the bar.
			s.applyOutcome(o)
		case failure := <-w.errc:
			if failure.kittySHM {
				return fmt.Errorf("write Kitty shm frame: %w", failure.err)
			}
			// Non-shm writes keep the serial path's original I/O error.
			return failure.err
		}
	}
}

// drainFrames keeps only the newest immediately-available frame, given the one
// already received. It pulls any further queued frames non-blocking and returns
// the last, coalescing a burst to one paint; each superseded frame's pooled
// plane, when present, is released back to the engine as it is dropped. The
// int counts the extra frames pulled, so the caller's cumulative received-frame
// total stays exact across coalescing. The bool is false when the channel
// closed mid-drain, so the caller stops the loop the same way the main receive
// does; otherwise it is true with the newest frame.
func drainFrames(frames <-chan render.Frame, latest render.Frame) (render.Frame, int, bool) {
	drained := 0
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				return latest, drained, false
			}
			releaseFrame(latest)
			latest = f
			drained++
		default:
			return latest, drained, true
		}
	}
}

// kittySHMAckTimeout bounds the wait for one shm transmit's acknowledgement.
// It is the conservative self-pace fallback: a terminal that renders t=s but
// never replies (the reply-quiet key is q=0, so a conforming terminal answers
// every transmit) still advances at 1/timeout fps at worst, and a lost reply
// cannot wedge the stream.
const kittySHMAckTimeout = 40 * time.Millisecond

// kittySHMReplyLedgerLimit bounds the number of handed-off shm transmits kept
// for late reply correlation. Normal operation removes each entry on its reply;
// the bound covers resets and silent terminals without growing session state.
const kittySHMReplyLedgerLimit = 32

// kittySHMFrame identifies an assembled shm escape before writer handoff. Its
// generation records which reset lifetime owns the referenced object.
type kittySHMFrame struct {
	id         int
	generation uint64
}

// kittySHMTransmit identifies one handed-off shm escape. Replies for a reused
// image id consume the oldest matching entry, preserving terminal order across
// resets. invalidated records an object Nyala unlinked before the terminal read
// it, while sequence correlates the pacing gate with the exact live transmit.
type kittySHMTransmit struct {
	kittySHMFrame
	sequence    uint64
	invalidated bool
}

// kittySHMReplyLedger is the bounded ordered set of shm transmits which may
// still receive a terminal reply.
type kittySHMReplyLedger struct {
	entries      [kittySHMReplyLedgerLimit]kittySHMTransmit
	len          int
	nextSequence uint64
}

// add records one handed-off transmit and returns its correlation data. The
// oldest entry expires only after the fixed late-reply budget is exhausted.
func (l *kittySHMReplyLedger) add(frame kittySHMFrame, currentGeneration uint64) kittySHMTransmit {
	l.nextSequence++
	tx := kittySHMTransmit{
		kittySHMFrame: frame,
		sequence:      l.nextSequence,
		invalidated:   frame.generation != currentGeneration,
	}
	if l.len == len(l.entries) {
		copy(l.entries[:], l.entries[1:])
		l.len--
	}
	l.entries[l.len] = tx
	l.len++
	return tx
}

// invalidate marks every outstanding transmit from generation as unlinked by
// Nyala. It leaves each entry in reply order so a stale reused-id response
// cannot claim a later transmit.
func (l *kittySHMReplyLedger) invalidate(generation uint64) {
	for i := range l.len {
		if l.entries[i].generation == generation {
			l.entries[i].invalidated = true
		}
	}
}

// invalidateID marks every outstanding transmit for id as unlinked before a
// new object reuses that double-buffer slot. The entries stay in reply order,
// so their delayed failures cannot claim the new transmit's pacing gate.
func (l *kittySHMReplyLedger) invalidateID(id int) {
	for i := range l.len {
		if l.entries[i].id == id {
			l.entries[i].invalidated = true
		}
	}
}

// take removes and returns the oldest outstanding transmit for id.
func (l *kittySHMReplyLedger) take(id int) (kittySHMTransmit, bool) {
	for i := range l.len {
		if l.entries[i].id != id {
			continue
		}
		tx := l.entries[i]
		copy(l.entries[i:], l.entries[i+1:l.len])
		l.len--
		l.entries[l.len] = kittySHMTransmit{}
		return tx, true
	}
	return kittySHMTransmit{}, false
}

// kittySHMGated reports whether the shm one-in-flight pacing gate holds the
// next frame back. The gate opens (and clears its state) once the transmit is
// acknowledged, the deadline passes, the backend leaves Kitty, or the shm
// transport stops being live. Producer goroutine only.
func (s *streamSession) kittySHMGated() bool {
	if !s.kittyShmAwait {
		return false
	}
	if s.st.Backend != BackendKitty || !s.st.kittySHMLive() || !time.Now().Before(s.kittyShmDeadline) {
		s.kittyShmAwait = false
		s.kittyShmAwaitSeq = 0
		return false
	}
	return true
}

// markKittySHMHandoff arms the shm pacing gate when the frame just handed to
// the writer was an shm transmit: the next shm frame waits for the terminal's
// acknowledgement or the conservative deadline. Producer goroutine only.
func (s *streamSession) markKittySHMHandoff() {
	if s.assembledSHM.id == 0 {
		return
	}
	tx := s.kittySHMLedger.add(s.assembledSHM, s.kittySHMGeneration)
	s.assembledSHM = kittySHMFrame{}
	if tx.invalidated {
		return
	}
	s.kittyShmAwait = true
	s.kittyShmAwaitSeq = tx.sequence
	s.kittyShmDeadline = time.Now().Add(kittySHMAckTimeout)
}

// markKittySHMAssembled records one encoded shm transmit before writer handoff.
// Creating its object has already unlinked any unconsumed object for the same
// image id, so the reply ledger invalidates those older transmits at this point.
func (s *streamSession) markKittySHMAssembled(id int) {
	if id == 0 {
		s.assembledSHM = kittySHMFrame{}
		return
	}
	s.kittySHMLedger.invalidateID(id)
	s.assembledSHM = kittySHMFrame{id: id, generation: s.kittySHMGeneration}
}

// invalidateKittySHMTransmits marks every object owned by the current reset
// generation as unlinked, advances the generation for later frames, and drops
// only the old generation's pacing gate.
func (s *streamSession) invalidateKittySHMTransmits() {
	s.kittySHMLedger.invalidate(s.kittySHMGeneration)
	s.kittySHMGeneration++
	s.kittyShmAwait = false
	s.kittyShmAwaitSeq = 0
}

// resetKitty invalidates reply correlation before State unlinks any shm
// objects. Both resize and backend cycling use this order before their control
// bytes flush an assembled frame.
func (s *streamSession) resetKitty(w io.Writer) error {
	s.invalidateKittySHMTransmits()
	return s.st.resetKitty(w)
}

// tmuxSixelMinInterval is the hard floor between Sixel frame handoffs under
// tmux 3.7 and later, bisected live on tmux 3.7b with ~4 MB full-quality
// frames. tmux consumes such a frame in about 230 ms, so at 200 ms the pty
// write blocked for the full consumption time (228 ms mean), a downstream
// backlog built, and the display lagged live content, replaying the queue on
// quit. At 250 ms writes stay cheap (33 ms mean) and tmux drains fully
// between frames. Sustained animation at 100 ms killed the pane's Sixel
// rendering permanently, so the failure tracks the DCS sequence rate, not
// the byte rate; the floor stays conservative to guard that rate-kill. The
// gate holds the encode, not a queue: pending frames keep coalescing
// latest-wins while it is closed, so no backlog grows. The effective floor
// resolves per session through resolveTmuxSixelMinInterval and lands on
// sixelBudget.minInterval; this constant is the 3.7-and-later value and the
// zero-value fallback.
const tmuxSixelMinInterval = 250 * time.Millisecond

// tmuxSixelMinIntervalLegacy is the pacing floor below tmux 3.7 and when the
// tmux version is unknown. Live tmux 3.6a at the 250 ms floor showed
// periodic display stalls followed by a rapid release of several frames even
// though the pty writes stayed fully drained at ~20 ms: the backlog sits in
// 3.6a's output scheduler toward the host terminal, which the write-duration
// signal cannot observe, so no adaptive back-off can catch it. The same
// content at 300 ms showed no bursts, so older tmux pays the wider floor.
const tmuxSixelMinIntervalLegacy = 300 * time.Millisecond

// tmuxSixelWriteMargin is the headroom added to the last measured tmux Sixel
// frame write duration when it stretches the emit interval past the floor,
// so tmux gets slack to drain fully between frames instead of running flat
// out at its own consumption pace.
const tmuxSixelWriteMargin = 50 * time.Millisecond

// tmuxSixelMaxInterval caps the adaptive emit interval, so one pathological
// write cannot freeze the stream: even a write that blocked for seconds
// releases the next frame within a second.
const tmuxSixelMaxInterval = time.Second

// tmuxSixelInterval returns the effective emit interval for the next tmux
// Sixel frame: the resolved floor, stretched to the last write duration plus
// the margin when tmux consumed the previous frame slower than the floor,
// and capped at the ceiling. A large frame leaves Nyala only as fast as tmux
// reads it from the pty, so the write duration is a direct saturation
// signal: a healthy pipe writes in tens of milliseconds and keeps the floor,
// while a saturated pipe blocks the write for tmux's full consumption time,
// which varies with machine, grid, and content. The most recent write drives
// the interval with no smoothing: the write blocks at tmux's own read pace,
// so one sample is already ground truth and reacting within one frame beats
// averaging in stale ones. Producer goroutine only.
func (s *streamSession) tmuxSixelInterval() time.Duration {
	floor := s.st.sixelBudget.floor()
	iv := s.tmuxSixelWriteDur + tmuxSixelWriteMargin
	if iv <= floor {
		return floor
	}
	return min(iv, tmuxSixelMaxInterval)
}

// recordTmuxSixelWrite folds one measured tmux Sixel frame write duration
// into the adaptive pacing state and the downscale-ladder state, and, under
// NYALA_DEBUG_TIMING, reports the write and the next effective interval on
// stderr beside the per-frame budget line. Producer goroutine only: the
// writer goroutine measures the duration, returns it on the free channel,
// and the producer folds it in here, so both the pacing state and the
// sixelBudget ladder stay single-goroutine.
func (s *streamSession) recordTmuxSixelWrite(d time.Duration) {
	s.tmuxSixelWriteDur = d
	s.st.sixelBudget.recordWrite(d)
	if s.st.sixelBudget.debug {
		fmt.Fprintf(os.Stderr, "nyala timing: sixel write_ms=%d next_interval_ms=%d\n",
			d.Milliseconds(), s.tmuxSixelInterval().Milliseconds())
	}
}

// tmuxSixelGated reports whether the tmux Sixel emit-rate gate holds the next
// frame back: only while the active backend is Sixel under tmux and the last
// Sixel handoff is younger than the effective emit interval. Producer
// goroutine only.
func (s *streamSession) tmuxSixelGated() bool {
	if s.st.Backend != BackendSixel || s.st.mux != muxTmux {
		return false
	}
	return time.Since(s.lastTmuxSixel) < s.tmuxSixelInterval()
}

// markTmuxSixelHandoff stamps the emit-rate gate when the frame just handed
// to the writer was a tmux Sixel frame, so the next frame waits out the
// minimum interval. Producer goroutine only.
func (s *streamSession) markTmuxSixelHandoff() {
	if !s.assembledTmuxSixel {
		return
	}
	s.assembledTmuxSixel = false
	s.lastTmuxSixel = time.Now()
}

// tmuxSixelSettleIntervals is how many effective pacing intervals the tmux
// Sixel stream must stay idle after the last handoff before the blurred last
// frame is re-emitted at full quality: two, so the settle can never race the
// emit gate and a live animation, which hands a frame off inside every
// interval, never triggers it.
const tmuxSixelSettleIntervals = 2

// tmuxSixelSettleDelay returns the idle span before the settle-to-sharp
// emit fires. It follows the effective interval, not the floor, so the
// settle never fires into a pipe the last write measured as saturated.
// Producer goroutine only.
func (s *streamSession) tmuxSixelSettleDelay() time.Duration {
	return tmuxSixelSettleIntervals * s.tmuxSixelInterval()
}

// tmuxSixelSettlePending reports whether the idle settle-to-sharp emit is
// armed: the session is on Sixel under tmux and the budget state holds a
// blurred last emit with its retained source. Producer goroutine only.
func (s *streamSession) tmuxSixelSettlePending() bool {
	return s.st.Backend == BackendSixel && s.st.mux == muxTmux && s.st.sixelSettleNeeded()
}

// assembleSettle builds the one-shot settle frame into buf: the retained
// last budgeted frame re-encoded at full quality, wrapped like a steady tmux
// Sixel frame (bare cursor-home prefix, DECSDM bracket, no DEC 2026 markers,
// which tmux Sixel drops on every version). The caller hands it off as a
// normal tmux Sixel frame, so the handoff stamps the pacing gate.
func (s *streamSession) assembleSettle(buf *bytes.Buffer) error {
	buf.Reset()
	buf.WriteString(cursorHome)
	buf.WriteString(beginSixelDisplayMode)
	if err := s.st.renderSixelSettle(buf); err != nil {
		return err
	}
	buf.WriteString(endSixelDisplayMode)
	return nil
}

// handleKittyResponse consumes one Kitty graphics response from the input
// stream. Probe replies never affect live pacing. A frame reply consumes the
// oldest ledger entry with that image id, so an invalidated reply for a reused
// id cannot settle the current gate. Nyala ignores a failure only when it
// invalidated that transmit; every current or unknown failure remains fatal.
func (s *streamSession) handleKittyResponse(body string) error {
	idText, hasID := kittyResponseImageID(body)
	if hasID && (idText == kittyDirectID || idText == kittySHMID) {
		return nil
	}
	id, err := strconv.Atoi(idText)
	if !hasID || err != nil {
		if strings.Contains(body, ";OK") {
			return nil
		}
		return fmt.Errorf("terminal rejected shm transmit: %s", body)
	}
	tx, found := s.kittySHMLedger.take(id)
	if !found {
		if strings.Contains(body, ";OK") {
			return nil
		}
		return fmt.Errorf("terminal rejected shm transmit: %s", body)
	}
	if tx.sequence == s.kittyShmAwaitSeq {
		s.kittyShmAwait = false
		s.kittyShmAwaitSeq = 0
	}
	if strings.Contains(body, ";OK") || tx.invalidated {
		return nil
	}
	return fmt.Errorf("terminal rejected shm transmit: %s", body)
}

// kittyResponseImageID returns the exact i= value from a Kitty graphics
// response's comma-separated control data.
func kittyResponseImageID(body string) (string, bool) {
	control, _, _ := strings.Cut(body, ";")
	for _, field := range strings.Split(control, ",") {
		if id, ok := strings.CutPrefix(field, "i="); ok && id != "" {
			return id, true
		}
	}
	return "", false
}

// barSpinnerInterval paces the startup spinner in the bar's indicator slot:
// fast enough to read as live progress, slow enough to cost nothing.
const barSpinnerInterval = 90 * time.Millisecond

// waitForNavigate keeps the raw-mode controls live while engine startup runs.
// Browser launch and CDP setup can block before frames exist, so Ctrl-C and
// resize handling must not wait for Navigate to return. A ticker animates the
// bar's braille spinner until Navigate returns; the first frame then swaps
// the spinner for the backend glyph.
func (s *streamSession) waitForNavigate(winch <-chan os.Signal, keys <-chan keyEvent, navigated <-chan error) error {
	spin := time.NewTicker(barSpinnerInterval)
	defer spin.Stop()
	for {
		select {
		case <-spin.C:
			s.spinner++
			if b := s.barBytes(); b != nil {
				if err := s.emitControl(b); err != nil {
					return err
				}
			}
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

// startupQuit reports whether a pre-navigation key requests exit (Ctrl+Q, or
// the Ctrl+C interrupt fallback). Browser input and backend cycling are
// ignored until Navigate finishes and the live loop owns the page.
func startupQuit(ev keyEvent) bool {
	return ev.scroll == ScrollNone && ev.act == actionNone && byteKeyAction(ev.b) == actionQuit
}

// assemble builds one frame's whole terminal byte sequence into buf, ready for the
// writer to present with a single io.Write: the optional DEC 2026 open, the cursor
// prefix, the rendered frame with its crisp DOM-text overlay, the bottom bar, and
// the optional DEC 2026 close. Foot and tmux Sixel omit only the
// synchronised-update markers; every other backend and terminal keeps them.
// Assembling the whole unit
// into one owned buffer keeps the atomic present where DEC 2026 is active while
// the producer encodes the next frame in parallel with the current frame's write.
// Byte order remains identical for every frame, independent of which goroutine
// writes it.
//
// The first frame emits the full clearScreen (via framePrefix) to wipe the
// startup bar screen; steady frames emit cursorHome only. The bar rides inside
// the sync bracket, so bar and frame present atomically. It returns the encode
// time, the RenderStreamFrame span alone (matching the out_encode split), when
// the fps meter is armed, or zero when off (no clock read). buf is one of the
// two ping-pong buffers; the producer owns it until the writer returns it on free.
func (s *streamSession) assemble(buf *bytes.Buffer, frame render.Frame) (encodeMS float64, err error) {
	buf.Reset()
	prefix := framePrefix(s.firstDrawn.Load(), s.diff)
	syncUpdates := s.st.syncUpdatesEnabled()
	if syncUpdates {
		buf.WriteString(beginSyncUpdate)
	}
	buf.WriteString(prefix)

	timed := s.fps != nil
	var encodeStart time.Time
	if timed {
		encodeStart = time.Now()
	}
	// DECSDM makes Sixel placement non-scrolling and cursor-neutral. Without it,
	// a terminal may advance a full-height image below the bottom margin before
	// any following cursor restore can run, shifting the viewport up by one row.
	// Keep the mode wrap in the live placement seam so the renderer and
	// diagnostics continue to exchange a bare, decodable DCS payload.
	nonScrollingSixel := s.st.Backend == BackendSixel && frame.HasImage()
	if nonScrollingSixel {
		buf.WriteString(beginSixelDisplayMode)
	}
	kittyBelowText := timed && s.st.Backend == BackendKitty
	if rerr := s.st.renderStreamFrame(frame, buf, kittyBelowText); rerr != nil {
		return 0, rerr
	}
	s.firstDrawn.Store(true)
	if nonScrollingSixel {
		buf.WriteString(endSixelDisplayMode)
	}
	if timed {
		encodeMS = msSince(encodeStart)
	}
	// Tag an shm transmit for reply correlation and the pacing gate.
	s.markKittySHMAssembled(s.st.takeKittySHMID())
	s.assembledTmuxSixel = nonScrollingSixel && s.st.mux == muxTmux

	if s.fps != nil {
		// The engine stamps frames received after main-frame first contentful paint.
		// That lifecycle marker opens one backend-neutral warm-up gate, including
		// direct Kitty frames and text-free pages. recordPaint is deferred until
		// the writer returns the buffer, because the write time is known only then.
		s.fps.tick(time.Now(), frame.AfterFCP)
	}
	// Fold the bottom bar into the same buffer so bar and frame present
	// atomically where DEC 2026 is active. The bar is a cursor-positioned
	// bottom-row overlay outside the page grid, so it never enters the block
	// grid the differ tracks and no backend paints over it.
	s.appendBar(buf)
	if syncUpdates {
		buf.WriteString(endSyncUpdate)
	}
	return encodeMS, nil
}

// flushAssembled hands any assembled-but-unsent frame to the writer with a
// blocking send, so a following control write or teardown keeps frame order. It
// runs only on the producer goroutine, which owns assembled and the writer handle.
func (s *streamSession) flushAssembled() {
	if s.writer == nil || s.assembled == nil {
		return
	}
	s.writer.send(writeItem{
		buf: s.assembled, encodeMS: s.assembledEncodeMS, timed: s.assembledTimed,
		tmuxSixel: s.assembledTmuxSixel, kittySHM: s.assembledSHM.id != 0,
	})
	s.assembled = nil
	s.markKittySHMHandoff()
	s.markTmuxSixelHandoff()
}

// emitControl routes a one-off terminal write (a resize clear, a kitty reset, or a
// bar repaint) so it stays ordered against the frame writes. In the draw loop it
// flushes any assembled frame, then queues the bytes on the single writer, so the
// control write never interleaves with an in-flight frame's bytes. Before the
// writer exists (a startup resize) or after it stops, it writes the tty directly.
// It copies the bytes because the writer emits them later, off this goroutine. It
// runs only on the producer goroutine.
//
// It does two blocking sends on the cap-1 ready channel (flushAssembled then the
// control bytes), so while inside it the producer is not draining eng.Frames() for
// up to about two write spans. This is transient: the engine collector parks on its
// cap-2 buffer and resumes the moment emitControl returns. In production it fires
// only on a rare resize, backend cycle, or startup spinner tick, because the
// per-second heartbeat runs only when the fps meter is armed.
func (s *streamSession) emitControl(b []byte) error {
	if s.writer == nil {
		_, err := s.tty.Write(b)
		return err
	}
	s.flushAssembled()
	s.writer.send(writeItem{ctrl: append([]byte(nil), b...), timed: s.fps != nil})
	return nil
}

// resize re-fits to the new terminal size and restarts the stream at the
// recomputed capture size, so frames arrive at the new grid, not the old one. A
// terminal font-size change (zoom) moves the cell pixel pair with the grid, so
// it re-derives the pair from the winsize and hands a changed pair to the
// engine with the restart; resizeCellPx keeps the old pair when the override is
// forced or the winsize reports nothing. It clears the screen and repaints the
// bar at the new geometry; a pre-frame resize is safe because the bar repaint
// follows every clear.
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
	derived, derivedOK := winsizeCellPixels(s.fd, nc, nr)
	px, changed := resizeCellPx(s.st.cellPxForced, s.st.cellPx, derived, derivedOK)
	if changed {
		s.st.cellPx = px
	}
	s.st.Resize(nc, nr)
	// A shrink below two rows hides the bar, so an open bar mode must close
	// rather than keep swallowing keys invisibly.
	s.exitBarModeIfHidden()
	// The engine restart runs on the control goroutine so a slow restartStream
	// never freezes the consumer loop; the loop keeps drawing and handling quit.
	// The engine grid takes engineRows, so the bottom-row bar reservation
	// survives a resize while State keeps the full grid.
	s.ctl.submit(resizeEngineOp(px, changed, nc, engineRows(nr)))
	// The resize clears the screen, so the differ full-repaints the next frame
	// even when the grid dimensions are unchanged. The clear routes through the
	// writer so it never races an in-flight frame write.
	if s.diff != nil {
		s.diff.Reset()
	}
	var ctl bytes.Buffer
	if s.st.Backend == BackendKitty {
		// A Kitty placement survives the ANSI clear, so a resize that shrank the
		// image (a zoom-out moves the cell pair) would leave the old image's
		// pixels standing outside the new one. Reuse the backend-switch delete-all
		// seam: free both double-buffer ids so the next frame rebuilds from id 1,
		// and drop the shm pacing gate so a stale in-flight wait cannot gate the
		// first post-resize frame.
		_ = s.resetKitty(&ctl)
	}
	ctl.WriteString(clearScreen)
	s.appendBar(&ctl)
	return s.emitControl(ctl.Bytes())
}

// resizeEngineOp returns the control-goroutine closure for one resize. A
// changed cell pair is set on the engine inside the same closure as the grid
// restart, before Resize, so the restart recomputes the capture from the new
// pair; the control channel is one-slot drop-on-full, so a separately queued
// SetCellPx could be dropped while the Resize survives and restart at the
// stale pair.
func resizeEngineOp(px cellPx, changed bool, cols, rows int) func(Engine) {
	return func(eng Engine) {
		if changed {
			eng.SetCellPx(px.W, px.H)
		}
		eng.Resize(cols, rows)
	}
}

// switchBackendOp returns the control-goroutine closure for one backend
// switch or a same-backend restart. The engine grid never moves on a switch:
// the bottom-row bar reservation applies to every backend, so SetBackend
// restarts the stream at the grid the engine already holds.
func switchBackendOp(backend Backend) func(Engine) {
	return func(eng Engine) {
		eng.SetBackend(backend)
	}
}

// handleMouse routes one decoded mouse event by bar mode and row and reports
// whether it quits the session (a menu Quit click). The side buttons
// dispatch history navigation from any row. In edit mode a bar-row press places
// the editor's caret at the clicked cell and a page-region press cancels the
// editor without navigating, and nothing forwards to CDP either way; find mode
// places its caret the same way and closes on its ✕ or a page press; in menu
// and About modes a bar-row press hits the mode's spans and any other press
// closes the mode. In chrome, bar-row events go to bar hit-testing and every
// other event forwards to the engine off the input loop through the
// keySender worker, the same stall policy as scroll keys, so a wedged
// browser never freezes quit or draw.
//
// A press captures the pointer for the surface under it until the release, the
// way a browser does, so a drag is one whole gesture: a drag that started on
// the page keeps reaching the page even once the pointer crosses the bar row
// (the engine clamps the cell to its grid, so the drag tracks the bottom row),
// and a drag that started on the bar row never injects page motion, because the
// bar has no draggable control. Button-held motion with no capture is dropped,
// because without a matching press the gesture is not the page's, and a bar
// mode taking over input drops the capture with it. Buttonless motion needs no
// capture: mode 1003 reports it whenever the pointer moves, and off the bar row
// it forwards as a hover so CSS hover state follows the pointer.
func (s *streamSession) handleMouse(ev MouseEvent) (quit bool) {
	if ev.Button == MouseButtonBack || ev.Button == MouseButtonForward {
		if s.barMode == barModeChrome && ev.Action == MouseActionPress {
			nav := NavBack
			if ev.Button == MouseButtonForward {
				nav = NavForward
			}
			s.forwardNav(nav)
		}
		return false
	}
	onBar := s.onBarRow(ev.Row)
	if s.barMode != barModeChrome {
		// A bar mode owns input, so any page gesture in flight is over: drop the
		// capture here, because the mode swallows the release that would
		// otherwise clear it. Without this, a page press, a Ctrl+L, and a
		// release would leave the capture armed for the rest of the session.
		s.dragOnPage = false
	}
	switch s.barMode {
	case barModeEdit:
		if ev.Action != MouseActionPress {
			return false
		}
		if !onBar {
			s.leaveEdit()
			return false
		}
		// A press inside the editor places the caret at the clicked cell. The
		// prompt and field widths come from the last repaint, so the mapping
		// matches the row as drawn.
		s.editor.placeCursor(ev.Col-s.editPromptW, s.editFieldW)
		s.repaintBar()
		return false
	case barModeFind:
		s.handleFindMouse(ev, onBar)
		return false
	case barModeMenu, barModeAbout:
		if ev.Action != MouseActionPress {
			return false
		}
		if !onBar {
			s.leaveMode()
			return false
		}
		item, ok := spanAt(s.modeSpans, ev.Col+1)
		if !ok {
			return false
		}
		if item == barSpanClose {
			s.leaveMode()
			return false
		}
		if s.barMode == barModeMenu {
			s.menuSel = item
			return s.activateMenu(item)
		}
		return false
	}
	switch ev.Action {
	case MouseActionPress:
		s.dragOnPage = !onBar
	case MouseActionMove:
		switch {
		case s.dragOnPage:
			// A captured drag owns the pointer, so its motion reaches the page
			// even over the bar row; the engine clamps the cell to its grid.
		case ev.Button != MouseButtonNone:
			// A held button with no capture behind it is somebody else's
			// gesture, so the page never sees it.
			return false
		case onBar:
			// The bar row sits outside the engine grid and holds no page, so
			// there is nothing there to hover.
			return false
		}
		// Buttonless motion off the bar row is a hover.
		s.forwardMouse(ev)
		return false
	case MouseActionRelease:
		captured := s.dragOnPage
		s.dragOnPage = false
		if s.pressedSet {
			// Any release ends the bar's press feedback, including one that
			// landed off the bar row, so a bold button cannot stick.
			s.pressedSet = false
			s.repaintBar()
		}
		// Only a captured gesture's release belongs to the page. Without a
		// matching press the page never saw the gesture, so forwarding a bare
		// release would click whatever now sits under the pointer.
		if captured {
			s.forwardMouse(ev)
		}
		return false
	}
	if onBar {
		s.handleBarMouse(ev)
		return false
	}
	s.forwardMouse(ev)
	return false
}

// motionCoalesceInterval bounds how often coalesced pointer motion reaches the
// page. Mode 1003 reports every move, and where most terminals report one only
// when the pointer enters a new cell, VTE reports every pixel: a pointer that
// never left one cell was measured producing about thirty motion reports, and
// VTE drives gnome-terminal, so uncoalesced hover would hand a GNOME user a
// firehose of CDP dispatches. Sixteen milliseconds is roughly one frame at
// 60 Hz, and a hover is a request to repaint, so dispatching faster than the
// page can repaint buys nothing.
const motionCoalesceInterval = 16 * time.Millisecond

// forwardMouse forwards one mouse event to the engine, coalescing motion
// against the wall clock. The clock is a parameter on forwardMouseAt, the way
// nextClickCount takes the press instant, so tests drive explicit timestamps
// with no sleeping.
func (s *streamSession) forwardMouse(ev MouseEvent) {
	s.forwardMouseAt(ev, time.Now())
}

// forwardMouseAt forwards one mouse event as at the instant now, with motion
// coalesced latest-wins: a move always becomes the held report, because the
// newest position is the one worth sending, and it goes out at once only when
// the current window has closed. A move inside an open window stays held, and
// the loop wakes at the window edge to send it, so the last move of a burst is
// never swallowed. Every other action (a press, a release, a wheel) flushes the
// held report before dispatching itself, so coalescing can never reorder motion
// against a button. It runs on the consumer loop goroutine, which owns the held
// state, so it needs no lock.
func (s *streamSession) forwardMouseAt(ev MouseEvent, now time.Time) {
	if ev.Action != MouseActionMove {
		s.flushMotionAt(now)
		s.dispatchMouse(ev)
		return
	}
	s.heldMotion = ev
	s.motionHeld = true
	if !now.Before(s.motionWindow) {
		s.flushMotionAt(now)
	}
}

// flushMotionAt dispatches the held motion report and re-arms the window from
// now, so the next burst gets a whole interval of its own. It returns at once
// when nothing is held, so the loop's timer and the press, release, and wheel
// paths all call it unconditionally.
func (s *streamSession) flushMotionAt(now time.Time) {
	if !s.motionHeld {
		return
	}
	s.motionHeld = false
	s.motionWindow = now.Add(motionCoalesceInterval)
	s.dispatchMouse(s.heldMotion)
}

// dispatchMouse forwards one mouse event to the engine off the input loop
// through the keySender worker, so a wedged browser never freezes quit or
// draw. A drag's motion reports ride the same queue as its press and release,
// so the gesture reaches the page in order.
func (s *streamSession) dispatchMouse(ev MouseEvent) {
	if s.keySender != nil {
		s.keySender.sendMouse(ev)
		return
	}
	s.eng.SendMouse(ev)
}

// handleKey forwards one key to the engine or handles the viewer hotkeys and
// reports whether the user asked to quit. A non-chrome bar mode owns the
// whole keyboard first: the URL editor and the find field take every key
// including the Alt chords, and the menu and About rows take their navigation
// keys, so nothing leaks to CDP while a mode is open. In chrome, a scroll key
// forwards its code (the engine owns the offset); Tab, Shift+Tab, and Enter
// forward to the page as full key strokes for native focus traversal and
// activation; Ctrl+L opens the URL editor, Ctrl+F the find field, and Alt+F the
// menu; Alt+W copies the page's selection to the terminal clipboard and Ctrl+A
// selects it, Chromium choosing between the focused field and the document;
// Alt+R cycles the backend and restarts the stream at the new capture
// size; Alt+B and Alt+C cycle the
// Block glyph scheme and colour mode. Both cycles only affect Block
// rendering, so they are no-ops unless Block is the active backend: cycling
// under a graphics backend would change hidden state with no visible
// feedback, and the user would later land on Block in an unexpected mode.
// Printable bytes and assembled runes are page text and forward to the focused
// element; Backspace and Escape forward as full key strokes, so Escape reaches
// the page only when no bar mode claimed it. A paste forwards as one unit to
// the focused element, the parser having already bounded and assembled it.
func (s *streamSession) handleKey(ev keyEvent) (quit bool) {
	switch s.barMode {
	case barModeEdit:
		return s.handleEditKey(ev)
	case barModeMenu:
		return s.handleMenuKey(ev)
	case barModeAbout:
		return s.handleAboutKey(ev)
	case barModeFind:
		return s.handleFindKey(ev)
	}
	if ev.paste != "" {
		s.forwardPaste(ev.paste)
		return false
	}
	if ev.r != 0 {
		s.forwardText(string(ev.r))
		return false
	}
	if ev.scroll != ScrollNone {
		s.forwardKey(scrollKeyCode(ev.scroll))
		return false
	}
	act := ev.act
	if act == actionNone {
		act = byteKeyAction(ev.b)
	}
	switch act {
	case actionNone:
		if printableByte(ev.b) {
			s.forwardText(string(rune(ev.b)))
		}
	case actionQuit:
		return true
	case actionEditURL:
		s.enterEdit()
	case actionOpenFind:
		s.enterFind()
	case actionOpenMenu:
		s.enterMenu()
	case actionCopySelection:
		s.copySelection()
	case actionSelectAll:
		s.forwardKey("SelectAll")
	case actionFocusNext:
		s.forwardKey("Tab")
	case actionFocusPrev:
		s.forwardKey("ShiftTab")
	case actionActivate:
		s.forwardKey("Enter")
	case actionBackspace:
		s.forwardKey("Backspace")
	case actionDelete:
		s.forwardKey("Delete")
	case actionEscape:
		s.forwardKey("Escape")
	case actionReload:
		s.forwardNav(NavReload)
	case actionBack:
		s.forwardNav(NavBack)
	case actionForward:
		s.forwardNav(NavForward)
	case actionOpenHome:
		s.forwardNav(NavHome)
	case actionCycleBackend:
		s.cycleBackend(false)
	case actionCycleBackendReverse:
		s.cycleBackend(true)
	case actionCycleBlocks, actionCycleBlocksReverse:
		if s.st.Backend != BackendBlock {
			break
		}
		before := s.st.Scheme
		s.recomposeBlock(s.st.CycleScheme(act == actionCycleBlocksReverse) != before)
	case actionCycleColour, actionCycleColourReverse:
		if s.st.Backend != BackendBlock {
			break
		}
		before := s.st.Colour
		s.recomposeBlock(s.st.CycleColour(act == actionCycleColourReverse) != before)
	}
	return false
}

// forwardKey forwards one key code to the engine off the input loop through
// the keySender worker, so a wedged browser never freezes quit or draw.
func (s *streamSession) forwardKey(code string) {
	if s.keySender != nil {
		s.keySender.send(code)
		return
	}
	s.eng.SendKey(code)
}

// forwardText forwards typed page text to the engine off the input loop
// through the same worker as keys, so text and page keys arrive in typing
// order and a wedged browser never freezes quit or draw.
func (s *streamSession) forwardText(text string) {
	if s.keySender != nil {
		s.keySender.sendText(text)
		return
	}
	s.eng.SendText(text)
}

// forwardPaste forwards one pasted unit to the engine off the input loop
// through the same worker as keys and typed text, so a paste lands in typing
// order and a wedged browser never freezes quit or draw. A paste the page
// dropped for want of a focused field reports itself on the bar, so the reader
// is not left to wonder where the text went.
func (s *streamSession) forwardPaste(text string) {
	if s.keySender != nil {
		s.keySender.sendPaste(text, s.outcomes)
		return
	}
	if s.eng.SendPaste(text) {
		s.showSignal(signalPasteDiscarded, 0)
	}
}

// forwardNav forwards one navigation request to the engine through the same
// serialised worker as keys, so a reload and a queued scroll burst reach the
// engine in arrival order.
func (s *streamSession) forwardNav(a NavAction) {
	if s.keySender != nil {
		s.keySender.sendNav(a)
		return
	}
	s.eng.SendNav(a)
}

// cycleBackend steps the backend cycle (Alt+R forwards, Alt+Shift+R
// backwards) and runs the whole switch: the leave-kitty reset, the shared
// switch clear, the kitty-shm decode sync, and the engine restart at the new
// capture size on the control goroutine.
func (s *streamSession) cycleBackend(reverse bool) {
	previous := s.st.Backend
	wasKitty := previous == BackendKitty
	if reverse {
		s.st.CycleBackendReverse()
	} else {
		s.st.CycleBackend()
	}
	// One control buffer carries the whole switch clear, routed through the
	// writer so it stays ordered against the frames and never races an
	// in-flight write. Best-effort: a write failure surfaces on the next
	// frame via the writer's error channel.
	var ctl bytes.Buffer
	if wasKitty && s.st.Backend != BackendKitty {
		// Free both double-buffer images when leaving kitty, so the terminal keeps
		// no stale image and a later return rebuilds from id 1.
		_ = s.resetKitty(&ctl)
	}
	if s.st.Backend != previous {
		// Every backend change erases the screen before the new backend's
		// first frame, one shared rule for all transitions. Leaving Block,
		// the old glyphs and cell backgrounds would otherwise sit under the
		// next graphics frame (and under Kitty's FPS page image, which
		// places below text). Leaving Sixel, the old raster persists over
		// cells the terminal believes unchanged. Reset the SGR
		// first: the Block differ leaves the last cell's background active,
		// and terminals erase with the current background (background
		// colour erase). Invalidate the differ against the blank terminal,
		// so the next Block frame full-repaints instead of skipping cells
		// matching the previous Block stint. The bar repaints after the
		// clear with the new backend's indicator, so a pre-frame cycle is
		// safe, matching resize.
		if s.diff != nil {
			s.diff.Reset()
		}
		ctl.WriteString(sgrReset + clearScreen)
		s.appendBar(&ctl)
	}
	if ctl.Len() > 0 {
		_ = s.emitControl(ctl.Bytes())
	}
	// Align the engine's kitty-shm decode with the new backend before the
	// restart, so frames after the switch arrive in the right form: planes
	// while Kitty runs the shm transport, direct PNG otherwise. The toggle
	// is a cheap flag write, safe on the consumer loop.
	syncEngineKittyDecode(s.eng, s.st)
	// Cycle the backend off the consumer loop: the engine restart runs on the
	// control goroutine so a slow restartStream cannot freeze draw or quit.
	s.ctl.submit(switchBackendOp(s.st.Backend))
}

// recomposeBlock repaints the live Block view after a scheme or colour cycle
// changed it. It follows the backend-switch clear rule: reset the differ
// against the blank terminal, reset the SGR before the erase, and route the
// clear through the writer, then restart the stream at the unchanged grid so
// the engine re-emits a frame for the new composition (a static page pushes
// no frame of its own). changed is false when the cycle was a no-op (every
// other mode capped) and a no-op skips the repaint. handleKey only dispatches
// the cycles while Block is the active backend, so this always repaints Block.
func (s *streamSession) recomposeBlock(changed bool) {
	if !changed {
		return
	}
	if s.diff != nil {
		s.diff.Reset()
	}
	var ctl bytes.Buffer
	ctl.WriteString(sgrReset + clearScreen)
	s.appendBar(&ctl)
	_ = s.emitControl(ctl.Bytes())
	s.ctl.submit(switchBackendOp(s.st.Backend))
}

// keySender serialises forwarded input (page keys and mouse events) away
// from the input loop, so a browser call that stalls never freezes quit or
// draw. Unlike the frame path (latest-wins-drop), input is cumulative: a rapid
// burst of caret keys or wheel events must all reach the engine, or the page
// barely moves and a held-down arrow loses caret steps. So it accumulates every
// submitted event and the worker forwards them all in order rather than
// dropping to the last event. send and sendMouse never block the input loop. It owns its
// done channel and closes it once on close, stopping the worker.
type keySender struct {
	eng     Engine
	wake    chan struct{}
	done    chan struct{}
	mu      sync.Mutex
	pending []func(Engine)
	once    sync.Once
}

func newKeySender(eng Engine) *keySender {
	s := &keySender{
		eng:  eng,
		wake: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
	go s.run()
	return s
}

// send queues one scroll-key code for the worker. The empty code is ignored.
func (s *keySender) send(code string) {
	if code == "" {
		return
	}
	s.enqueue(func(e Engine) { e.SendKey(code) })
}

// sendText queues one typed-text payload for the worker on the same queue as
// keys, so typing and page keys reach the engine in arrival order. The empty
// string is ignored.
func (s *keySender) sendText(text string) {
	if text == "" {
		return
	}
	s.enqueue(func(e Engine) { e.SendText(text) })
}

// sendPaste queues one pasted unit for the worker on the same queue as keys
// and typed text, so a paste never overtakes an earlier keystroke. The empty
// string is ignored. A paste the page dropped for want of a focused field posts
// its outcome on out for the consumer loop to signal; every other result posts
// nothing. The send waits on the loop rather than dropping the outcome, and
// gives up on close, so a paste taken as the session ends cannot park the
// worker.
func (s *keySender) sendPaste(text string, out chan<- inputOutcome) {
	if text == "" {
		return
	}
	s.enqueue(func(e Engine) {
		if !e.SendPaste(text) {
			return
		}
		select {
		case out <- inputOutcome{kind: signalPasteDiscarded}:
		case <-s.done:
		}
	})
}

// sendMouse queues one decoded mouse event for the worker on the same queue
// as scroll keys, so a wheel burst and a click reach the engine in arrival
// order.
func (s *keySender) sendMouse(ev MouseEvent) {
	s.enqueue(func(e Engine) { e.SendMouse(ev) })
}

// sendNav queues one navigation request for the worker on the same queue as
// keys, so a reload never overtakes an earlier queued key.
func (s *keySender) sendNav(a NavAction) {
	s.enqueue(func(e Engine) { e.SendNav(a) })
}

// sendOpen queues one typed-URL navigation for the worker on the same queue
// as keys, so a commit never overtakes an earlier queued key.
func (s *keySender) sendOpen(url string) {
	s.enqueue(func(e Engine) { e.OpenURL(url) })
}

// sendFind queues one page search for the worker on the same queue as keys, so
// a search never overtakes an earlier keystroke and a slow browser search never
// blocks the input loop. The counts land on the engine's state snapshot, which
// the bar reads on its next repaint.
func (s *keySender) sendFind(query string) {
	s.enqueue(func(e Engine) { e.Find(query) })
}

// sendFindNext queues one step to the next match on the same queue as keys.
func (s *keySender) sendFindNext() {
	s.enqueue(func(e Engine) { e.FindNext() })
}

// sendFindPrev queues one step to the previous match on the same queue as keys.
func (s *keySender) sendFindPrev() {
	s.enqueue(func(e Engine) { e.FindPrev() })
}

// sendCopy queues one selection read on the same queue as keys, so it never
// overtakes an earlier keystroke and a slow read never blocks the input loop. The
// worker posts the outcome on out for the consumer loop to write and signal: the
// text for a completed copy, the refusal for a selection over the bound, and
// nothing at all when there was nothing to copy. The send waits on the loop
// rather than dropping the outcome, and gives up on close, so a copy taken as
// the session ends cannot park the worker.
func (s *keySender) sendCopy(out chan<- inputOutcome) {
	s.enqueue(func(e Engine) {
		o, ok := copyOutcome(e.Selection())
		if !ok {
			return
		}
		select {
		case out <- o:
		case <-s.done:
		}
	})
}

// sendFindClear queues one search clear on the same queue as keys, so it never
// overtakes a search still waiting to dispatch.
func (s *keySender) sendFindClear() {
	s.enqueue(func(e Engine) { e.FindClear() })
}

// enqueue appends one forward closure with a non-blocking wake, so the input
// loop never blocks on a stalled browser. A queued event is never dropped: a
// burst accumulates and the worker forwards every one, so the engine sums the
// scroll instead of taking a single step.
func (s *keySender) enqueue(fn func(Engine)) {
	s.mu.Lock()
	s.pending = append(s.pending, fn)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// drain removes and returns all queued events in submission order, resetting
// the buffer. The worker forwards the whole batch, so events queued while a
// dispatch was in flight are summed by the engine, not lost.
func (s *keySender) drain() []func(Engine) {
	s.mu.Lock()
	events := s.pending
	s.pending = nil
	s.mu.Unlock()
	return events
}

// run forwards queued events serially off the input loop until done closes. A
// single wake may cover several appends because drain takes the whole batch, so
// no event is dropped between the append and the drain.
func (s *keySender) run() {
	for {
		select {
		case <-s.wake:
			for _, fn := range s.drain() {
				fn(s.eng)
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

// fpsBadgeInterval bounds how often the bar's fps text is rebuilt, so it
// changes at most once per second: the meter counts every draw, but the text
// is regenerated no more than once per interval and the cached string is
// reused between rebuilds. The idle heartbeat runs at the same interval.
const fpsBadgeInterval = time.Second

// fpsPeakHold is how long the badge holds the highest In/Out it has seen. The
// instantaneous windowed rate decays in a second, so a discrete scroll (a
// sub-second frame burst) would flash a number and fall back to 0 before it can be
// read. The badge shows the peak over this trailing span instead, so a scroll burst
// stays legible for a few seconds, then fades as the burst ages out.
const fpsPeakHold = 3 * time.Second

// fpsSample is one badge-rate observation: the In and Out rates measured at t. The
// meter keeps a trailing fpsPeakHold window of these so the badge can show the peak.
type fpsSample struct {
	t   time.Time
	in  float64
	out float64
}

// fpsMeter tracks the live usable-frames-per-second figure for the NYALA_FPS_TIMING
// instrument: a rolling window feeds the badge and running accumulators feed the
// quit summary. The rate stats are gated to steady state: they accumulate only from
// the first frame received after main-frame first contentful paint, so the page
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

	// Paint-phase split, in ms per post-gate frame: the encode (RenderStreamFrame
	// into the buffer) and the write (buffer flush to the tty). The sums divide by
	// paintFrames at quit for the means; the maxes hold the worst single frame.
	paintFrames int
	encodeSum   float64
	encodeMax   float64
	writeSum    float64
	writeMax    float64

	text     string    // cached bar fps text, rebuilt at most once per interval
	textAt   time.Time // instant text was last rebuilt
	haveText bool      // set once text holds a real string

	peaks []fpsSample // trailing fpsPeakHold window of In/Out samples, for the badge peak

	// received is the cumulative count of frames the draw loop pulled from the
	// engine channel, including coalesced ones, from stream start with no warm-up
	// gate. The badge appends it in parentheses so a stalled stream (the total
	// frozen while the page should repaint) reads differently from an idle one
	// where the rates alone show the ambiguous "--" for both.
	received int

	// ingest is the optional engine ingest source. When set the badge and summary
	// show the screencast ingest rate (In) beside the paint rate (Out); nil (a fake
	// engine, or the switch off) shows Out only. It is read on the draw goroutine.
	ingest ingestReporter
}

// ingestReporter is the optional engine capability the viewer reads for the
// session-end decode warning and, when armed, the fps meter's In rate and summary.
// ChromiumEngine implements it; a fake engine that does not is shown as Out-only.
// The viewer type-asserts the Engine against it, so the Engine seam and all fakes
// stay unchanged.
type ingestReporter interface {
	// IngestFPS returns the rolling window ingest rate at now, for the live badge.
	IngestFPS(now time.Time) float64
	// IngestSummary returns the session ingest statistics, including decode failures,
	// for the quit summary or default warning.
	IngestSummary() render.IngestStats
}

// newFPSMeter returns a meter when NYALA_FPS_TIMING is set in env, or nil
// otherwise. The env read happens once per Stream, so an off run does no per-frame
// work beyond the draw path's nil check. start is stamped now, so the warm-up span
// measures from Stream construction.
func newFPSMeter(env []string) *fpsMeter {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, fpsTimingEnv); ok && v != "" {
			return armFPSMeter()
		}
	}
	return nil
}

// armFPSMeter returns a meter measuring from now. Stream calls it through
// newFPSMeter for the NYALA_FPS_TIMING switch, and the menu's fps command calls
// it directly when a user asks for the badge on a session the switch left off.
// Only the switch's meter reaches printSummary: Stream registers that defer
// against the startup meter, so a meter armed later prints no summary and the
// measurement controls keep their exact output.
func armFPSMeter() *fpsMeter {
	return &fpsMeter{window: fpsWindow, start: time.Now()}
}

// tick records a draw at now. afterFCP reports that the frame arrived after the
// main frame's first contentful paint. Before that marker it records nothing, so
// neither the live badge nor the summary measures page load. From the gate frame
// onward it counts
// frames, the wall span, the inter-frame gaps (min, max, and the slice the median
// sorts at quit). It allocates nothing per call beyond the amortised window and gap
// slices, so an armed run stays cheap.
func (m *fpsMeter) tick(now time.Time, afterFCP bool) {
	if !m.gated {
		if !afterFCP {
			return
		}
		m.gated = true
		m.warmup = now.Sub(m.start)
		m.gateAt = now
	}

	cutoff := now.Add(-m.window)
	m.times = append(m.times, now)
	drop := 0
	for drop < len(m.times) && m.times[drop].Before(cutoff) {
		drop++
	}
	m.times = m.times[drop:]

	// Sample the live In/Out peak from the FCP gate onward.
	m.samplePeak(now)

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

// recordPaint accumulates one drawn frame's encode and write times, in ms, for the
// out_encode_* and out_write_* summary fields. It records only from the warm-up gate
// onward, so the load-period paints stay out of the steady-state budget, mirroring
// the Out rate stats. The draw path calls it once per armed draw, right after tick,
// so it reads the gate state this frame set.
func (m *fpsMeter) recordPaint(encodeMS, writeMS float64) {
	if !m.gated {
		return
	}
	m.paintFrames++
	m.encodeSum += encodeMS
	if encodeMS > m.encodeMax {
		m.encodeMax = encodeMS
	}
	m.writeSum += writeMS
	if writeMS > m.writeMax {
		m.writeMax = writeMS
	}
}

// countReceived adds n delivered frames to the cumulative badge total. The
// receive arm calls it for every frame pulled from the engine channel, before
// any draw gating, so the total moves whenever the engine delivers at all.
func (m *fpsMeter) countReceived(n int) {
	m.received += n
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

// barText returns the fps segment body for the bottom bar. It rebuilds the
// string (the only per-call allocation) at most once per fpsBadgeInterval and
// returns the cached string between rebuilds, so the fps text refreshes about
// once per second while the bar repaints on every frame.
func (m *fpsMeter) barText(now time.Time) string {
	if !m.haveText || now.Sub(m.textAt) >= fpsBadgeInterval {
		m.formatBarText(now)
	}
	return m.text
}

// formatBarText rebuilds and caches the fps text at now, the compact
// "↓ NN.n │ ↑ NN.n │ ≌ N" segment: ↓ is the ingest rate (frames arriving
// from the engine, the ingest reporter's windowed rate), ↑ the output rate
// (frames written to the terminal, the meter's own paint window rate), and
// ≌ prefixes N, the cumulative received-frame total. Both rates are windowed
// to one decimal and decay live when idle; N never decays, so a frozen N
// with "--" rates is a stalled or idle stream, while a moving N with "--"
// rates is delivery the peak window has already aged out. Without an ingest
// reporter the ↓ field is absent. The text carries no escapes and no
// positioning; the bar owns placement and style.
func (m *fpsMeter) formatBarText(now time.Time) {
	in, out := m.peakRates()
	sep := " " + barSeparator + " "
	m.text = "↑ " + fpsField(out) + sep + "≌ " + strconv.Itoa(m.received)
	if m.ingest != nil {
		m.text = "↓ " + fpsField(in) + sep + m.text
	}
	m.textAt = now
	m.haveText = true
}

// fpsField renders one badge rate: the value to one decimal, or "--" when it is
// zero (no frames in the peak-hold window). A dash reads as idle, which is clearer
// feedback than a misleading "0.0" when nothing is rendering.
func fpsField(v float64) string {
	if v <= 0 {
		return "--"
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

// samplePeak records the current In and Out rates at now and ages the peak window
// out to fpsPeakHold. tick calls it on every draw and refresh calls it on the idle
// heartbeat, so the window sees both frame bursts and idle gaps. Out is the paint
// window rate; In is the engine ingest rate, or 0 with no reporter.
func (m *fpsMeter) samplePeak(now time.Time) {
	in := 0.0
	if m.ingest != nil {
		in = m.ingest.IngestFPS(now)
	}
	m.peaks = append(m.peaks, fpsSample{t: now, in: in, out: m.windowRate()})
	cutoff := now.Add(-fpsPeakHold)
	drop := 0
	for drop < len(m.peaks) && m.peaks[drop].t.Before(cutoff) {
		drop++
	}
	m.peaks = m.peaks[drop:]
}

// peakRates returns the highest In and Out rates in the trailing peak window, so the
// badge holds a scroll burst's rate for fpsPeakHold before it fades. An empty window
// (no samples yet) reports 0.
func (m *fpsMeter) peakRates() (in, out float64) {
	for _, s := range m.peaks {
		if s.in > in {
			in = s.in
		}
		if s.out > out {
			out = s.out
		}
	}
	return in, out
}

// refresh ages the trailing window out to now without recording a frame, then
// rebuilds the fps text. The draw loop calls it on a heartbeat between frames,
// so an idle page (the block backend pushes no frames while static) decays the
// live rate to 0 instead of freezing the text at its last value. It records no
// frame and touches no summary stat, so a heartbeat never inflates the
// counted-frame total.
func (m *fpsMeter) refresh(now time.Time) {
	cutoff := now.Add(-m.window)
	drop := 0
	for drop < len(m.times) && m.times[drop].Before(cutoff) {
		drop++
	}
	m.times = m.times[drop:]
	// Sample the (now decayed) rate so the peak window ages out on idle too: once a
	// burst is older than fpsPeakHold the text falls back to 0.
	m.samplePeak(now)
	m.formatBarText(now)
}

// printStreamSummary writes the one session-end report. An armed fps meter owns
// the output, including decode failures. Without one, a non-zero decode-drop count
// produces one warning; a clean session stays silent.
func printStreamSummary(w io.Writer, fps *fpsMeter, ingest ingestReporter) {
	if fps != nil {
		fps.printSummary(w)
		return
	}
	if ingest == nil {
		return
	}
	stats := ingest.IngestSummary()
	if stats.DecodeDrops == 0 {
		return
	}
	fmt.Fprintf(w, "nyala: warning: frame decode failures: dropped=%d latest_error=%q\n", stats.DecodeDrops, stats.DecodeLatestError)
}

// printSummary writes one compact fps summary line to w after the session ends. It
// reports warmup_ms (time to the first counted frame, so the "loads slowly" signal
// stays visible), then the two rate streams. The In stream (screencast ingest) gets
// the full steady-state stats over the ingest span: in_frames, in_mean, the in_min
// and in_max instantaneous fps (the slowest and fastest single frames), in_median,
// and in_worst_gap_ms, plus the worker decode budget in_decode_mean_ms and
// in_decode_max_ms (how close each frame sits to the ~16.6ms 60fps ceiling the decode
// wall imposes), the decode-drop total, and the quoted latest failure. Under the
// decoupled receiver/worker model the arrival times are true
// per-frame instants, so the In extremes are meaningful and mirror Out exactly
// (in_min is 1/maxGap, and so on). The Out stream (paint) gets out_frames, wall
// seconds, out_mean and out_median fps, out_min and out_max, out_worst_gap_ms
// (which exposes a post-load stall a mean hides), then the paint-phase split in ms
// per frame: out_encode_mean_ms and out_encode_max_ms (rendering the frame into the
// buffer, our terminal-byte encode) and out_write_mean_ms and out_write_max_ms
// (flushing that buffer to the tty). The split localises where each paint's time
// goes: our encode versus the terminal write. The trailing ack_rtt_mean_ms and
// ack_rtt_max_ms report the engine's screencast acknowledgement command round
// trip, so a slow ack loop that starves Chromium's frame credit shows up here.
// When no ingest reporter is wired the In and ack fields read zero. A session whose gate never opened reports
// warmup_ms=unavailable and no invented duration. A nil meter stays silent.
func (m *fpsMeter) printSummary(w io.Writer) {
	if m == nil {
		return
	}
	var in render.IngestStats
	if m.ingest != nil {
		in = m.ingest.IngestSummary()
	}
	if m.frames == 0 {
		if in.DecodeDrops != 0 {
			fmt.Fprintf(w, "nyala fps: warmup_ms=unavailable in_decode_drops=%d in_decode_latest_error=%q\n", in.DecodeDrops, in.DecodeLatestError)
			return
		}
		fmt.Fprintln(w, "nyala fps: warmup_ms=unavailable")
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
	encodeMean, writeMean := 0.0, 0.0
	if m.paintFrames > 0 {
		encodeMean = m.encodeSum / float64(m.paintFrames)
		writeMean = m.writeSum / float64(m.paintFrames)
	}
	fmt.Fprintf(w, "nyala fps: warmup_ms=%.0f in_frames=%d in_mean=%.1f in_min=%.1f in_max=%.1f in_median=%.1f in_worst_gap_ms=%.1f in_decode_mean_ms=%.1f in_decode_max_ms=%.1f in_decode_drops=%d in_decode_latest_error=%q out_frames=%d wall=%.2fs out_mean=%.1f out_median=%.1f out_min=%.1f out_max=%.1f out_worst_gap_ms=%.1f out_encode_mean_ms=%.1f out_encode_max_ms=%.1f out_write_mean_ms=%.1f out_write_max_ms=%.1f ack_rtt_mean_ms=%.1f ack_rtt_max_ms=%.1f\n",
		warmupMS, in.Frames, in.MeanFPS, in.MinFPS, in.MaxFPS, in.MedianFPS, in.WorstGapMS, in.DecodeMeanMS, in.DecodeMaxMS, in.DecodeDrops, in.DecodeLatestError,
		m.frames, wall, mean, medianFPS(m.gaps), minInst, maxInst, worstGapMS,
		encodeMean, m.encodeMax, writeMean, m.writeMax, in.AckRTTMeanMS, in.AckRTTMaxMS)
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

// scrollKeyCode maps a recognised caret key to the CDP key code the engine
// strokes into the page. The viewer forwards the code; Chromium decides
// whether it moves the caret or scrolls, and the engine adopts the offset the
// page settled at.
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
	case ScrollHome:
		return "Home"
	case ScrollEnd:
		return "End"
	default:
		return ""
	}
}
