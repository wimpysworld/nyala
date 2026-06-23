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

// TestGraphemeCells asserts an ASCII run splits one element per character and a
// ZWJ emoji cluster stays a single element.
func TestGraphemeCells(t *testing.T) {
	got := graphemeCells("abc")
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("ASCII run: got %v, want [a b c]", got)
	}
	// Family emoji: man + ZWJ + woman + ZWJ + girl, one grapheme cluster.
	zwj := "\U0001F468\u200d\U0001F469\u200d\U0001F467"
	got = graphemeCells(zwj)
	if len(got) != 1 {
		t.Fatalf("ZWJ cluster: got %d elements, want 1", len(got))
	}
}

// TestCellWidth asserts ASCII is width 1, a CJK ideograph and an emoji width 2.
func TestCellWidth(t *testing.T) {
	if w := cellWidth("a"); w != 1 {
		t.Fatalf("ASCII width = %d, want 1", w)
	}
	if w := cellWidth("世"); w != 2 {
		t.Fatalf("CJK width = %d, want 2", w)
	}
	if w := cellWidth("\U0001F600"); w != 2 {
		t.Fatalf("emoji width = %d, want 2", w)
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
