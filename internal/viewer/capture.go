package viewer

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/wimpysworld/nyala/internal/capture"
	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// ErrPartialBundle is the sentinel a capture run returns when it writes a
// partial bundle instead of a full one: a forced-backend rejection, a navigate
// failure, a first-draw or per-frame idle timeout, a mid-loop draw or write
// error, or the frames channel closing before n frames drew. app.Run maps it to
// a non-zero exit so cmd/nyala exits
// via its existing os.Exit(1). A full capture returns nil. Callers test it with
// errors.Is.
var ErrPartialBundle = errors.New("viewer: capture wrote a partial bundle")

// captureIdle is the first-draw and per-frame idle deadline. The loop arms a
// timer at this duration when the painted latch closes, and resets it to this
// duration on every drawn frame. captureLoop takes the duration as a parameter
// so timeout tests inject a tiny value and run instantly.
const captureIdle = 5 * time.Second

// capturePaintDeadline bounds the wait for the first contentful paint before the
// driver counts usable frames. A real page paints slower than the 5s per-frame
// idle, so this is wider. If the paint signal never fires within it, the driver
// writes a partial bundle reasoned "first contentful paint timeout". captureLoop
// takes it as a parameter so tests inject a tiny value and run instantly.
const capturePaintDeadline = 15 * time.Second

// captureTextGrace bounds the wait for the first DOM snapshot to publish after
// paint, before the driver counts the first usable frame. The snapshot goroutine
// publishes its runs only after a frame is consumed, so the first delivered frame
// carries empty runs until the first snapshot lands; this grace folds the text
// layer on before the count. A page may legitimately have no text, so the wait is
// bounded: when the grace elapses with no published snapshot the driver proceeds
// with a text-less frame rather than hang. It sits well under the 5s first-draw
// deadline so a never-publishing page still terminates. captureLoop takes it as a
// parameter so tests inject a tiny value and run instantly.
const captureTextGrace = 750 * time.Millisecond

// bundleWriter is the capture-bundle sink the driver depends on. It is the seam
// the real capture.Bundle satisfies and a fake stands in for, so the bounded
// loop and the forced-backend rejection path stay testable without writing a
// bundle to disk. The driver opens the bundle, hands the engine its
// cdp-events.ndjson sink before Navigate, writes each drawn frame, writes the
// cdp-session.json bytes at finalise (after fillSession has run during
// navigation), finalises full on success or partial on a rejection, timeout, or
// early channel close, and reads the raw-file handle and the bundle path. The
// driver mirrors the engine mu discipline: it never holds a lock across a
// channel send, and these methods do no channel work.
type bundleWriter interface {
	// RawFile returns the open terminal-stream.raw handle the per-frame tee
	// mirrors the drawn bytes into. It is an io.Writer so a fake can hand back a
	// bytes.Buffer.
	RawFile() io.Writer
	// CDPEventsFile returns the open cdp-events.ndjson handle the engine recorder
	// appends event lines to. The driver hands it to the engine before Navigate.
	CDPEventsFile() io.Writer
	// Path returns the bundle directory path, printed on every exit.
	Path() string
	// WriteFrame records one drawn frame's artefacts. The driver fills the
	// FrameInput inline per drawn frame and calls it before resetting the idle
	// timer.
	WriteFrame(in capture.FrameInput) error
	// WriteSession writes the cdp-session.json bytes the engine assembled during
	// navigation. The driver calls it once at finalise, after fillSession has run,
	// before each finalise. Empty bytes leave the placeholder untouched.
	WriteSession(data []byte) error
	// FinaliseFull writes the full-bundle root files and closes the bundle after
	// n frames drew.
	FinaliseFull() error
	// FinalisePartial writes the partial-bundle root files plus the failure
	// reason and closes the bundle after a rejection, timeout, or early channel
	// close.
	FinalisePartial(reason string) error
}

