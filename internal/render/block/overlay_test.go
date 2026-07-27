package block

import (
	"image"
	"image/color"
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
)

// TestCellZeroValueIsBitmap asserts the zero Cell is a bitmap cell: empty Text
// and not Wide. A non-empty Text is the text-cell signal.
func TestCellZeroValueIsBitmap(t *testing.T) {
	var c Cell
	if c.Text != "" {
		t.Fatalf("zero Cell Text = %q, want empty", c.Text)
	}
	if c.Wide {
		t.Fatal("zero Cell Wide = true, want false")
	}
	c.Text = "x"
	if c.Text == "" {
		t.Fatal("a cell with Text set must read as a text cell")
	}
}

// overlaySrc builds a cols*cellPxW x rows*cellPxH image filled with one colour,
// the quadrant source footprint for a cols x rows grid.
func overlaySrc(cols, rows int, c color.RGBA) *image.RGBA {
	return fillRGBA(cols*cellPxW, rows*cellPxH, c)
}

// TestComposePlacesRunAtCell composes a 2x2 quadrant grid with a one-grapheme run
// at (1,0) and asserts the target cell carries the grapheme, the run FG, and a BG
// equal to the whole-cell source average.
func TestComposePlacesRunAtCell(t *testing.T) {
	red := color.RGBA{R: 200, G: 10, B: 10, A: 0xff}
	img := overlaySrc(2, 2, red)
	fg := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	br := &Renderer{Cols: 2, Rows: 2}
	f := render.Frame{Img: img, Text: []render.GlyphRun{{Col: 1, Row: 0, Text: "X", FG: fg}}}
	g := br.Compose(f)
	cell := g.At(1, 0)
	if cell.Text != "X" {
		t.Fatalf("Text = %q, want X", cell.Text)
	}
	if cell.FG != fg {
		t.Fatalf("FG = %v, want %v", cell.FG, fg)
	}
	// A uniform source averages to its own colour.
	if cell.BG != red {
		t.Fatalf("BG = %v, want whole-cell average %v", cell.BG, red)
	}
	// An untouched cell keeps no overlay text.
	if g.At(0, 0).Text != "" {
		t.Fatalf("untouched cell carries Text %q", g.At(0, 0).Text)
	}
}

// TestComposeGraphemeWins asserts the overlay sets Text while leaving the bitmap
// Glyph field intact, so the consumers decide by preferring Text.
func TestComposeGraphemeWins(t *testing.T) {
	img := overlaySrc(2, 1, color.RGBA{R: 50, G: 60, B: 70, A: 0xff})
	br := &Renderer{Cols: 2, Rows: 1}
	f := render.Frame{Img: img, Text: []render.GlyphRun{{Col: 0, Row: 0, Text: "Z", FG: color.RGBA{A: 0xff}}}}
	g := br.Compose(f)
	cell := g.At(0, 0)
	if cell.Text != "Z" {
		t.Fatalf("Text = %q, want Z", cell.Text)
	}
	if cell.Glyph == 0 {
		t.Fatal("bitmap Glyph cleared; overlay must overwrite only Text/FG/BG")
	}
}

// TestComposeWideClaimsTrailingCell places a width-2 grapheme and asserts the next
// cell is a Wide trailing cell with empty Text and the same BG.
func TestComposeWideClaimsTrailingCell(t *testing.T) {
	src := color.RGBA{R: 30, G: 40, B: 50, A: 0xff}
	img := overlaySrc(3, 1, src)
	br := &Renderer{Cols: 3, Rows: 1}
	f := render.Frame{Img: img, Text: []render.GlyphRun{{Col: 0, Row: 0, Text: "世", FG: color.RGBA{A: 0xff}}}}
	g := br.Compose(f)
	lead := g.At(0, 0)
	trail := g.At(1, 0)
	if lead.Text != "世" {
		t.Fatalf("lead Text = %q, want 世", lead.Text)
	}
	if !trail.Wide {
		t.Fatal("trailing cell Wide = false, want true")
	}
	if trail.Text != "" {
		t.Fatalf("trailing Text = %q, want empty", trail.Text)
	}
	if trail.BG != lead.BG {
		t.Fatalf("trailing BG = %v, want lead BG %v", trail.BG, lead.BG)
	}
}

