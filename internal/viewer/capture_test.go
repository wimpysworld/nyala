package viewer

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/capture"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// TestRenderStreamFrameGraphicsNoSecondDownscale asserts the graphics path treats
// frame.Img as final: the engine sized the bitmap, so RenderStreamFrame encodes it
// at its exact dimensions with no second Downscale. The image height is chosen so
// the fit's own GraphicsCaptureSize rounds to a different height (160, not 170), so
// the pre-2.4 double-downscale would resize the bitmap to 320x160 and the decoded
// extent would differ from the engine-sized 320x170.
func TestRenderStreamFrameGraphicsNoSecondDownscale(t *testing.T) {
	const cols, rows = 40, 20
	const cellW, cellH = 8, 16
	st := NewState(cols, rows, 1, 1)
	st.cellPx = cellPx{W: cellW, H: cellH}
	st.SetEnabled([]Backend{BackendSixel, BackendBlock})
	st.Backend = BackendSixel

	// The engine sizes the graphics bitmap; the viewer must encode it as-is. 320x170
	// fits to 40x10 cells, whose GraphicsCaptureSize is 320x160, so a second
	// downscale would shrink the height. Treating the frame as final keeps 320x170.
	const imgW, imgH = 320, 170
	img := image.NewRGBA(image.Rect(0, 0, imgW, imgH))
	// Fill with an opaque colour so the sixel encoder emits a non-empty raster; an
	// all-transparent image can encode to nothing.
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	frame := render.Frame{Img: img}
	st.ImgW, st.ImgH = imgW, imgH

	var out bytes.Buffer
	if err := st.RenderStreamFrame(frame, &out); err != nil {
		t.Fatalf("RenderStreamFrame(sixel) = %v, want nil", err)
	}
	// The letterboxed fit prefixes a cursor-position escape; slice from the sixel
	// DCS introducer so DecodeSixel sees the raster alone.
	raster := out.Bytes()
	if i := bytes.Index(raster, []byte("\x1bPq")); i >= 0 {
		raster = raster[i:]
	}
	decoded, err := capture.DecodeSixel(raster)
	if err != nil {
		t.Fatalf("DecodeSixel: %v", err)
	}
	b := decoded.Bounds()
	if b.Dx() != imgW || b.Dy() != imgH {
		t.Errorf("decoded sixel = %dx%d, want the engine-sized %dx%d (no second downscale)", b.Dx(), b.Dy(), imgW, imgH)
	}
}

// TestRenderStreamFrameDegenerateGuardBothBackends asserts the single degenerate
// guard hoisted into RenderStreamFrame covers both the block and the graphics
// backends: a zero-extent frame.Img draws nothing rather than downscaling to a
// zero extent.
func TestRenderStreamFrameDegenerateGuardBothBackends(t *testing.T) {
	frame := render.Frame{Img: image.NewRGBA(image.Rect(0, 0, 0, 0))}
	for _, b := range []Backend{BackendBlock, BackendSixel} {
		st := NewState(40, 20, 1, 1)
		st.SetEnabled([]Backend{BackendSixel, BackendBlock})
		st.Backend = b
		var out bytes.Buffer
		if err := st.RenderStreamFrame(frame, &out); err != nil {
			t.Fatalf("RenderStreamFrame(%v) on a zero-extent image = %v, want nil", b, err)
		}
		if out.Len() != 0 {
			t.Errorf("RenderStreamFrame(%v) wrote %d bytes for a zero-extent image, want a no-op", b, out.Len())
		}
	}
}

// fakeBundle is the bundleWriter the capture-driver tests drive instead of a
// real on-disk capture.Bundle. It hands back in-memory buffers for the tee and
// the event sink, records each WriteFrame input, and remembers the finalise
// outcome and reason, so the tests assert the driver's control flow and the
// partial-bundle path without touching disk. Wave 10 reuses it for the
// four-exit-condition matrix.
type fakeBundle struct {
	raw           bytes.Buffer
	events        bytes.Buffer
	frames        []capture.FrameInput
	session       []byte
	sessionBefore bool // true when WriteSession ran before any finalise
	finalised     bool // set once the first finalise ran
	onWriteFrame  func()
	finalise      string // "full", or "partial: <reason>", empty until finalised
	writeErr      error  // when set, WriteFrame returns it instead of recording
}

func (b *fakeBundle) RawFile() io.Writer       { return &b.raw }
func (b *fakeBundle) CDPEventsFile() io.Writer { return &b.events }
func (b *fakeBundle) Path() string             { return "/tmp/nyala-capture-fake" }

func (b *fakeBundle) WriteFrame(in capture.FrameInput) error {
	if b.onWriteFrame != nil {
		b.onWriteFrame()
	}
	if b.writeErr != nil {
		return b.writeErr
	}
	b.frames = append(b.frames, in)
	return nil
}

func (b *fakeBundle) WriteSession(data []byte) error {
	b.session = data
	if !b.finalised {
		b.sessionBefore = true
	}
	return nil
}

func (b *fakeBundle) FinaliseFull() error {
	b.finalised = true
	b.finalise = "full"
	return nil
}

func (b *fakeBundle) FinalisePartial(reason string) error {
	b.finalised = true
	b.finalise = "partial: " + reason
	return nil
}

