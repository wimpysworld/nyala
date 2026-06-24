package engine

import (
	"image/color"
	"testing"

	"github.com/chromedp/cdproto/domsnapshot"
)

// cannedDoc builds one DocumentSnapshot holding a single inline text box. The
// box owns one layout node, so the string table needs two entries: the layout
// text at index 0 and the computed colour at index 1. The box covers the whole
// layout text (Start 0, Length in UTF-16 code units). bounds is the device-pixel
// rectangle [x, y, w, h] the snapshot reports, before the DPR divide and scroll
// subtract. colour is the serialised CSS colour fed to GlyphRun.FG.
//
// The decode reads bounds, the box Start/Length, the layout Text index, and the
// layout Styles[0] colour index, so this is the minimal shape decodeGlyphRuns
// walks. No browser is involved.
func cannedDoc(text, colour string, bounds domsnapshot.Rectangle, utf16Len int64) (*domsnapshot.DocumentSnapshot, []string) {
	strs := []string{text, colour}
	doc := &domsnapshot.DocumentSnapshot{
		Layout: &domsnapshot.LayoutTreeSnapshot{
			Text:   []domsnapshot.StringIndex{0},
			Styles: []domsnapshot.ArrayOfStrings{{1}},
			Bounds: []domsnapshot.Rectangle{bounds},
		},
		TextBoxes: &domsnapshot.TextBoxSnapshot{
			LayoutIndex: []int64{0},
			Bounds:      []domsnapshot.Rectangle{bounds},
			Start:       []int64{0},
			Length:      []int64{utf16Len},
		},
	}
	return doc, strs
}

// cannedTwoBoxDoc builds one DocumentSnapshot holding two inline text boxes, each
// owning its own layout node, so the string table holds two layout texts and two
// colours. Box order in the snapshot drives run order. boundsA and boundsB are the
// device-pixel rectangles [x, y, w, h]; a zero-width box clips to no cells, which
// is how an overflow-clipped box reaches the decode. This mirrors cannedDoc but
// with two boxes, so a test can assert how many runs survive and in which order.
func cannedTwoBoxDoc(textA, textB, colourA, colourB string, boundsA, boundsB domsnapshot.Rectangle, utf16LenA, utf16LenB int64) (*domsnapshot.DocumentSnapshot, []string) {
	strs := []string{textA, colourA, textB, colourB}
	doc := &domsnapshot.DocumentSnapshot{
		Layout: &domsnapshot.LayoutTreeSnapshot{
			Text:   []domsnapshot.StringIndex{0, 2},
			Styles: []domsnapshot.ArrayOfStrings{{1}, {3}},
			Bounds: []domsnapshot.Rectangle{boundsA, boundsB},
		},
		TextBoxes: &domsnapshot.TextBoxSnapshot{
			LayoutIndex: []int64{0, 1},
			Bounds:      []domsnapshot.Rectangle{boundsA, boundsB},
			Start:       []int64{0, 0},
			Length:      []int64{utf16LenA, utf16LenB},
		},
	}
	return doc, strs
}

// TestDecodeGlyphRunsMultiBoxDropsClippedBox checks a two-box document where the
// first box yields a run and the second clips to empty (zero width budget from an
// overflow clip). Exactly one run must survive, at the first box's cell, with the
// first box's text. A regression that emitted a zero-width box or reordered the
// surviving run would fail here.
func TestDecodeGlyphRunsMultiBoxDropsClippedBox(t *testing.T) {
	// Box A: at (64, 48), wide enough for "A". DPR 1, 8x16 cells.
	//   col = floor(64/8) = 8, row = floor(48/16) = 3.
	// Box B: zero width, so maxCells = floor(0/8) = 0, clips to "".
	boundsA := domsnapshot.Rectangle{64, 48, 100, 16}
	boundsB := domsnapshot.Rectangle{0, 0, 0, 16}
	doc, strs := cannedTwoBoxDoc("A", "B", "rgb(0, 0, 0)", "rgb(0, 0, 0)", boundsA, boundsB, 1, 1)

	runs := decodeGlyphRuns([]*domsnapshot.DocumentSnapshot{doc}, strs, 1, 0, 0, 8, 16)
	if len(runs) != 1 {
		t.Fatalf("want exactly 1 surviving run, got %d", len(runs))
	}
	if runs[0].Text != "A" {
		t.Fatalf("surviving run text = %q, want %q (the un-clipped box)", runs[0].Text, "A")
	}
	if runs[0].Col != 8 || runs[0].Row != 3 {
		t.Fatalf("surviving run cell = (%d, %d), want (8, 3)", runs[0].Col, runs[0].Row)
	}
}