// TestComposeFirstWriterWinsNarrow places two width-1 runs at the same cell. The
// first run's grapheme must survive; the second is skipped, so there is no weld
// of the later run onto the claimed cell.
func TestComposeFirstWriterWinsNarrow(t *testing.T) {
	src := color.RGBA{R: 30, G: 40, B: 50, A: 0xff}
	img := overlaySrc(2, 1, src)
	br := &Renderer{Cols: 2, Rows: 1}
	f := render.Frame{Img: img, Text: []render.GlyphRun{
		{Col: 0, Row: 0, Text: "A", FG: color.RGBA{R: 0xff, A: 0xff}},
		{Col: 0, Row: 0, Text: "B", FG: color.RGBA{B: 0xff, A: 0xff}},
	}}
	g := br.Compose(f)
	cell := g.At(0, 0)
	if cell.Text != "A" {
		t.Fatalf("cell Text = %q, want A (first writer wins)", cell.Text)
	}
	if cell.FG != (color.RGBA{R: 0xff, A: 0xff}) {
		t.Fatalf("cell FG = %v, want the first run's FG (no weld from the second)", cell.FG)
	}
}

// TestComposeFrontMostRankWinsNarrow is the ranked pair of
// TestComposeFirstWriterWinsNarrow. Two width-1 runs contest one cell, but here
// the run later in the slice carries the lower (front-most) StackRank. The fold
// reads runs front-to-back, so that later run must win the cell and bring its own
// FG: stacking rank decides a contested cell, not DOM order.
func TestComposeFrontMostRankWinsNarrow(t *testing.T) {
	src := color.RGBA{R: 30, G: 40, B: 50, A: 0xff}
	img := overlaySrc(2, 1, src)
	front := color.RGBA{B: 0xff, A: 0xff}
	br := &Renderer{Cols: 2, Rows: 1}
	f := render.Frame{Img: img, Text: []render.GlyphRun{
		{Col: 0, Row: 0, Text: "A", FG: color.RGBA{R: 0xff, A: 0xff}, StackRank: 1},
		{Col: 0, Row: 0, Text: "B", FG: front, StackRank: 0},
	}}
	g := br.Compose(f)
	cell := g.At(0, 0)
	if cell.Text != "B" {
		t.Fatalf("cell Text = %q, want B (the front-most rank wins, not DOM order)", cell.Text)
	}
	if cell.FG != front {
		t.Fatalf("cell FG = %v, want the front-most run's FG %v", cell.FG, front)
	}
}

// TestComposeEqualRanksKeepDOMOrder pins the sort as stable: two runs contest one
// cell with the same rank, so DOM order still decides and the earlier run wins. A
// third run with a higher rank sits first in the slice, which puts the frame out
// of rank order and forces the copy-and-sort path; equal ranks alone are already
// non-decreasing and would take the no-copy fast path instead.
func TestComposeEqualRanksKeepDOMOrder(t *testing.T) {
	src := color.RGBA{R: 30, G: 40, B: 50, A: 0xff}
	img := overlaySrc(2, 1, src)
	first := color.RGBA{R: 0xff, A: 0xff}
	br := &Renderer{Cols: 2, Rows: 1}
	f := render.Frame{Img: img, Text: []render.GlyphRun{
		// Behind both contenders, and out of rank order, so the sort really runs.
		{Col: 1, Row: 0, Text: "Z", FG: color.RGBA{G: 0xff, A: 0xff}, StackRank: 9},
		{Col: 0, Row: 0, Text: "A", FG: first, StackRank: 3},
		{Col: 0, Row: 0, Text: "B", FG: color.RGBA{B: 0xff, A: 0xff}, StackRank: 3},
	}}
	g := br.Compose(f)
	cell := g.At(0, 0)
	if cell.Text != "A" {
		t.Fatalf("cell Text = %q, want A (equal ranks keep DOM order; the sort must be stable)", cell.Text)
	}
	if cell.FG != first {
		t.Fatalf("cell FG = %v, want the earlier equal-rank run's FG %v", cell.FG, first)
	}
	if z := g.At(1, 0).Text; z != "Z" {
		t.Fatalf("col 1 Text = %q, want Z (the behind run still places where nothing contests it)", z)
	}
}

