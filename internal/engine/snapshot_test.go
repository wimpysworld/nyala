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

// cannedHiddenDoc builds one DocumentSnapshot holding a single inline text box
// whose layout node carries the full style whitelist (colour, visibility, opacity)
// plus a blended text opacity. It is the visibility-filter counterpart to
// cannedDoc: where cannedDoc gives the box only a colour slot, this one fills the
// styleVisibility and styleOpacity slots too and sets TextColorOpacities[0], so a
// test can drive every drop condition boxHidden reads. The string table holds the
// layout text, the colour, the visibility keyword, and the opacity string, in the
// index order the Styles entry references.
func cannedHiddenDoc(text, colour, visibility, opacity string, textOpacity float64, bounds domsnapshot.Rectangle, utf16Len int64) (*domsnapshot.DocumentSnapshot, []string) {
	strs := []string{text, colour, visibility, opacity}
	doc := &domsnapshot.DocumentSnapshot{
		Layout: &domsnapshot.LayoutTreeSnapshot{
			Text:               []domsnapshot.StringIndex{0},
			Styles:             []domsnapshot.ArrayOfStrings{{1, 2, 3}},
			Bounds:             []domsnapshot.Rectangle{bounds},
			TextColorOpacities: []float64{textOpacity},
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

// cannedVisibleHiddenDoc builds one DocumentSnapshot holding two inline text boxes
// that both carry the full style whitelist: box A visible, box B driven hidden by
// the caller's visibility/opacity/textOpacity. It is the two-box counterpart to
// cannedHiddenDoc, so a test can assert the filter drops only the hidden box and
// leaves the visible one at its own cell. The string table interleaves each box's
// layout text, colour, visibility, and opacity in the index order its Styles entry
// references.
func cannedVisibleHiddenDoc(textA, textB, visibilityB, opacityB string, textOpacityB float64, boundsA, boundsB domsnapshot.Rectangle, utf16LenA, utf16LenB int64) (*domsnapshot.DocumentSnapshot, []string) {
	// Box A is fully visible: solid colour, "visible" keyword, opacity "1".
	// Box B carries the caller's hidden-driving styles.
	strs := []string{textA, "rgb(0, 0, 0)", "visible", "1", textB, "rgb(0, 0, 0)", visibilityB, opacityB}
	doc := &domsnapshot.DocumentSnapshot{
		Layout: &domsnapshot.LayoutTreeSnapshot{
			Text:               []domsnapshot.StringIndex{0, 4},
			Styles:             []domsnapshot.ArrayOfStrings{{1, 2, 3}, {5, 6, 7}},
			Bounds:             []domsnapshot.Rectangle{boundsA, boundsB},
			TextColorOpacities: []float64{1, textOpacityB},
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

// TestDecodeGlyphRunsDropsHiddenBox drives each drop condition in boxHidden on a
// single-box document and asserts the box yields no run: visibility hidden or
// collapse, opacity zero (own computed opacity or the blended text opacity), and a
// subpixel box height below a quarter cell. A wrong threshold or a missed
// condition would let the phantom run through, which the zero-run assertion
// catches. The last case pins the absent-means-visible default: a normal visible
// box at full opacity and full height survives.
func TestDecodeGlyphRunsDropsHiddenBox(t *testing.T) {
	// A box wide and tall enough to survive the clip and the subpixel test, so
	// only the hidden condition under test can drop it. DPR 1, 8x16 cells, so
	// the quarter-cell floor is 4 CSS px of height.
	visibleBounds := domsnapshot.Rectangle{0, 0, 100, 16}

	cases := []struct {
		name        string
		colour      string
		visibility  string
		opacity     string
		textOpacity float64
		bounds      domsnapshot.Rectangle
		wantRuns    int
	}{
		{
			name:   "visibility hidden drops the box",
			colour: "rgb(0, 0, 0)", visibility: "hidden", opacity: "1", textOpacity: 1,
			bounds: visibleBounds, wantRuns: 0,
		},
		{
			name:   "visibility collapse drops the box",
			colour: "rgb(0, 0, 0)", visibility: "collapse", opacity: "1", textOpacity: 1,
			bounds: visibleBounds, wantRuns: 0,
		},
		{
			name:   "own opacity zero drops the box",
			colour: "rgb(0, 0, 0)", visibility: "visible", opacity: "0", textOpacity: 1,
			bounds: visibleBounds, wantRuns: 0,
		},
		{
			name:   "blended text opacity zero drops the box",
			colour: "rgb(0, 0, 0)", visibility: "visible", opacity: "1", textOpacity: 0,
			bounds: visibleBounds, wantRuns: 0,
		},
		{
			// Height 3 CSS px is below the quarter-cell floor of 4, so a
			// zero-height box leaves no legible glyph and drops.
			name:   "subpixel height drops the box",
			colour: "rgb(0, 0, 0)", visibility: "visible", opacity: "1", textOpacity: 1,
			bounds: domsnapshot.Rectangle{0, 0, 100, 3}, wantRuns: 0,
		},
		{
			name:   "visible full-opacity box survives",
			colour: "rgb(0, 0, 0)", visibility: "visible", opacity: "1", textOpacity: 1,
			bounds: visibleBounds, wantRuns: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, strs := cannedHiddenDoc("A", tc.colour, tc.visibility, tc.opacity, tc.textOpacity, tc.bounds, 1)
			runs := decodeGlyphRuns([]*domsnapshot.DocumentSnapshot{doc}, strs, 1, 0, 0, 8, 16, 200)
			if len(runs) != tc.wantRuns {
				t.Fatalf("want %d runs, got %d", tc.wantRuns, len(runs))
			}
		})
	}
}

// TestDecodeGlyphRunsHiddenBoxLeavesVisibleBox checks the filter is per-box: a
// two-box document with one hidden box and one visible box keeps exactly the
// visible run, at its own cell. It drives the hidden box through each condition in
// turn, so a filter that dropped both boxes or the wrong box would fail. The
// visible box sits at (64, 48), so it bins to (8, 3) with DPR 1 and 8x16 cells.
func TestDecodeGlyphRunsHiddenBoxLeavesVisibleBox(t *testing.T) {
	boundsA := domsnapshot.Rectangle{64, 48, 100, 16}
	boundsB := domsnapshot.Rectangle{0, 0, 100, 16}

	cases := []struct {
		name        string
		visibility  string
		opacity     string
		textOpacity float64
	}{
		{"hidden box beside visible box", "hidden", "1", 1},
		{"opacity-zero box beside visible box", "visible", "0", 1},
		{"blended-opacity-zero box beside visible box", "visible", "1", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, strs := cannedVisibleHiddenDoc("A", "B", tc.visibility, tc.opacity, tc.textOpacity, boundsA, boundsB, 1, 1)
			runs := decodeGlyphRuns([]*domsnapshot.DocumentSnapshot{doc}, strs, 1, 0, 0, 8, 16, 200)
			if len(runs) != 1 {
				t.Fatalf("want exactly 1 surviving run, got %d", len(runs))
			}
			if runs[0].Text != "A" {
				t.Fatalf("surviving run text = %q, want %q (the visible box)", runs[0].Text, "A")
			}
			if runs[0].Col != 8 || runs[0].Row != 3 {
				t.Fatalf("surviving run cell = (%d, %d), want (8, 3)", runs[0].Col, runs[0].Row)
			}
		})
	}
}

// TestDecodeGlyphRunsNoStyleSlotsKeepsRuns pins the absent-means-visible default:
// a document built by cannedDoc carries only a colour style slot and a nil
// TextColorOpacities, so the visibility, opacity, and blended-opacity readers all
// fall back to visible. The run must survive; a filter that dropped a box on a
// missing style slot would regress every fixture that omits these styles.
func TestDecodeGlyphRunsNoStyleSlotsKeepsRuns(t *testing.T) {
	bounds := domsnapshot.Rectangle{0, 0, 100, 16}
	doc, strs := cannedDoc("A", "rgb(0, 0, 0)", bounds, 1)
	runs := decodeGlyphRuns([]*domsnapshot.DocumentSnapshot{doc}, strs, 1, 0, 0, 8, 16, 200)
	if len(runs) != 1 {
		t.Fatalf("want 1 run for a box with no visibility/opacity slots, got %d", len(runs))
	}
	if runs[0].Text != "A" {
		t.Fatalf("run text = %q, want %q", runs[0].Text, "A")
	}
}

// TestDecodeGlyphRunsMultiBoxDropsClippedBox checks a two-box document where the
// first box yields a run and the second clips to empty (zero width budget from an
// overflow clip). Exactly one run must survive, at the first box's cell, with the
// first box's text. A regression that emitted a zero-width box or reordered the
// surviving run would fail here.
func TestDecodeGlyphRunsMultiBoxDropsClippedBox(t *testing.T) {
	// Box A: at (64, 48), wide enough for "A". DPR 1, 8x16 cells.
	//   col = round(64/8) = 8, row = round(48/16) = 3.
	// Box B: zero width, so maxCells = round(0/8) = 0, clips to "".
	boundsA := domsnapshot.Rectangle{64, 48, 100, 16}
	boundsB := domsnapshot.Rectangle{0, 0, 0, 16}
	doc, strs := cannedTwoBoxDoc("A", "B", "rgb(0, 0, 0)", "rgb(0, 0, 0)", boundsA, boundsB, 1, 1)

	runs := decodeGlyphRuns([]*domsnapshot.DocumentSnapshot{doc}, strs, 1, 0, 0, 8, 16, 200)
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
// box top-left by DPR, subtract the page scroll, then round by the cell size. The
// bounds are document-absolute and the bitmap is the scrolled view, so the screen
// cell is round((bounds/dpr - scroll) / cell). A wrong DPR divide or the old
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
			//   col = round((200/2 - 10)/8)  = round(90/8)   = round(11.25) = 11
			//   row = round((100/2 - 4)/16)  = round(46/16)  = round(2.875) = 3
			name: "worked example dpr 2 with scroll",
			x:    200, y: 100, dpr: 2, scrollX: 10, scrollY: 4,
			cellW: 8, cellH: 16, wantCol: 11, wantRow: 3,
		},
		{
			// No scroll, DPR 1: the box top-left bins straight to a cell.
			//   col = round(64/8)  = 8
			//   row = round(48/16) = 3
			name: "dpr 1 no scroll",
			x:    64, y: 48, dpr: 1, scrollX: 0, scrollY: 0,
			cellW: 8, cellH: 16, wantCol: 8, wantRow: 3,
		},
		{
			// Scroll subtract crosses a cell boundary the bare bounds would not.
			// Bare 40/2 = 20 rounds to cell round(20/8) = round(2.5) = 3; the
			// scroll subtract lands it a cell lower.
			//   col = round((40/2 - 5)/8) = round(15/8) = round(1.875) = 2
			//   row = round((40/2 - 5)/8) = round(15/8) = round(1.875) = 2
			name: "scroll subtract crosses cell boundary",
			x:    40, y: 40, dpr: 2, scrollX: 5, scrollY: 5,
			cellW: 8, cellH: 8, wantCol: 2, wantRow: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A wide box so the single grapheme is never clipped here; this
			// test is about position, not the clip budget.
			bounds := domsnapshot.Rectangle{tc.x, tc.y, 1000, 16}
			doc, strs := cannedDoc("A", "rgb(0, 0, 0)", bounds, 1)
			runs := decodeGlyphRuns([]*domsnapshot.DocumentSnapshot{doc}, strs, tc.dpr, tc.scrollX, tc.scrollY, tc.cellW, tc.cellH, 200)
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
			runs := decodeGlyphRuns([]*domsnapshot.DocumentSnapshot{doc}, strs, 2, 0, 0, 8, 16, 200)
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

// TestDecodeGlyphRunsGeometry checks the per-box CSS-pixel geometry: X and Y are
// the box top-left with DPR divided and scroll subtracted (on-screen), W and H are
// the box width and height with DPR divided. The capture text-map carries these so
// placement can be prototyped offline. A wrong DPR divide or a dropped scroll
// subtract would move X/Y; the hand-computed values catch it.
func TestDecodeGlyphRunsGeometry(t *testing.T) {
	// Box at (200, 100) device px, size 320x48 device px, DPR 2, scroll (10, 4).
	//   X = 200/2 - 10 = 90
	//   Y = 100/2 - 4  = 46
	//   W = 320/2      = 160
	//   H = 48/2       = 24
	bounds := domsnapshot.Rectangle{200, 100, 320, 48}
	doc, strs := cannedDoc("A", "rgb(0, 0, 0)", bounds, 1)
	runs := decodeGlyphRuns([]*domsnapshot.DocumentSnapshot{doc}, strs, 2, 10, 4, 8, 16, 200)
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
	r := runs[0]
	if r.X != 90 || r.Y != 46 || r.W != 160 || r.H != 24 {
		t.Fatalf("geometry = (X %v, Y %v, W %v, H %v), want (90, 46, 160, 24)", r.X, r.Y, r.W, r.H)
	}
}

// TestMapBoxToCellsWidthBudget checks the box-width-to-cell-count rule directly on
// the pure mapping function, without a document wrapper, so the arithmetic is
// visible. The box width in CSS px divides by cellW and rounds to a cell budget,
// so a box exactly N*cellW wide yields N cells and a box a rounding epsilon short
// of N*cellW still yields N (the guard against a font-metric residue dropping the
// last glyph). The budget also clips at the grid's right edge (cols - col cells).
// DPR 1 and zero scroll throughout, so bounds are CSS px straight through; the
// binning and scroll cases live in TestDecodeGlyphRunsBinning.
func TestMapBoxToCellsWidthBudget(t *testing.T) {
	const cellW, cellH, cols = 8, 16, 120

	cases := []struct {
		name        string
		x, w        float64
		text        string
		wantCol     int
		wantClipped string
	}{
		{
			// Box exactly 5*cellW = 40 px wide, text 5 glyphs: round(40/8) = 5
			// cells, all 5 kept.
			name: "exact N cells keeps all glyphs",
			x:    0, w: 40, text: "abcde",
			wantCol: 0, wantClipped: "abcde",
		},
		{
			// A sub-cell inset below the half-cell mark rounds down to the same
			// column, so a small left inset does not shift the run.
			//   col = round(27/8) = round(3.375) = 3
			name: "sub-cell inset below half rounds down",
			x:    27, w: 40, text: "abcde",
			wantCol: 3, wantClipped: "abcde",
		},
		{
			// A sub-cell inset above the half-cell mark rounds up to the next
			// column, so a larger inset lands the run one cell right. Floor would
			// keep it at column 3.
			//   col = round(29/8) = round(3.625) = 4
			name: "sub-cell inset above half rounds up",
			x:    29, w: 40, text: "abcde",
			wantCol: 4, wantClipped: "abcde",
		},
		{
			// Box 5*cellW - epsilon = 39.999 px, text 5 glyphs: round is the
			// guard, round(39.999/8) = round(4.9998) = 5, so all 5 survive. A
			// floor here would drop "e".
			name: "N cells minus epsilon keeps all glyphs via round",
			x:    0, w: 39.999, text: "abcde",
			wantCol: 0, wantClipped: "abcde",
		},
		{
			// A mid-paragraph fragment the layout soft-wraps carries a trailing
			// space. The box is wide enough for all 6 code units (round(48/8) =
			// 6), but the trailing space is trimmed before the clip, so the real
			// 5-glyph word survives and the phantom space never reaches a cell.
			name: "soft-wrap trailing space trimmed to glyph count",
			x:    0, w: 48, text: "hello ",
			wantCol: 0, wantClipped: "hello",
		},
		{
			// A soft-wrap can leave a trailing newline rather than a space. It
			// too must trim before the clip, or the phantom whitespace claims a
			// cell and fuses the word with the next box's text across the wrap.
			// The 10-glyph word survives at its own glyph count.
			name: "soft-wrap trailing newline trimmed to glyph count",
			x:    0, w: 88, text: "developing\n",
			wantCol: 0, wantClipped: "developing",
		},
		{
			// Box starts at the last two columns and is far wider than the grid.
			// col = round(944/8) = 118, so edge budget = cols - col = 2, and the
			// text clips to two cells even though its box budget is larger.
			name: "box past grid edge clips at cellW*cols",
			x:    944, w: 800, text: "abcdef",
			wantCol: 118, wantClipped: "ab",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bounds := []float64{tc.x, 0, tc.w, 16}
			col, row, clipped := mapBoxToCells(bounds, 1, 0, 0, cellW, cellH, cols, tc.text)
			if col != tc.wantCol {
				t.Fatalf("col = %d, want %d", col, tc.wantCol)
			}
			if row != 0 {
				t.Fatalf("row = %d, want 0", row)
			}
			if clipped != tc.wantClipped {
				t.Fatalf("clipped = %q, want %q", clipped, tc.wantClipped)
			}
		})
	}
}

// TestSnapshotStyleIndexMapping pins the style whitelist order to the index
// constants. The order is load-bearing: the decode reads each style by position,
// so a reorder of snapshotStyles without matching the constants silently misreads
// every computed style. Colour must sit at index 0, visibility at 1, opacity at 2.
func TestSnapshotStyleIndexMapping(t *testing.T) {
	want := []struct {
		idx  int
		name string
	}{
		{styleColour, "color"},
		{styleVisibility, "visibility"},
		{styleOpacity, "opacity"},
	}
	if len(snapshotStyles) != len(want) {
		t.Fatalf("snapshotStyles has %d entries, want %d", len(snapshotStyles), len(want))
	}
	for _, w := range want {
		if w.idx < 0 || w.idx >= len(snapshotStyles) {
			t.Fatalf("index %d for %q out of range", w.idx, w.name)
		}
		if snapshotStyles[w.idx] != w.name {
			t.Fatalf("snapshotStyles[%d] = %q, want %q", w.idx, snapshotStyles[w.idx], w.name)
		}
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
			runs := decodeGlyphRuns([]*domsnapshot.DocumentSnapshot{doc}, strs, 1, 0, 0, 8, 16, 200)
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
			runs := decodeGlyphRuns(tc.docs, strs, tc.dpr, 0, 0, tc.cellW, tc.cellH, 200)
			if len(runs) != 0 {
				t.Fatalf("want no runs for %s, got %d", tc.name, len(runs))
			}
		})
	}
}
