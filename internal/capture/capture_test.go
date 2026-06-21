package capture

import (
	"bytes"
	"image/color"
	"testing"

	"github.com/wimpysworld/nyala/internal/render/block"
)

// gridFixture builds a small deterministic grid: every cell holds the same
// glyph, foreground, and background. It is the input for the determinism and
// channel-order checks.
func gridFixture() block.Grid {
	fg := color.RGBA{R: 0xff, G: 0x00, B: 0x00, A: 0xff} // red
	bg := color.RGBA{R: 0x00, G: 0x00, B: 0xff, A: 0xff} // blue
	g := block.Grid{Cols: 3, Rows: 2, Cells: make([]block.Cell, 6)}
	for i := range g.Cells {
		g.Cells[i] = block.Cell{Glyph: 'A', FG: fg, BG: bg}
	}
	return g
}

func TestRasteriseCellSize(t *testing.T) {
	r, err := NewRasteriser()
	if err != nil {
		t.Fatalf("NewRasteriser: %v", err)
	}
	g := gridFixture()
	img := r.Rasterise(g)
	wantW := g.Cols * CellPxW
	wantH := g.Rows * CellPxH
	if got := img.Bounds().Dx(); got != wantW {
		t.Errorf("width = %d, want %d (Cols*%d)", got, wantW, CellPxW)
	}
	if got := img.Bounds().Dy(); got != wantH {
		t.Errorf("height = %d, want %d (Rows*%d)", got, wantH, CellPxH)
	}
	if CellPxW != 8 || CellPxH != 16 {
		t.Errorf("cell size = %dx%d, want 8x16", CellPxW, CellPxH)
	}
}

func TestRasteriseDeterministicRGBA(t *testing.T) {
	r, err := NewRasteriser()
	if err != nil {
		t.Fatalf("NewRasteriser: %v", err)
	}
	g := gridFixture()
	a := r.Rasterise(g)
	b := r.Rasterise(g)
	if !bytes.Equal(a.Pix, b.Pix) {
		t.Error("two rasterisations of the same grid differ in Pix bytes")
	}
}

func TestRasteriseDeterministicPNG(t *testing.T) {
	r, err := NewRasteriser()
	if err != nil {
		t.Fatalf("NewRasteriser: %v", err)
	}
	g := gridFixture()
	a, err := EncodePNGBytes(r.Rasterise(g))
	if err != nil {
		t.Fatalf("encode a: %v", err)
	}
	b, err := EncodePNGBytes(r.Rasterise(g))
	if err != nil {
		t.Fatalf("encode b: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Error("two PNG encodes of the same grid differ; PNG is not timestamp-free or not deterministic")
	}
}

// TestCellGlyphOverBackground checks a known cell paints its glyph in the
// foreground colour over the background. The cell corners are glyph-free, so they
// hold the background; at least one interior pixel holds the foreground.
func TestCellGlyphOverBackground(t *testing.T) {
	r, err := NewRasteriser()
	if err != nil {
		t.Fatalf("NewRasteriser: %v", err)
	}
	fg := color.RGBA{R: 0xff, G: 0x00, B: 0x00, A: 0xff}
	bg := color.RGBA{R: 0x00, G: 0x00, B: 0xff, A: 0xff}
	g := block.Grid{Cols: 1, Rows: 1, Cells: []block.Cell{{Glyph: 'A', FG: fg, BG: bg}}}
	img := r.Rasterise(g)

	// Top-left corner of the cell is outside the 'A' outline: background.
	if got := img.RGBAAt(0, 0); got != bg {
		t.Errorf("corner pixel = %v, want background %v", got, bg)
	}

	foundFG := false
	for y := 0; y < CellPxH; y++ {
		for x := 0; x < CellPxW; x++ {
			if img.RGBAAt(x, y) == fg {
				foundFG = true
			}
		}
	}
	if !foundFG {
		t.Error("no fully foreground pixel found; glyph did not render in fg over bg")
	}
}

// TestRGBAChannelOrder asserts the byte layout is R, G, B, A. A pure-green
// background must store G in the second byte of the pixel, not the first or
// third.
func TestRGBAChannelOrder(t *testing.T) {
	r, err := NewRasteriser()
	if err != nil {
		t.Fatalf("NewRasteriser: %v", err)
	}
	green := color.RGBA{R: 0x00, G: 0x80, B: 0x00, A: 0xff}
	// Use the half-block glyph so the corner is a flat background fill with no
	// glyph coverage.
	g := block.Grid{Cols: 1, Rows: 1, Cells: []block.Cell{{Glyph: ' ', FG: green, BG: green}}}
	img := r.Rasterise(g)
	i := img.PixOffset(0, 0)
	r0, g0, b0, a0 := img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3]
	if r0 != 0x00 || g0 != 0x80 || b0 != 0x00 || a0 != 0xff {
		t.Errorf("pixel bytes = R%02X G%02X B%02X A%02X, want R00 G80 B00 AFF", r0, g0, b0, a0)
	}
}
