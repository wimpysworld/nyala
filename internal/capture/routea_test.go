package capture

import (
	"bytes"
	"image/color"
	"testing"

	"github.com/wimpysworld/nyala/internal/render/block"
)

// TestRasteriseDrawsTextCell asserts a text cell rasterises through the glyph
// face: the same cell drawn as its bitmap glyph and as an overlay grapheme differ
// in pixels, and a Wide trailing cell draws background only.
func TestRasteriseDrawsTextCell(t *testing.T) {
	r, err := NewRasteriser()
	if err != nil {
		t.Fatalf("NewRasteriser: %v", err)
	}
	fg := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	bg := color.RGBA{R: 0x00, G: 0x00, B: 0x00, A: 0xff}

	textGrid := block.Grid{Cols: 1, Rows: 1, Cells: []block.Cell{{Glyph: 0x2588, FG: fg, BG: bg, Text: "A"}}}
	glyphGrid := block.Grid{Cols: 1, Rows: 1, Cells: []block.Cell{{Glyph: 0x2588, FG: fg, BG: bg}}}
	textImg := r.Rasterise(textGrid)
	glyphImg := r.Rasterise(glyphGrid)
	if bytes.Equal(textImg.Pix, glyphImg.Pix) {
		t.Fatal("text cell rasterised identically to its bitmap glyph; overlay text not drawn")
	}

	// A Wide trailing cell draws its background only, so every pixel is bg.
	wideGrid := block.Grid{Cols: 1, Rows: 1, Cells: []block.Cell{{Glyph: 0x2588, FG: fg, BG: bg, Wide: true}}}
	wideImg := r.Rasterise(wideGrid)
	for y := 0; y < CellPxH; y++ {
		for x := 0; x < CellPxW; x++ {
			if got := wideImg.RGBAAt(x, y); got != bg {
				t.Fatalf("Wide cell pixel (%d,%d) = %v, want background %v", x, y, got, bg)
			}
		}
	}
}

// TestRenderGlyphCell asserts RenderGlyphCell draws one grapheme over bg through
// the face, producing a CellPxW x CellPxH tile with foreground pixels present.
func TestRenderGlyphCell(t *testing.T) {
	r, err := NewRasteriser()
	if err != nil {
		t.Fatalf("NewRasteriser: %v", err)
	}
	fg := color.RGBA{R: 0xff, G: 0x00, B: 0x00, A: 0xff}
	bg := color.RGBA{R: 0x00, G: 0x00, B: 0xff, A: 0xff}
	tile := r.RenderGlyphCell("A", fg, bg)
	if tile.Bounds().Dx() != CellPxW || tile.Bounds().Dy() != CellPxH {
		t.Fatalf("tile size = %dx%d, want %dx%d", tile.Bounds().Dx(), tile.Bounds().Dy(), CellPxW, CellPxH)
	}
	foundFG := false
	for y := 0; y < CellPxH; y++ {
		for x := 0; x < CellPxW; x++ {
			if tile.RGBAAt(x, y) == fg {
				foundFG = true
			}
		}
	}
	if !foundFG {
		t.Fatal("no foreground pixel in tile; glyph not drawn")
	}
}
