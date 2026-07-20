package engine

import (
	"image"
	"reflect"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"

	"github.com/wimpysworld/nyala/internal/render"
)

// TestComposeFrameCopiesMeta asserts composeFrame copies screencast metadata for
// diagnostic consumers that opt in before navigation.
func TestComposeFrameCopiesMeta(t *testing.T) {
	e := &ChromiumEngine{backend: backendBlock, diagnosticFrameMeta: true}
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
	if frame.Diagnostics == nil {
		t.Fatal("frame has no screencast metadata")
	}
	meta := frame.Diagnostics.Meta
	if meta.ScrollOffsetY != 240 || meta.ScrollOffsetX != 33 {
		t.Errorf("frame metadata scroll = (%v,%v), want (33,240)", meta.ScrollOffsetX, meta.ScrollOffsetY)
	}
	if meta.PageScaleFactor != 2 {
		t.Errorf("frame metadata page scale = %v, want 2", meta.PageScaleFactor)
	}
	if meta.DeviceWidth != 1024 || meta.DeviceHeight != 768 {
		t.Errorf("frame metadata device = (%v,%v), want (1024,768)", meta.DeviceWidth, meta.DeviceHeight)
	}
	if meta.OffsetTop != 5 || meta.Timestamp != 1.5 {
		t.Errorf("frame metadata offsetTop=%v timestamp=%v, want 5 and 1.5", meta.OffsetTop, meta.Timestamp)
	}
}

func TestScreencastMetaDoesNotAllocate(t *testing.T) {
	meta := &page.ScreencastFrameMetadata{DeviceWidth: 1024, DeviceHeight: 768}
	if got := testing.AllocsPerRun(1000, func() { _ = screencastMeta(meta) }); got != 0 {
		t.Fatalf("screencastMeta allocations = %v, want 0", got)
	}
}

// TestComposeFrameFoldsPublishedText asserts composeFrame folds the latest runs
// the snapshot goroutine published into Frame.Text, without running any snapshot
// itself. The snapshot is now decoupled onto its own goroutine; composeFrame only
// reads the published set.
func TestComposeFrameFoldsPublishedText(t *testing.T) {
	e := &ChromiumEngine{backend: backendBlock}
	ts := &textSync{}
	want := decodedRuns
	ts.store(want, 0, 0)

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
	// Graphics: runTextSync gates on textOverlayBackend, which must be false, so the
	// snapshot is skipped for sixel and kitty.
	for _, backend := range []backend{backendSixel, backendKitty} {
		ge := &ChromiumEngine{backend: backend}
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

func TestComposeFrameOmitsMetaInNormalFlow(t *testing.T) {
	e := &ChromiumEngine{backend: backendBlock}
	pending := pendingFrame{
		img:  image.NewRGBA(image.Rect(0, 0, 2, 2)),
		meta: &page.ScreencastFrameMetadata{ScrollOffsetY: 240},
	}
	frame := e.composeFrame(pending, nil, nil)
	if frame.Diagnostics != nil {
		t.Errorf("frame.Diagnostics = %+v, want nil", frame.Diagnostics)
	}
	if got := testing.AllocsPerRun(1000, func() {
		_ = e.composeFrame(pending, nil, nil)
	}); got != 0 {
		t.Fatalf("production compose allocations = %v, want 0", got)
	}
}

var benchmarkComposedFrame render.Frame

func BenchmarkComposeFrameProduction(b *testing.B) {
	e := &ChromiumEngine{backend: backendBlock}
	pending := pendingFrame{img: image.NewRGBA(image.Rect(0, 0, 2, 2))}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkComposedFrame = e.composeFrame(pending, nil, nil)
	}
}
