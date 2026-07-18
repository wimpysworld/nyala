package cellraster

import (
	"bytes"
	"image"
	"image/color"
	"testing"

	"github.com/wimpysworld/nyala/internal/render/block"
)

func encodePNGBytes(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := EncodePNG(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gridFixture() block.Grid {
	fg := color.RGBA{R: 0xff, A: 0xff}
	bg := color.RGBA{B: 0xff, A: 0xff}
	grid := block.Grid{Cols: 3, Rows: 2, Cells: make([]block.Cell, 6)}
	for i := range grid.Cells {
		grid.Cells[i] = block.Cell{Glyph: 'A', FG: fg, BG: bg}
	}
	return grid
}

func TestRasteriseCellSize(t *testing.T) {
	r, err := NewRasteriser()
	if err != nil {
		t.Fatal(err)
	}
	grid := gridFixture()
	img := r.Rasterise(grid)
	if img.Bounds().Dx() != grid.Cols*CellPxW || img.Bounds().Dy() != grid.Rows*CellPxH {
		t.Fatalf("size = %v, want %dx%d", img.Bounds(), grid.Cols*CellPxW, grid.Rows*CellPxH)
	}
}

func TestRasteriseDeterministicRGBA(t *testing.T) {
	r, err := NewRasteriser()
	if err != nil {
		t.Fatal(err)
	}
	grid := gridFixture()
	if !bytes.Equal(r.Rasterise(grid).Pix, r.Rasterise(grid).Pix) {
		t.Fatal("same grid produced different RGBA bytes")
	}
}

func TestRasteriseDeterministicPNG(t *testing.T) {
	r, err := NewRasteriser()
	if err != nil {
		t.Fatal(err)
	}
	grid := gridFixture()
	a, err := encodePNGBytes(r.Rasterise(grid))
	if err != nil {
		t.Fatal(err)
	}
	b, err := encodePNGBytes(r.Rasterise(grid))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("same grid produced different PNG bytes")
	}
}

func TestCellGlyphOverBackground(t *testing.T) {
	r, err := NewRasteriser()
	if err != nil {
		t.Fatal(err)
	}
	fg := color.RGBA{R: 0xff, A: 0xff}
	bg := color.RGBA{B: 0xff, A: 0xff}
	img := r.Rasterise(block.Grid{Cols: 1, Rows: 1, Cells: []block.Cell{{Glyph: 'A', FG: fg, BG: bg}}})
	if got := img.RGBAAt(0, 0); got != bg {
		t.Fatalf("corner = %v, want background %v", got, bg)
	}
	for y := range CellPxH {
		for x := range CellPxW {
			if img.RGBAAt(x, y) == fg {
				return
			}
		}
	}
	t.Fatal("glyph has no foreground pixel")
}

func TestRGBAChannelOrder(t *testing.T) {
	r, err := NewRasteriser()
	if err != nil {
		t.Fatal(err)
	}
	green := color.RGBA{G: 0x80, A: 0xff}
	img := r.Rasterise(block.Grid{Cols: 1, Rows: 1, Cells: []block.Cell{{Glyph: ' ', FG: green, BG: green}}})
	offset := img.PixOffset(0, 0)
	got := img.Pix[offset : offset+4]
	want := []byte{0x00, 0x80, 0x00, 0xff}
	if !bytes.Equal(got, want) {
		t.Fatalf("pixel bytes = %v, want %v", got, want)
	}
}

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
	for y := range CellPxH {
		for x := range CellPxW {
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
	for y := range CellPxH {
		for x := range CellPxW {
			if tile.RGBAAt(x, y) == fg {
				foundFG = true
			}
		}
	}
	if !foundFG {
		t.Fatal("no foreground pixel in tile; glyph not drawn")
	}
}