// fakeSession is the CaptureSession the session-timing test drives. SessionJSON
// returns nil until filled flips true, simulating the engine's fillSession
// running during navigation: the bytes are empty right after Navigate and
// populated only once a frame has flowed. The test flips filled on the first
// WriteFrame so the populated bytes exist at finalise but not before.
type fakeSession struct {
	filled bool
	data   []byte
	// painted is the painted latch the driver waits on. A nil channel means
	// "already painted": Painted returns a pre-closed channel so the session-timing
	// tests, not the gate, drive the bounded loop. The gate tests set their own
	// channel.
	painted <-chan struct{}
	// textReady is the first-publish latch the driver waits on after paint. A nil
	// channel means "already published": TextReady returns a pre-closed channel so
	// the session-timing tests proceed straight to the bounded loop. The gate tests
	// set their own channel.
	textReady <-chan struct{}
	// scrollSettle is the post-scroll settle signal the scroll hook waits on. A nil
	// channel means "already settled": ScrollSettle returns a pre-closed channel so
	// the non-gate tests fall back to the advanced-offset wait. The settle-gate test
	// sets its own channel and pokes it after the stale advanced frame.
	scrollSettle <-chan struct{}
}

func (s *fakeSession) SessionJSON() []byte {
	if !s.filled {
		return nil
	}
	return s.data
}

// closedChan is a pre-closed channel the fake session hands back when no explicit
// painted latch is set, so a session-timing test proceeds straight to the bounded
// loop.
var closedChan = func() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

func (s *fakeSession) Painted() <-chan struct{} {
	if s.painted == nil {
		return closedChan
	}
	return s.painted
}

func (s *fakeSession) TextReady() <-chan struct{} {
	if s.textReady == nil {
		return closedChan
	}
	return s.textReady
}

func (s *fakeSession) ScrollSettle() <-chan struct{} {
	if s.scrollSettle == nil {
		return closedChan
	}
	return s.scrollSettle
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
	// onScroll, when set, runs on SendScroll with the requested CSS-pixel scroll,
	// so a scroll-hook test can script the scrolled frame the engine would emit.
	onScroll func(dy int)
}

func (e *fakeCaptureEngine) EnableCapture(io.Writer) CaptureSession { return nil }
func (e *fakeCaptureEngine) SetCaptureSettleHold(bool)              {}
func (e *fakeCaptureEngine) SetBackend(string)                      {}
func (e *fakeCaptureEngine) SetCellPx(int, int)                     {}
func (e *fakeCaptureEngine) SetLayoutWidth(int)                     {}
func (e *fakeCaptureEngine) Resize(int, int)                        {}

func (e *fakeCaptureEngine) Navigate(string) error {
	e.navigated = true
	return e.navigateErr
}

func (e *fakeCaptureEngine) Frames() <-chan render.Frame { return e.frames }
func (e *fakeCaptureEngine) SendKey(string)              {}
func (e *fakeCaptureEngine) SendScroll(dy int) {
	if e.onScroll != nil {
		e.onScroll(dy)
	}
}
func (e *fakeCaptureEngine) Close() error { return nil }

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

// kittyState builds a State whose detection confirms kitty, sixel, then block,
// simulating wezterm. SetEnabled resolves the active backend to kitty, the
// highest-fidelity entry. A forced block must override that active backend, so
// this is the fixture for the forced-backend pinning tests. It injects the
// detection result via SetEnabled, the same seam configureBackends fills, so the
// test needs no real terminal.
func kittyState() *State {
	st := NewState(8, 4, 16, 16)
	st.SetEnabled([]Backend{BackendKitty, BackendSixel, BackendBlock})
	return &st
}

// TestCaptureForcedBlockOverridesDetectedKitty asserts the capture path pins the
// forced --backend even when detection advertised kitty. It mirrors the Capture
// flow at the seams: a kitty-enabled state, validate then force block, build the
// bundle meta, then run the bounded loop. The manifest meta must record backend
// block, and the drawn frame must carry a non-nil Grid so the real bundle writes
// terminal-model.json (the block path), not omit it as a graphics backend would.
func TestCaptureForcedBlockOverridesDetectedKitty(t *testing.T) {
	st := kittyState()

	// Mirror the Capture ordering: validation gates forcing, forcing precedes
	// captureMeta so the manifest records the forced backend.
	if err := ValidateForcedBackend(ModeBlock, st.Enabled); err != nil {
		t.Fatalf("ValidateForcedBackend(block) = %v, want nil", err)
	}
	applyForcedBackend(st, ModeBlock)

	meta := captureMeta("https://example.test", st, ModeBlock, 1, 0)
	if meta.Backend != capture.Backend("block") {
		t.Fatalf("meta backend = %q, want block; the manifest must record the forced backend", meta.Backend)
	}

	ch := make(chan render.Frame, 1)
	ch <- blockFrame()
	eng := &fakeCaptureEngine{frames: ch}
	bundle := &fakeBundle{}
	var tty bytes.Buffer

	err := runCapture(eng, "https://example.test", st, &tty, bundle, ModeBlock, 1, 0, true, captureIdle, captureIdle)
	if err != nil {
		t.Fatalf("runCapture err = %v, want nil for a full forced-block capture", err)
	}
	if bundle.finalise != "full" {
		t.Fatalf("bundle finalise = %q, want full", bundle.finalise)
	}
	if len(bundle.frames) != 1 {
		t.Fatalf("bundle wrote %d frames, want 1", len(bundle.frames))
	}
	if bundle.frames[0].Grid == nil {
		t.Fatal("drawn frame Grid is nil; want the block path so terminal-model.json is written, not omitted")
	}
}

