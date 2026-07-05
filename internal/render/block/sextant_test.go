package block

import (
	"image/color"
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
)

// TestSextantGlyphTableAnchors asserts the sextantGlyphs table at the anchor
// masks documented in the mask formula: the four legacy-glyph masks (0, 21,
// 42, 63) and a handful of Legacy Computing range entries either side of the
// two mid-range anchors.
func TestSextantGlyphTableAnchors(t *testing.T) {
	cases := []struct {
		mask int
		want rune
	}{
		{0, 0x0020},
		{1, 0x1FB00},
		{2, 0x1FB01},
		{20, 0x1FB13},
		{21, 0x258C},
		{22, 0x1FB14},
		{42, 0x2590},
		{62, 0x1FB3B},
		{63, 0x2588},
	}
	for _, c := range cases {
		if got := sextantGlyphs[c.mask]; got != c.want {
			t.Errorf("mask %d: got %#U, want %#U", c.mask, got, c.want)
		}
	}
}

// TestBinarizeSextantSplit checks the luma-midpoint split and the resulting
// glyph and fg/bg pair for known sub-cell colour layouts, mirroring
// TestBinarizeQuadrantSplit.
func TestBinarizeSextantSplit(t *testing.T) {
	cases := []struct {
		name                   string
		tl, tr, ml, mr, bl, br color.RGBA
		wantGlyph              rune
		wantFG, wantBG         color.RGBA
	}{
		{
			// All six sub-cells equal (flat cell): the minimum sub-cell's luma
			// always equals the midpoint, so no bit can ever be strictly above
			// it and the mask is always 0, never 63, regardless of the actual
			// brightness. Mask 0 is the space glyph, with fg == bg == the
			// uniform colour: visually identical to a full block of that
			// colour, since the background SGR paints the whole cell either
			// way. This mirrors binarizeQuadrant, where a flat cell always
			// resolves to mask 0000, and the quadrant table maps 0000 and
			// 1111 to the same glyph so the choice is moot there; the sextant
			// table gives 0 and 63 different glyphs, but 63 is unreachable
			// through this split, only present as a direct table entry.
			name: "flat cell (uniform luma)", tl: bright, tr: bright, ml: bright, mr: bright, bl: bright, br: bright,
			wantGlyph: 0x0020, wantFG: bright, wantBG: bright,
		},
		{
			// Top row bright, rest dark: mask bit0 | bit1 = 3. fg averages the
			// bright top pair, bg the four dark sub-cells.
			name: "top row bright", tl: bright, tr: bright, ml: dark, mr: dark, bl: dark, br: dark,
			wantGlyph: sextantGlyphs[3], wantFG: avgRGBA(bright, bright), wantBG: avgRGBA(dark, dark, dark, dark),
		},
		{
			// Left column bright: mask bit0 | bit2 | bit4 = 21, left half
			// block. fg averages the bright left column, bg the dark right
			// column.
			name: "left column bright", tl: bright, tr: dark, ml: bright, mr: dark, bl: bright, br: dark,
			wantGlyph: 0x258C, wantFG: avgRGBA(bright, bright, bright), wantBG: avgRGBA(dark, dark, dark),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			glyph, fg, bg := binarizeSextant(c.tl, c.tr, c.ml, c.mr, c.bl, c.br)
			if glyph != c.wantGlyph {
				t.Fatalf("glyph: got %#U, want %#U", glyph, c.wantGlyph)
			}
			if fg != c.wantFG {
				t.Fatalf("fg: got %v, want %v", fg, c.wantFG)
			}
			if bg != c.wantBG {
				t.Fatalf("bg: got %v, want %v", bg, c.wantBG)
			}
		})
	}
}

// TestComposeSextantSingleSubCell builds a 2x3 image with one sub-cell white
// and the rest black, composes a 1x1 sextant grid, and asserts the glyph is
// the single-sub-cell rune for the top-left position, with a bright
// foreground and a dark background, mirroring
// TestComposeQuadrantNotInverted.
func TestComposeSextantSingleSubCell(t *testing.T) {
	white := color.RGBA{R: 255, G: 255, B: 255, A: 0xff}
	black := color.RGBA{R: 0, G: 0, B: 0, A: 0xff}

	img := fillRGBA(sextCellPxW, sextCellPxH, black)
	i := img.PixOffset(0, 0)
	img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = white.R, white.G, white.B, white.A

	br := &Renderer{Cols: 1, Rows: 1, Scheme: SchemeSextant}
	g := br.Compose(render.Frame{Img: img})
	cell := g.At(0, 0)
	wantGlyph := sextantGlyphs[1] // top-left only: mask bit0 = 1
	if cell.Glyph != wantGlyph {
		t.Fatalf("glyph: got %#U, want %#U", cell.Glyph, wantGlyph)
	}
	if cell.FG != white {
		t.Fatalf("fg: got %v, want white (inverted?)", cell.FG)
	}
	if cell.BG != black {
		t.Fatalf("bg: got %v, want black", cell.BG)
	}
}
