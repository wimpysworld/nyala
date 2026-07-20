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
	// Frames returns the channel of pixel or direct-PNG frames, each with Text synced on.
	Frames() <-chan render.Frame
	// SendKey forwards a key by its code (for example ArrowDown) to the page.
	// Scroll keys route to the engine's wheel transport; the engine owns the
	// offset.
	SendKey(code string)
	// SendScroll scrolls the page down by dy CSS pixels through the engine's wheel
	// transport. Diagnostic tests use it to exercise scroll synchronisation.
	SendScroll(dy int)
	// Close tears down the engine and the browser process.
	Close() error
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
	// Wire the engine's optional ingest reporter so the badge and summary can show
	// the screencast ingest rate (In) beside the paint rate (Out). ChromiumEngine
	// implements it; a fake engine that does not leaves ingest nil, shown Out-only.
	// Only when the meter is armed, so an off run never type-asserts.
	if fps != nil {
		if r, ok := eng.(ingestReporter); ok {
			fps.ingest = r
		}
	}
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
	// handling. It stops when Stream returns and the deferred close(done) fires.
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
// and hands the resolved backend, cell px, and layout width to the engine before
// Navigate so the engine sizes its screencast capture and block device scale. It
// returns the tty fd, the State, and a restore closure the caller must defer to
// leave raw mode. Scheme is the resolved block glyph scheme; it is set directly
// on State. The caller resolves it through detection.
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

	state := NewState(cols, rows)
	// Set the resolved block glyph scheme, overriding the NewState quadrant default
	// for this run. The caller resolves the --blocks mode; auto keeps quadrant as
	// the production and corpus default.
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
	eng.SetBackend(backendName(state.Backend))
	eng.Resize(cols, rows)

	return fd, &state, func() { _ = cterm.Restore(fd, oldState) }, nil
}

// streamSession bundles the per-run viewer state the push loop mutates: the
// engine it forwards to, the State it draws from, the tty it writes to, and the
// tty fd it reads the resized geometry from. firstDrawn gates the resize clear
// so no clear lands before the loading cue is replaced by a real frame.
type streamSession struct {
	eng Engine
	st  *State
	// tty is the terminal sink. It is an io.Writer (satisfied by the *os.File the
	// production path passes) so the writer goroutine and a test can drive it; the
	// resized geometry is read from fd, not tty.
	tty          io.Writer
	fd           uintptr
	keySender    *keySender
	ctl          *control
	diff         *block.GridDiff
	firstDrawn   bool
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
	// fps is the live fps counter, non-nil only when NYALA_FPS_TIMING is set. When
	// set, assemble stamps each frame and folds a rolling usable-fps badge into the
	// frame buffer (rebuilt at most once per second); when nil the draw path pays
	// only a nil check, so production is unchanged.
	fps *fpsMeter
}