// TestRunCaptureForcedSixelRejectedWritesPartial asserts that forcing sixel on a
// block-only detection result writes a partial bundle, prints the bundle path,
// and returns ErrPartialBundle before any Navigate (task 7.2).
func TestRunCaptureForcedSixelRejectedWritesPartial(t *testing.T) {
	eng := &fakeCaptureEngine{frames: make(chan render.Frame)}
	bundle := &fakeBundle{}
	var tty bytes.Buffer

	err := runCapture(eng, "https://example.test", blockState(), &tty, bundle, ModeSixel, 1, 0, true, captureIdle, time.Millisecond)

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

	err := runCapture(eng, "https://example.test", blockState(), &tty, bundle, ModeKitty, 1, 0, true, captureIdle, time.Millisecond)

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
	eng.frames <- blockFrame()
	bundle := &fakeBundle{}
	var tty bytes.Buffer

	err := runCapture(eng, "https://example.test", blockState(), &tty, bundle, ModeBlock, 1, 0, true, captureIdle, captureIdle)
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

		err := runCapture(eng, "https://example.test", blockState(), &tty, &fakeBundle{}, ModeBlock, 1, 0, true, captureIdle, captureIdle)
		if err != nil {
			t.Fatalf("runCapture err = %v, want nil for a full capture", err)
		}
	})

	t.Run("partial returns the sentinel", func(t *testing.T) {
		// A forced sixel on a block-only detection result writes a partial bundle.
		eng := &fakeCaptureEngine{frames: make(chan render.Frame)}
		var tty bytes.Buffer

		err := runCapture(eng, "https://example.test", blockState(), &tty, &fakeBundle{}, ModeSixel, 1, 0, true, captureIdle, time.Millisecond)
		if !errors.Is(err, ErrPartialBundle) {
			t.Fatalf("runCapture err = %v, want ErrPartialBundle for a partial capture", err)
		}
	})
}

// blockFrame builds one drawable frame: a 16x16 RGBA image with no text overlay.
// frame.Img != nil, so the capture loop counts and draws it. The size matches the
// blockState cell px so the block compose has a non-empty grid. The bitmap is
// painted (a non-uniform fill), so it clears the first-frame completeness gate
// that skips blank pre-paint bitmaps.
func blockFrame() render.Frame {
	return render.Frame{Img: paintedImg(16, 16)}
}

// paintedImg builds a w by h RGBA image with a non-uniform, fully opaque fill, so
// frameBlank reads it as painted. The corner is black and every other pixel is
// white, so the subsampled foreground fraction sits far above frameBlankThreshold
// regardless of the sampling stride.
func paintedImg(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	white := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	for y := range h {
		for x := range w {
			img.SetRGBA(x, y, white)
		}
	}
	// A single black corner is the background frameBlank reads; the white field is
	// all foreground against it.
	img.SetRGBA(0, 0, color.RGBA{A: 0xff})
	return img
}

// TestCaptureLoopWritesSessionAtFinalise asserts captureLoop writes the session
// at finalise, after frames have flowed, not right after Navigate. The fake
// session is empty until the first WriteFrame flips it, simulating the engine's
// fillSession running during navigation. So an early write (right after Navigate)
// would capture empty bytes; only a finalise-time write reaches the populated
// bytes. The test asserts the populated bytes reach WriteSession and that the
// write ran before the finalise.
func TestCaptureLoopWritesSessionAtFinalise(t *testing.T) {
	want := []byte(`{"navigation_url":"https://example.test"}`)
	sess := &fakeSession{data: want}

	ch := make(chan render.Frame, 1)
	ch <- blockFrame()
	eng := &fakeCaptureEngine{frames: ch}
	bundle := &fakeBundle{onWriteFrame: func() { sess.filled = true }}
	var tty bytes.Buffer

	err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, sess, 1, 0, true, captureIdle, captureIdle)
	if err != nil {
		t.Fatalf("captureLoop err = %v, want nil for a full capture", err)
	}
	if string(bundle.session) != string(want) {
		t.Fatalf("bundle session = %q, want the populated bytes %q (write must run at finalise, not right after Navigate)", bundle.session, want)
	}
	if !bundle.sessionBefore {
		t.Error("WriteSession ran after finalise, want it before FinaliseFull")
	}
}

// TestCaptureLoopWritesSessionOnPartialExit asserts captureLoop writes the
// session before a partial finalise too. The frames channel closes before n, so
// the loop finalises partial; the session, populated by then, must still reach
// WriteSession before the finalise.
func TestCaptureLoopWritesSessionOnPartialExit(t *testing.T) {
	want := []byte(`{"navigation_result":"ok"}`)
	sess := &fakeSession{data: want, filled: true}

	ch := make(chan render.Frame)
	close(ch)
	eng := &fakeCaptureEngine{frames: ch}
	bundle := &fakeBundle{}
	var tty bytes.Buffer

	err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, sess, 3, 0, true, captureIdle, captureIdle)
	if !errors.Is(err, ErrPartialBundle) {
		t.Fatalf("captureLoop err = %v, want ErrPartialBundle", err)
	}
	if string(bundle.session) != string(want) {
		t.Fatalf("bundle session = %q, want %q written before the partial finalise", bundle.session, want)
	}
	if !bundle.sessionBefore {
		t.Error("WriteSession ran after finalise, want it before FinalisePartial")
	}
}