// Ensure capture.Bundle satisfies bundleWriter.
var _ bundleWriter = (*capture.Bundle)(nil)

// Capture records n drawn frames of url into one bundle, then exits. It holds
// the not-a-terminal guard and the shared viewer setup (setupViewer), opens the
// bundle, validates the forced backend (a rejection writes a partial bundle
// before navigation), enables engine capture and sets the recorder before
// Navigate, then runs the bounded loop. Unlike Stream it has no key reader and
// no Tab: the loop selects on the frames channel and one idle timer only. It
// draws each frame through a tee that mirrors the drawn bytes into the bundle's
// terminal-stream.raw, and it does not enter the full-screen mode or write the
// loading cue, so the raw capture holds only the per-frame clear and render
// bytes. It returns nil on a full capture and ErrPartialBundle when it writes a
// partial bundle. colourFlag is the --color intent; mode is the --backend mode
// the forced-backend validator checks.
func Capture(eng Engine, url string, tty *os.File, colourFlag, mode string, n int) (err error) {
	_, st, restore, err := setupViewer(eng, tty, colourFlag)
	if err != nil {
		return err
	}
	defer restore()

	defer func() { _ = eng.Close() }()

	// Pin the active backend to a named --backend mode before building the bundle
	// meta, so the manifest records the forced backend. Validation gates forcing:
	// an unsupported forced backend leaves the detected cycle untouched, and
	// runCapture's own ValidateForcedBackend then writes the partial bundle.
	// setupViewer handed the engine the detected backend; re-notify it after a
	// successful force so the engine sizes its capture for the forced backend.
	if ValidateForcedBackend(mode, st.Enabled) == nil {
		applyForcedBackend(st, mode)
		eng.SetBackend(backendName(st.Backend))
	}

	b, err := capture.NewBundle(captureMeta(url, st, mode, n))
	if err != nil {
		return fmt.Errorf("viewer: open capture bundle: %w", err)
	}

	return runCapture(eng, url, st, tty, bundleWriter(b), mode, n, capturePaintDeadline, captureTextGrace, captureIdle)
}

// runCapture runs the capture flow below the not-a-terminal guard: it validates
// the forced backend, enables engine capture before Navigate, then runs the
// bounded loop. It is unexported and takes the Engine, the State, the tty
// io.Writer, the bundle writer, the backend mode, the frame count, and the idle
// duration, so a unit test drives it directly with a fake Engine, a fake bundle,
// a bytes.Buffer tty, and a tiny duration, bypassing the terminal check.
//
// A forced graphics backend detection did not confirm writes a partial bundle
// and returns ErrPartialBundle before any browser launch, so validation runs
// after detection and before Navigate; placing it later would launch the browser
// before failing. ModeAuto and ModeBlock always pass. paintWait bounds the wait
// for first contentful paint; textGrace bounds the wait for the first published
// snapshot after paint; idle is the per-frame deadline.
func runCapture(eng Engine, url string, st *State, tty io.Writer, bundle bundleWriter, mode string, n int, paintWait, textGrace, idle time.Duration) error {
	if verr := ValidateForcedBackend(mode, st.Enabled); verr != nil {
		_ = bundle.FinalisePartial(verr.Error())
		fmt.Fprintf(tty, "capture bundle: %s\r\n", bundle.Path())
		return ErrPartialBundle
	}

	// Enable engine capture and set the recorder over the bundle's
	// cdp-events.ndjson sink before Navigate, before any goroutine starts,
	// matching the proxy set-once discipline. The session exposes the
	// cdp-session.json bytes the engine assembles during navigation and the painted
	// latch the loop waits on before counting usable frames.
	session := eng.EnableCapture(bundle.CDPEventsFile())

	return captureLoop(eng, url, st, tty, bundle, session, n, paintWait, textGrace, idle)
}