// TestComposeFrontMostWideKeepsTrailingCell pins the wide claim against a behind
// run at the trailing column. The width-2 run is front-most but sits second in the
// slice, so only the front-to-back order can let it keep both its lead cell and
// its Wide trailing cell; the behind run must not land in the trailing column.
func TestComposeFrontMostWideKeepsTrailingCell(t *testing.T) {
	src := color.RGBA{R: 30, G: 40, B: 50, A: 0xff}
	img := overlaySrc(3, 1, src)
	br := &Renderer{Cols: 3, Rows: 1}
	f := render.Frame{Img: img, Text: []render.GlyphRun{
		{Col: 1, Row: 0, Text: "A", FG: color.RGBA{R: 0xff, A: 0xff}, StackRank: 1},
		{Col: 0, Row: 0, Text: "世", FG: color.RGBA{A: 0xff}, StackRank: 0},
	}}
	g := br.Compose(f)
	lead := g.At(0, 0)
	if lead.Text != "世" {
		t.Fatalf("lead Text = %q, want 世 (the front-most wide run claims its lead)", lead.Text)
	}
	trail := g.At(1, 0)
	if !trail.Wide {
		t.Fatal("trailing cell Wide = false, want true (the front-most wide claim holds)")
	}
	if trail.Text != "" {
		t.Fatalf("trailing Text = %q, want empty (the behind run must not land in the trailing column)", trail.Text)
	}
}

// TestComposeBehindWideSkipsWholeAtClaimedTrailing pins the other half of the
// wide rule: a front-most narrow run claims the cell a behind width-2 run needs as
// its trailing cell. The wide run sits first in the slice, so only the front-to-back
// order claims the trailing cell first. The wide run must then be skipped whole
// rather than half-written: its lead column carries no overlay text at all.
func TestComposeBehindWideSkipsWholeAtClaimedTrailing(t *testing.T) {
	src := color.RGBA{R: 30, G: 40, B: 50, A: 0xff}
	img := overlaySrc(3, 1, src)
	br := &Renderer{Cols: 3, Rows: 1}
	f := render.Frame{Img: img, Text: []render.GlyphRun{
		{Col: 0, Row: 0, Text: "世", FG: color.RGBA{A: 0xff}, StackRank: 1},
		{Col: 1, Row: 0, Text: "A", FG: color.RGBA{R: 0xff, A: 0xff}, StackRank: 0},
	}}
	g := br.Compose(f)
	lead := g.At(0, 0)
	if lead.Text != "" {
		t.Fatalf("lead Text = %q, want empty (the behind wide run is skipped whole)", lead.Text)
	}
	if lead.Wide {
		t.Fatal("lead cell Wide = true, want false (no half-written wide glyph)")
	}
	trail := g.At(1, 0)
	if trail.Text != "A" {
		t.Fatalf("col 1 Text = %q, want A (the front-most narrow run claims the cell)", trail.Text)
	}
	if trail.Wide {
		t.Fatal("col 1 Wide = true, want false (it is the narrow run's own cell)")
	}
}

// TestComposeDoesNotReorderCallerRuns pins the sort as non-destructive. The frame's
// runs are in descending rank order, so the fold must reorder them to compose the
// front-most run onto the contested cell; the caller's own slice must still read in
// its original order afterwards. The engine reuses that slice across frames, so a
// sort in place would corrupt every later frame.
func TestComposeDoesNotReorderCallerRuns(t *testing.T) {
	src := color.RGBA{R: 30, G: 40, B: 50, A: 0xff}
	img := overlaySrc(2, 1, src)
	runs := []render.GlyphRun{
		{Col: 0, Row: 0, Text: "C", FG: color.RGBA{B: 0xff, A: 0xff}, StackRank: 2},
		{Col: 0, Row: 0, Text: "B", FG: color.RGBA{G: 0xff, A: 0xff}, StackRank: 1},
		{Col: 0, Row: 0, Text: "A", FG: color.RGBA{R: 0xff, A: 0xff}, StackRank: 0},
	}
	br := &Renderer{Cols: 2, Rows: 1}
	g := br.Compose(render.Frame{Img: img, Text: runs})
	// The fold read the runs front-to-back, so the last run in the slice won.
	if cell := g.At(0, 0); cell.Text != "A" {
		t.Fatalf("cell Text = %q, want A (the front-most rank wins)", cell.Text)
	}
	want := []struct {
		text string
		rank int
	}{{"C", 2}, {"B", 1}, {"A", 0}}
	for i, w := range want {
		if runs[i].Text != w.text || runs[i].StackRank != w.rank {
			t.Fatalf("caller run %d = (%q, rank %d), want (%q, rank %d); the fold must not sort the caller's slice in place",
				i, runs[i].Text, runs[i].StackRank, w.text, w.rank)
		}
	}
}

