package viewer

import (
	"bytes"
	"errors"
	"image"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/capture"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// fakeBundle is the bundleWriter the capture-driver tests drive instead of a
// real on-disk capture.Bundle. It hands back in-memory buffers for the tee and
// the event sink, records each WriteFrame input, and remembers the finalise
// outcome and reason, so the tests assert the driver's control flow and the
// partial-bundle path without touching disk. Wave 10 reuses it for the
// four-exit-condition matrix.
type fakeBundle struct {
	raw      bytes.Buffer
	events   bytes.Buffer
	frames   []capture.FrameInput
	session  []byte
	finalise string // "full", or "partial: <reason>", empty until finalised
	writeErr error  // when set, WriteFrame returns it instead of recording
}

func (b *fakeBundle) RawFile() io.Writer       { return &b.raw }
func (b *fakeBundle) CDPEventsFile() io.Writer { return &b.events }
func (b *fakeBundle) Path() string             { return "/tmp/nyala-capture-fake" }

func (b *fakeBundle) WriteFrame(in capture.FrameInput) error {
	if b.writeErr != nil {
		return b.writeErr
	}
	b.frames = append(b.frames, in)
	return nil
}

func (b *fakeBundle) WriteSession(data []byte) error {
	b.session = data
	return nil
}

func (b *fakeBundle) FinaliseFull() error {
	b.finalise = "full"
	return nil
}

func (b *fakeBundle) FinalisePartial(reason string) error {
	b.finalise = "partial: " + reason
	return nil
}

// fakeCaptureEngine is the viewer.Engine the capture-driver tests drive. It
// scripts the frames channel and records whether Navigate ran, so the tests
// assert the forced-backend rejection returns before any Navigate. navigateErr,
// when set, makes Navigate fail. Wave 10 scripts frames and channel closes
// through it for the exit-condition matrix.
type fakeCaptureEngine struct {
	frames      chan render.Frame
	navigated   bool
	navigateErr error
}

func (e *fakeCaptureEngine) EnableCapture(io.Writer) CaptureSession { return nil }
func (e *fakeCaptureEngine) SetBackend(string)                      {}
func (e *fakeCaptureEngine) SetCellPx(int, int)                     {}
func (e *fakeCaptureEngine) Resize(int, int)                        {}

func (e *fakeCaptureEngine) Navigate(string) error {
	e.navigated = true
	return e.navigateErr
}

func (e *fakeCaptureEngine) Frames() <-chan render.Frame { return e.frames }
func (e *fakeCaptureEngine) SendKey(string)              {}
func (e *fakeCaptureEngine) Close() error                { return nil }

// blockState builds a State whose detection confirms block only, the floor
// backend. A forced sixel or kitty is not in its enabled set, so the validator
// rejects it; a forced block or auto passes. It injects the detection result via
// SetEnabled, the same seam production resolves through configureBackends, so the
// test needs no real terminal.
func blockState() *State {
	st := NewState(8, 4, 16, 16)
	st.SetEnabled([]Backend{BackendBlock})
	return &st
}

// TestRunCaptureForcedSixelRejectedWritesPartial asserts that forcing sixel on a
// block-only detection result writes a partial bundle, prints the bundle path,
// and returns ErrPartialBundle before any Navigate (task 7.2).
func TestRunCaptureForcedSixelRejectedWritesPartial(t *testing.T) {
	eng := &fakeCaptureEngine{frames: make(chan render.Frame)}
	bundle := &fakeBundle{}
	var tty bytes.Buffer

	err := runCapture(eng, "https://example.test", blockState(), &tty, bundle, ModeSixel, 1, time.Millisecond)

	if !errors.Is(err, ErrPartialBundle) {
		t.Fatalf("runCapture err = %v, want ErrPartialBundle", err)
	}
	if eng.navigated {
		t.Fatal("forced-backend rejection navigated, want rejection before Navigate")
	}
	if !strings.HasPrefix(bundle.finalise, "partial: ") {
		t.Fatalf("bundle finalise = %q, want a partial finalise", bundle.finalise)
	}
	if !strings.Contains(bundle.finalise, "sixel") {
		t.Errorf("partial reason = %q, want it to name sixel", bundle.finalise)
	}
	if !strings.Contains(tty.String(), bundle.Path()) {
		t.Errorf("tty = %q, want the bundle path printed", tty.String())
	}
}

// TestRunCaptureForcedKittyRejectedWritesPartial mirrors the sixel case for a
// forced kitty backend the detection result did not confirm.
func TestRunCaptureForcedKittyRejectedWritesPartial(t *testing.T) {
	eng := &fakeCaptureEngine{frames: make(chan render.Frame)}
	bundle := &fakeBundle{}
	var tty bytes.Buffer

	err := runCapture(eng, "https://example.test", blockState(), &tty, bundle, ModeKitty, 1, time.Millisecond)

	if !errors.Is(err, ErrPartialBundle) {
		t.Fatalf("runCapture err = %v, want ErrPartialBundle", err)
	}
	if eng.navigated {
		t.Fatal("forced-backend rejection navigated, want rejection before Navigate")
	}
	if !strings.Contains(bundle.finalise, "kitty") {
		t.Errorf("partial reason = %q, want it to name kitty", bundle.finalise)
	}
}

// TestRunCaptureForcedBlockNotRejected asserts the same harness with a forced
// block backend is not rejected: it passes validation and navigates. A
// one-frame script then drives a full capture so the not-rejected path runs to a
// full bundle.
func TestRunCaptureForcedBlockNotRejected(t *testing.T) {
	eng := &fakeCaptureEngine{frames: make(chan render.Frame, 1)}
	eng.frames <- render.Frame{Img: image.NewRGBA(image.Rect(0, 0, 16, 16))}
	bundle := &fakeBundle{}
	var tty bytes.Buffer

	err := runCapture(eng, "https://example.test", blockState(), &tty, bundle, ModeBlock, 1, captureIdle)
	if err != nil {
		t.Fatalf("runCapture err = %v, want nil for a full block capture", err)
	}
	if !eng.navigated {
		t.Fatal("forced block was not rejected but did not navigate")
	}
	if bundle.finalise != "full" {
		t.Fatalf("bundle finalise = %q, want full", bundle.finalise)
	}
	if len(bundle.frames) != 1 {
		t.Fatalf("bundle wrote %d frames, want 1", len(bundle.frames))
	}
}

// TestErrPartialBundleSentinelIsNonNil asserts the partial-bundle sentinel is a
// non-nil error app.Run propagates so cmd/nyala exits via its os.Exit(1) (task
// 8.2). It pins the sentinel identity that errors.Is matches across the four
// partial-bundle exit paths.
func TestErrPartialBundleSentinelIsNonNil(t *testing.T) {
	if ErrPartialBundle == nil {
		t.Fatal("ErrPartialBundle is nil, want a non-nil sentinel")
	}
	if !errors.Is(ErrPartialBundle, ErrPartialBundle) {
		t.Fatal("ErrPartialBundle does not match itself under errors.Is")
	}
}

// TestRunCaptureFullReturnsNilPartialReturnsSentinel asserts the full-versus-partial
// exit contract app.Run maps to the process exit code (task 8.2): a full capture
// returns nil, and a partial returns ErrPartialBundle which propagates so
// cmd/nyala would os.Exit(1). It drives runCapture below the terminal guard with a
// forced block backend, one path full, one path partial.
func TestRunCaptureFullReturnsNilPartialReturnsSentinel(t *testing.T) {
	t.Run("full returns nil", func(t *testing.T) {
		ch := make(chan render.Frame, 1)
		ch <- blockFrame()
		eng := &fakeCaptureEngine{frames: ch}
		var tty bytes.Buffer

		err := runCapture(eng, "https://example.test", blockState(), &tty, &fakeBundle{}, ModeBlock, 1, captureIdle)
		if err != nil {
			t.Fatalf("runCapture err = %v, want nil for a full capture", err)
		}
	})

	t.Run("partial returns the sentinel", func(t *testing.T) {
		// A forced sixel on a block-only detection result writes a partial bundle.
		eng := &fakeCaptureEngine{frames: make(chan render.Frame)}
		var tty bytes.Buffer

		err := runCapture(eng, "https://example.test", blockState(), &tty, &fakeBundle{}, ModeSixel, 1, time.Millisecond)
		if !errors.Is(err, ErrPartialBundle) {
			t.Fatalf("runCapture err = %v, want ErrPartialBundle for a partial capture", err)
		}
	})
}

// blockFrame builds one drawable frame: a 16x16 RGBA image with no text overlay.
// frame.Img != nil, so the capture loop counts and draws it. The size matches the
// blockState cell px so the block compose has a non-empty grid.
func blockFrame() render.Frame {
	return render.Frame{Img: image.NewRGBA(image.Rect(0, 0, 16, 16))}
}

// TestCaptureLoopFullCaptureReturnsNil covers exit condition 1: n scripted frames
// deliver, so the loop draws n, finalises a full bundle, and returns nil (task
// 3.4). It buffers all n frames so the loop never waits on the idle timer.
func TestCaptureLoopFullCaptureReturnsNil(t *testing.T) {
	const n = 3
	ch := make(chan render.Frame, n)
	for i := 0; i < n; i++ {
		ch <- blockFrame()
	}
	eng := &fakeCaptureEngine{frames: ch}
	bundle := &fakeBundle{}
	var tty bytes.Buffer

	err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, nil, n, captureIdle)
	if err != nil {
		t.Fatalf("captureLoop err = %v, want nil for a full capture", err)
	}
	if bundle.finalise != "full" {
		t.Fatalf("bundle finalise = %q, want full", bundle.finalise)
	}
	if len(bundle.frames) != n {
		t.Fatalf("bundle wrote %d frames, want %d", len(bundle.frames), n)
	}
	if !strings.Contains(tty.String(), bundle.Path()) {
		t.Errorf("tty = %q, want the bundle path printed on exit", tty.String())
	}
}