// TestDrawCaptureFrameCopiesMeta asserts drawCaptureFrame copies frame.Meta into
// the returned FrameInput.Meta, so the screencast metadata reaches the bundle. A
// frame with a non-nil Meta must yield a non-nil FrameInput.Meta carrying the
// same values; a frame with a nil Meta yields a nil FrameInput.Meta.
func TestDrawCaptureFrameCopiesMeta(t *testing.T) {
	st := blockState()
	frame := blockFrame()
	frame.Meta = &render.ScreencastMeta{PageScaleFactor: 2, ScrollOffsetY: 240}
	var buf bytes.Buffer

	in, err := drawCaptureFrame(st, frame, &buf, false)
	if err != nil {
		t.Fatalf("drawCaptureFrame err = %v, want nil", err)
	}
	if in.Meta == nil {
		t.Fatal("FrameInput.Meta is nil, want frame.Meta copied through")
	}
	if in.Meta.ScrollOffsetY != 240 || in.Meta.PageScaleFactor != 2 {
		t.Errorf("FrameInput.Meta = %+v, want scroll 240 scale 2", in.Meta)
	}

	noMeta, err := drawCaptureFrame(blockState(), blockFrame(), &bytes.Buffer{}, false)
	if err != nil {
		t.Fatalf("drawCaptureFrame (no meta) err = %v, want nil", err)
	}
	if noMeta.Meta != nil {
		t.Errorf("FrameInput.Meta = %+v with a nil frame.Meta, want nil", noMeta.Meta)
	}
}

// TestDrawCaptureFrameRecordsSixelPayloadBytes asserts the sixel payload byte
// count reaches FrameInput.Perf.SixelPayloadBytes for the sixel backend, and stays
// zero for the block backend. The count is the renderer's own output, excluding
// the clearScreen prefix, so it is positive but below the total terminal bytes.
// No real terminal: the draw writes to a bytes.Buffer.
func TestDrawCaptureFrameRecordsSixelPayloadBytes(t *testing.T) {
	st := blockState()
	st.SetEnabled([]Backend{BackendSixel, BackendBlock})
	st.Backend = BackendSixel
	var buf bytes.Buffer

	in, err := drawCaptureFrame(st, blockFrame(), &buf, false)
	if err != nil {
		t.Fatalf("drawCaptureFrame err = %v, want nil", err)
	}
	if in.Perf.SixelPayloadBytes <= 0 {
		t.Fatalf("SixelPayloadBytes = %d, want a positive sixel payload size", in.Perf.SixelPayloadBytes)
	}
	if in.Perf.SixelPayloadBytes >= in.Perf.TerminalBytes {
		t.Errorf("SixelPayloadBytes = %d, want below TerminalBytes %d (the clearScreen prefix is excluded)",
			in.Perf.SixelPayloadBytes, in.Perf.TerminalBytes)
	}

	blockIn, err := drawCaptureFrame(blockState(), blockFrame(), &bytes.Buffer{}, false)
	if err != nil {
		t.Fatalf("drawCaptureFrame (block) err = %v, want nil", err)
	}
	if blockIn.Perf.SixelPayloadBytes != 0 {
		t.Errorf("block SixelPayloadBytes = %d, want 0", blockIn.Perf.SixelPayloadBytes)
	}
}

// TestDrawCaptureFrameSplitsEncodeFromWrite asserts the encode phase is timed and
// recorded apart from the terminal write for the graphics backends, and that the
// block backend routes its cost to BlockComposeMS while leaving BackendEncodeMS
// zero. A graphics encode (sixel palette plus bands, kitty zlib plus base64) over
// a non-trivial bitmap takes measurable time, so BackendEncodeMS is positive;
// because the buffered bytes copy to a bytes.Buffer tee, the write stays distinct
// from the encode rather than folding into it.
func TestDrawCaptureFrameSplitsEncodeFromWrite(t *testing.T) {
	// A non-trivial opaque bitmap so each encoder emits a real payload and the
	// encode span is long enough to register above zero.
	const imgW, imgH = 320, 170
	frame := func() render.Frame {
		img := image.NewRGBA(image.Rect(0, 0, imgW, imgH))
		for i := range img.Pix {
			img.Pix[i] = 0xff
		}
		return render.Frame{Img: img}
	}

	for _, b := range []Backend{BackendSixel, BackendKitty} {
		st := NewState(40, 20, 8, 16)
		st.SetEnabled([]Backend{BackendKitty, BackendSixel, BackendBlock})
		st.Backend = b
		var tee bytes.Buffer

		in, err := drawCaptureFrame(&st, frame(), &tee, false)
		if err != nil {
			t.Fatalf("drawCaptureFrame(%v) = %v, want nil", b, err)
		}
		if in.Perf.BackendEncodeMS <= 0 {
			t.Errorf("%v BackendEncodeMS = %v, want > 0; the encode must be timed", b, in.Perf.BackendEncodeMS)
		}
		if in.Perf.BlockComposeMS != 0 {
			t.Errorf("%v BlockComposeMS = %v, want 0 for a graphics backend", b, in.Perf.BlockComposeMS)
		}
		// The write must not absorb the encode: the buffered render bytes copy to
		// the tee separately, so terminal_write_ms reflects only the io.Write.
		if in.Perf.TerminalWriteMS < 0 {
			t.Errorf("%v TerminalWriteMS = %v, want non-negative", b, in.Perf.TerminalWriteMS)
		}
	}

	// The block backend has no image encode, so its cost lands in BlockComposeMS
	// and BackendEncodeMS stays zero.
	st := blockState()
	in, err := drawCaptureFrame(st, frame(), &bytes.Buffer{}, false)
	if err != nil {
		t.Fatalf("drawCaptureFrame(block) = %v, want nil", err)
	}
	if in.Perf.BackendEncodeMS != 0 {
		t.Errorf("block BackendEncodeMS = %v, want 0; block has no image encode", in.Perf.BackendEncodeMS)
	}
	if in.Perf.BlockComposeMS <= 0 {
		t.Errorf("block BlockComposeMS = %v, want > 0; the compose cost must be recorded", in.Perf.BlockComposeMS)
	}
}