// TestComposeFirstWriterWinsWideLead places a width-2 run, then a second run over
// its lead cell. The wide grapheme and its Wide trailing cell must survive; the
// second run is skipped because the lead is claimed.
func TestComposeFirstWriterWinsWideLead(t *testing.T) {
	src := color.RGBA{R: 30, G: 40, B: 50, A: 0xff}
	img := overlaySrc(3, 1, src)
	br := &Renderer{Cols: 3, Rows: 1}
	f := render.Frame{Img: img, Text: []render.GlyphRun{
		{Col: 0, Row: 0, Text: "世", FG: color.RGBA{A: 0xff}},
		{Col: 0, Row: 0, Text: "A", FG: color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}},
	}}
	g := br.Compose(f)
	lead := g.At(0, 0)
	if lead.Text != "世" {
		t.Fatalf("lead Text = %q, want 世 (first writer wins)", lead.Text)
	}
	trail := g.At(1, 0)
	if !trail.Wide {
		t.Fatal("trailing cell Wide = false, want true (wide claim survives)")
	}
	if trail.Text != "" {
		t.Fatalf("trailing Text = %q, want empty", trail.Text)
	}
}

// TestComposeWideAtRightEdgeWritesSpace places a width-2 grapheme in the last
// column and asserts the cell holds a background space, no Wide neighbour, no
// panic.
func TestComposeWideAtRightEdgeWritesSpace(t *testing.T) {
	img := overlaySrc(2, 1, color.RGBA{R: 10, G: 20, B: 30, A: 0xff})
	br := &Renderer{Cols: 2, Rows: 1}
	f := render.Frame{Img: img, Text: []render.GlyphRun{{Col: 1, Row: 0, Text: "世", FG: color.RGBA{A: 0xff}}}}
	g := br.Compose(f)
	cell := g.At(1, 0)
	if cell.Text != " " {
		t.Fatalf("right-edge cell Text = %q, want a space", cell.Text)
	}
	if cell.Wide {
		t.Fatal("right-edge cell Wide = true, want false")
	}
}

// TestComposeBGAverageRGBAOrder composes a cell over a known red source block and
// asserts the BG average keeps RGBA order: red high, blue low.
func TestComposeBGAverageRGBAOrder(t *testing.T) {
	red := color.RGBA{R: 240, G: 0, B: 0, A: 0xff}
	img := overlaySrc(1, 1, red)
	br := &Renderer{Cols: 1, Rows: 1}
	f := render.Frame{Img: img, Text: []render.GlyphRun{{Col: 0, Row: 0, Text: "A", FG: color.RGBA{A: 0xff}}}}
	g := br.Compose(f)
	bg := g.At(0, 0).BG
	if bg.R < 200 {
		t.Fatalf("BG.R = %d, want high (red source)", bg.R)
	}
	if bg.B > 20 {
		t.Fatalf("BG.B = %d, want low (no blue in source) - channel swap?", bg.B)
	}
}

// TestComposeLeftEdgeClipAdvancesByWidth asserts a run clipped at the left edge
// advances col by each clipped cluster's display width, so a wide cluster
// straddling the edge consumes both its columns and the visible remainder lands
// in the right place. The run starts at Col -1 with a width-2 CJK ideograph
// followed by ASCII; the wide cluster spans columns -1 and 0 (off-grid), so the
// first ASCII grapheme must land at column 1, not column 0.
func TestComposeLeftEdgeClipAdvancesByWidth(t *testing.T) {
	img := overlaySrc(4, 1, color.RGBA{R: 20, G: 30, B: 40, A: 0xff})
	br := &Renderer{Cols: 4, Rows: 1}
	f := render.Frame{Img: img, Text: []render.GlyphRun{
		{Col: -1, Row: 0, Text: "世AB", FG: color.RGBA{A: 0xff}},
	}}
	g := br.Compose(f)
	// Column 0 is off the visible span of the clipped wide cluster, so it carries
	// no overlay text.
	if t0 := g.At(0, 0).Text; t0 != "" {
		t.Fatalf("col 0 Text = %q, want empty (clipped wide cluster)", t0)
	}
	if t1 := g.At(1, 0).Text; t1 != "A" {
		t.Fatalf("col 1 Text = %q, want A (full-width accounting for the clipped wide cluster)", t1)
	}
	if t2 := g.At(2, 0).Text; t2 != "B" {
		t.Fatalf("col 2 Text = %q, want B", t2)
	}
}