// TestDecodeGlyphRunsBinning checks the device-pixel-to-cell binning: divide the
// box top-left by DPR, subtract the page scroll, then floor by the cell size. The
// bounds are document-absolute and the bitmap is the scrolled view, so the screen
// cell is floor((bounds/dpr - scroll) / cell). A wrong DPR divide or the old
// add-scroll sign lands the box in the wrong cell, off by 2*scroll; the
// hand-computed col/row catch it. These non-zero scroll cases fail against the
// add-scroll bug and pass against the subtract fix.
func TestDecodeGlyphRunsBinning(t *testing.T) {
	cases := []struct {
		name                  string
		x, y                  float64
		dpr, scrollX, scrollY float64
		cellW, cellH          int
		wantCol, wantRow      int
	}{
		{
			// Worked example: (200, 100) device px, DPR 2, scroll (10, 4),
			// 8x16 cells. The bitmap is scrolled, so screen = document - scroll.
			//   col = floor((200/2 - 10)/8)  = floor(90/8)  = 11
			//   row = floor((100/2 - 4)/16)  = floor(46/16) = 2
			name: "worked example dpr 2 with scroll",
			x:    200, y: 100, dpr: 2, scrollX: 10, scrollY: 4,
			cellW: 8, cellH: 16, wantCol: 11, wantRow: 2,
		},
		{
			// No scroll, DPR 1: the box top-left bins straight to a cell.
			//   col = floor(64/8)  = 8
			//   row = floor(48/16) = 3
			name: "dpr 1 no scroll",
			x:    64, y: 48, dpr: 1, scrollX: 0, scrollY: 0,
			cellW: 8, cellH: 16, wantCol: 8, wantRow: 3,
		},
		{
			// Scroll subtract crosses a cell boundary the bare bounds would not.
			//   col = floor((40/2 - 5)/8) = floor(15/8) = 1
			//   row = floor((40/2 - 5)/8) = floor(15/8) = 1
			name: "scroll subtract crosses cell boundary",
			x:    40, y: 40, dpr: 2, scrollX: 5, scrollY: 5,
			cellW: 8, cellH: 8, wantCol: 1, wantRow: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A wide box so the single grapheme is never clipped here; this
			// test is about position, not the clip budget.
			bounds := domsnapshot.Rectangle{tc.x, tc.y, 1000, 16}
			doc, strs := cannedDoc("A", "rgb(0, 0, 0)", bounds, 1)
			runs := decodeGlyphRuns([]*domsnapshot.DocumentSnapshot{doc}, strs, tc.dpr, tc.scrollX, tc.scrollY, tc.cellW, tc.cellH)
			if len(runs) != 1 {
				t.Fatalf("want 1 run, got %d", len(runs))
			}
			if runs[0].Col != tc.wantCol || runs[0].Row != tc.wantRow {
				t.Fatalf("cell = (%d, %d), want (%d, %d)", runs[0].Col, runs[0].Row, tc.wantCol, tc.wantRow)
			}
		})
	}
}