// TestCaptureLoopFullCaptureReturnsNil covers exit condition 1: n scripted frames
// deliver, so the loop draws n, finalises a full bundle, and returns nil (task
// 3.4). It buffers all n frames so the loop never waits on the idle timer.
func TestCaptureLoopFullCaptureReturnsNil(t *testing.T) {
	const n = 3
	ch := make(chan render.Frame, n)
	for range n {
		ch <- blockFrame()
	}
	eng := &fakeCaptureEngine{frames: ch}
	bundle := &fakeBundle{}
	var tty bytes.Buffer

	err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, nil, n, 0, true, captureIdle, captureIdle)
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

// TestCaptureLoopScrollHookDrawsScrolledLastFrame asserts the capture scroll hook:
// after the n frames draw, a positive scrollPx injects the scroll through the
// engine and draws one more frame whose screencast offset advanced as the bundle's
// last frame. The fake engine's SendScroll pushes a scrolled frame (non-zero
// ScrollOffsetY), simulating the engine's debounced re-snapshot. The bundle must
// hold n+1 frames, finalise full, and its last frame must carry the scrolled
// offset. A pre-scroll frame (offset zero) delivered after the injection is skipped
// so the last frame is the scrolled one.
func TestCaptureLoopScrollHookDrawsScrolledLastFrame(t *testing.T) {
	const n = 1
	const scrollPx = 600
	ch := make(chan render.Frame, 3)
	ch <- blockFrame() // the one pre-scroll frame counted toward n
	eng := &fakeCaptureEngine{frames: ch}
	eng.onScroll = func(dy int) {
		if dy != scrollPx {
			t.Errorf("SendScroll dy = %d, want %d", dy, scrollPx)
		}
		// A stale pre-scroll frame lands first (offset zero), then the scrolled frame.
		ch <- render.Frame{Img: image.NewRGBA(image.Rect(0, 0, 16, 16)), Meta: &render.ScreencastMeta{ScrollOffsetY: 0}}
		scrolled := render.Frame{Img: image.NewRGBA(image.Rect(0, 0, 16, 16)), Meta: &render.ScreencastMeta{ScrollOffsetY: float64(dy)}}
		ch <- scrolled
	}
	bundle := &fakeBundle{}
	var tty bytes.Buffer

	err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, nil, n, scrollPx, true, captureIdle, captureIdle)
	if err != nil {
		t.Fatalf("captureLoop err = %v, want nil for a full scrolled capture", err)
	}
	if bundle.finalise != "full" {
		t.Fatalf("bundle finalise = %q, want full", bundle.finalise)
	}
	if len(bundle.frames) != n+1 {
		t.Fatalf("bundle wrote %d frames, want %d (n plus the scrolled frame)", len(bundle.frames), n+1)
	}
	last := bundle.frames[len(bundle.frames)-1]
	if last.Meta == nil || last.Meta.ScrollOffsetY != scrollPx {
		t.Fatalf("last frame ScrollOffsetY = %v, want the scrolled %d", last.Meta, scrollPx)
	}
}

// TestCaptureLoopScrollHookWaitsForSettle asserts the settle gate: an advanced-offset
// frame that arrives before the engine's settle snapshot has published is held back
// (it still carries the stale pre-scroll text), and only the advanced-offset frame
// that arrives after the settle poke is drawn as the bundle's last frame. The stale
// frame carries a distinct offset so the test proves the gate skipped it.
func TestCaptureLoopScrollHookWaitsForSettle(t *testing.T) {
	const n = 1
	const scrollPx = 600
	const staleOffset = 300 // an advanced offset that arrives before the settle poke
	settle := make(chan struct{}, 1)
	ch := make(chan render.Frame, 3)
	ch <- blockFrame() // the one pre-scroll frame counted toward n
	eng := &fakeCaptureEngine{frames: ch}
	eng.onScroll = func(dy int) {
		// A stale advanced-offset frame lands first, before any settle poke, so the gate
		// must skip it. Then the settle snapshot publishes and pokes settle, and the
		// settled advanced frame lands.
		ch <- render.Frame{Img: image.NewRGBA(image.Rect(0, 0, 16, 16)), Meta: &render.ScreencastMeta{ScrollOffsetY: staleOffset}}
		go func() {
			settle <- struct{}{}
			ch <- render.Frame{Img: image.NewRGBA(image.Rect(0, 0, 16, 16)), Meta: &render.ScreencastMeta{ScrollOffsetY: float64(dy)}}
		}()
	}
	sess := &fakeSession{scrollSettle: settle}
	bundle := &fakeBundle{}
	var tty bytes.Buffer

	err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, sess, n, scrollPx, true, captureIdle, captureIdle)
	if err != nil {
		t.Fatalf("captureLoop err = %v, want nil for a full settled capture", err)
	}
	if len(bundle.frames) != n+1 {
		t.Fatalf("bundle wrote %d frames, want %d (n plus the settled scrolled frame)", len(bundle.frames), n+1)
	}
	last := bundle.frames[len(bundle.frames)-1]
	if last.Meta == nil || last.Meta.ScrollOffsetY != scrollPx {
		t.Fatalf("last frame ScrollOffsetY = %v, want the settled %d, not the stale %d", last.Meta, scrollPx, staleOffset)
	}
}