// captureMeta builds the bundle run metadata from the resolved state and the
// invocation. The backend is the resolved active backend; the terminal
// capability mirrors it. Command omits secrets: it carries the URL and backend
// mode only, never proxy env values.
func captureMeta(url string, st *State, mode string, n int) capture.Meta {
	backend := capture.Backend(backendName(st.Backend))
	return capture.Meta{
		Command:            fmt.Sprintf("nyala --backend=%s %s", mode, url),
		Backend:            backend,
		TerminalCapability: backendName(st.Backend),
		FramesRequested:    n,
		NavigationURL:      url,
	}
}

// captureLoop runs the bounded capture loop below the not-a-terminal guard. It
// is unexported and takes the Engine, the State, the tty io.Writer, the bundle
// writer, the capture session, the frame count, the paint deadline, and the idle
// duration, so a unit test drives it directly with a fake Engine, a bytes.Buffer
// tty, and tiny durations, bypassing the terminal check. It draws each frame with
// frame.Img != nil through io.MultiWriter(tty, bundle.RawFile()), preceded by its
// own clearScreen, counts drawn frames, and records each frame's artefacts and
// viewer-phase timing.
//
// After Navigate returns nil it waits for the painted latch before counting any
// usable frame, draining and discarding pre-paint frames so the latest-wins slot
// does not stall. An early frame, captured before first contentful paint, is
// blank; waiting for paint keeps a blank frame out of the n. The wait is bounded
// by paintWait; if the paint signal never fires the loop writes a partial bundle
// reasoned "first contentful paint timeout".
//
// After paint it waits for the first DOM snapshot to publish so the first usable
// frame carries its text runs, draining frames while it waits so the snapshot
// goroutine wakes and publishes. This wait is bounded by textGrace: a page may
// have no text, so when the grace elapses the loop proceeds with a text-less
// frame rather than hang.
//
// Once painted, one time.Timer carries both per-frame deadlines: it arms at idle
// on entry (first-draw deadline) and resets to idle on every drawn frame (per-frame
// idle deadline). The loop returns on one of these outcomes:
//
//  1. n frames drawn: finalise a full bundle, return nil.
//  2. the painted latch never closes within paintWait: finalise a partial bundle,
//     return ErrPartialBundle.
//  3. the timer fires before the first post-paint frame: finalise a partial
//     bundle, return ErrPartialBundle.
//  4. the timer fires after the first frame: finalise a partial bundle, return
//     ErrPartialBundle.
//  5. the frames channel closes before n: finalise a partial bundle, return
//     ErrPartialBundle.
//  6. a mid-loop draw or write error: finalise a partial bundle with the wrapped
//     error as the reason, return ErrPartialBundle.
//
// It prints the bundle path on every exit. The single select orders the frames
// channel against the timer deterministically, so the timeout-after-frame and
// channel-close cases stay distinct.
func captureLoop(eng Engine, url string, st *State, tty io.Writer, bundle bundleWriter, session CaptureSession, n int, paintWait, textGrace, idle time.Duration) (err error) {
	defer func() { fmt.Fprintf(tty, "capture bundle: %s\r\n", bundle.Path()) }()

	// finaliseFull and finalisePartial write the cdp-session.json bytes the engine
	// assembled during navigation, then finalise. Navigate dispatches navigation
	// asynchronously and returns before fillSession populates the session, so the
	// session write must wait until finalise, after frames have flowed or the run
	// timed out. Funnelling every exit through these two helpers writes the session
	// exactly once before each finalise, on every full and partial path.
	finaliseFull := func() error {
		writeSession(bundle, session)
		return bundle.FinaliseFull()
	}
	finalisePartial := func(reason string) error {
		writeSession(bundle, session)
		return bundle.FinalisePartial(reason)
	}

	navigated := make(chan error, 1)
	go func() {
		navigated <- eng.Navigate(url)
	}()
	if nerr := <-navigated; nerr != nil {
		_ = finalisePartial(fmt.Sprintf("navigate: %v", nerr))
		return ErrPartialBundle
	}

	tee := io.MultiWriter(tty, bundle.RawFile())
	// Cache the frame channel once: the engine keeps it stable across Resize and
	// SetBackend and replaces it only by Navigate, which is single-shot (invariant
	// V3). The capture driver never resizes or cycles, so the handle is stable.
	frames := eng.Frames()

	// Wait for first contentful paint before counting usable frames, draining
	// pre-paint frames so the slot does not stall. A paint timeout or a closed
	// channel writes a partial bundle.
	if reason, ok := waitForPaint(session, frames, paintWait); !ok {
		_ = finalisePartial(reason)
		return ErrPartialBundle
	}

	// Fold the text layer before the first usable frame. The snapshot goroutine
	// publishes only after a frame is consumed, so wait for the first snapshot,
	// draining frames so the snapshot wakes, bounded by textGrace. A closed channel
	// writes a partial bundle; the grace elapsing proceeds text-less.
	if reason, ok := waitForText(session, frames, textGrace); !ok {
		_ = finalisePartial(reason)
		return ErrPartialBundle
	}

	// Arm the per-frame timer now that paint has fired, the first-draw deadline.
	timer := time.NewTimer(idle)
	defer timer.Stop()

	var nilImageOnce sync.Once
	drawn := 0
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				_ = finalisePartial(fmt.Sprintf("frames channel closed after %d of %d", drawn, n))
				return ErrPartialBundle
			}
			if frame.Img == nil {
				// A nil-image frame is skipped with a one-shot diagnostic so the dropped
				// frame is observable; it is not counted toward the n.
				nilImageOnce.Do(func() {
					fmt.Fprintln(os.Stderr, "nyala: capture skipped a frame with no image")
				})
				continue
			}
			in, derr := drawCaptureFrame(st, frame, tee)
			if derr != nil {
				_ = finalisePartial(fmt.Sprintf("draw frame %d of %d: %v", drawn+1, n, derr))
				return ErrPartialBundle
			}
			if werr := bundle.WriteFrame(in); werr != nil {
				_ = finalisePartial(fmt.Sprintf("write frame %d of %d: %v", drawn+1, n, werr))
				return ErrPartialBundle
			}
			drawn++
			if drawn >= n {
				if ferr := finaliseFull(); ferr != nil {
					return ferr
				}
				return nil
			}
			// Reset the per-frame idle deadline. Stop and drain before reset so a
			// fire that raced the draw does not leak onto the next select.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idle)
		case <-timer.C:
			reason := "first-draw timeout"
			if drawn > 0 {
				reason = fmt.Sprintf("per-frame idle timeout after %d of %d", drawn, n)
			}
			_ = finalisePartial(reason)
			return ErrPartialBundle
		}
	}
}

