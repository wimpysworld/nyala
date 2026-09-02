package cellraster

import (
	"image/color"
	"testing"

	"github.com/wimpysworld/nyala/internal/render/block"
	"github.com/wimpysworld/nyala/internal/render/font"
)

// shadeGlyphs is the Symbols for Legacy Computing candidate subset the
// procedural raster owns: every Shaded candidate at or above U+1FB8C.
func shadeGlyphs() []rune {
	var out []rune
	for _, g := range block.ShadedCandidates() {
		if g >= 0x1FB8C {
			out = append(out, g)
		}
	}
	return out
}

// TestShadeGlyphsNeedProceduralRaster records the font fact the procedural
// path exists for: neither embedded Route A font covers any shade candidate.
// When a font upgrade adds coverage, this fails, prompting a choice between
// the font path and the procedural model, which must stay deliberate because
// the compose coverage tables are built from the procedural geometry.
func TestShadeGlyphsNeedProceduralRaster(t *testing.T) {
	atlas, err := font.Load()
	if err != nil {
		t.Fatalf("font.Load: %v", err)
	}
	for _, g := range shadeGlyphs() {
		if _, _, ok := atlas.Resolve(g); ok {
			t.Fatalf("embedded font now covers %U; decide between font and procedural raster", g)
		}
	}
}

// TestShadeGlyphRasterMatchesModel proves every shade candidate rasterises
// visibly (never tofu-blank), inside one 8x16 cell, with exactly the ink the
// coverage model prescribes: solid points always paint, medium-shade points
// paint on the checkerboard half.
func TestShadeGlyphRasterMatchesModel(t *testing.T) {
	rast, err := NewRasteriser()
	if err != nil {
		t.Fatalf("NewRasteriser: %v", err)
	}
	fg := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	bg := color.RGBA{A: 0xff}
	for _, g := range shadeGlyphs() {
		tile := rast.RenderGlyphCell(string(g), fg, bg)
		inked := 0
		for y := range CellPxH {
			for x := range CellPxW {
				op, ok := block.ShadedOpacity(g, x, y)
				if !ok {
					t.Fatalf("%U is not a Shaded candidate", g)
				}
				want := bg
				if op == 2 || (op == 1 && (x+y)%2 == 0) {
					want = fg
					inked++
				}
				if got := tile.RGBAAt(x, y); got != want {
					t.Fatalf("%U pixel (%d,%d) = %v, want %v", g, x, y, got, want)
				}
			}
		}
		if inked == 0 || inked == CellPxW*CellPxH {
			t.Fatalf("%U inked %d pixels; a shade candidate must be visible and partial", g, inked)
		}
	}
}

// TestShadeGlyphRasterStaysInCell proves a shade glyph never bleeds into a
// neighbouring cell: on a 3x3 grid with the glyph in the centre, every other
// cell keeps its flat background.
func TestShadeGlyphRasterStaysInCell(t *testing.T) {
	rast, err := NewRasteriser()
	if err != nil {
		t.Fatalf("NewRasteriser: %v", err)
	}
	fg := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	bg := color.RGBA{R: 0x20, G: 0x20, B: 0x20, A: 0xff}
	for _, g := range shadeGlyphs() {
		grid := block.Grid{Cols: 3, Rows: 3, Cells: make([]block.Cell, 9)}
		for i := range grid.Cells {
			grid.Cells[i] = block.Cell{Glyph: 0x0020, FG: bg, BG: bg}
		}
		*grid.At(1, 1) = block.Cell{Glyph: g, FG: fg, BG: bg}
		img := rast.Rasterise(grid)
		for y := range img.Bounds().Dy() {
			for x := range img.Bounds().Dx() {
				inCentre := x >= CellPxW && x < 2*CellPxW && y >= CellPxH && y < 2*CellPxH
				if !inCentre && img.RGBAAt(x, y) != bg {
					t.Fatalf("%U bled to (%d,%d): %v", g, x, y, img.RGBAAt(x, y))
				}
			}
		}
	}
}