// TestCaptureLoopScrollHookSettleOffDrawsFirstAdvanced asserts that with the
// settle wait off (NYALA_CAPTURE_SCROLL_SETTLE=0), the hook draws the first frame
// whose scroll offset advanced, without waiting for the settle poke. The session
// carries a settle channel that is never poked, so a working settle gate would
// hang; settle=false must skip it and draw the advanced frame at once. The
// compose-time scroll compensation is what keeps that pre-settle frame glued.
func TestCaptureLoopScrollHookSettleOffDrawsFirstAdvanced(t *testing.T) {
	const n = 1
	const scrollPx = 600
	settle := make(chan struct{}, 1) // deliberately never poked
	ch := make(chan render.Frame, 3)
	ch <- blockFrame() // the one pre-scroll frame counted toward n
	eng := &fakeCaptureEngine{frames: ch}
	eng.onScroll = func(dy int) {
		// The advanced-offset frame lands with no settle poke. With settle off the hook
		// must draw it rather than hold it back.
		ch <- render.Frame{Img: image.NewRGBA(image.Rect(0, 0, 16, 16)), Meta: &render.ScreencastMeta{ScrollOffsetY: float64(dy)}}
	}
	sess := &fakeSession{scrollSettle: settle}
	bundle := &fakeBundle{}
	var tty bytes.Buffer

	err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, sess, n, scrollPx, false, captureIdle, captureIdle)
	if err != nil {
		t.Fatalf("captureLoop err = %v, want nil for a settle-off scrolled capture", err)
	}
	if len(bundle.frames) != n+1 {
		t.Fatalf("bundle wrote %d frames, want %d (n plus the pre-settle scrolled frame)", len(bundle.frames), n+1)
	}
	last := bundle.frames[len(bundle.frames)-1]
	if last.Meta == nil || last.Meta.ScrollOffsetY != scrollPx {
		t.Fatalf("last frame ScrollOffsetY = %v, want the advanced %d drawn without settle", last.Meta, scrollPx)
	}
}

// TestCaptureLoopScrollHookTimeoutWritesPartial asserts the scroll hook's bound:
// when the scrolled re-snapshot never lands (the injected scroll yields no frame
// with an advanced offset), the idle timer fires and the loop writes a partial
// bundle reasoned for the scroll timeout. A tiny idle runs it instantly.
func TestCaptureLoopScrollHookTimeoutWritesPartial(t *testing.T) {
	const n = 1
	ch := make(chan render.Frame, 1)
	ch <- blockFrame()
	eng := &fakeCaptureEngine{frames: ch}
	// SendScroll delivers nothing, so no scrolled frame ever arrives.
	bundle := &fakeBundle{}
	var tty bytes.Buffer

	err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, nil, n, 600, true, captureIdle, time.Millisecond)
	if !errors.Is(err, ErrPartialBundle) {
		t.Fatalf("captureLoop err = %v, want ErrPartialBundle", err)
	}
	if !strings.Contains(bundle.finalise, "scroll re-snapshot timeout") {
		t.Errorf("partial reason = %q, want it to name the scroll re-snapshot timeout", bundle.finalise)
	}
	if len(bundle.frames) != n {
		t.Errorf("bundle wrote %d frames, want %d (the scrolled frame never landed)", len(bundle.frames), n)
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

	err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, nil, 3, 0, true, captureIdle, time.Millisecond)
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

// TestCaptureLoopDiscardsPrePaintFrames asserts the paint gate: frames delivered
// before the painted latch closes are drained and discarded, never drawn or
// written, and only post-paint frames are counted up to n. A goroutine sends two
// pre-paint blank frames, closes the latch, then sends n post-paint frames. The
// bundle must hold exactly n frames and finalise full. This keeps a blank
// pre-paint frame out of the captured n. It forces block detection via blockState
// and uses no terminal.
func TestCaptureLoopDiscardsPrePaintFrames(t *testing.T) {
	const n = 2
	painted := make(chan struct{})
	sess := &fakeSession{painted: painted}
	ch := make(chan render.Frame)
	eng := &fakeCaptureEngine{frames: ch}
	bundle := &fakeBundle{}
	var tty bytes.Buffer

	go func() {
		// Two pre-paint frames the gate must discard.
		ch <- blockFrame()
		ch <- blockFrame()
		close(painted)
		// n post-paint frames the bounded loop must count.
		for range n {
			ch <- blockFrame()
		}
	}()

	err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, sess, n, 0, true, captureIdle, captureIdle)
	if err != nil {
		t.Fatalf("captureLoop err = %v, want nil for a full post-paint capture", err)
	}
	if bundle.finalise != "full" {
		t.Fatalf("bundle finalise = %q, want full", bundle.finalise)
	}
	if len(bundle.frames) != n {
		t.Fatalf("bundle wrote %d frames, want %d (pre-paint frames must be discarded)", len(bundle.frames), n)
	}
}