// TestCaptureLoopFirstDrawTimeoutWritesPartial covers exit condition 2: the engine
// never delivers a frame, so the idle timer fires before the first draw. The loop
// finalises a partial bundle and returns ErrPartialBundle (task 3.4). A tiny idle
// duration runs it instantly, with no real 5s wait. The frames channel stays open
// and empty so only the timer can fire.
func TestCaptureLoopFirstDrawTimeoutWritesPartial(t *testing.T) {
	eng := &fakeCaptureEngine{frames: make(chan render.Frame)}
	bundle := &fakeBundle{}
	var tty bytes.Buffer

	err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, nil, 3, time.Millisecond)
	if !errors.Is(err, ErrPartialBundle) {
		t.Fatalf("captureLoop err = %v, want ErrPartialBundle", err)
	}
	if !strings.HasPrefix(bundle.finalise, "partial: ") {
		t.Fatalf("bundle finalise = %q, want a partial finalise", bundle.finalise)
	}
	if !strings.Contains(bundle.finalise, "first-draw") {
		t.Errorf("partial reason = %q, want it to name the first-draw timeout", bundle.finalise)
	}
	if len(bundle.frames) != 0 {
		t.Errorf("bundle wrote %d frames, want 0 before the first draw", len(bundle.frames))
	}
}

