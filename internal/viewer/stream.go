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
	// Scroll keys route to the engine's wheel transport; the engine owns the
	// offset.
	SendKey(code string)
	// SendMouse forwards one decoded mouse event. A wheel step scrolls at the
	// pointer cell through the engine's wheel transport; a press or release
	// dispatches a mouse event at the cell centre. The engine owns the
	// cell-to-CSS conversion and clamps the cell to its grid.
	SendMouse(ev MouseEvent)
	// Close tears down the engine and the browser process.
	Close() error
}

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
// transport is live (resolved shm, not demoted). It runs at startup after the
// backend settles, on every Tab cycle, and on runtime demotion.
func syncEngineKittyDecode(eng Engine, st *State) {
	d, ok := eng.(kittyDecodeSetter)
	if !ok {
		return
	}
	d.SetKittyDecode(st.Backend == BackendKitty && st.kittySHMLive())
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
// NO_COLOR and detection. mode is the --backend mode: a named mode picks the
// starting backend and Tab still cycles every supported backend; auto and
// block always pass, and a graphics start is rejected before navigation when
// detection did not confirm it. blocks is the --blocks mode selecting the block
// glyph scheme; Stream resolves it with resolveScheme against os.Environ(), so
// auto and sextant render sextant on a supported terminal and quadrant otherwise,
// quadrant forces quadrant, and half renders half-block.
// kittyTransport is the --kitty-transport intent (auto, direct, or shm);
// configureBackends resolves it against the probe result and the allow and
// deny lists.
func Stream(eng Engine, url string, tty *os.File, colourFlag, mode, blocks, kittyTransport string) (err error) {
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
	fd, st, restore, err := setupViewer(eng, tty, colourFlag, kittyTransport, scheme)
	if err != nil {
		return err
	}
	// restoreTerminal is the one terminal-restore path: the exit escapes
	// (mouse reporting off, auto-wrap and cursor back, and, once a frame
	// drew, the cursor moved below the frame), then the termios restore. The
	// deferred teardown and the SIGTERM watcher below share it through the
	// Once, so the restore runs exactly once however the session ends. It
	// reads firstDrawn through the session's atomic, so the watcher goroutine
	// never races the producer's write.
	var restoreOnce sync.Once
	var session *streamSession
	restoreTerminal := func() {
		restoreOnce.Do(func() {
			drawn := session != nil && session.firstDrawn.Load()
			_, _ = io.WriteString(tty, exitTerminal(st.Rows, drawn))
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
	// block start overrides a higher-fidelity detected nomination while Tab keeps
	// the full detected cycle. setupViewer handed the engine the detected backend;
	// re-notify it so the engine sizes its capture for the starting backend, not
	// the detected one.
	// A forced start can flip the Sixel bottom-row reservation under tmux,
	// so re-resize the engine grid when the row count moved from the
	// detected nomination setupViewer sized for.
	detectedRows := engineRows(st.Backend, st.mux, st.Rows)
	applyForcedBackend(st, mode)
	eng.SetBackend(st.Backend)
	if forcedRows := engineRows(st.Backend, st.mux, st.Rows); forcedRows != detectedRows {
		eng.Resize(st.Cols, forcedRows)
	}
	// The backend is final now, so arm the engine's kitty-shm decode when the
	// resolved transport is shm and the session starts on Kitty. Unlink any
	// unconsumed shm objects when the session ends, however it ends.
	syncEngineKittyDecode(eng, st)
	defer st.closeKittySHM()

	// Mouse reporting enables here, beside the full-screen escapes and after
	// the setupViewer handshake, so mouse traffic cannot race the startup
	// probe replies. restoreTerminal's exitTerminal write disables it
	// unconditionally on every teardown.
	if _, err := io.WriteString(tty, enterFullScreen+enableMouseReporting); err != nil {
		return err
	}

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
	session = s
	// A SIGTERM must restore the terminal before the process dies: a hard
	// kill would otherwise leave raw mode and mouse reporting enabled, and
	// the shell would read a stream of SGR mouse reports. The watcher shares
	// restoreTerminal's Once with the deferred teardown, so the restore runs
	// exactly once, and it exits via done on a normal quit. It starts after
	// session is set, so the drawn-state read never races this goroutine's
	// start.
	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGTERM)
	defer signal.Stop(sigterm)
	go func() {
		select {
		case <-sigterm:
			restoreTerminal()
			os.Exit(143) // conventional SIGTERM exit status: 128 + 15
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
	// The engine grid may reserve the bottom row (Sixel under tmux), so the
	// engine gets engineRows while State keeps the full terminal grid.
	eng.SetCellPx(state.W, state.H)
	eng.SetBackend(state.Backend)
	eng.Resize(cols, engineRows(state.Backend, state.mux, rows))

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
	tty       io.Writer
	fd        uintptr
	keySender *keySender
	ctl       *control
	diff      *block.GridDiff
	// firstDrawn is atomic because the producer goroutine writes it per frame
	// while Stream's SIGTERM watcher reads it for the teardown escapes.
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
	// assembledSHM marks the assembled frame as a Kitty shm transmit, so the
	// handoff to the writer arms the one-in-flight pacing gate. Producer only.
	assembledSHM bool
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
	// acknowledgement; kittyShmDeadline bounds the wait so a terminal that
	// stays silent degrades to a conservative self-pace instead of stalling.
	// Both are producer-goroutine only.
	kittyShmAwait    bool
	kittyShmDeadline time.Time
	// fps is the live fps counter, non-nil only when NYALA_FPS_TIMING is set. When
	// set, assemble stamps each frame and, where compatibility permits, folds a
	// rolling usable-fps badge into the frame buffer (rebuilt at most once per
	// second); when nil the draw path pays only a nil check, so production is
	// unchanged.
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
			encodeMS, err := s.assemble(buf, pending)
			if err != nil {
				return err
			}
			s.assembled = buf
			s.assembledEncodeMS = encodeMS
			s.assembledTimed = s.fps != nil && s.fps.gated
			havePending = false
		}

		// Enable the handoff case only when a buffer waits. A nil channel send never
		// fires, so the select ignores it while nothing is assembled.
		var readyC chan<- writeItem
		var item writeItem
		if s.assembled != nil {
			readyC = w.ready
			item = writeItem{buf: s.assembled, encodeMS: s.assembledEncodeMS, timed: s.assembledTimed, tmuxSixel: s.assembledTmuxSixel}
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
		case now := <-heartbeat:
			// Route the idle badge refresh through the writer so it never races the
			// in-flight frame write.
			if !s.st.fpsBadgeAllowed() {
				continue
			}
			if s.st.Backend == BackendKitty {
				_ = s.emitControl([]byte(s.fps.refreshAtHome(now)))
			} else {
				_ = s.emitControl([]byte(s.fps.refresh(now)))
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
				s.handleKittyResponse(ev.apc)
				continue
			}
			if ev.mouse != nil {
				s.handleMouse(*ev.mouse)
				continue
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
		return false
	}
	return true
}

// markKittySHMHandoff arms the shm pacing gate when the frame just handed to
// the writer was an shm transmit: the next shm frame waits for the terminal's
// acknowledgement or the conservative deadline. Producer goroutine only.
func (s *streamSession) markKittySHMHandoff() {
	if !s.assembledSHM {
		return
	}
	s.assembledSHM = false
	s.kittyShmAwait = true
	s.kittyShmDeadline = time.Now().Add(kittySHMAckTimeout)
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
// stream. Any response settles the in-flight shm transmit, so the pacing gate
// opens. A failure response (no ";OK") while the session is on the Kitty
// backend with the shm transport live means the terminal could not read the
// object, so the stint demotes to direct. A failure that lands after Tab
// cycled away is ignored: resetKitty has already unlinked unconsumed objects,
// so a busy terminal that reads the switch-window escape late reports a
// failure Nyala itself caused, and demoting on it would falsely pin later
// Kitty stints on the parser-bound direct path.
func (s *streamSession) handleKittyResponse(body string) {
	s.kittyShmAwait = false
	if strings.Contains(body, ";OK") {
		return
	}
	if s.st.Backend != BackendKitty || !s.st.kittySHMLive() {
		return
	}
	s.st.demoteKittySHM(fmt.Errorf("terminal rejected shm transmit: %s", body))
	s.applyKittySHMDemotion()
}

// applyKittySHMDemotion runs the session side of a pending shm demotion: it
// logs the fallback once and turns off the engine's kitty-shm decode, so the
// following frames arrive as direct PNG payloads again. It is a no-op when no
// demotion is pending, so the assemble path calls it unconditionally.
func (s *streamSession) applyKittySHMDemotion() {
	err := s.st.takeKittySHMDemotion()
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "nyala: kitty shm transport disabled, falling back to direct: %v\n", err)
	syncEngineKittyDecode(s.eng, s.st)
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
// writer to present with a single io.Write: the optional DEC 2026 open, the cursor
// prefix, the rendered frame with its crisp DOM-text overlay, the fps badge, and
// the optional DEC 2026 close. Foot and tmux Sixel omit only the
// synchronised-update markers; every other backend and terminal keeps them.
// Assembling the whole unit
// into one owned buffer keeps the atomic present where DEC 2026 is active while
// the producer encodes the next frame in parallel with the current frame's write.
// Byte order remains identical for every frame, independent of which goroutine
// writes it.
//
// The first frame emits the full clearScreen (via framePrefix) to wipe the loading
// cue; steady frames emit cursorHome only. It returns the encode time, the
// RenderStreamFrame span alone (matching the out_encode split), when the fps meter
// is armed, or zero when off (no clock read). buf is one of the two ping-pong
// buffers; the producer owns it until the writer returns it on free.
func (s *streamSession) assemble(buf *bytes.Buffer, frame render.Frame) (encodeMS float64, err error) {
	buf.Reset()
	prefix := framePrefix(s.firstDrawn.Load(), s.diff)
	syncUpdates := s.st.syncUpdatesEnabled()
	if syncUpdates {
		buf.WriteString(beginSyncUpdate)
	}
	buf.WriteString(prefix)
	s.firstDrawn.Store(true)

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
	kittyBelowText := timed && s.st.Backend == BackendKitty && s.st.fpsBadgeAllowed()
	if rerr := s.st.renderStreamFrame(frame, buf, kittyBelowText); rerr != nil {
		return 0, rerr
	}
	if nonScrollingSixel {
		buf.WriteString(endSixelDisplayMode)
	}
	if timed {
		encodeMS = msSince(encodeStart)
	}
	// Tag an shm transmit for the pacing gate and settle any shm failure the
	// render just recorded: one log line, then direct PNG for the session.
	s.assembledSHM = s.st.takeKittySHMWrote()
	s.assembledTmuxSixel = nonScrollingSixel && s.st.mux == muxTmux
	s.applyKittySHMDemotion()

	// Fold the fps badge into the same buffer where compatibility permits it, gated
	// behind NYALA_FPS_TIMING. Where DEC 2026 is active it sits inside the bracket: painted
	// after the close it flickered out during a scroll, when back-to-back frames each
	// present the grid alone before the un-synced badge lands. The badge is a
	// cursor-positioned overlay, so it never enters the block grid the differ tracks
	// and leaves the golden-gated compose bytes untouched. Kitty uses absolute home
	// moves because image placement already preserves that tracked position; Block
	// and Sixel preserve their current position with the saved-cursor slot.
	// recordPaint is deferred until the writer returns the buffer, because the write
	// time is known only then.
	if s.fps != nil {
		now := time.Now()
		// The engine stamps frames received after main-frame first contentful paint.
		// That lifecycle marker opens one backend-neutral warm-up gate, including
		// direct Kitty frames and text-free pages.
		s.fps.tick(now, frame.AfterFCP)
		if s.st.fpsBadgeAllowed() {
			if s.st.Backend == BackendKitty {
				buf.WriteString(s.fps.badgeAtHome(now))
			} else {
				buf.WriteString(s.fps.badge(now))
			}
		}
	}
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
	s.writer.send(writeItem{buf: s.assembled, encodeMS: s.assembledEncodeMS, timed: s.assembledTimed, tmuxSixel: s.assembledTmuxSixel})
	s.assembled = nil
	s.markKittySHMHandoff()
	s.markTmuxSixelHandoff()
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
// recomputed capture size, so frames arrive at the new grid, not the old one. A
// terminal font-size change (zoom) moves the cell pixel pair with the grid, so
// it re-derives the pair from the winsize and hands a changed pair to the
// engine with the restart; resizeCellPx keeps the old pair when the override is
// forced or the winsize reports nothing. It clears the screen only once a frame
// has drawn, so the loading cue is not wiped before the first frame replaces it.
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
	// The engine restart runs on the control goroutine so a slow restartStream
	// never freezes the consumer loop; the loop keeps drawing and handling quit.
	// The engine grid takes engineRows, so the Sixel bottom-row reservation
	// under tmux survives a resize while State keeps the full grid.
	s.ctl.submit(resizeEngineOp(px, changed, nc, engineRows(s.st.Backend, s.st.mux, nr)))
	if !s.firstDrawn.Load() {
		return nil
	}
	// The resize clears the screen, so the differ full-repaints the next frame
	// even when the grid dimensions are unchanged. The clear routes through the
	// writer so it never races an in-flight frame write.
	s.diff.Reset()
	var ctl bytes.Buffer
	if s.st.Backend == BackendKitty {
		// A Kitty placement survives the ANSI clear, so a resize that shrank the
		// image (a zoom-out moves the cell pair) would leave the old image's
		// pixels standing outside the new one. Reuse the backend-switch delete-all
		// seam: free both double-buffer ids so the next frame rebuilds from id 1,
		// and drop the shm pacing gate so a stale in-flight wait cannot gate the
		// first post-resize frame.
		_ = s.st.resetKitty(&ctl)
		s.kittyShmAwait = false
	}
	ctl.WriteString(clearScreen)
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

// switchBackendOp returns the control-goroutine closure for one Tab backend
// switch. When the switch flips the Sixel bottom-row reservation under tmux
// (Sixel and Block use different engine row counts there), the grid
// resize rides in the same closure: the one-slot drop-on-full control channel
// could otherwise drop one half of the pair. The resize runs before
// SetBackend, so the backend restart, which always runs, lands last at the
// final grid.
func switchBackendOp(backend Backend, cols, prevRows, newRows int) func(Engine) {
	return func(eng Engine) {
		if newRows != prevRows {
			eng.Resize(cols, newRows)
		}
		eng.SetBackend(backend)
	}
}

// handleMouse forwards one decoded mouse event to the engine off the input
// loop through the keySender worker, the same stall policy as scroll keys, so
// a wedged browser never freezes quit or draw.
func (s *streamSession) handleMouse(ev MouseEvent) {
	if s.keySender != nil {
		s.keySender.sendMouse(ev)
		return
	}
	s.eng.SendMouse(ev)
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
		previous := s.st.Backend
		wasKitty := previous == BackendKitty
		s.st.CycleBackend()
		// One control buffer carries the whole switch clear, routed through the
		// writer so it stays ordered against the frames and never races an
		// in-flight write. Best-effort: a write failure surfaces on the next
		// frame via the writer's error channel.
		var ctl bytes.Buffer
		if wasKitty && s.st.Backend != BackendKitty {
			// Free both double-buffer images when leaving kitty, so the terminal keeps
			// no stale image and a later return rebuilds from id 1.
			_ = s.st.resetKitty(&ctl)
			// Drop the shm pacing gate with the stint, so a stale in-flight
			// wait cannot gate the next stint's first frame against the old
			// stream's deadline.
			s.kittyShmAwait = false
		}
		if s.st.Backend == BackendKitty && !wasKitty {
			// Returning to Kitty re-arms a demoted shm transport: the startup
			// probe proved it, so a switch-window false failure costs at most
			// one stint, never the session.
			s.st.rearmKittySHM()
		}
		if s.st.Backend != previous && s.firstDrawn.Load() {
			// Every backend change erases the screen before the new backend's
			// first frame, one shared rule for all transitions. Leaving Block,
			// the old glyphs and cell backgrounds would otherwise sit under the
			// next graphics frame (and under Kitty's FPS page image, which
			// places below text). Leaving Sixel, the old raster persists over
			// cells the terminal believes unchanged. Entering Sixel under
			// tmux, the reserved bottom row keeps the previous backend's
			// cells because the rows-1 frame never overwrites it. Reset the SGR
			// first: the Block differ leaves the last cell's background active,
			// and terminals erase with the current background (background
			// colour erase). Invalidate the differ against the blank terminal,
			// so the next Block frame full-repaints instead of skipping cells
			// matching the previous Block stint. Gated on firstDrawn so a
			// pre-frame Tab never wipes the loading cue, matching resize.
			if s.diff != nil {
				s.diff.Reset()
			}
			ctl.WriteString(sgrReset + clearScreen)
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
		backend := s.st.Backend
		s.ctl.submit(switchBackendOp(backend, s.st.Cols,
			engineRows(previous, s.st.mux, s.st.Rows),
			engineRows(backend, s.st.mux, s.st.Rows)))
	}
	return false
}

// keySender serialises forwarded input (scroll keys and mouse events) away
// from the input loop, so a browser call that stalls never freezes quit or
// draw. Unlike the frame path (latest-wins-drop), input is cumulative: a rapid
// burst of scroll keys or wheel events must all reach the engine, or the page
// barely moves. So it accumulates every submitted event and the worker forwards
// them all in order, letting the engine's wheel transport sum the burst
// (dispatchWheel adds each delta into its held offset) rather than dropping to
// the last event. send and sendMouse never block the input loop. It owns its
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

// sendMouse queues one decoded mouse event for the worker on the same queue
// as scroll keys, so a wheel burst and a click reach the engine in arrival
// order.
func (s *keySender) sendMouse(ev MouseEvent) {
	s.enqueue(func(e Engine) { e.SendMouse(ev) })
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

// fpsBadgeInterval bounds how often the badge string is rebuilt, so the fps text
// repaints at most once per second: the meter counts every draw, but the badge
// string is regenerated no more than once per interval and the cached string is
// reused between rebuilds.
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

	badgeStr   string    // cached escape-wrapped badge, rebuilt at most once per interval
	badgeAt    time.Time // instant badgeStr was last rebuilt
	haveBadge  bool      // set once badgeStr holds a real string
	badgeHome  bool      // badgeStr uses absolute home positioning for Kitty
	badgeWidth int       // widest badge body emitted, so shorter values clear its full span

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

// badge returns the escape-wrapped badge string to write this draw. It rebuilds
// the string (the only per-call allocation) at most once per fpsBadgeInterval and
// returns the cached string between rebuilds, so the fps text refreshes about once
// per second while the badge stays painted over every frame.
func (m *fpsMeter) badge(now time.Time) string {
	return m.positionedBadge(now, false)
}

// badgeAtHome returns the cached Kitty badge using absolute cursor movement at
// both ends. Kitty image placement leaves the cursor at the tracked home, so the
// trailing CUP restores that known position without consuming a saved-cursor
// slot.
func (m *fpsMeter) badgeAtHome(now time.Time) string {
	return m.positionedBadge(now, true)
}

func (m *fpsMeter) positionedBadge(now time.Time, atHome bool) string {
	if !m.haveBadge || m.badgeHome != atHome || now.Sub(m.badgeAt) >= fpsBadgeInterval {
		return m.formatPositionedBadge(now, atHome)
	}
	return m.badgeStr
}

// formatPositionedBadge rebuilds and caches the badge string at now. With an
// ingest reporter it shows "In: X.X / Out: Y.Y fps (N)"; both rates are
// windowed, so both decay live when idle. Without one it shows
// "Out: Y.Y fps (N)" alone. N is the cumulative received-frame total, which
// never decays: a frozen N with "--" rates is a stalled or idle stream, while a
// moving N with "--" rates is delivery the peak window has already aged out.
// It is the sole per-call allocation and the single place the badge escape is
// built. The SGR opens with a reset then reverse ("\x1b[0;7m"): the reset
// clears the grid's ambient colour so reverse inverts the terminal default, not
// whatever page colour the differ left active, which otherwise tinted the badge
// from a scrolled-away cell.
func (m *fpsMeter) formatPositionedBadge(now time.Time, atHome bool) string {
	in, out := m.peakRates()
	prefix, suffix := "\x1b7\x1b[1;1H", "\x1b[0m\x1b8"
	if atHome {
		prefix, suffix = "\x1b[1;1H", "\x1b[0m\x1b[1;1H"
	}
	total := strconv.Itoa(m.received)
	if m.ingest != nil {
		inField, outField := fpsField(in), fpsField(out)
		width := len(" In:  / Out:  fps () ") + len(inField) + len(outField) + len(total)
		if width > m.badgeWidth {
			m.badgeWidth = width
		}
		m.badgeStr = fmt.Sprintf("%s\x1b[0;7m In: %s / Out: %s fps (%s) %*s%s", prefix, inField, outField, total, m.badgeWidth-width, "", suffix)
	} else {
		outField := fpsField(out)
		width := len(" Out:  fps () ") + len(outField) + len(total)
		if width > m.badgeWidth {
			m.badgeWidth = width
		}
		m.badgeStr = fmt.Sprintf("%s\x1b[0;7m Out: %s fps (%s) %*s%s", prefix, outField, total, m.badgeWidth-width, "", suffix)
	}
	m.badgeAt = now
	m.haveBadge = true
	m.badgeHome = atHome
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
	return m.refreshPositioned(now, false)
}

// refreshAtHome refreshes the idle Kitty badge and restores the tracked home
// with absolute cursor movement.
func (m *fpsMeter) refreshAtHome(now time.Time) string {
	return m.refreshPositioned(now, true)
}

func (m *fpsMeter) refreshPositioned(now time.Time, atHome bool) string {
	cutoff := now.Add(-m.window)
	drop := 0
	for drop < len(m.times) && m.times[drop].Before(cutoff) {
		drop++
	}
	m.times = m.times[drop:]
	// Sample the (now decayed) rate so the peak window ages out on idle too: once a
	// burst is older than fpsPeakHold the badge falls back to 0.
	m.samplePeak(now)
	return m.formatPositionedBadge(now, atHome)
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
// goes: our encode versus the terminal write. The trailing ack_rtt_mean_ms and
// ack_rtt_max_ms report the engine's screencast acknowledgement command round
// trip, so a slow ack loop that starves Chromium's frame credit shows up here.
// When no ingest reporter is wired the In and ack fields read zero. A session whose gate never opened reports
// warmup_ms=unavailable and no invented duration. A nil meter stays silent.
func (m *fpsMeter) printSummary(w io.Writer) {
	if m == nil {
		return
	}
	if m.frames == 0 {
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
	var in render.IngestStats
	if m.ingest != nil {
		in = m.ingest.IngestSummary()
	}
	fmt.Fprintf(w, "nyala fps: warmup_ms=%.0f in_frames=%d in_mean=%.1f in_min=%.1f in_max=%.1f in_median=%.1f in_worst_gap_ms=%.1f in_decode_mean_ms=%.1f in_decode_max_ms=%.1f out_frames=%d wall=%.2fs out_mean=%.1f out_median=%.1f out_min=%.1f out_max=%.1f out_worst_gap_ms=%.1f out_encode_mean_ms=%.1f out_encode_max_ms=%.1f out_write_mean_ms=%.1f out_write_max_ms=%.1f ack_rtt_mean_ms=%.1f ack_rtt_max_ms=%.1f\n",
		warmupMS, in.Frames, in.MeanFPS, in.MinFPS, in.MaxFPS, in.MedianFPS, in.WorstGapMS, in.DecodeMeanMS, in.DecodeMaxMS,
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
