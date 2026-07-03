package engine

import (
	"image"
	"testing"

	"github.com/chromedp/cdproto/page"

	"github.com/wimpysworld/nyala/internal/render"
)

// TestShiftRunsToBitmapScroll covers the compose-time scroll compensation: a
// bitmap scrolled past the runs' snapshot scroll shifts every run down by the
// delta in cells and shifts its CSS box by the delta in pixels. It exercises
// Z == 1 (whole cell), Z == 1.5 (fractional cell), a negative delta, and the
// zero-delta fast path that returns the slice untouched.
func TestShiftRunsToBitmapScroll(t *testing.T) {
	base := []render.GlyphRun{{Col: 3, Row: 2, Text: "hi", X: 30, Y: 40}}
	for _, tc := range []struct {
		name             string
		snapY, bitmapY   float64
		snapX, bitmapX   float64
		cellCSSW         float64
		cellCSSH         float64
		wantRow, wantCol int
		wantX, wantY     float64
	}{
		{
			// Z == 1: cell is 8x16 CSS px. The bitmap scrolled 160 px past the runs, so
			// the runs move up 160/16 = 10 rows (row 2 - 10 = -8) and the box Y drops by
			// 160 (40 - 160 = -120), row and box in step.
			name: "z1 vertical", snapY: 0, bitmapY: 160, cellCSSW: 8, cellCSSH: 16,
			wantRow: -8, wantCol: 3, wantX: 30, wantY: -120,
		},
		{
			// Z == 1.5: the CSS cell height is 16/1.5 ~= 10.667. A 160 px bitmap advance
			// rounds to round(160/10.667) = 15 rows up (row 2 - 15 = -13). The box Y still
			// drops by the full 160 CSS px.
			name: "z1.5 fractional", snapY: 0, bitmapY: 160, cellCSSW: 8.0 / 1.5, cellCSSH: 16.0 / 1.5,
			wantRow: -13, wantCol: 3, wantX: 30, wantY: -120,
		},
		{
			// A negative delta (the bitmap scrolled back above the runs) shifts down.
			name: "negative", snapY: 160, bitmapY: 0, cellCSSW: 8, cellCSSH: 16,
			wantRow: 12, wantCol: 3, wantX: 30, wantY: 200,
		},
		{
			// Horizontal delta shifts columns and box X in step.
			name: "horizontal", snapX: 0, bitmapX: 24, cellCSSW: 8, cellCSSH: 16,
			wantRow: 2, wantCol: 0, wantX: 6, wantY: 40,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runs := cloneRuns(base)
			meta := &page.ScreencastFrameMetadata{ScrollOffsetX: tc.bitmapX, ScrollOffsetY: tc.bitmapY}
			got := shiftRunsToBitmapScroll(runs, meta, tc.snapX, tc.snapY, tc.cellCSSW, tc.cellCSSH)
			if got[0].Row != tc.wantRow || got[0].Col != tc.wantCol {
				t.Errorf("shifted cell = (%d,%d), want (%d,%d)", got[0].Col, got[0].Row, tc.wantCol, tc.wantRow)
			}
			if got[0].X != tc.wantX || got[0].Y != tc.wantY {
				t.Errorf("shifted box = (%v,%v), want (%v,%v)", got[0].X, got[0].Y, tc.wantX, tc.wantY)
			}
		})
	}
}

// TestShiftRunsToBitmapScrollZeroDelta asserts the zero-delta fast path returns
// the stored slice untouched, without copying it. Production frames whose bitmap
// and snapshot scroll agree take this path, so it must add no per-frame copy.
func TestShiftRunsToBitmapScrollZeroDelta(t *testing.T) {
	runs := []render.GlyphRun{{Col: 1, Row: 1, Text: "x", X: 8, Y: 16}}
	meta := &page.ScreencastFrameMetadata{ScrollOffsetX: 240, ScrollOffsetY: 240}
	got := shiftRunsToBitmapScroll(runs, meta, 240, 240, 8, 16)
	if &got[0] != &runs[0] {
		t.Fatal("zero-delta shift copied the slice, want the original returned untouched")
	}
	if got[0].Row != 1 || got[0].Y != 16 {
		t.Errorf("zero-delta run changed to (%d,%v), want (1,16)", got[0].Row, got[0].Y)
	}
}

// TestShiftRunsToBitmapScrollDoesNotMutateStored proves the stored slice is never
// mutated: two composes at different bitmap scrolls from one published snapshot
// must each shift from the original rows, not accumulate. The store is shared
// across composes, so a shift that mutated it would compound the second compose.
func TestShiftRunsToBitmapScrollDoesNotMutateStored(t *testing.T) {
	stored := []render.GlyphRun{{Col: 0, Row: 5, Text: "row", X: 0, Y: 80}}
	meta1 := &page.ScreencastFrameMetadata{ScrollOffsetY: 160} // -10 rows at cellCSSH 16
	meta2 := &page.ScreencastFrameMetadata{ScrollOffsetY: 320} // -20 rows from the ORIGINAL

	first := shiftRunsToBitmapScroll(stored, meta1, 0, 0, 8, 16)
	if first[0].Row != -5 || first[0].Y != -80 {
		t.Fatalf("first compose = (row %d, Y %v), want (-5, -80)", first[0].Row, first[0].Y)
	}
	if stored[0].Row != 5 || stored[0].Y != 80 {
		t.Fatalf("stored run mutated to (row %d, Y %v) after first compose, want (5, 80)", stored[0].Row, stored[0].Y)
	}

	second := shiftRunsToBitmapScroll(stored, meta2, 0, 0, 8, 16)
	if second[0].Row != -15 || second[0].Y != -240 {
		t.Fatalf("second compose = (row %d, Y %v), want (-15, -240) from the original, not accumulated", second[0].Row, second[0].Y)
	}
}

// TestComposeFrameShiftsRunsToBitmapScroll drives composeFrame end to end: a
// published snapshot tagged at scroll 0 folded onto a bitmap scrolled 160 CSS px
// must land the runs shifted down by the bitmap's scroll, gluing the overlay to
// the scrolled page. The engine carries a 120x40 grid so snapshotCellPx resolves a
// real CSS cell size.
func TestComposeFrameShiftsRunsToBitmapScroll(t *testing.T) {
	e := &ChromiumEngine{backend: "block", cols: 120, rows: 40, cellW: 8, cellH: 16, layoutWidth: 1280}
	ts := &textSync{}
	ts.store([]render.GlyphRun{{Col: 0, Row: 0, Text: "top", X: 0, Y: 0}}, 0, 0, 0)

	_, cellCSSH := e.snapshotCellPx()
	meta := &page.ScreencastFrameMetadata{ScrollOffsetY: 160}
	frame := e.composeFrame(pendingFrame{img: image.NewRGBA(image.Rect(0, 0, 8, 16)), meta: meta}, ts)
	if len(frame.Text) != 1 {
		t.Fatalf("frame folded %d runs, want 1", len(frame.Text))
	}
	wantRow := -int(160/cellCSSH + 0.5)
	if frame.Text[0].Row != wantRow {
		t.Errorf("folded run row = %d, want %d (shifted to the bitmap scroll)", frame.Text[0].Row, wantRow)
	}
	if frame.Text[0].Y != -160 {
		t.Errorf("folded run box Y = %v, want -160", frame.Text[0].Y)
	}
}

// cloneRuns returns a fresh copy of runs so a shift test never mutates the shared
// table fixture across subtests.
func cloneRuns(runs []render.GlyphRun) []render.GlyphRun {
	out := make([]render.GlyphRun, len(runs))
	copy(out, runs)
	return out
}