// loop runs the push consumer until the frames or keys channel closes, a quit
// key arrives, or an I/O error occurs. It draws each pushed frame, restarts the
// stream on SIGWINCH at the recomputed grid, and forwards keys. It caches the
// frame channel once: the engine keeps the channel stable across Resize and
// SetBackend and replaces it only by Navigate, which is single-shot, so a backend
// switch or a resize never strands this cached handle.
func (s *streamSession) loop(winch <-chan os.Signal, keys <-chan keyEvent) error {
	frames := s.eng.Frames()
	// Heartbeat the fps badge so an idle page decays it to a live 0 rather than
	// freezing it at the last frame's rate. The block backend pushes no frames while
	// static, so without this a scroll that settles leaves the badge wedged. A nil
	// channel blocks forever in the select, so an off run (no meter) starts no ticker
	// and the case never fires.
	var heartbeat <-chan time.Time
	if s.fps != nil {
		t := time.NewTicker(fpsBadgeInterval)
		defer t.Stop()
		heartbeat = t.C
	}

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

	for {
		// Encode the newest pending frame into a spare buffer when one is free and
		// nothing already waits to hand off. Holding only one assembled frame at a
		// time bounds the in-flight buffers to two.
		if s.assembled == nil && havePending && len(avail) > 0 {
			buf := avail[len(avail)-1]
			avail = avail[:len(avail)-1]
			encodeMS, err := s.assemble(buf, pending)
			if err != nil {
				return err
			}
			s.assembled = buf
			s.assembledEncodeMS = encodeMS
			havePending = false
		}

		// Enable the handoff case only when a buffer waits. A nil channel send never
		// fires, so the select ignores it while nothing is assembled.
		var readyC chan<- writeItem
		var item writeItem
		if s.assembled != nil {
			readyC = w.ready
			item = writeItem{buf: s.assembled, encodeMS: s.assembledEncodeMS, timed: s.fps != nil}
		}

		select {
		case readyC <- item:
			s.assembled = nil
		case <-winch:
			if err := s.resize(); err != nil {
				return err
			}
		case now := <-heartbeat:
			// Route the idle badge refresh through the writer so it never races the
			// in-flight frame write.
			_ = s.emitControl([]byte(s.fps.refresh(now)))
		case frame, ok := <-frames:
			if !ok {
				return nil
			}
			// Coalesce a queued burst to the newest frame, so a scroll burst presents
			// once with the latest frame instead of every stale one. Channel close
			// mid-drain stops the loop, matching the main receive. The differ stays
			// correct because it compares the assembled frame against the previous
			// assembled grid, so skipped frames do not corrupt its state.
			frame, ok = drainFrames(frames, frame)
			if !ok {
				return nil
			}
			if !frame.HasImage() {
				s.nilImageOnce.Do(func() {
					fmt.Fprintln(os.Stderr, "nyala: dropped a frame with no image")
				})
				continue
			}
			pending = frame
			havePending = true
		case fi := <-w.free:
			// A drained buffer came back: record its paint split on this goroutine
			// (the meter is single-goroutine) and return the buffer to the free set.
			if s.fps != nil {
				s.fps.recordPaint(fi.encodeMS, fi.writeMS)
			}
			avail = append(avail, fi.buf)
		case ev, ok := <-keys:
			if !ok {
				return nil
			}
			if s.handleKey(ev) {
				return nil
			}
		case err := <-w.errc:
			// The writer hit a terminal write error; surface it as an I/O failure the
			// same way the serial path did.
			return err
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

// assemble builds one frame's whole terminal byte sequence into buf, ready for the
// writer to present with a single io.Write: the DEC 2026 open, the cursor prefix,
// the rendered frame with its crisp DOM-text overlay, the fps badge, and the DEC
// 2026 close. Assembling the whole unit into one owned buffer keeps the atomic
// present (the terminal never shows a torn partial frame) while the producer
// encodes the next frame in parallel with the current frame's write. Byte order
// remains identical for every frame, independent of which goroutine writes it.
//
// The first frame emits the full clearScreen (via framePrefix) to wipe the loading
// cue; steady frames emit cursorHome only. It returns the encode time, the
// RenderStreamFrame span alone (matching the out_encode split), when the fps meter
// is armed, or zero when off (no clock read). buf is one of the two ping-pong
// buffers; the producer owns it until the writer returns it on free.
func (s *streamSession) assemble(buf *bytes.Buffer, frame render.Frame) (encodeMS float64, err error) {
	buf.Reset()
	prefix := framePrefix(s.firstDrawn, s.diff)
	buf.WriteString(beginSyncUpdate)
	buf.WriteString(prefix)
	s.firstDrawn = true

	timed := s.fps != nil
	var encodeStart time.Time
	if timed {
		encodeStart = time.Now()
	}
	if rerr := s.st.RenderStreamFrame(frame, buf); rerr != nil {
		return 0, rerr
	}
	if timed {
		encodeMS = msSince(encodeStart)
	}

	// Fold the fps badge into the same buffer so each atomic present carries it,
	// gated behind NYALA_FPS_TIMING. It sits inside the DEC 2026 bracket: painted
	// after the close it flickered out during a scroll, when back-to-back frames each
	// present the grid alone before the un-synced badge lands. The badge is a
	// cursor-saved overlay, so it never enters the block grid the differ tracks and
	// leaves the golden-gated compose bytes untouched. recordPaint is deferred to
	// when the writer returns the buffer, because the write time is known only then.
	if s.fps != nil {
		now := time.Now()
		// A frame carrying text runs is the first-text-published milestone reaching
		// the viewer directly, no engine plumbing: it opens the warm-up gate so the
		// page-load stall stays out of the rate stats.
		s.fps.tick(now, len(frame.Text) > 0)
		buf.WriteString(s.fps.badge(now))
	}
	buf.WriteString(endSyncUpdate)
	return encodeMS, nil
}

// flushAssembled hands any assembled-but-unsent frame to the writer with a
// blocking send, so a following control write or teardown keeps frame order. It
// runs only on the producer goroutine, which owns assembled and the writer handle.
func (s *streamSession) flushAssembled() {
	if s.writer == nil || s.assembled == nil {
		return
	}
	s.writer.send(writeItem{buf: s.assembled, encodeMS: s.assembledEncodeMS, timed: s.fps != nil})
	s.assembled = nil
}

// emitControl routes a one-off terminal write (a resize clear, a kitty reset, or an
// fps badge) so it stays ordered against the frame writes. In the draw loop it
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
// only on a rare resize or Tab, because the per-second heartbeat runs only when the
// fps meter is armed.
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
	// even when the grid dimensions are unchanged. The clear routes through the
	// writer so it never races an in-flight frame write.
	s.diff.Reset()
	return s.emitControl([]byte(clearScreen))
}

// handleKey forwards one key to the engine or handles the viewer hotkeys and
// reports whether the user asked to quit. A scroll key forwards its code; the
// engine owns the offset. Tab cycles the backend and restarts the stream at the
// new capture size.
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
	switch ev.b {
	case 'q', 'Q', keyQuitCtrlC:
		return true
	case keyTab:
		wasKitty := s.st.Backend == BackendKitty
		s.st.CycleBackend()
		if wasKitty && s.st.Backend != BackendKitty {
			// Free both double-buffer images when leaving kitty, so the terminal keeps
			// no stale image and a later return rebuilds from id 1. Capture the reset
			// bytes and route them through the writer so they stay ordered against the
			// frames and never race an in-flight write. Best-effort: a write failure
			// surfaces on the next frame via the writer's error channel.
			var kb bytes.Buffer
			_ = s.st.resetKitty(&kb)
			if kb.Len() > 0 {
				_ = s.emitControl(kb.Bytes())
			}
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

	// Paint-phase split, in ms per post-gate frame: the encode (RenderStreamFrame
	// into the buffer) and the write (buffer flush to the tty). The sums divide by
	// paintFrames at quit for the means; the maxes hold the worst single frame.
	paintFrames int
	encodeSum   float64
	encodeMax   float64
	writeSum    float64
	writeMax    float64

	badgeStr  string    // cached escape-wrapped badge, rebuilt at most once per interval
	badgeAt   time.Time // instant badgeStr was last rebuilt
	haveBadge bool      // set once badgeStr holds a real string

	peaks []fpsSample // trailing fpsPeakHold window of In/Out samples, for the badge peak

	// ingest is the optional engine ingest source. When set the badge and summary
	// show the screencast ingest rate (In) beside the paint rate (Out); nil (a fake
	// engine, or the switch off) shows Out only. It is read on the draw goroutine.
	ingest ingestReporter
}

// ingestReporter is the optional engine capability the fps meter reads to show the
// screencast ingest rate beside the paint rate. ChromiumEngine implements it; a
// fake engine that does not is shown as Out-only. The viewer type-asserts the
// Engine against it, so the Engine seam and all fakes stay unchanged.
type ingestReporter interface {
	// IngestFPS returns the rolling window ingest rate at now, for the live badge.
	IngestFPS(now time.Time) float64
	// IngestSummary returns the session ingest statistics (frame count, mean/min/max/
	// median fps, worst gap, and the worker decode mean and max) for the quit summary.
	IngestSummary() render.IngestStats
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

	// Sample the live In/Out peak every draw, before the warm-up early-return, so a
	// scroll burst is captured whether or not the gate has opened.
	m.samplePeak(now)

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
		return m.formatBadge(now)
	}
	return m.badgeStr
}

// formatBadge rebuilds and caches the badge string at now: a cursor save, a home,
// the reverse-video rate, an SGR reset, and a cursor restore. With an ingest
// reporter it shows "In: X.X / Out: Y.Y fps"; both are windowed, so both decay live
// when idle. Without one it shows "Out: Y.Y fps" alone. It is the sole per-call
// allocation and the single place the badge escape is built. The SGR opens with a
// reset then reverse ("\x1b[0;7m"): the reset clears the grid's ambient colour so
// reverse inverts the terminal default, not whatever page colour the differ left
// active, which otherwise tinted the badge from a scrolled-away cell.
func (m *fpsMeter) formatBadge(now time.Time) string {
	in, out := m.peakRates()
	if m.ingest != nil {
		m.badgeStr = fmt.Sprintf("\x1b7\x1b[1;1H\x1b[0;7m In: %s / Out: %s fps \x1b[0m\x1b8", fpsField(in), fpsField(out))
	} else {
		m.badgeStr = fmt.Sprintf("\x1b7\x1b[1;1H\x1b[0;7m Out: %s fps \x1b[0m\x1b8", fpsField(out))
	}
	m.badgeAt = now
	m.haveBadge = true
	return m.badgeStr
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
// rebuilds the badge. The draw loop calls it on a heartbeat between frames, so an
// idle page (the block backend pushes no frames while static) decays the live rate
// to 0 instead of freezing the badge at its last value. It records no frame and
// touches no summary stat, so a heartbeat never inflates the counted-frame total.
func (m *fpsMeter) refresh(now time.Time) string {
	cutoff := now.Add(-m.window)
	drop := 0
	for drop < len(m.times) && m.times[drop].Before(cutoff) {
		drop++
	}
	m.times = m.times[drop:]
	// Sample the (now decayed) rate so the peak window ages out on idle too: once a
	// burst is older than fpsPeakHold the badge falls back to 0.
	m.samplePeak(now)
	return m.formatBadge(now)
}

// printSummary writes one compact fps summary line to w after the session ends. It
// reports warmup_ms (time to the first counted frame, so the "loads slowly" signal
// stays visible), then the two rate streams. The In stream (screencast ingest) gets
// the full steady-state stats over the ingest span: in_frames, in_mean, the in_min
// and in_max instantaneous fps (the slowest and fastest single frames), in_median,
// and in_worst_gap_ms, plus the worker decode budget in_decode_mean_ms and
// in_decode_max_ms (how close each frame sits to the ~16.6ms 60fps ceiling the decode
// wall imposes). Under the decoupled receiver/worker model the arrival times are true
// per-frame instants, so the In extremes are meaningful and mirror Out exactly
// (in_min is 1/maxGap, and so on). The Out stream (paint) gets out_frames, wall
// seconds, out_mean and out_median fps, out_min and out_max, out_worst_gap_ms
// (which exposes a post-load stall a mean hides), then the paint-phase split in ms
// per frame: out_encode_mean_ms and out_encode_max_ms (rendering the frame into the
// buffer, our terminal-byte encode) and out_write_mean_ms and out_write_max_ms
// (flushing that buffer to the tty). The split localises where each paint's time
// goes: our encode versus the terminal write. When no ingest reporter is wired the
// In fields read zero. A nil meter or a session whose gate never opened prints
// nothing, so an off run and an empty run are both silent.
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
	encodeMean, writeMean := 0.0, 0.0
	if m.paintFrames > 0 {
		encodeMean = m.encodeSum / float64(m.paintFrames)
		writeMean = m.writeSum / float64(m.paintFrames)
	}
	var in render.IngestStats
	if m.ingest != nil {
		in = m.ingest.IngestSummary()
	}
	fmt.Fprintf(w, "nyala fps: warmup_ms=%.0f in_frames=%d in_mean=%.1f in_min=%.1f in_max=%.1f in_median=%.1f in_worst_gap_ms=%.1f in_decode_mean_ms=%.1f in_decode_max_ms=%.1f out_frames=%d wall=%.2fs out_mean=%.1f out_median=%.1f out_min=%.1f out_max=%.1f out_worst_gap_ms=%.1f out_encode_mean_ms=%.1f out_encode_max_ms=%.1f out_write_mean_ms=%.1f out_write_max_ms=%.1f\n",
		warmupMS, in.Frames, in.MeanFPS, in.MinFPS, in.MaxFPS, in.MedianFPS, in.WorstGapMS, in.DecodeMeanMS, in.DecodeMaxMS,
		m.frames, wall, mean, medianFPS(m.gaps), minInst, maxInst, worstGapMS,
		encodeMean, m.encodeMax, writeMean, m.writeMax)
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
