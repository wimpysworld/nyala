package engine

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
)

// TestInitialScreencastFormatBackendMatrix pins the backend-format matrix: capture
// forces PNG on every backend, live block runs JPEG, and live graphics start in PNG
// (settled). It is a pure read of the backend and capture flag, no goroutine.
func TestInitialScreencastFormatBackendMatrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		backend    string
		capture    bool
		wantFormat page.ScreencastFormat
		wantQual   int
	}{
		{"block", true, page.ScreencastFormatPng, 0},                         // block, capture: PNG
		{"block", false, page.ScreencastFormatJpeg, movingScreencastQuality}, // block, live: JPEG
		{"kitty", true, page.ScreencastFormatPng, 0},                         // graphics, capture: PNG
		{"kitty", false, page.ScreencastFormatPng, 0},                        // graphics, live: PNG (settled start)
		{"sixel", false, page.ScreencastFormatPng, 0},                        // graphics, live: PNG (settled start)
	}
	for _, c := range cases {
		e := &ChromiumEngine{backend: c.backend, captureOn: c.capture}
		got := e.initialScreencastFormat()
		if got.format != c.wantFormat {
			t.Errorf("backend=%s capture=%v: format = %q, want %q", c.backend, c.capture, got.format, c.wantFormat)
		}
		if got.quality != c.wantQual {
			t.Errorf("backend=%s capture=%v: quality = %d, want %d", c.backend, c.capture, got.quality, c.wantQual)
		}
	}
}

// fmtRecorder records the formats the controller switches to, and updates the engine's
// current format so currentScreencastPNG tracks the switch, mimicking the real
// switchScreencastFormat without a browser.
type fmtRecorder struct {
	mu       sync.Mutex
	switches []page.ScreencastFormat
}

func (r *fmtRecorder) list() []page.ScreencastFormat {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]page.ScreencastFormat(nil), r.switches...)
}

// newFmtTestEngine builds a live-or-capture engine on the given backend with a
// recording switch hook and the current format seeded to PNG, so a test can drive
// runScreencastFormat and assert the switches without a browser.
func newFmtTestEngine(backend string, capture bool) (*ChromiumEngine, *fmtRecorder) {
	e := &ChromiumEngine{backend: backend, captureOn: capture}
	e.screencastFmt = screencastFmt{format: page.ScreencastFormatPng}
	rec := &fmtRecorder{}
	e.switchFmt = func(_ context.Context, format page.ScreencastFormat, quality int) {
		rec.mu.Lock()
		rec.switches = append(rec.switches, format)
		rec.mu.Unlock()
		e.mu.Lock()
		e.screencastFmt = screencastFmt{format: format, quality: quality}
		e.mu.Unlock()
	}
	return e, rec
}

// waitForFormat polls the recorder until it saw a switch to want, or fails on the
// deadline, so a switch assertion does not race the controller goroutine.
func waitForFormat(t *testing.T, rec *fmtRecorder, want page.ScreencastFormat) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if slices.Contains(rec.list(), want) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("controller never switched to %q (saw %v)", want, rec.list())
}

// TestScreencastFormatControllerMotionToJPEG proves a graphics-backend live stream
// flips PNG->JPEG once sustained motion crosses the onset: motionOnsetCount short-gap
// frames within the window. Timestamps are injected, so the onset needs no real wait.
func TestScreencastFormatControllerMotionToJPEG(t *testing.T) {
	e, rec := newFmtTestEngine("kitty", false)
	ctx := t.Context()
	motion := make(chan time.Time, 8)
	go e.runScreencastFormat(ctx, motion)

	// Three frames 50ms apart: two short gaps, at or above motionOnsetCount.
	base := time.Unix(100, 0)
	motion <- base
	motion <- base.Add(50 * time.Millisecond)
	motion <- base.Add(100 * time.Millisecond)

	waitForFormat(t, rec, page.ScreencastFormatJpeg)
}

// TestScreencastFormatControllerSettlesToPNG proves that after motion flips the stream
// to JPEG, an idle gap flips it back to PNG (the settled, lossless resting frame). It
// shrinks the settle idle so the trailing timer fires quickly.
func TestScreencastFormatControllerSettlesToPNG(t *testing.T) {
	old := screencastSettleIdle
	screencastSettleIdle = 20 * time.Millisecond
	defer func() { screencastSettleIdle = old }()

	e, rec := newFmtTestEngine("kitty", false)
	ctx := t.Context()
	motion := make(chan time.Time, 8)
	go e.runScreencastFormat(ctx, motion)

	base := time.Unix(200, 0)
	motion <- base
	motion <- base.Add(50 * time.Millisecond)
	motion <- base.Add(100 * time.Millisecond)
	waitForFormat(t, rec, page.ScreencastFormatJpeg)

	// No further frames: the settle timer fires and flips back to PNG.
	waitForFormat(t, rec, page.ScreencastFormatPng)
}

// TestScreencastFormatControllerBlockLiveNoSwitch proves the block backend runs no
// state machine: even under a rapid motion burst it never switches (its whole live
// stream is a fixed JPEG chosen at start).
func TestScreencastFormatControllerBlockLiveNoSwitch(t *testing.T) {
	e, rec := newFmtTestEngine("block", false)
	ctx := t.Context()
	motion := make(chan time.Time, 8)
	go e.runScreencastFormat(ctx, motion)
	base := time.Unix(400, 0)
	for i := range 6 {
		motion <- base.Add(time.Duration(i) * 30 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := rec.list(); len(got) != 0 {
		t.Fatalf("block live switched formats %v, want none (fixed JPEG, no state machine)", got)
	}
}

// TestScreencastFormatControllerCaptureNoSwitch proves the capture path never switches
// on any backend, so the corpus stays deterministic PNG.
func TestScreencastFormatControllerCaptureNoSwitch(t *testing.T) {
	e, rec := newFmtTestEngine("kitty", true)
	ctx := t.Context()
	motion := make(chan time.Time, 8)
	go e.runScreencastFormat(ctx, motion)
	base := time.Unix(500, 0)
	for i := range 6 {
		motion <- base.Add(time.Duration(i) * 30 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := rec.list(); len(got) != 0 {
		t.Fatalf("capture switched formats %v, want none (deterministic PNG)", got)
	}
}

// TestScreencastFormatControllerNoFlipOnSparseFrames proves a graphics-backend stream
// does not flip to JPEG on sparse frames (a one-off repaint): with gaps wider than
// motionShortGap no short gap accrues, so it stays PNG (the hysteresis onset guard).
func TestScreencastFormatControllerNoFlipOnSparseFrames(t *testing.T) {
	e, rec := newFmtTestEngine("kitty", false)
	ctx := t.Context()
	motion := make(chan time.Time, 8)
	go e.runScreencastFormat(ctx, motion)
	// Frames 300ms apart: each gap exceeds motionShortGap, so none counts as motion.
	base := time.Unix(600, 0)
	for i := range 5 {
		motion <- base.Add(time.Duration(i) * 300 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := rec.list(); len(got) != 0 {
		t.Fatalf("sparse frames switched formats %v, want none (no motion onset)", got)
	}
}