// TestDecodeGlyphRunsClip checks the box-width clip: a run is trimmed to
// floor(boxWidthPx/dpr/cellW) display cells, counting each grapheme with the
// Stage 2 width rule. The width-2 boundary cases are the point: a width-2
// cluster that would spill past the budget is dropped whole, never half-emitted,
// because a half-emitted wide grapheme would misalign every following cell.
func TestDecodeGlyphRunsClip(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		utf16Len int64
		boxWidth float64 // device px; with DPR 2 and cellW 8, budget = floor(boxWidth/2/8)
		want     string
	}{
		{
			// Budget 4 cells, text 6 cells: clip to the first 4.
			name: "narrow run clipped to budget",
			text: "abcdef", utf16Len: 6,
			boxWidth: 64, // floor(64/2/8) = 4
			want:     "abcd",
		},
		{
			// Budget 3 cells. "A世B" is widths 1+2+1 = 4 cells. The width-2
			// "世" fits (1+2 = 3), but the trailing "B" would make 4, so it
			// drops. Result keeps "A世".
			name: "wide cluster fits exactly at budget",
			text: "A世B", utf16Len: 3,
			boxWidth: 48, // floor(48/2/8) = 3
			want:     "A世",
		},
		{
			// Budget 2 cells. "A世" is 1+2 = 3 cells. "A" fits (1), but "世"
			// would make 3, past the budget, so the width-2 cluster drops
			// whole. The run is "A", never "A" plus half of "世".
			name: "wide cluster at boundary dropped whole",
			text: "A世", utf16Len: 3,
			boxWidth: 32, // floor(32/2/8) = 2
			want:     "A",
		},
		{
			// Budget 1 cell. A lone width-2 emoji cannot fit, so the run is
			// empty rather than a half cluster. The emoji is a surrogate pair,
			// so its UTF-16 length is 2.
			name: "lone wide emoji dropped at one-cell budget",
			text: "\U0001F600", utf16Len: 2, // grinning face emoji
			boxWidth: 16, // floor(16/2/8) = 1
			want:     "",
		},
		{
			// Budget 1 cell, width-2 emoji preceded by a width-1 glyph. The
			// "X" fills the single cell; the emoji drops whole.
			name: "wide emoji after narrow glyph at one-cell budget",
			text: "X\U0001F600", utf16Len: 3,
			boxWidth: 16, // floor(16/2/8) = 1
			want:     "X",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bounds := domsnapshot.Rectangle{0, 0, tc.boxWidth, 16}
			doc, strs := cannedDoc(tc.text, "rgb(0, 0, 0)", bounds, tc.utf16Len)
			runs := decodeGlyphRuns([]*domsnapshot.DocumentSnapshot{doc}, strs, 2, 0, 0, 8, 16)
			if tc.want == "" {
				if len(runs) != 0 {
					t.Fatalf("want no run for empty clip, got %d: %q", len(runs), runs[0].Text)
				}
				return
			}
			if len(runs) != 1 {
				t.Fatalf("want 1 run, got %d", len(runs))
			}
			if runs[0].Text != tc.want {
				t.Fatalf("clipped text = %q, want %q", runs[0].Text, tc.want)
			}
		})
	}
}

// TestDecodeGlyphRunsColour checks that the computed color maps to GlyphRun.FG
// in RGBA channel order. A channel swap (the RGBA-vs-BGRA gotcha) would put the
// red value in B, which this catches by asserting each channel separately.
func TestDecodeGlyphRunsColour(t *testing.T) {
	cases := []struct {
		name   string
		colour string
		want   color.RGBA
	}{
		{"opaque rgb", "rgb(10, 20, 30)", color.RGBA{R: 10, G: 20, B: 30, A: 0xff}},
		{"rgba with alpha", "rgba(200, 100, 50, 0.5)", color.RGBA{R: 200, G: 100, B: 50, A: 128}},
		{"unparseable falls back to opaque black", "currentcolor", color.RGBA{A: 0xff}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bounds := domsnapshot.Rectangle{0, 0, 100, 16}
			doc, strs := cannedDoc("A", tc.colour, bounds, 1)
			runs := decodeGlyphRuns([]*domsnapshot.DocumentSnapshot{doc}, strs, 1, 0, 0, 8, 16)
			if len(runs) != 1 {
				t.Fatalf("want 1 run, got %d", len(runs))
			}
			if runs[0].FG != tc.want {
				t.Fatalf("FG = %+v, want %+v", runs[0].FG, tc.want)
			}
		})
	}
}

// TestDecodeGlyphRunsGuards checks the degenerate inputs return no runs rather
// than panicking: a non-positive cell size or DPR, and a nil document. These are
// the boundaries decodeGlyphRuns guards before any division.
func TestDecodeGlyphRunsGuards(t *testing.T) {
	bounds := domsnapshot.Rectangle{0, 0, 100, 16}
	doc, strs := cannedDoc("A", "rgb(0, 0, 0)", bounds, 1)

	cases := []struct {
		name         string
		docs         []*domsnapshot.DocumentSnapshot
		dpr          float64
		cellW, cellH int
	}{
		{"zero cellW", []*domsnapshot.DocumentSnapshot{doc}, 1, 0, 16},
		{"zero cellH", []*domsnapshot.DocumentSnapshot{doc}, 1, 8, 0},
		{"zero dpr", []*domsnapshot.DocumentSnapshot{doc}, 0, 8, 16},
		{"nil document", []*domsnapshot.DocumentSnapshot{nil}, 1, 8, 16},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runs := decodeGlyphRuns(tc.docs, strs, tc.dpr, 0, 0, tc.cellW, tc.cellH)
			if len(runs) != 0 {
				t.Fatalf("want no runs for %s, got %d", tc.name, len(runs))
			}
		})
	}
}