// twoColumnQuadrant builds a one-cell quadrant source image whose left source
// column is left and right source column is right, so a sub-column mask changes
// the averaged background.
func twoColumnQuadrant(left, right color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, cellPxW, cellPxH))
	for y := range cellPxH {
		img.SetRGBA(0, y, left)
		img.SetRGBA(1, y, right)
	}
	return img
}

// twoColumnQuadrantAt builds a cols-wide quadrant source image, filling the cell
// at column col with left/right source columns and every other column with fill,
// so a sub-cell mask on that cell is measurable while its neighbours differ.
func twoColumnQuadrantAt(cols, col int, left, right, fill color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, cols*cellPxW, cellPxH))
	for c := range cols {
		x0 := c * cellPxW
		for y := range cellPxH {
			if c == col {
				img.SetRGBA(x0, y, left)
				img.SetRGBA(x0+1, y, right)
			} else {
				img.SetRGBA(x0, y, fill)
				img.SetRGBA(x0+1, y, fill)
			}
		}
	}
	return img
}

// TestCellBGAverageWholeCellIdentity pins the geometry-free path: a run with no
// box geometry (W == 0) must reproduce the whole-cell mean byte for byte. The
// expected value is the arithmetic mean of the two source columns, computed
// here independently of the function.
func TestCellBGAverageWholeCellIdentity(t *testing.T) {
	left := color.RGBA{R: 200, G: 100, B: 40, A: 0xff}
	right := color.RGBA{R: 40, G: 60, B: 220, A: 0xff}
	img := twoColumnQuadrant(left, right)
	// Whole-cell mean over the 2x4 block: each column fills 4 rows, so the mean
	// is (left+right)/2 per channel with the same integer rounding the function
	// uses (sum over 8 pixels, integer divide by 8).
	want := color.RGBA{
		R: clampByte((int(left.R)*4 + int(right.R)*4) / 8),
		G: clampByte((int(left.G)*4 + int(right.G)*4) / 8),
		B: clampByte((int(left.B)*4 + int(right.B)*4) / 8),
		A: 0xff,
	}
	// No geometry: run.W == 0, so cellCSSW is 0 and every column is uncovered.
	got := cellBGAverageFrom(rgbaPixelSource{img: img}, 0, 0, SchemeQuadrant, render.GlyphRun{Col: 0, Row: 0, Text: "A"}, 0, 0)
	if got != want {
		t.Fatalf("geometry-free BG = %v, want whole-cell mean %v", got, want)
	}
}

// TestCellBGAveragePartialCoverage proves the snapped anchoring: the box is
// compared against the cell's snapped grid position ((Col+off)*cellCSSW), not
// its own left edge. The cell sits at column 1, so its snapped left is 10; the
// box spans [15, 20), covering the cell's right source column only. The average
// must therefore be the uncovered left column.
func TestCellBGAveragePartialCoverage(t *testing.T) {
	left := color.RGBA{R: 200, G: 100, B: 40, A: 0xff}
	right := color.RGBA{R: 40, G: 60, B: 220, A: 0xff}
	fill := color.RGBA{R: 10, G: 10, B: 10, A: 0xff}
	img := twoColumnQuadrantAt(2, 1, left, right, fill)
	cellCSSW := 10.0
	// Cell at column 1 (snapped left 10); box [15, 20) covers source column 1.
	run := render.GlyphRun{Col: 1, Row: 0, Text: "A", X: 15, W: 5}
	got := cellBGAverageFrom(rgbaPixelSource{img: img}, 1, 0, SchemeQuadrant, run, 0, cellCSSW)
	if got != left {
		t.Fatalf("partial-coverage BG = %v, want uncovered left column %v", got, left)
	}
}

