package viewer

import (
	"errors"
	"fmt"
	"io"
	"os"
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
// timer at this duration when Navigate returns nil, and resets it to this
// duration on every drawn frame. captureLoop takes the duration as a parameter
// so timeout tests inject a tiny value and run instantly.
const captureIdle = 5 * time.Second

// bundleWriter is the capture-bundle sink the driver depends on. It is the seam
// the real capture.Bundle satisfies and a fake stands in for, so the bounded
// loop and the forced-backend rejection path stay testable without writing a
// bundle to disk. The driver opens the bundle, hands the engine its
// cdp-events.ndjson sink before Navigate, writes each drawn frame, writes the
// cdp-session.json bytes after navigation, finalises full on success or partial
// on a rejection, timeout, or early channel close, and reads the raw-file handle
// and the bundle path. The driver mirrors the engine mu discipline: it never
// holds a lock across a channel send, and these methods do no channel work.
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
	// navigation. The driver calls it once after Navigate returns, before
	// finalise. Empty bytes leave the placeholder untouched.
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

	return runCapture(eng, url, st, tty, bundleWriter(b), mode, n, captureIdle)
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
// before failing. ModeAuto and ModeBlock always pass.
func runCapture(eng Engine, url string, st *State, tty io.Writer, bundle bundleWriter, mode string, n int, idle time.Duration) error {
	if verr := ValidateForcedBackend(mode, st.Enabled); verr != nil {
		_ = bundle.FinalisePartial(verr.Error())
		fmt.Fprintf(tty, "capture bundle: %s\r\n", bundle.Path())
		return ErrPartialBundle
	}

	// Enable engine capture and set the recorder over the bundle's
	// cdp-events.ndjson sink before Navigate, before any goroutine starts,
	// matching the proxy set-once discipline. The session exposes the
	// cdp-session.json bytes the engine assembles during navigation.
	session := eng.EnableCapture(bundle.CDPEventsFile())

	return captureLoop(eng, url, st, tty, bundle, session, n, idle)
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
// writer, the capture session, the frame count, and the idle duration, so a unit
// test drives it directly with a fake Engine, a bytes.Buffer tty, and a tiny
// duration, bypassing the terminal check. It draws each frame with frame.Img !=
// nil through io.MultiWriter(tty, bundle.RawFile()), preceded by its own
// clearScreen, counts drawn frames, and records each frame's artefacts and
// viewer-phase timing.
//
// One time.Timer carries both deadlines: it arms at idle when Navigate returns
// nil (first-draw deadline) and resets to idle on every drawn frame (per-frame
// idle deadline). The loop returns on one of four outcomes:
//
//  1. n frames drawn: finalise a full bundle, return nil.
//  2. the timer fires before the first frame: finalise a partial bundle, return
//     ErrPartialBundle.
//  3. the timer fires after the first frame: finalise a partial bundle, return
//     ErrPartialBundle.
//  4. the frames channel closes before n: finalise a partial bundle, return
//     ErrPartialBundle.
//  5. a mid-loop draw or write error: finalise a partial bundle with the wrapped
//     error as the reason, return ErrPartialBundle.
//
// It prints the bundle path on every exit. The timer anchor matches
// streamSession.waitForNavigate: it arms exactly when the navigate goroutine
// returns nil. The single select orders the frames channel against the timer
// deterministically, so the timeout-after-frame and channel-close cases stay
// distinct.
func captureLoop(eng Engine, url string, st *State, tty io.Writer, bundle bundleWriter, session CaptureSession, n int, idle time.Duration) (err error) {
	defer func() { fmt.Fprintf(tty, "capture bundle: %s\r\n", bundle.Path()) }()

	navigated := make(chan error, 1)
	go func() {
		navigated <- eng.Navigate(url)
	}()
	if nerr := <-navigated; nerr != nil {
		writeSession(bundle, session)
		_ = bundle.FinalisePartial(fmt.Sprintf("navigate: %v", nerr))
		return ErrPartialBundle
	}

	// The engine assembled the cdp-session.json bytes during navigation; write
	// them into the bundle now that Navigate returned.
	writeSession(bundle, session)

	// Arm the timer exactly when Navigate returns nil, the first-draw deadline.
	timer := time.NewTimer(idle)
	defer timer.Stop()

	tee := io.MultiWriter(tty, bundle.RawFile())
	frames := eng.Frames()
	drawn := 0
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				_ = bundle.FinalisePartial(fmt.Sprintf("frames channel closed after %d of %d", drawn, n))
				return ErrPartialBundle
			}
			if frame.Img == nil {
				continue
			}
			in, derr := drawCaptureFrame(st, frame, tee)
			if derr != nil {
				_ = bundle.FinalisePartial(fmt.Sprintf("draw frame %d of %d: %v", drawn+1, n, derr))
				return ErrPartialBundle
			}
			if werr := bundle.WriteFrame(in); werr != nil {
				_ = bundle.FinalisePartial(fmt.Sprintf("write frame %d of %d: %v", drawn+1, n, werr))
				return ErrPartialBundle
			}
			drawn++
			if drawn >= n {
				if ferr := bundle.FinaliseFull(); ferr != nil {
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
			_ = bundle.FinalisePartial(reason)
			return ErrPartialBundle
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
// the bundle, emits its own clearScreen, then renders with the crisp DOM-text
// overlay folded on while timing the draw phases. The tee is io.MultiWriter(tty,
// rawFile), so the clear and render bytes mirror into the bundle raw file, and a
// CountingWriter over it counts the terminal bytes for this frame.
func drawCaptureFrame(st *State, frame render.Frame, tee io.Writer) (capture.FrameInput, error) {
	b := frame.Img.Bounds()
	st.ImgW, st.ImgH = b.Dx(), b.Dy()

	var grid *block.Grid
	if st.Backend == BackendBlock {
		grid = &block.Grid{}
		st.SetCaptureGrid(grid)
		defer st.SetCaptureGrid(nil)
	}

	counter := metrics.NewCountingWriter(tee)
	writeStart := time.Now()
	if _, err := io.WriteString(counter, clearScreen); err != nil {
		return capture.FrameInput{}, err
	}
	if err := st.RenderStreamFrame(frame, counter); err != nil {
		return capture.FrameInput{}, err
	}
	writeMS := msSince(writeStart)

	return capture.FrameInput{
		Frame: frame,
		Grid:  grid,
		Perf:  capturePerf(st, frame, grid, counter.N, writeMS),
	}, nil
}

// capturePerf fills the viewer-phase timing and counts for one drawn frame. It
// reads the resolved grid size and cell px from State, the capture and draw
// extent from the frame image and grid, the overlay run and cell counts from the
// frame text, the terminal bytes from the driver's CountingWriter, and the
// terminal write time the driver measured. The per-phase fit, encode, and
// compose timings stay zero here: the viewer draws through one RenderStreamFrame
// call, so the write time is the measured phase and the finer splits are not
// instrumented separately in this pass.
func capturePerf(st *State, frame render.Frame, grid *block.Grid, terminalBytes int64, writeMS float64) capture.ViewerPerf {
	p := capture.ViewerPerf{
		CellPxW:         st.W,
		CellPxH:         st.H,
		GlyphRuns:       len(frame.Text),
		OverlayCells:    overlayCells(frame.Text),
		TerminalBytes:   terminalBytes,
		TerminalWriteMS: writeMS,
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