// TestCaptureLoopSkipsBlankFirstFrame asserts the completeness gate: a blank
// pre-paint bitmap delivered as the first post-latch frame is skipped, not
// counted, and the first painted frame that follows is drawn as the bundle's
// opening frame. On a large surface Chromium composites progressively, so the
// frame satisfying the paint and publish latches can still be mostly background;
// this keeps that half-composited bitmap out of the counted n. A goroutine sends
// one blank frame then n painted frames; the bundle must hold exactly n and each
// must be the painted bitmap.
func TestCaptureLoopSkipsBlankFirstFrame(t *testing.T) {
	const n = 1
	ch := make(chan render.Frame, 2)
	// A uniform (blank) bitmap the gate must skip, then a painted one it must draw.
	ch <- render.Frame{Img: image.NewRGBA(image.Rect(0, 0, 16, 16))}
	ch <- blockFrame()
	eng := &fakeCaptureEngine{frames: ch}
	bundle := &fakeBundle{}
	var tty bytes.Buffer

	err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, nil, n, 0, true, captureIdle, captureIdle)
	if err != nil {
		t.Fatalf("captureLoop err = %v, want nil for a full capture after the blank skip", err)
	}
	if bundle.finalise != "full" {
		t.Fatalf("bundle finalise = %q, want full", bundle.finalise)
	}
	if len(bundle.frames) != n {
		t.Fatalf("bundle wrote %d frames, want %d (the blank first frame must be skipped)", len(bundle.frames), n)
	}
}

// TestCaptureLoopBlankOnlyStreamTimesOut asserts the completeness gate stays
// bounded: when every delivered frame is blank, the first-draw idle timer fires
// and the loop writes a partial bundle reasoned for the first-draw timeout, with
// no frame counted. A tiny idle runs it instantly. This is the loud timeout the
// gate falls back to when the page never paints.
func TestCaptureLoopBlankOnlyStreamTimesOut(t *testing.T) {
	ch := make(chan render.Frame, 2)
	ch <- render.Frame{Img: image.NewRGBA(image.Rect(0, 0, 16, 16))}
	ch <- render.Frame{Img: image.NewRGBA(image.Rect(0, 0, 16, 16))}
	eng := &fakeCaptureEngine{frames: ch}
	bundle := &fakeBundle{}
	var tty bytes.Buffer

	err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, nil, 1, 0, true, captureIdle, time.Millisecond)
	if !errors.Is(err, ErrPartialBundle) {
		t.Fatalf("captureLoop err = %v, want ErrPartialBundle for a blank-only stream", err)
	}
	if !strings.Contains(bundle.finalise, "first-draw timeout") {
		t.Errorf("partial reason = %q, want the first-draw timeout", bundle.finalise)
	}
	if len(bundle.frames) != 0 {
		t.Errorf("bundle wrote %d frames, want 0 (every frame was blank)", len(bundle.frames))
	}
}

// TestFrameBlank asserts frameBlank separates a no-ink pre-paint bitmap from a
// painted one: a uniform fill (any single colour) reads as blank, a page with
// real content reads as painted, a sparse page carrying about one percent ink
// reads as painted (the fraction the old two-percent gate wrongly starved), and a
// zero-extent image reads as blank. It pins the gate's core predicate independent
// of the loop.
func TestFrameBlank(t *testing.T) {
	if !frameBlank(image.NewRGBA(image.Rect(0, 0, 100, 100))) {
		t.Error("frameBlank(uniform black) = false, want true; a flat field is blank")
	}
	white := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for i := range white.Pix {
		white.Pix[i] = 0xff
	}
	if !frameBlank(white) {
		t.Error("frameBlank(uniform white) = false, want true; a flat field is blank")
	}
	if !frameBlank(image.NewRGBA(image.Rect(0, 0, 0, 0))) {
		t.Error("frameBlank(zero-extent) = false, want true")
	}
	if frameBlank(paintedImg(100, 100)) {
		t.Error("frameBlank(painted) = true, want false; a page with content is not blank")
	}
	// A sparse page: a white field seeded so the frameBlank subsample reads about one
	// percent ink, the fraction a wide-grid RTL or CJK corpus page paints. The old
	// gate read anything below two percent as blank and starved these pages; the new
	// floor near zero must read this as painted.
	sparse := sparseImg(200, 200, 0.004)
	if got := frameInkFraction(sparse); got < frameBlankThreshold || got >= 0.02 {
		t.Fatalf("sparse image ink fraction = %.4f, want it in (%.4f, 0.02) to model the starved page", got, frameBlankThreshold)
	}
	if frameBlank(sparse) {
		t.Error("frameBlank(sparse ~1% ink) = true, want false; a sparse painted page is not blank")
	}
}

// TestFirstFrameUnpainted asserts the first-frame skip predicate is text-aware: a
// blank bitmap counts as unpainted only when no overlay runs ride with it, so a
// painted text-only page (no glyph ink in the bitmap, text carried by the overlay)
// is not skipped, while a true pre-paint frame (no ink, no runs) still is. It pins
// the fix for the block path hiding page text into the overlay.
func TestFirstFrameUnpainted(t *testing.T) {
	blank := image.NewRGBA(image.Rect(0, 0, 100, 100))
	runs := []render.GlyphRun{{Text: "hello"}}
	if !firstFrameUnpainted(blank, nil) {
		t.Error("firstFrameUnpainted(blank, no runs) = false, want true; a true pre-paint frame is skipped")
	}
	if firstFrameUnpainted(blank, runs) {
		t.Error("firstFrameUnpainted(blank, runs) = true, want false; a painted text-only page must not be skipped")
	}
	if firstFrameUnpainted(paintedImg(100, 100), nil) {
		t.Error("firstFrameUnpainted(painted, no runs) = true, want false; a painted bitmap is not unpainted")
	}
}