// TestCaptureLoopPerFrameIdleTimeoutWritesPartial covers exit condition 3: one
// frame draws, then the channel goes silent, so the reset idle timer fires after
// the first draw. The loop finalises a partial bundle and returns ErrPartialBundle
// (task 3.4). A tiny idle duration runs it instantly.
func TestCaptureLoopPerFrameIdleTimeoutWritesPartial(t *testing.T) {
	ch := make(chan render.Frame, 1)
	ch <- blockFrame()
	eng := &fakeCaptureEngine{frames: ch}
	bundle := &fakeBundle{}
	var tty bytes.Buffer

	err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, nil, 3, time.Millisecond)
	if !errors.Is(err, ErrPartialBundle) {
		t.Fatalf("captureLoop err = %v, want ErrPartialBundle", err)
	}
	if !strings.Contains(bundle.finalise, "per-frame idle timeout") {
		t.Errorf("partial reason = %q, want it to name the per-frame idle timeout", bundle.finalise)
	}
	if len(bundle.frames) != 1 {
		t.Errorf("bundle wrote %d frames, want 1 before the idle timeout", len(bundle.frames))
	}
}

// TestCaptureLoopSteadyStreamDoesNotTripIdle asserts a steady stream of frames
// inside the idle window does not trip the per-frame timeout: the loop resets the
// timer on every draw, so n frames delivered without a gap longer than idle reach
// a full capture and return nil (task 3.4). A goroutine feeds frames spaced well
// inside the idle window.
func TestCaptureLoopSteadyStreamDoesNotTripIdle(t *testing.T) {
	const n = 5
	const idle = 200 * time.Millisecond
	ch := make(chan render.Frame)
	eng := &fakeCaptureEngine{frames: ch}
	bundle := &fakeBundle{}
	var tty bytes.Buffer

	go func() {
		for i := 0; i < n; i++ {
			ch <- blockFrame()
			time.Sleep(idle / 10)
		}
	}()

	err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, nil, n, idle)
	if err != nil {
		t.Fatalf("captureLoop err = %v, want nil; a steady stream must not trip the idle timeout", err)
	}
	if bundle.finalise != "full" {
		t.Fatalf("bundle finalise = %q, want full", bundle.finalise)
	}
	if len(bundle.frames) != n {
		t.Fatalf("bundle wrote %d frames, want %d", len(bundle.frames), n)
	}
}

