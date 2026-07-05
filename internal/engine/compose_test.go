package engine

import (
	"context"
	"image"
	"reflect"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"

	"github.com/wimpysworld/nyala/internal/render"
)

// cancelledContext returns a context that is already cancelled, so the snapshot
// inside snapshotRuns fails fast and takes the error-hold branch without
// launching a browser. It avoids passing a nil context, which staticcheck flags.
func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// TestComposeFrameCopiesMetaUnderCapture asserts composeFrame copies the
// pendingFrame screencast metadata onto frame.Meta when capture is on, so the
// metadata reaches the bundle. A nil textSync gives a bitmap-only frame; the meta
// copy is independent of the overlay.
func TestComposeFrameCopiesMetaUnderCapture(t *testing.T) {
	e := &ChromiumEngine{captureOn: true, backend: "block"}
	ts := cdp.TimeSinceEpoch(time.Unix(0, int64(1500*float64(time.Millisecond))))
	pending := pendingFrame{
		img: image.NewRGBA(image.Rect(0, 0, 2, 2)),
		meta: &page.ScreencastFrameMetadata{
			OffsetTop:       5,
			PageScaleFactor: 2,
			DeviceWidth:     1024,
			DeviceHeight:    768,
			ScrollOffsetX:   33,
			ScrollOffsetY:   240,
			Timestamp:       &ts,
		},
	}
	frame := e.composeFrame(pending, nil, nil)
	if frame.Meta == nil {
		t.Fatal("frame.Meta is nil under capture, want the screencast metadata copied")
	}
	if frame.Meta.ScrollOffsetY != 240 || frame.Meta.ScrollOffsetX != 33 {
		t.Errorf("frame.Meta scroll = (%v,%v), want (33,240)", frame.Meta.ScrollOffsetX, frame.Meta.ScrollOffsetY)
	}
	if frame.Meta.PageScaleFactor != 2 {
		t.Errorf("frame.Meta page scale = %v, want 2", frame.Meta.PageScaleFactor)
	}
	if frame.Meta.DeviceWidth != 1024 || frame.Meta.DeviceHeight != 768 {
		t.Errorf("frame.Meta device = (%v,%v), want (1024,768)", frame.Meta.DeviceWidth, frame.Meta.DeviceHeight)
	}
	if frame.Meta.OffsetTop != 5 || frame.Meta.Timestamp != 1.5 {
		t.Errorf("frame.Meta offsetTop=%v timestamp=%v, want 5 and 1.5", frame.Meta.OffsetTop, frame.Meta.Timestamp)
	}
}

// TestComposeFrameFoldsPublishedText asserts composeFrame folds the latest runs
// the snapshot goroutine published into Frame.Text, without running any snapshot
// itself. The snapshot is now decoupled onto its own goroutine; composeFrame only
// reads the published set.
func TestComposeFrameFoldsPublishedText(t *testing.T) {
	e := &ChromiumEngine{backend: "block"}
	ts := &textSync{}
	want := decodedRuns
	ts.store(want, 1.5, 0, 0)

	frame := e.composeFrame(pendingFrame{img: image.NewRGBA(image.Rect(0, 0, 2, 2))}, ts, nil)
	if len(frame.Text) != len(want) || (len(want) > 0 && frame.Text[0].Text != want[0].Text) {
		t.Errorf("frame.Text = %v, want the published runs %v", frame.Text, want)
	}
}

// TestSnapshotRunsRecordsForBlockOnly asserts snapshotRuns records a dom_snapshot
// event when it runs (the block path), and that the graphics backends report
// textOverlayBackend false so runTextSync skips the snapshot for them. The
// graphics backends draw the bitmap alone and ignore the overlay, so the snapshot
// over the full graphics viewport is wasted work that runTextSync skips. A
// cancelled ctx fails the snapshot fast, so no browser is launched.
func TestSnapshotRunsRecordsForBlockOnly(t *testing.T) {
	// Block: snapshotRuns runs and records dom_snapshot even though the cancelled
	// ctx fails it fast.
	rec := &fakeRecorder{}
	e := &ChromiumEngine{captureOn: true, backend: "block", recorder: rec}
	e.snapshotRuns(cancelledContext(), overlayState{})
	if !rec.has(captureEventDOMSnapshot) {
		t.Error("block snapshotRuns did not record dom_snapshot")
	}

	// Graphics: runTextSync gates on textOverlayBackend, which must be false, so the
	// snapshot is skipped for sixel and kitty.
	for _, backend := range []string{"sixel", "kitty"} {
		ge := &ChromiumEngine{captureOn: true, backend: backend}
		if ge.textOverlayBackend() {
			t.Errorf("backend %q reports textOverlayBackend true, want false (snapshot skipped)", backend)
		}
	}
}

// TestShiftRunsToBitmapScrollTracksScroll asserts the compose-time shift moves each
// run onto the bitmap's scroll and drops a run scrolled off the grid. The bitmap
// scrolled two cells (2*cellCSSH CSS pixels) past the snapshot, so every run shifts up
// two rows, and a run at row 1 shifts to row -1 and is dropped. The shared input slice
// is never mutated.
func TestShiftRunsToBitmapScrollTracksScroll(t *testing.T) {
	const cellCSSH = 33.0
	const rows = 40
	runs := []render.GlyphRun{
		{Col: 0, Row: 5, Text: "here", Y: 5 * cellCSSH},
		{Col: 2, Row: 1, Text: "gone", Y: 1 * cellCSSH}, // shifts to row -1, dropped
	}
	orig := append([]render.GlyphRun(nil), runs...)
	bitmapScrollY := 2 * cellCSSH // bitmap scrolled two cells past the snapshot at 0
	got := shiftRunsToBitmapScroll(runs, bitmapScrollY, 0, cellCSSH, rows)

	if len(got) != 1 {
		t.Fatalf("shifted runs = %d, want 1 (the off-grid run dropped)", len(got))
	}
	if got[0].Row != 3 {
		t.Errorf("kept run row = %d, want 3 (5 - 2)", got[0].Row)
	}
	if got[0].Y != 3*cellCSSH {
		t.Errorf("kept run Y = %v, want %v (5c - 2c)", got[0].Y, 3*cellCSSH)
	}
	if got[0].Col != 0 {
		t.Errorf("kept run col = %d, want 0 (columns untouched)", got[0].Col)
	}
	if !reflect.DeepEqual(runs, orig) {
		t.Error("shiftRunsToBitmapScroll mutated the shared input slice")
	}
}

// TestShiftRunsToBitmapScrollZeroDeltaIsFastPath asserts a zero scroll delta returns
// the input slice untouched (no copy), the production fast path.
func TestShiftRunsToBitmapScrollZeroDeltaIsFastPath(t *testing.T) {
	runs := decodedRuns
	got := shiftRunsToBitmapScroll(runs, 100, 100, 33, 40)
	if &got[0] != &runs[0] {
		t.Error("zero delta returned a copy, want the input slice untouched")
	}
}

// TestComposeFrameLeavesMetaNilOffCapture asserts composeFrame leaves frame.Meta
// nil when capture is off, so production stays zero-cost.
func TestComposeFrameLeavesMetaNilOffCapture(t *testing.T) {
	e := &ChromiumEngine{captureOn: false, backend: "block"}
	pending := pendingFrame{
		img:  image.NewRGBA(image.Rect(0, 0, 2, 2)),
		meta: &page.ScreencastFrameMetadata{ScrollOffsetY: 240},
	}
	frame := e.composeFrame(pending, nil, nil)
	if frame.Meta != nil {
		t.Errorf("frame.Meta = %+v off capture, want nil", frame.Meta)
	}
}