// TestComposeBoundaryCellExcludesCoveredColumns proves the boundary recompose:
// a run whose box residue spills into the neighbour cell the overlay never
// wrote repaints that neighbour from its uncovered source columns only. The run
// sits at column 1 (one grapheme, cellCSSW 10) with box [10, 25); the right
// boundary cell is column 2 (snapped left 20, source columns [20,25) and
// [25,30)). The box covers column 2's left source column, so its recomposed
// background is the right source column alone, not the whole-cell blend.
func TestComposeBoundaryCellExcludesCoveredColumns(t *testing.T) {
	left := color.RGBA{R: 200, G: 100, B: 40, A: 0xff}
	right := color.RGBA{R: 40, G: 60, B: 220, A: 0xff}
	fill := color.RGBA{R: 10, G: 10, B: 10, A: 0xff}
	// Column 2 carries the distinct left/right columns; the run is at column 1.
	img := twoColumnQuadrantAt(4, 2, left, right, fill)
	br := &Renderer{Cols: 4, Rows: 1}
	run := render.GlyphRun{Col: 1, Row: 0, Text: "A", X: 10, W: 15, CellW: 10, FG: color.RGBA{A: 0xff}}
	g := br.Compose(render.Frame{Img: img, Text: []render.GlyphRun{run}})
	boundary := g.At(2, 0)
	if boundary.Text != "" || boundary.Wide {
		t.Fatalf("boundary cell claimed an overlay: Text=%q Wide=%v", boundary.Text, boundary.Wide)
	}
	if boundary.BG != right {
		t.Fatalf("boundary BG = %v, want uncovered right column %v", boundary.BG, right)
	}
	// The unrelated far cell keeps its plain bitmap background (fill), untouched.
	if far := g.At(3, 0); far.BG != fill {
		t.Fatalf("far cell BG = %v, want untouched fill %v", far.BG, fill)
	}
}

// TestComposeBoundaryCellUsesSharedDisplayWidth proves the zero-only fallback
// measures hand-built runs through render.DisplayWidth for representative
// grapheme classes and recomposes the boundary at the measured display column.
func TestComposeBoundaryCellUsesSharedDisplayWidth(t *testing.T) {
	tests := []struct {
		name         string
		text         string
		displayCells int
	}{
		{name: "ASCII", text: "A", displayCells: 1},
		{name: "combining", text: "e\u0301", displayCells: 1},
		{name: "emoji", text: "🙂", displayCells: 2},
		{name: "width-2", text: "界", displayCells: 2},
	}
	left := color.RGBA{R: 200, G: 100, B: 40, A: 0xff}
	right := color.RGBA{R: 40, G: 60, B: 220, A: 0xff}
	fill := color.RGBA{R: 10, G: 10, B: 10, A: 0xff}
	const cellCSSW = 10.0
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			boundaryCol := 1 + tc.displayCells
			img := twoColumnQuadrantAt(boundaryCol+2, boundaryCol, left, right, fill)
			br := &Renderer{Cols: boundaryCol + 2, Rows: 1}
			run := render.GlyphRun{
				Col: 1, Row: 0, Text: tc.text,
				X: cellCSSW, W: float64(tc.displayCells)*cellCSSW + cellCSSW/2,
				CellW: cellCSSW, FG: color.RGBA{A: 0xff},
			}
			g := br.Compose(render.Frame{Img: img, Text: []render.GlyphRun{run}})
			boundary := g.At(boundaryCol, 0)
			if boundary.Text != "" || boundary.Wide {
				t.Fatalf("boundary cell claimed an overlay: Text=%q Wide=%v", boundary.Text, boundary.Wide)
			}
			if boundary.BG != right {
				t.Fatalf("boundary at column %d has BG %v, want uncovered right column %v", boundaryCol, boundary.BG, right)
			}
		})
	}
}

