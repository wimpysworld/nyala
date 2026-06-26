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

// TestComposeNarrowOverwriteClearsStaleWide places a width-2 grapheme at a cell,
// then a width-1 grapheme at the same cell in a later run. The lead cell must hold
// the ASCII Text, and the trailing cell must drop its stale Wide and return to a
// plain bitmap cell.
func TestComposeNarrowOverwriteClearsStaleWide(t *testing.T) {
	src := color.RGBA{R: 30, G: 40, B: 50, A: 0xff}
	img := overlaySrc(3, 1, src)
	br := &Renderer{Cols: 3, Rows: 1}
	f := render.Frame{Img: img, Text: []render.GlyphRun{
		{Col: 0, Row: 0, Text: "世", FG: color.RGBA{A: 0xff}},
		{Col: 0, Row: 0, Text: "A", FG: color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}},
	}}
	g := br.Compose(f)
	lead := g.At(0, 0)
	if lead.Text != "A" {
		t.Fatalf("lead Text = %q, want A", lead.Text)
	}
	trail := g.At(1, 0)
	if trail.Wide {
		t.Fatal("trailing cell Wide = true, want false (stale wide not cleared)")
	}
	if trail.Text != "" {
		t.Fatalf("trailing Text = %q, want empty", trail.Text)
	}
	if want := bitmapCell(img, 1, 0, SchemeQuadrant); *trail != want {
		t.Fatalf("trailing cell = %+v, want recomputed bitmap cell %+v", *trail, want)
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