// sparseImg builds a w by h white RGBA image with the given fraction of pixels
// inked black, so frameBlank sees a low but non-zero ink fraction. It models a
// sparse corpus page: mostly background with a little text. The ink is spread by a
// hash of the coordinates rather than a fixed lattice, so it stays near the target
// fraction under any subsample stride and does not alias against the frameBlank
// stride.
func sparseImg(w, h int, fraction float64) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	white := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	period := int(math.Round(1.0 / fraction))
	if period < 1 {
		period = 1
	}
	for y := range h {
		for x := range w {
			// Two coprime multipliers spread the inked pixels so no stride aligns with
			// the pattern; one in period of them ink black, the rest stay white. The
			// corner stays white so frameBlank reads white as the background and the
			// sparse black pixels as the ink, not the reverse.
			if (x != 0 || y != 0) && (x*7+y*13)%period == 0 {
				img.SetRGBA(x, y, color.RGBA{A: 0xff})
			} else {
				img.SetRGBA(x, y, white)
			}
		}
	}
	return img
}

// TestCaptureLoopPaintTimeoutWritesPartial asserts the paint gate's deadline: the
// painted latch never closes, so the loop writes a partial bundle reasoned "first
// contentful paint timeout" and returns ErrPartialBundle, without counting any
// frame. A tiny paint deadline runs it instantly. The frames channel stays open
// and empty so only the paint deadline can fire.
func TestCaptureLoopPaintTimeoutWritesPartial(t *testing.T) {
	sess := &fakeSession{painted: make(chan struct{})} // never closed
	eng := &fakeCaptureEngine{frames: make(chan render.Frame)}
	bundle := &fakeBundle{}
	var tty bytes.Buffer

	err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, sess, 3, 0, true, time.Millisecond, captureIdle)
	if !errors.Is(err, ErrPartialBundle) {
		t.Fatalf("captureLoop err = %v, want ErrPartialBundle", err)
	}
	if !strings.Contains(bundle.finalise, "first contentful paint timeout") {
		t.Errorf("partial reason = %q, want it to name the first contentful paint timeout", bundle.finalise)
	}
	if len(bundle.frames) != 0 {
		t.Errorf("bundle wrote %d frames, want 0 on a paint timeout", len(bundle.frames))
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

	err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, nil, 3, 0, true, captureIdle, time.Millisecond)
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
		for range n {
			ch <- blockFrame()
			time.Sleep(idle / 10)
		}
	}()

	err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, nil, n, 0, true, captureIdle, idle)
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
	for range k {
		ch <- blockFrame()
	}
	close(ch)
	eng := &fakeCaptureEngine{frames: ch}
	bundle := &fakeBundle{}
	var tty bytes.Buffer

	err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, nil, n, 0, true, captureIdle, captureIdle)
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

	err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, nil, 3, 0, true, captureIdle, captureIdle)
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

	err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, nil, 3, 0, true, captureIdle, time.Millisecond)
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
// first-draw clearScreen, the per-frame cursorHome, and the cell-diff render
// output into both the tty and the raw file, and excludes enterFullScreen and the
// loading cue. The expected bytes are computed by replaying framePrefix and
// RenderStreamFrame through the same cell differ over a fresh identical State, so
// the test asserts byte equality against a derived reference, never a hardcoded
// golden. The second identical frame emits cursorHome and no changed cells, so the
// captured stream matches a live draw after the first-draw prefix (AC 7, AC 12).
func TestCaptureLoopTeeMirrorsClearAndRenderOnly(t *testing.T) {
	const n = 2
	frame := blockFrame()
	ch := make(chan render.Frame, n)
	for range n {
		ch <- frame
	}
	eng := &fakeCaptureEngine{frames: ch}
	bundle := &fakeBundle{}
	var tty bytes.Buffer

	if err := captureLoop(eng, "https://example.test", blockState(), &tty, bundle, nil, n, 0, true, captureIdle, captureIdle); err != nil {
		t.Fatalf("captureLoop err = %v, want nil", err)
	}

	// Build the expected bytes the driver writes by replaying the same draw shape:
	// framePrefix (clearScreen on the first frame, cursorHome after) then the
	// rendered frame, through one shared cell differ. The reference State matches the
	// driver's: same dimensions, sized to the frame image, with a capture grid and
	// the same differ set so the block draw matches the driver's path. This derives
	// the bytes rather than hardcoding a golden.
	ref := blockState()
	ref.ImgW, ref.ImgH = 16, 16
	ref.SetCaptureGrid(&block.Grid{})
	refDiff := &block.GridDiff{}
	ref.SetDiff(refDiff)
	var want bytes.Buffer
	for i := range n {
		if _, err := io.WriteString(&want, framePrefix(i > 0, refDiff)); err != nil {
			t.Fatalf("write prefix: %v", err)
		}
		if err := ref.RenderStreamFrame(frame, &want); err != nil {
			t.Fatalf("reference render: %v", err)
		}
	}

	wantStr := want.String()
	raw := bundle.raw.String()
	if raw != wantStr {
		t.Errorf("raw file bytes do not equal the first-draw clear then cursorHome diff sequence\n got len %d\nwant len %d", len(raw), len(wantStr))
	}

	// The tty carries the same drawn bytes plus the trailing bundle-path line. The
	// drawn prefix must equal the raw file: the tee mirrors the same bytes to both.
	ttyStr := tty.String()
	if !strings.HasPrefix(ttyStr, wantStr) {
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