// TestCaptureLoopChannelCloseBeforeNWritesPartial covers exit condition 4: the
// frames channel closes after k < n frames, so the loop finalises a partial bundle
// and returns ErrPartialBundle (task 3.4). The channel is buffered with the k
// frames then closed, so the receive sees the frames then the close.
func TestCaptureLoopChannelCloseBeforeNWritesPartial(t *testing.T) {
	const k = 2
	const n = 5
	ch := make(chan render.Frame, k)
	for i := 0; i < k; i++ {
		ch <- blockFrame()
	}
	close(ch)
	eng := &fakeCaptureEngine{frames: ch}
	bundle := &fakeBundle{}
	var tty bytes.Buffer

	err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, nil, n, captureIdle)
	if !errors.Is(err, ErrPartialBundle) {
		t.Fatalf("captureLoop err = %v, want ErrPartialBundle", err)
	}
	if !strings.Contains(bundle.finalise, "frames channel closed") {
		t.Errorf("partial reason = %q, want it to name the channel close", bundle.finalise)
	}
	if len(bundle.frames) != k {
		t.Errorf("bundle wrote %d frames, want %d before the close", len(bundle.frames), k)
	}
}

// TestCaptureLoopWriteFrameErrorWritesPartial covers the mid-loop write-error
// exit: WriteFrame fails on the first frame, so the loop finalises a partial
// bundle, names the wrapped write error in the reason, prints the bundle path,
// and returns ErrPartialBundle. A buffered frame plus a large idle duration mean
// only the write error can end the loop.
func TestCaptureLoopWriteFrameErrorWritesPartial(t *testing.T) {
	ch := make(chan render.Frame, 1)
	ch <- blockFrame()
	eng := &fakeCaptureEngine{frames: ch}
	bundle := &fakeBundle{writeErr: errors.New("disk full")}
	var tty bytes.Buffer

	err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, nil, 3, captureIdle)
	if !errors.Is(err, ErrPartialBundle) {
		t.Fatalf("captureLoop err = %v, want ErrPartialBundle", err)
	}
	if !strings.HasPrefix(bundle.finalise, "partial: ") {
		t.Fatalf("bundle finalise = %q, want a partial finalise, not a full one", bundle.finalise)
	}
	if !strings.Contains(bundle.finalise, "disk full") {
		t.Errorf("partial reason = %q, want it to preserve the write error detail", bundle.finalise)
	}
	if !strings.Contains(tty.String(), bundle.Path()) {
		t.Errorf("tty = %q, want the bundle path printed on exit", tty.String())
	}
}