// waitForPaint blocks until the session's painted latch closes, then returns
// ok=true. While waiting it drains and discards frames from the frames channel so
// the engine's latest-wins slot does not stall on an unconsumed pre-paint frame;
// these blank pre-paint frames are never counted or written. It bounds the wait
// with a timer at paintWait. It returns ok=false with a reason when the paint
// deadline fires ("first contentful paint timeout") or the frames channel closes
// before paint ("frames channel closed before first contentful paint"). A nil
// painted latch reads as never closed, so the deadline bounds the wait.
func waitForPaint(session CaptureSession, frames <-chan render.Frame, paintWait time.Duration) (reason string, ok bool) {
	if session == nil {
		// No session means no paint gate (a unit test that bypasses it); proceed
		// straight to the bounded loop. The production path always wires a session.
		return "", true
	}
	painted := session.Painted()
	timer := time.NewTimer(paintWait)
	defer timer.Stop()
	for {
		// Check paint first with priority. A plain three-way select picks at random
		// when paint is already signalled and a frame is also ready, which could
		// discard a post-paint frame the bounded loop should count.
		select {
		case <-painted:
			return "", true
		default:
		}
		select {
		case <-painted:
			return "", true
		case _, open := <-frames:
			if !open {
				return "frames channel closed before first contentful paint", false
			}
			// Discard the pre-paint frame: do not draw, count, or write it.
		case <-timer.C:
			return "first contentful paint timeout", false
		}
	}
}