// TestComposeAnchorsWithCarriedCellW proves the overlay anchors on the exact
// cell width the run carries, not one derived from the untrimmed box. The box is
// one cell wider than its trimmed text: W = 3*cellW (a trailing space the engine
// trimmed) while Text is two cells, so a W/display-cells derivation would inflate
// the cell width to 1.5x. With CellW carried, the mask anchors at the true cell
// width. The run sits at column 0 (cellCSSW 10, box [0, 30)); the right boundary
// cell is column 2 (snapped left 20, source columns [20,25) and [25,30)), both
// inside the box, so the recompose falls back to that neighbour's whole-cell
// mean. The inflated 15px derivation would have anchored column 2 at snapped left
// 30, outside the box, covering nothing and leaving the neighbour untouched.
func TestComposeAnchorsWithCarriedCellW(t *testing.T) {
	left := color.RGBA{R: 200, G: 100, B: 40, A: 0xff}
	right := color.RGBA{R: 40, G: 60, B: 220, A: 0xff}
	fill := color.RGBA{R: 10, G: 10, B: 10, A: 0xff}
	img := twoColumnQuadrantAt(4, 2, left, right, fill)
	br := &Renderer{Cols: 4, Rows: 1}
	// Box "hi " (3 cells wide) trimmed to "hi" (2 cells); CellW is the true 10px.
	run := render.GlyphRun{Col: 0, Row: 0, Text: "hi", X: 0, W: 30, CellW: 10, FG: color.RGBA{A: 0xff}}
	g := br.Compose(render.Frame{Img: img, Text: []render.GlyphRun{run}})
	boundary := g.At(2, 0)
	if boundary.Text != "" || boundary.Wide {
		t.Fatalf("boundary cell claimed an overlay: Text=%q Wide=%v", boundary.Text, boundary.Wide)
	}
	// Both of column 2's source columns lie inside the box [0, 30), so the mask is
	// fully covered and averageUncovered falls back to the whole-cell mean.
	want := color.RGBA{
		R: clampByte((int(left.R)*4 + int(right.R)*4) / 8),
		G: clampByte((int(left.G)*4 + int(right.G)*4) / 8),
		B: clampByte((int(left.B)*4 + int(right.B)*4) / 8),
		A: 0xff,
	}
	if boundary.BG != want {
		t.Fatalf("boundary BG = %v, want whole-cell mean %v (box covers both source columns at the true cell width)", boundary.BG, want)
	}
}

// TestCellBGAverageFullCoverageIdentity covers the whole cell and asserts the
// background falls back to the whole-cell mean byte for byte, matching the
// geometry-free identity.
func TestCellBGAverageFullCoverageIdentity(t *testing.T) {
	left := color.RGBA{R: 200, G: 100, B: 40, A: 0xff}
	right := color.RGBA{R: 40, G: 60, B: 220, A: 0xff}
	img := twoColumnQuadrant(left, right)
	cellCSSW := 10.0
	want := color.RGBA{
		R: clampByte((int(left.R)*4 + int(right.R)*4) / 8),
		G: clampByte((int(left.G)*4 + int(right.G)*4) / 8),
		B: clampByte((int(left.B)*4 + int(right.B)*4) / 8),
		A: 0xff,
	}
	// Box spans the whole cell: both source columns covered, fall back to mean.
	run := render.GlyphRun{Col: 0, Row: 0, Text: "A", X: 0, W: cellCSSW}
	got := cellBGAverageFrom(rgbaPixelSource{img: img}, 0, 0, SchemeQuadrant, run, 0, cellCSSW)
	if got != want {
		t.Fatalf("full-coverage BG = %v, want whole-cell mean %v", got, want)
	}
}

// TestComposeRunOutOfBoundsClipped asserts a run starting past the grid edge
// places nothing and does not panic.
func TestComposeRunOutOfBoundsClipped(t *testing.T) {
	img := overlaySrc(2, 2, color.RGBA{A: 0xff})
	br := &Renderer{Cols: 2, Rows: 2}
	f := render.Frame{Img: img, Text: []render.GlyphRun{
		{Col: 5, Row: 0, Text: "A", FG: color.RGBA{A: 0xff}},
		{Col: 0, Row: 9, Text: "B", FG: color.RGBA{A: 0xff}},
	}}
	g := br.Compose(f)
	for i := range g.Cells {
		if g.Cells[i].Text != "" {
			t.Fatalf("cell %d carries Text %q from a clipped run", i, g.Cells[i].Text)
		}
	}
}