// TestCaptureLoopNavigateFailureWritesPartial asserts a Navigate failure finalises
// a partial bundle and returns ErrPartialBundle before the frame loop, naming the
// navigate error (task 3.4 control flow, the navigate-fail branch of the driver).
func TestCaptureLoopNavigateFailureWritesPartial(t *testing.T) {
	eng := &fakeCaptureEngine{
		frames:      make(chan render.Frame),
		navigateErr: errors.New("boom"),
	}
	bundle := &fakeBundle{}
	var tty bytes.Buffer

	err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, nil, 3, time.Millisecond)
	if !errors.Is(err, ErrPartialBundle) {
		t.Fatalf("captureLoop err = %v, want ErrPartialBundle", err)
	}
	if !strings.Contains(bundle.finalise, "navigate") {
		t.Errorf("partial reason = %q, want it to name the navigate failure", bundle.finalise)
	}
	if len(bundle.frames) != 0 {
		t.Errorf("bundle wrote %d frames, want 0 on a navigate failure", len(bundle.frames))
	}
}

// TestCaptureLoopTeeMirrorsClearAndRenderOnly asserts the tee mirrors exactly the
// per-frame clearScreen plus the per-frame render output into both the tty and the
// raw file, and excludes enterFullScreen and the loading cue (task 3.4). The
// expected bytes are computed by replaying clearScreen and RenderStreamFrame over
// a fresh identical State, so the test asserts byte equality against a derived
// reference, never a hardcoded golden.
func TestCaptureLoopTeeMirrorsClearAndRenderOnly(t *testing.T) {
	const n = 2
	frame := blockFrame()
	ch := make(chan render.Frame, n)
	for i := 0; i < n; i++ {
		ch <- frame
	}
	eng := &fakeCaptureEngine{frames: ch}
	bundle := &fakeBundle{}
	var tty bytes.Buffer

	if err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, nil, n, captureIdle); err != nil {
		t.Fatalf("captureLoop err = %v, want nil", err)
	}

	// Build the expected per-frame bytes the driver writes: clearScreen then the
	// rendered frame, mirroring drawCaptureFrame. The reference State matches the
	// driver's: same dimensions, sized to the frame image, with a capture grid set
	// so the block draw matches the driver's path. This derives the bytes rather
	// than hardcoding a golden.
	ref := blockState()
	ref.ImgW, ref.ImgH = 16, 16
	ref.SetCaptureGrid(&block.Grid{})
	var oneFrame bytes.Buffer
	if _, err := io.WriteString(&oneFrame, clearScreen); err != nil {
		t.Fatalf("write clearScreen: %v", err)
	}
	if err := ref.RenderStreamFrame(frame, &oneFrame); err != nil {
		t.Fatalf("reference render: %v", err)
	}
	want := strings.Repeat(oneFrame.String(), n)

	raw := bundle.raw.String()
	if raw != want {
		t.Errorf("raw file bytes do not equal the per-frame clear+render sequence\n got len %d\nwant len %d", len(raw), len(want))
	}

	// The tty carries the same drawn bytes plus the trailing bundle-path line. The
	// drawn prefix must equal the raw file: the tee mirrors the same bytes to both.
	ttyStr := tty.String()
	if !strings.HasPrefix(ttyStr, want) {
		t.Errorf("tty drawn prefix does not equal the raw file bytes")
	}

	// The capture path must not mirror the full-screen enter or the loading cue.
	if strings.Contains(raw, enterFullScreen) {
		t.Error("raw file carries enterFullScreen, want it excluded from the capture")
	}
	if strings.Contains(ttyStr, enterFullScreen) {
		t.Error("tty carries enterFullScreen, want it excluded from the capture")
	}
	if strings.Contains(raw, "loading ") || strings.Contains(ttyStr, "loading https://example.test") {
		t.Error("capture carries the loading cue, want it excluded")
	}
}