// waitForText blocks until the session's text latch closes, then returns ok=true,
// so the first usable frame carries the published text runs. While waiting it
// drains and discards frames from the frames channel: each drained frame pokes
// the engine's snapshot trigger, which drives the snapshot that closes the latch,
// and the drain keeps the latest-wins slot from stalling. It bounds the wait with
// a timer at textGrace, because a page may legitimately have no text; when the
// grace elapses it returns ok=true so the loop proceeds with a text-less frame
// rather than hang. It returns ok=false with a reason only when the frames
// channel closes before the latch ("frames channel closed before first DOM
// snapshot"). A nil session or a nil latch reads as never closed, so the grace
// bounds the wait.
func waitForText(session CaptureSession, frames <-chan render.Frame, textGrace time.Duration) (reason string, ok bool) {
	if session == nil {
		// No session means no text gate (a unit test that bypasses it); proceed
		// straight to the bounded loop. The production path always wires a session.
		return "", true
	}
	textReady := session.TextReady()
	timer := time.NewTimer(textGrace)
	defer timer.Stop()
	for {
		// Check the latch first with priority, mirroring waitForPaint: a plain select
		// picks at random when the latch is already closed and a frame is also ready,
		// which could discard a frame the bounded loop should count.
		select {
		case <-textReady:
			return "", true
		default:
		}
		select {
		case <-textReady:
			return "", true
		case _, open := <-frames:
			if !open {
				return "frames channel closed before first DOM snapshot", false
			}
			// Discard the frame: do not draw, count, or write it. The drain pokes the
			// snapshot trigger so the snapshot publishes and the latch closes.
		case <-timer.C:
			// The grace elapsed with no published snapshot. A text-less page is valid,
			// so proceed rather than hang; the first usable frame carries no runs.
			return "", true
		}
	}
}

// writeSession writes the engine-assembled cdp-session.json bytes into the
// bundle. A nil session (no capture wiring, as in a fake-engine test) or empty
// bytes leaves the placeholder untouched. A write error is dropped: the session
// file is a diagnostic, and a failed write must not abort the capture.
func writeSession(bundle bundleWriter, session CaptureSession) {
	if session == nil {
		return
	}
	_ = bundle.WriteSession(session.SessionJSON())
}

// drawCaptureFrame draws one frame through the tee and returns the FrameInput the
// bundle serialises for it. It sizes the State to the frame image, sets the
// capture grid sink for a block draw so the composed letterbox grid travels to
// the bundle, then splits the draw into two timed phases: it renders the frame
// (with the crisp DOM-text overlay folded on) into an in-memory buffer, which is
// the escape-byte production (the graphics encode or the block compose plus SGR),
// then writes clearScreen and the buffered bytes to the tee, which is the
// terminal write. Buffering first separates encode from write so neither span
// overlaps the other. The tee is io.MultiWriter(tty, rawFile), so the clear and
// render bytes mirror into the bundle raw file in the same order Stream emits
// them, and a CountingWriter over it counts the terminal bytes for this frame.
func drawCaptureFrame(st *State, frame render.Frame, tee io.Writer) (capture.FrameInput, error) {
	b := frame.Img.Bounds()
	st.ImgW, st.ImgH = b.Dx(), b.Dy()

	var grid *block.Grid
	if st.Backend == BackendBlock {
		grid = &block.Grid{}
		st.SetCaptureGrid(grid)
		defer st.SetCaptureGrid(nil)
	}

	// Encode phase: render into a buffer so the escape-byte production is timed
	// apart from the terminal write. The buffer length is the renderer's own
	// output, excluding the clearScreen prefix, so the sixel payload size stays
	// accurate.
	var rendered bytes.Buffer
	encodeStart := time.Now()
	if err := st.RenderStreamFrame(frame, &rendered); err != nil {
		return capture.FrameInput{}, err
	}
	encodeMS := msSince(encodeStart)
	renderBytes := int64(rendered.Len())

	// Write phase: clearScreen then the buffered render bytes go to the tee. Only
	// the io.Write is timed here, so terminal_write_ms no longer includes the
	// encode.
	counter := metrics.NewCountingWriter(tee)
	writeStart := time.Now()
	if _, err := io.WriteString(counter, clearScreen); err != nil {
		return capture.FrameInput{}, err
	}
	if _, err := counter.Write(rendered.Bytes()); err != nil {
		return capture.FrameInput{}, err
	}
	writeMS := msSince(writeStart)

	return capture.FrameInput{
		Frame: frame,
		Meta:  frame.Meta,
		Grid:  grid,
		Perf:  capturePerf(st, frame, grid, counter.N, renderBytes, encodeMS, writeMS),
	}, nil
}

