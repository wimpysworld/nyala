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
	ts.store(want, nil, 0, 0)

	frame := e.composeFrame(pendingFrame{img: image.NewRGBA(image.Rect(0, 0, 2, 2))}, ts, nil)
	if len(frame.Text) != len(want) || (len(want) > 0 && frame.Text[0].Text != want[0].Text) {
		t.Errorf("frame.Text = %v, want the published runs %v", frame.Text, want)
	}
}

// TestPublishSnapshotCarriesFixedBoxes asserts the viewport-anchored boxes the walk
// reported travel with the runs the whole way from the snapshot goroutine to the
// compose read. The snapshot returns them on the hold state, so the snapshot function
// type the test hook implements is unchanged, and publishSnapshot pairs them to the
// emitted runs in textSync. A graphics backend takes no overlay, so it publishes no
// boxes either.
func TestPublishSnapshotCarriesFixedBoxes(t *testing.T) {
	boxes := []walkFixedBox{{Left: 0, Top: 0, Right: 320, Bottom: 48, Opaque: true}}
	runs := []render.GlyphRun{{Col: 0, Row: 0, Text: "banner"}}
	snapshot := func(context.Context, overlayState) ([]render.GlyphRun, scrollOffset, int64, overlayState) {
		return runs, scrollOffset{Y: 96}, 3, overlayState{lastText: runs, lastFixed: boxes, lastScroll: scrollOffset{Y: 96}, lastGen: 3}
	}

	e := &ChromiumEngine{backend: backendBlock}
	ts := &textSync{}
	state := e.publishSnapshot(t.Context(), snapshot, ts, overlayState{})
	if !reflect.DeepEqual(state.lastFixed, boxes) {
		t.Errorf("held boxes = %v, want the walked boxes %v", state.lastFixed, boxes)
	}
	gotRuns, gotFixed, gotScrollY, gotGen := ts.latest()
	if len(gotRuns) != 1 || gotScrollY != 96 || gotGen != 3 {
		t.Fatalf("published set = (%d runs, scrollY %v, gen %d), want (1, 96, 3)", len(gotRuns), gotScrollY, gotGen)
	}
	if !reflect.DeepEqual(gotFixed, boxes) {
		t.Errorf("published boxes = %v, want the boxes the runs were walked against %v", gotFixed, boxes)
	}

	// Graphics: no overlay, so the publish drops the runs and the boxes together.
	ge := &ChromiumEngine{backend: backendSixel}
	gts := &textSync{}
	ge.publishSnapshot(t.Context(), snapshot, gts, overlayState{})
	if _, gfixed, _, _ := gts.latest(); gfixed != nil {
		t.Errorf("graphics backend published boxes %v, want none", gfixed)
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
	got := shiftRunsToBitmapScroll(runs, nil, bitmapScrollY, 0, cellCSSH, rows)

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
	got := shiftRunsToBitmapScroll(runs, nil, 100, 100, 33, 40)
	if &got[0] != &runs[0] {
		t.Error("zero delta returned a copy, want the input slice untouched")
	}
}

// TestShiftRunsToBitmapScrollHoldsAnchoredRuns asserts a run the walk tagged
// ViewportAnchored keeps its row, its Y, and its column while every other run shifts,
// and that the tag alone decides it. The cell is 20 CSS pixels and the banner box
// spans Y 0-60, so it owns rows 0 to 2. The bitmap scrolls 40 CSS pixels, exactly two
// cells, past the snapshot in the first case and 40 back up in the second.
//
// The third case is the defect the tag replaced. Geometry cannot stand in for DOM
// descent: a flow run sitting inside a large transparent fixed container is not that
// container's text, and holding it freezes the overlay for the whole scroll.
func TestShiftRunsToBitmapScrollHoldsAnchoredRuns(t *testing.T) {
	const cellCSSH = 20.0
	const rows = 30
	banner := []walkFixedBox{{Left: 0, Top: 0, Right: 400, Bottom: 60, Opaque: true}}

	// Scrolled down two cells: the page moved up by 40, the banner did not.
	down := []render.GlyphRun{
		{Col: 1, Row: 1, Text: "banner", X: 10, Y: 20, W: 100, H: 20, ViewportAnchored: true},
		{Col: 1, Row: 10, Text: "flow", X: 10, Y: 200, W: 100, H: 20},
	}
	got := shiftRunsToBitmapScroll(down, banner, 40, 0, cellCSSH, rows)
	if len(got) != 2 {
		t.Fatalf("scrolled-down runs = %d, want 2 (nothing is off-grid or covered)", len(got))
	}
	if got[0].Col != 1 || got[0].Row != 1 || got[0].Y != 20 {
		t.Errorf("anchored run = (col %d, row %d, Y %v), want (1, 1, 20): a run descending from a fixed element holds its screen position", got[0].Col, got[0].Row, got[0].Y)
	}
	if got[1].Row != 8 || got[1].Y != 160 {
		t.Errorf("flow run = (row %d, Y %v), want (8, 160): 40 CSS pixels is two cells", got[1].Row, got[1].Y)
	}

	// The banner's own text is held before the cover test runs, so its own opaque box
	// never culls it. The bitmap scrolled 40 back up, so a shift would put it at Y 60-80
	// and the flow run moves down two cells.
	up := []render.GlyphRun{
		{Col: 1, Row: 1, Text: "banner", X: 10, Y: 20, W: 100, H: 20, ViewportAnchored: true},
		{Col: 1, Row: 4, Text: "flow", X: 10, Y: 80, W: 100, H: 20},
	}
	got = shiftRunsToBitmapScroll(up, banner, 0, 40, cellCSSH, rows)
	if len(got) != 2 {
		t.Fatalf("scrolled-up runs = %d, want 2", len(got))
	}
	if got[0].Row != 1 || got[0].Y != 20 {
		t.Errorf("anchored run = (row %d, Y %v), want (1, 20): the anchor test runs before the cover test, so a banner's own box never culls its own text", got[0].Row, got[0].Y)
	}
	if got[1].Row != 6 || got[1].Y != 120 {
		t.Errorf("flow run = (row %d, Y %v), want (6, 120): the delta shifted every unanchored run down two cells", got[1].Row, got[1].Y)
	}

	// A full-viewport transparent container, the shape a toast root or a portal takes.
	// Both runs sit wholly inside its rect and neither descends from it, so both shift.
	// The box is non-opaque, so the cover test cannot decide these either.
	container := []walkFixedBox{{Left: 0, Top: 0, Right: 400, Bottom: 600}}
	inside := []render.GlyphRun{
		{Col: 1, Row: 10, Text: "flowalpha", X: 10, Y: 200, W: 100, H: 20},
		{Col: 1, Row: 20, Text: "flowbravo", X: 10, Y: 400, W: 100, H: 20},
	}
	got = shiftRunsToBitmapScroll(inside, container, 40, 0, cellCSSH, rows)
	if len(got) != 2 {
		t.Fatalf("runs inside the container = %d, want 2 (a transparent box culls nothing)", len(got))
	}
	if got[0].Row != 8 || got[1].Row != 18 {
		t.Errorf("rows inside a full-viewport fixed container = [%d %d], want [8 18]: sitting inside a fixed box's rect is not descending from it, and freezing these disables the overlay for the whole scroll", got[0].Row, got[1].Row)
	}
}

// TestShiftRunsToBitmapScrollHoldsAnchoredRunsWithoutBoxes asserts the tag stands on
// its own. The walk skips collecting a box it knows can cover nothing (a negative
// z-index, a degenerate rect) and stops collecting at walkFixedBoxCap, but such an
// element still anchors its own text, so a truncated or skipped box may only weaken
// the cover cull and never let anchored text drift.
func TestShiftRunsToBitmapScrollHoldsAnchoredRunsWithoutBoxes(t *testing.T) {
	const cellCSSH = 20.0
	const rows = 30
	runs := []render.GlyphRun{
		{Col: 1, Row: 1, Text: "uncollected", X: 10, Y: 20, W: 100, H: 20, ViewportAnchored: true},
		{Col: 1, Row: 10, Text: "flow", X: 10, Y: 200, W: 100, H: 20},
	}
	got := shiftRunsToBitmapScroll(runs, nil, 40, 0, cellCSSH, rows)
	if len(got) != 2 {
		t.Fatalf("runs = %d, want 2", len(got))
	}
	if got[0].Row != 1 || got[0].Y != 20 {
		t.Errorf("anchored run with no box list = (row %d, Y %v), want (1, 20): the tag is the page's DOM answer and needs no box to confirm it", got[0].Row, got[0].Y)
	}
	if got[1].Row != 8 {
		t.Errorf("flow run = row %d, want 8", got[1].Row)
	}
}

// TestShiftRunsToBitmapScrollCullsRunsUnderOpaqueBoxes asserts a flow run is dropped
// only when an opaque viewport-anchored box covers all three of the walk's samples on
// its post-shift box. The wide line is 200 CSS pixels at x 100, so its samples land at
// l+inset=101, (l+r)/2=200 and r-inset=299, with inset min(1, 200/4)=1. The narrow line
// is 2 wide at x 100, where the inset is min(1, 2/4)=0.5, so its samples land at 100.5,
// 101 and 101.5. Every case but one shifts the line up four cells of 20, from Y 120 to
// Y 40, so its centre sample sits at Y 50 and only the x arithmetic decides.
func TestShiftRunsToBitmapScrollCullsRunsUnderOpaqueBoxes(t *testing.T) {
	const cellCSSH = 20.0
	const rows = 30
	wide := render.GlyphRun{Col: 5, Row: 6, Text: "flow text", X: 100, Y: 120, W: 200, H: 20}
	narrow := render.GlyphRun{Col: 5, Row: 6, Text: "ab", X: 100, Y: 120, W: 2, H: 20}

	cases := []struct {
		name          string
		run           render.GlyphRun
		boxes         []walkFixedBox
		bitmapScrollY float64
		wantKept      bool
		wantRow       int
		wantY         float64
	}{{
		name:          "opaque box over all three samples drops the line",
		run:           wide,
		boxes:         []walkFixedBox{{Left: 0, Top: 0, Right: 400, Bottom: 60, Opaque: true}},
		bitmapScrollY: 80,
	}, {
		// The same run and the same box as above. At one cell of shift the line lands at
		// Y 100, its centre sample at Y 110, clear of the box's bottom edge at 60. Only
		// the post-shift box can decide this, and it keeps the run.
		name:          "the same box misses the line at a one-cell shift",
		run:           wide,
		boxes:         []walkFixedBox{{Left: 0, Top: 0, Right: 400, Bottom: 60, Opaque: true}},
		bitmapScrollY: 20,
		wantKept:      true,
		wantRow:       5,
		wantY:         100,
	}, {
		name:          "opaque box over the centre sample only keeps the whole line",
		run:           wide,
		boxes:         []walkFixedBox{{Left: 150, Top: 0, Right: 250, Bottom: 60, Opaque: true}},
		bitmapScrollY: 80,
		wantKept:      true,
		wantRow:       2,
		wantY:         40,
	}, {
		name:          "opaque box over the left half misses the right sample at 299",
		run:           wide,
		boxes:         []walkFixedBox{{Left: 0, Top: 0, Right: 250, Bottom: 60, Opaque: true}},
		bitmapScrollY: 80,
		wantKept:      true,
		wantRow:       2,
		wantY:         40,
	}, {
		name:          "opaque box reaching exactly 299 covers the right sample",
		run:           wide,
		boxes:         []walkFixedBox{{Left: 0, Top: 0, Right: 299, Bottom: 60, Opaque: true}},
		bitmapScrollY: 80,
	}, {
		name:          "non-opaque box over the whole line culls nothing",
		run:           wide,
		boxes:         []walkFixedBox{{Left: 0, Top: 0, Right: 400, Bottom: 60}},
		bitmapScrollY: 80,
		wantKept:      true,
		wantRow:       2,
		wantY:         40,
	}, {
		name:          "opaque box over the narrow line's 0.5 insets drops it",
		run:           narrow,
		boxes:         []walkFixedBox{{Left: 100.5, Top: 0, Right: 101.5, Bottom: 60, Opaque: true}},
		bitmapScrollY: 80,
	}, {
		name:          "opaque box missing the narrow line's left sample keeps it",
		run:           narrow,
		boxes:         []walkFixedBox{{Left: 100.6, Top: 0, Right: 101.5, Bottom: 60, Opaque: true}},
		bitmapScrollY: 80,
		wantKept:      true,
		wantRow:       2,
		wantY:         40,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shiftRunsToBitmapScroll([]render.GlyphRun{tc.run}, tc.boxes, tc.bitmapScrollY, 0, cellCSSH, rows)
			if !tc.wantKept {
				if len(got) != 0 {
					t.Fatalf("shifted runs = %d, want 0: every sample of the line falls in an opaque box", len(got))
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("shifted runs = %d, want 1: an uncovered sample keeps the whole line", len(got))
			}
			if got[0].Row != tc.wantRow || got[0].Y != tc.wantY {
				t.Errorf("kept run = (row %d, Y %v), want (%d, %v)", got[0].Row, got[0].Y, tc.wantRow, tc.wantY)
			}
			if got[0].Text != tc.run.Text || got[0].W != tc.run.W {
				t.Errorf("kept run = (%q, W %v), want the line whole (%q, W %v)", got[0].Text, got[0].W, tc.run.Text, tc.run.W)
			}
		})
	}
}

// TestShiftRunsToBitmapScrollBoxListEdgeCases asserts the three cases where the boxes
// must change nothing. A nil and an empty list both reproduce the plain shift, off-grid
// drop included: at a cell of 20 and a delta of 40 the run at row 5 lands at row 3 and
// the run at row 1 lands at row -1 and is dropped. A zero delta still returns the input
// slice with no copy even when boxes are present. And the shared input slice is never
// mutated when a box list decides the outcome.
func TestShiftRunsToBitmapScrollBoxListEdgeCases(t *testing.T) {
	const cellCSSH = 20.0
	const rows = 30
	banner := []walkFixedBox{{Left: 0, Top: 0, Right: 400, Bottom: 60, Opaque: true}}
	runs := []render.GlyphRun{
		{Col: 0, Row: 5, Text: "here", X: 10, Y: 100, W: 100, H: 20},
		{Col: 2, Row: 1, Text: "gone", X: 10, Y: 20, W: 100, H: 20}, // shifts to row -1, dropped
	}

	nilBoxes := shiftRunsToBitmapScroll(runs, nil, 40, 0, cellCSSH, rows)
	emptyBoxes := shiftRunsToBitmapScroll(runs, []walkFixedBox{}, 40, 0, cellCSSH, rows)
	if len(nilBoxes) != 1 || nilBoxes[0].Row != 3 || nilBoxes[0].Y != 60 {
		t.Fatalf("nil box list gave %v, want one run at row 3, Y 60 (the plain shift with the off-grid run dropped)", nilBoxes)
	}
	if !reflect.DeepEqual(nilBoxes, emptyBoxes) {
		t.Errorf("empty box list gave %v, want the nil-list result %v", emptyBoxes, nilBoxes)
	}

	fast := shiftRunsToBitmapScroll(runs, banner, 100, 100, cellCSSH, rows)
	if &fast[0] != &runs[0] {
		t.Error("zero delta with a box list returned a copy, want the input slice untouched")
	}

	// The tagged run is held and the "here" run shifts, so both branches write, and
	// neither may write through to the input.
	mixed := []render.GlyphRun{
		runs[0],
		{Col: 2, Row: 1, Text: "gone", X: 10, Y: 20, W: 100, H: 20, ViewportAnchored: true},
	}
	orig := append([]render.GlyphRun(nil), mixed...)
	if got := shiftRunsToBitmapScroll(mixed, banner, 40, 0, cellCSSH, rows); len(got) != 2 {
		t.Fatalf("shifted runs = %d, want 2 (the anchored run is held, not dropped off-grid)", len(got))
	}
	if !reflect.DeepEqual(mixed, orig) {
		t.Error("shiftRunsToBitmapScroll mutated the shared input slice when boxes were present")
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
