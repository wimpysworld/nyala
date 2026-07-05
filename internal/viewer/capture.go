package viewer

import (
	"bytes"
	"errors"
	"fmt"
	"image"
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
// loading cue, so the raw capture holds only the first-draw clear, the per-frame
// cursorHome, and the render bytes, byte-matching a live draw after the
// first-draw prefix. It returns nil on a full capture and ErrPartialBundle when it writes a
// partial bundle. colourFlag is the --color intent; mode is the --backend mode
// the forced-backend validator checks; layoutWidth is the block-backend target
// layout width in CSS pixels, handed to the engine before Navigate. scrollPx is
// the capture scroll hook's requested vertical scroll in CSS pixels: when
// positive, after the n frames draw the driver injects the scroll through the
// engine's wheel transport, waits for the engine's settle snapshot to publish the
// text at the resting scroll position, then draws one more usable frame that
// reflects the scrolled state, so the bundle's last frame is the scrolled bitmap
// with its settled text overlay. A zero or negative scrollPx skips the hook.
// settle gates the scroll hook's settle wait: true waits for the settled overlay,
// false draws the first frame whose scroll offset advanced (the pre-settle state).
func Capture(eng Engine, url string, tty *os.File, colourFlag, mode string, layoutWidth, n, scrollPx int, settle bool) (err error) {
	_, st, restore, err := setupViewer(eng, tty, colourFlag, layoutWidth)
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

	b, err := capture.NewBundle(captureMeta(url, st, mode, n, scrollPx))
	if err != nil {
		return fmt.Errorf("viewer: open capture bundle: %w", err)
	}

	return runCapture(eng, url, st, tty, bundleWriter(b), mode, n, scrollPx, settle, capturePaintDeadline, captureIdle)
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
// for first contentful paint; idle is the per-frame deadline. scrollPx is the
// capture scroll hook's requested vertical scroll, passed through to the loop;
// settle gates the hook's settle wait.
func runCapture(eng Engine, url string, st *State, tty io.Writer, bundle bundleWriter, mode string, n, scrollPx int, settle bool, paintWait, idle time.Duration) error {
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

	return captureLoop(eng, url, st, tty, bundle, session, n, scrollPx, settle, paintWait, idle)
}

// captureMeta builds the bundle run metadata from the resolved state and the
// invocation. The backend is the resolved active backend; the terminal
// capability mirrors it. Command omits secrets: it carries the URL and backend
// mode only, never proxy env values.
func captureMeta(url string, st *State, mode string, n, scrollPx int) capture.Meta {
	backend := capture.Backend(backendName(st.Backend))
	return capture.Meta{
		Command:            fmt.Sprintf("nyala --backend=%s %s", mode, url),
		Backend:            backend,
		TerminalCapability: backendName(st.Backend),
		FramesRequested:    n,
		NavigationURL:      url,
		RequestedScrollPx:  scrollPx,
	}
}

// captureLoop runs the bounded capture loop below the not-a-terminal guard. It
// is unexported and takes the Engine, the State, the tty io.Writer, the bundle
// writer, the capture session, the frame count, the paint deadline, and the idle
// duration, so a unit test drives it directly with a fake Engine, a bytes.Buffer
// tty, and tiny durations, bypassing the terminal check. It draws each frame with
// frame.Img != nil through io.MultiWriter(tty, bundle.RawFile()), preceded by the
// same cursor prefix the live path emits (full clearScreen on the first frame,
// cursorHome after), counts drawn frames, and records each frame's artefacts and
// viewer-phase timing.
//
// After Navigate returns nil it waits for the painted latch before counting any
// usable frame, draining and discarding pre-paint frames so the latest-wins slot
// does not stall. An early frame, captured before first contentful paint, is
// blank; waiting for paint keeps a blank frame out of the n. The wait is bounded
// by paintWait; if the paint signal never fires the loop writes a partial bundle
// reasoned "first contentful paint timeout".
//
// After paint the engine publishes the first DOM snapshot off the painted latch
// and re-emits the held bitmap recomposed with the new text, so the first usable
// frame the loop draws already carries its text runs. The loop no longer waits on
// a separate text latch; it draws what the live path draws.
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
//
// When scrollPx is positive the loop runs the capture scroll hook after the n
// frames draw: it injects the scroll through the engine's wheel transport and
// draws one more scrolled frame as the bundle's last frame (captureScrollFrame).
// A scroll timeout or a closed channel during the hook writes a partial bundle.
// A zero or negative scrollPx skips the hook and finalises after the n frames.
// settle gates the scroll hook's settle wait, passed through to captureScrollFrame.
func captureLoop(eng Engine, url string, st *State, tty io.Writer, bundle bundleWriter, session CaptureSession, n, scrollPx int, settle bool, paintWait, idle time.Duration) (err error) {
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

	// Wait for the first overlay publish before counting a usable frame, draining
	// post-paint pre-publish frames so the slot does not stall. Paint and the first
	// publish both wake off the painted latch, but the publish trails paint by the
	// calibration pass, so without this wait the driver latches the first frame with
	// no text. A publish timeout or a closed channel writes a partial bundle.
	if reason, ok := waitForText(session, frames, paintWait); !ok {
		_ = finalisePartial(reason)
		return ErrPartialBundle
	}

	// The cell differ drives the capture block paint exactly as it drives the live
	// path: the first frame full-repaints against a clearScreen, every later frame
	// emits cursorHome and diffs. Setting it on the State routes renderBlock through
	// the differ, so a captured stream byte-matches a live draw after the first-draw
	// prefix. The graphics backends ignore it.
	diff := &block.GridDiff{}
	st.SetDiff(diff)

	// Arm the per-frame timer now that paint has fired, the first-draw deadline.
	timer := time.NewTimer(idle)
	defer timer.Stop()

	var nilImageOnce sync.Once
	drawn := 0
	// drawOne draws one usable frame, records its artefacts, and resets the idle
	// timer. It returns a reason on a draw or write failure; the caller finalises a
	// partial bundle. firstDrawn selects the cursor prefix and seeds the differ.
	drawOne := func(frame render.Frame) (reason string, ok bool) {
		in, derr := drawCaptureFrame(st, frame, tee, drawn > 0)
		if derr != nil {
			return fmt.Sprintf("draw frame %d of %d: %v", drawn+1, n, derr), false
		}
		if werr := bundle.WriteFrame(in); werr != nil {
			return fmt.Sprintf("write frame %d of %d: %v", drawn+1, n, werr), false
		}
		drawn++
		// Reset the per-frame idle deadline. Stop and drain before reset so a fire
		// that raced the draw does not leak onto the next select.
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(idle)
		return "", true
	}

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
			// Blank-frame gate on the first counted frame only. The paint and publish
			// latches gate on event order, not bitmap content: on a large surface
			// Chromium can satisfy both latches on the frame before the first paint
			// lands, a uniform field of the background colour with no ink. Skip that
			// no-ink bitmap so the bundle's opening frame is the painted page. A
			// text-only page now paints no glyph ink into the bitmap (the block path
			// hides page text and the crisp overlay carries it), so bitmap blankness
			// alone no longer means unpainted; the overlay runs distinguish a painted
			// text page from a true pre-paint frame. Only the first frame is gated:
			// once a painted frame is counted, later frames draw as-is (a legitimately
			// dark or sparse page must not stall). The idle timer, already armed, bounds
			// the wait and writes a "first-draw timeout" partial if no painted frame
			// arrives.
			if drawn == 0 && firstFrameUnpainted(frame.Img, frame.Text) {
				continue
			}
			if reason, ok := drawOne(frame); !ok {
				_ = finalisePartial(reason)
				return ErrPartialBundle
			}
			if drawn >= n {
				// The n frames drew. With a scroll requested, inject it and draw one more
				// scrolled frame as the bundle's last frame; a scroll failure writes a
				// partial bundle. With no scroll, finalise the full bundle straight away.
				if scrollPx > 0 {
					// The last drawn frame is the pre-injection baseline: the scrolled
					// frame must advance past its offset, not merely read positive, so a
					// page that loads already scrolled is not mistaken for the result.
					var baseScrollY float64
					if frame.Meta != nil {
						baseScrollY = frame.Meta.ScrollOffsetY
					}
					if reason, ok := captureScrollFrame(eng, session, frames, tee, st, bundle, timer, scrollPx, settle, idle, drawn, baseScrollY); !ok {
						_ = finalisePartial(reason)
						return ErrPartialBundle
					}
				}
				if ferr := finaliseFull(); ferr != nil {
					return ferr
				}
				return nil
			}
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

// captureScrollFrame runs the capture scroll hook: it injects a vertical scroll
// of scrollPx CSS pixels through the engine's wheel transport (the same chain a
// user wheel or scroll key takes), then draws one more usable frame that reflects
// the scrolled state as the bundle's last frame. It draws a frame only once both
// hold: the engine's settle snapshot has published the text at the resting scroll
// position after the injection (session.ScrollSettle poked, so the folded overlay
// is the scrolled text, not the stale pre-scroll text), and the frame's screencast
// scroll offset has advanced (so the bitmap is the scrolled page). The advanced
// bitmap frame lands before the debounced settle snapshot, so without the settle
// gate the drawn frame carries a stale text map; the gate holds the draw until the
// settled overlay has published and the frame loop has re-emitted the held bitmap
// with it. The settle signal is drained before the injection so a stale pre-scroll
// poke does not satisfy the gate. baseScrollY is the pre-injection scroll offset:
// the scrolled bitmap must advance strictly past it, so a page that loaded already
// scrolled is not mistaken for the injection result. A nil session or a nil settle
// channel (a fake-engine test that scripts the scrolled frame directly) reads as
// settled at once, so the hook falls back to the advanced-offset wait alone. nilImage frames
// are skipped; the wait is bounded by the idle timer, already armed by the last
// main-loop draw. It returns a reason on a draw or write failure, a scroll timeout,
// or a closed frames channel; ok is true once the scrolled frame is drawn and
// recorded. drawn is the count of frames already drawn, so the failure reasons name
// the right frame index. settle gates the settle wait: when false the hook skips it
// and draws the first frame whose scroll offset advanced (the pre-settle state, whose
// overlay holds the previous runs until the settle re-snapshot lands at the bitmap's
// scroll).
func captureScrollFrame(eng Engine, session CaptureSession, frames <-chan render.Frame, tee io.Writer, st *State, bundle bundleWriter, timer *time.Timer, scrollPx int, settle bool, idle time.Duration, drawn int, baseScrollY float64) (reason string, ok bool) {
	// Read the settle channel and drain any stale poke before the injection, so only a
	// settle publish that lands after this scroll satisfies the gate. A nil session or
	// a nil channel means no settle gate (a fake-engine test): treat it as settled at
	// once and fall back to the advanced-offset wait. settle=false skips the gate the
	// same way, drawing the first advanced-offset frame before the settle snapshot.
	var settleC <-chan struct{}
	if settle && session != nil {
		settleC = session.ScrollSettle()
	}
	settled := settleC == nil
	if settleC != nil {
		select {
		case <-settleC:
		default:
		}
	}

	eng.SendScroll(scrollPx)
	// Re-arm the idle timer to bound the wait for the scrolled re-snapshot: the
	// engine debounces the scroll before it re-snapshots and re-emits.
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(idle)

	var nilImageOnce sync.Once
	for {
		select {
		case <-settleC:
			// The settle snapshot published the scrolled text. Mark settled and keep
			// waiting for the advanced-offset frame the settle re-emit produces.
			settled = true
		case frame, open := <-frames:
			if !open {
				return fmt.Sprintf("frames channel closed before scrolled frame after %d", drawn), false
			}
			if frame.Img == nil {
				nilImageOnce.Do(func() {
					fmt.Fprintln(os.Stderr, "nyala: capture skipped a frame with no image")
				})
				continue
			}
			// Check the settle signal before judging this frame: the settle snapshot
			// pokes reEmit and scrollSettle together, so the re-emitted scrolled frame and
			// its settle poke race onto their channels. Draining the poke here keeps the
			// select from skipping that frame as still-stale when the poke has landed.
			if !settled && settleC != nil {
				select {
				case <-settleC:
					settled = true
				default:
				}
			}
			// Wait for both the settled text publish and the scrolled bitmap. The
			// scrolled frame's screencast metadata reports a vertical offset past the
			// pre-injection baseline; a pre-scroll frame still shows the baseline and a
			// frame with no metadata cannot confirm the scroll, so both are skipped. The
			// strict advance over baseScrollY, not a bare positive test, keeps a page
			// that loaded already scrolled from finalising on a stale frame. Until the
			// settle poke lands, an advanced-offset frame still carries the stale
			// pre-scroll overlay, so it is held back too.
			if !settled || frame.Meta == nil || frame.Meta.ScrollOffsetY <= baseScrollY {
				continue
			}
			in, derr := drawCaptureFrame(st, frame, tee, true)
			if derr != nil {
				return fmt.Sprintf("draw scrolled frame after %d: %v", drawn, derr), false
			}
			if werr := bundle.WriteFrame(in); werr != nil {
				return fmt.Sprintf("write scrolled frame after %d: %v", drawn, werr), false
			}
			return "", true
		case <-timer.C:
			return fmt.Sprintf("scroll re-snapshot timeout after %d", drawn), false
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
	return waitForLatch(session.Painted(), frames, paintWait, false,
		"first contentful paint timeout", "frames channel closed before first contentful paint")
}

// waitForText blocks until the session's first-publish latch closes, then returns
// ok=true. While waiting it drains and discards post-paint pre-publish frames so
// the engine's latest-wins slot does not stall. It bounds the wait with a timer at
// paintWait. It returns ok=false with a reason when the publish deadline fires
// ("first overlay publish timeout") or the frames channel closes before the publish
// ("frames channel closed before first overlay publish"). A nil session or a nil
// textReady latch reads as never closed, so the deadline bounds the wait; the
// production path always wires the latch.
func waitForText(session CaptureSession, frames <-chan render.Frame, paintWait time.Duration) (reason string, ok bool) {
	if session == nil {
		return "", true
	}
	return waitForLatch(session.TextReady(), frames, paintWait, false,
		"first overlay publish timeout", "frames channel closed before first overlay publish")
}

// waitForLatch blocks until latch closes, then returns ok=true. While waiting it
// drains and discards frames from the frames channel so the engine's latest-wins
// slot does not stall on an unconsumed frame. It bounds the wait with a timer at
// timeout. On a closed channel it returns ok=false with channelReason. On the
// timeout it returns timeoutIsSuccess as ok with an empty reason when true, or
// timeoutReason with ok=false when false. A nil latch reads as never closed, so
// the timeout bounds the wait.
func waitForLatch(latch <-chan struct{}, frames <-chan render.Frame, timeout time.Duration, timeoutIsSuccess bool, timeoutReason, channelReason string) (reason string, ok bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		// Check the latch first with priority. A plain three-way select picks at
		// random when the latch is already signalled and a frame is also ready, which
		// could discard a post-latch frame the bounded loop should count.
		select {
		case <-latch:
			return "", true
		default:
		}
		select {
		case <-latch:
			return "", true
		case _, open := <-frames:
			if !open {
				return channelReason, false
			}
			// Discard the frame: do not draw, count, or write it.
		case <-timer.C:
			if timeoutIsSuccess {
				return "", true
			}
			return timeoutReason, false
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
// then writes the cursor prefix and the buffered bytes to the tee, which is the
// terminal write. Buffering first separates encode from write so neither span
// overlaps the other. The tee is io.MultiWriter(tty, rawFile), so the prefix and
// render bytes mirror into the bundle raw file in the same order Stream emits
// them, and a CountingWriter over it counts the terminal bytes for this frame.
//
// firstDrawn matches the live path: the first frame emits the full clearScreen
// and seeds the differ to full-repaint, every later frame emits cursorHome only
// and diffs, so a captured stream byte-matches a live draw after the first-draw
// prefix. The differ comes from the State (SetDiff); a nil differ (a direct test
// or the graphics backends) falls back to a full grid write inside
// RenderStreamFrame.
func drawCaptureFrame(st *State, frame render.Frame, tee io.Writer, firstDrawn bool) (capture.FrameInput, error) {
	b := frame.Img.Bounds()
	st.ImgW, st.ImgH = b.Dx(), b.Dy()

	var grid *block.Grid
	if st.Backend == BackendBlock {
		grid = &block.Grid{}
		st.SetCaptureGrid(grid)
		defer st.SetCaptureGrid(nil)
	}

	// The cursor prefix matches the live path and seeds the differ on the first
	// draw, so it must be computed before the encode phase reads the differ.
	prefix := framePrefix(firstDrawn, st.diff)

	// Encode phase: render into a buffer so the escape-byte production is timed
	// apart from the terminal write. The buffer length is the renderer's own
	// output, excluding the cursor prefix, so the sixel payload size stays
	// accurate.
	var rendered bytes.Buffer
	encodeStart := time.Now()
	if err := st.RenderStreamFrame(frame, &rendered); err != nil {
		return capture.FrameInput{}, err
	}
	encodeMS := msSince(encodeStart)
	renderBytes := int64(rendered.Len())

	// Write phase: the cursor prefix then the buffered render bytes go to the tee.
	// Only the io.Write is timed here, so terminal_write_ms no longer includes the
	// encode.
	counter := metrics.NewCountingWriter(tee)
	writeStart := time.Now()
	if _, err := io.WriteString(counter, prefix); err != nil {
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

// frameBlankThreshold is the fraction of sampled pixels that must differ from
// the background colour for a frame to count as painted. The gate exists to skip
// the fully unpainted pre-paint compositor frame, a uniform field of the
// background colour with no ink. Any real page carries far more ink than this:
// the sparsest corpus page (a wide-grid RTL or CJK layout with little text) still
// covers about one percent of sampled pixels, so the floor sits well below that
// and well above the zero a uniform pre-paint frame produces. Raising it to catch
// "half-composited" frames starved those sparse pages, timing the capture out on a
// legitimately painted bitmap; the floor is deliberately near zero so only a
// no-ink frame reads as blank.
const frameBlankThreshold = 0.0025

// frameBlankTolerance is the per-channel delta a sampled pixel must exceed from
// the background colour to count as foreground. It absorbs the sub-pixel dither
// a flat background carries so anti-aliasing noise is not mistaken for content.
const frameBlankTolerance = 12

// frameBlankStride subsamples the bitmap so the completeness check stays cheap:
// every strideth pixel on both axes is tested, which is ample to separate a
// painted page from a blank one without walking every pixel of a multi-megapixel
// surface. The check runs capture-only, so production never pays it.
const frameBlankStride = 8

// firstFrameUnpainted reports whether the first counted frame is a genuine
// pre-paint frame: its bitmap carries no ink and it carries no overlay text runs.
// A text-only page paints no glyph ink into the bitmap now that the block path
// hides page text, so bitmap blankness alone no longer proves the frame is
// unpainted; a painted text page has overlay runs (the text latch holds the first
// counted frame until the runs are folded on), so the runs keep it from being
// skipped. It is capture-only.
func firstFrameUnpainted(img *image.RGBA, runs []render.GlyphRun) bool {
	return frameBlank(img) && len(runs) == 0
}

// frameBlank reports whether img is a no-ink pre-paint bitmap: its
// frameInkFraction is below frameBlankThreshold. A flat background of any colour
// reads as blank and a page with real content, however sparse, does not. A
// degenerate (zero-extent) image reads as blank.
func frameBlank(img *image.RGBA) bool {
	return frameInkFraction(img) < frameBlankThreshold
}

// frameInkFraction returns the fraction of subsampled pixels that differ from the
// background colour (read from the top-left corner) by more than
// frameBlankTolerance on any RGB channel. It skips the alpha channel and samples
// every frameBlankStride pixel on both axes. A degenerate (zero-extent) image
// returns 0, so frameBlank reads it as blank. It is capture-only, so its cost
// never touches the live path.
func frameInkFraction(img *image.RGBA) float64 {
	b := img.Bounds()
	if b.Empty() {
		return 0
	}
	bg := img.RGBAAt(b.Min.X, b.Min.Y)
	sampled, foreground := 0, 0
	for y := b.Min.Y; y < b.Max.Y; y += frameBlankStride {
		for x := b.Min.X; x < b.Max.X; x += frameBlankStride {
			sampled++
			p := img.RGBAAt(x, y)
			if absDelta(p.R, bg.R) > frameBlankTolerance ||
				absDelta(p.G, bg.G) > frameBlankTolerance ||
				absDelta(p.B, bg.B) > frameBlankTolerance {
				foreground++
			}
		}
	}
	if sampled == 0 {
		return 0
	}
	return float64(foreground) / float64(sampled)
}

// absDelta returns the absolute difference of two 8-bit channel values as an int,
// avoiding the wraparound a uint8 subtraction would leave.
func absDelta(a, b uint8) int {
	if a > b {
		return int(a) - int(b)
	}
	return int(b) - int(a)
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