// capturePerf fills the viewer-phase timing and counts for one drawn frame. It
// reads the resolved grid size and cell px from State, the capture and draw
// extent from the frame image and grid, the overlay run and cell counts from the
// frame text, the terminal bytes from the driver's CountingWriter, and the encode
// and terminal write times the driver measured. The encode time is the
// escape-byte production: for the graphics backends it is the sixel or kitty
// encode (recorded as BackendEncodeMS); for the block backend it is the grid
// compose plus SGR build (recorded as BlockComposeMS, leaving BackendEncodeMS
// zero, because block has no image encode). The fit-downscale split stays zero
// here: the graphics fit runs inside the same RenderStreamFrame buffer render and
// is not instrumented separately in this pass.
func capturePerf(st *State, frame render.Frame, grid *block.Grid, terminalBytes, renderBytes int64, encodeMS, writeMS float64) capture.ViewerPerf {
	p := capture.ViewerPerf{
		CellPxW:         st.W,
		CellPxH:         st.H,
		GlyphRuns:       len(frame.Text),
		OverlayCells:    overlayCells(frame.Text),
		TerminalBytes:   terminalBytes,
		TerminalWriteMS: writeMS,
	}
	// Route the escape-byte production time to the field that owns it: the block
	// path has no image encode, so its cost lands in BlockComposeMS; the graphics
	// backends record it as BackendEncodeMS.
	if st.Backend == BackendBlock {
		p.BlockComposeMS = encodeMS
	} else {
		p.BackendEncodeMS = encodeMS
	}
	// The sixel renderer's output is the sixel payload, so record its byte count.
	// The bundle serialises it only for the sixel backend; other backends leave it
	// zero.
	if st.Backend == BackendSixel {
		p.SixelPayloadBytes = renderBytes
	}
	if frame.Img != nil {
		b := frame.Img.Bounds()
		p.CapturePxW, p.CapturePxH = b.Dx(), b.Dy()
		p.DrawPxW, p.DrawPxH = b.Dx(), b.Dy()
	}
	if grid != nil {
		p.GridCols, p.GridRows = grid.Cols, grid.Rows
	}
	return p
}

// overlayCells counts the grid cells the overlay covers across all runs. Each
// run covers one cell per rune in its text, matching the flatten the bundle
// writes.
func overlayCells(runs []render.GlyphRun) int {
	cells := 0
	for _, run := range runs {
		cells += len([]rune(run.Text))
	}
	return cells
}

// msSince returns the wall-clock milliseconds elapsed since start, never
// negative.
func msSince(start time.Time) float64 {
	d := time.Since(start)
	if d < 0 {
		return 0
	}
	return float64(d) / float64(time.Millisecond)
}
