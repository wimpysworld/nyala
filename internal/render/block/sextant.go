package block

import (
	"image"
	"image/color"

	"github.com/wimpysworld/nyala/internal/render"
)

// sextCellPxW and sextCellPxH are the source-pixel footprint of one cell in
// the sextant scheme. The sextant scheme maps one cell to a 2x3 source block:
// two columns and three rows, one pixel per sub-cell.
const (
	sextCellPxW = 2
	sextCellPxH = 3
)

// sextantGlyphs is the 64-entry sextant block glyph table, built by
// buildSextantGlyphs from the documented mask formula rather than a hand
// transcription.
var sextantGlyphs = buildSextantGlyphs()

// buildSextantGlyphs computes the sextantGlyphs table, keyed by a 6-bit mask
// with bit0=top-left, bit1=top-right, bit2=mid-left, bit3=mid-right,
// bit4=bottom-left, bit5=bottom-right, row-major, set when that sub-cell is
// bright. This is the opposite bit order from quadrantGlyphs' TL<<3
// convention: bit0 here is the lowest-order bit and the top-left sub-cell.
//
// Masks 0, 21, and 42 fall outside the Unicode Legacy Computing sextant block
// and route to legacy glyphs instead: 0 (all dark) to U+0020 space, 21 (the
// checkerboard left-column pattern) to U+258C left half block, 42 (the
// checkerboard right-column pattern) to U+2590 right half block. Mask 63 (all
// bright) also has no dedicated sextant code point and routes to U+2588 full
// block. Every other mask maps into the Legacy Computing sextant range
// starting at U+1FB00, offset by (mask - 1) minus one for each of masks 21 and
// 42 already consumed below it, since those two are legacy glyphs rather than
// range entries.
func buildSextantGlyphs() [64]rune {
	var t [64]rune
	for mask := range 64 {
		switch mask {
		case 0:
			t[mask] = 0x0020
		case 21:
			t[mask] = 0x258C
		case 42:
			t[mask] = 0x2590
		case 63:
			t[mask] = 0x2588
		default:
			offset := mask - 1
			if mask > 21 {
				offset--
			}
			if mask > 42 {
				offset--
			}
			t[mask] = rune(0x1FB00 + offset)
		}
	}
	return t
}

// sampleSextant reads the six sub-cell colours of cell (col, row) for the
// sextant scheme: tl, tr, ml, mr, bl, br, one pixel per sub-cell from the 2x3
// source block anchored at (col*sextCellPxW, row*sextCellPxH). Pixels outside
// the image read as opaque black.
func sampleSextant(img *image.RGBA, col, row int) (tl, tr, ml, mr, bl, br color.RGBA) {
	x0 := col * sextCellPxW
	y0 := row * sextCellPxH
	r, g, b := pixel(img, x0, y0)
	tl = color.RGBA{R: r, G: g, B: b, A: 0xff}
	r, g, b = pixel(img, x0+1, y0)
	tr = color.RGBA{R: r, G: g, B: b, A: 0xff}
	r, g, b = pixel(img, x0, y0+1)
	ml = color.RGBA{R: r, G: g, B: b, A: 0xff}
	r, g, b = pixel(img, x0+1, y0+1)
	mr = color.RGBA{R: r, G: g, B: b, A: 0xff}
	r, g, b = pixel(img, x0, y0+2)
	bl = color.RGBA{R: r, G: g, B: b, A: 0xff}
	r, g, b = pixel(img, x0+1, y0+2)
	br = color.RGBA{R: r, G: g, B: b, A: 0xff}
	return tl, tr, ml, mr, bl, br
}

// binarizeSextant turns the six sub-cell colours (tl top-left, tr top-right,
// ml mid-left, mr mid-right, bl bottom-left, br bottom-right) into one sextant
// block glyph and two averaged colours. It converts each sub-cell to Rec.601
// luma (reusing luma), splits at the luma midpoint, and builds the 6-bit mask
// bit0=top-left, bit1=top-right, bit2=mid-left, bit3=mid-right,
// bit4=bottom-left, bit5=bottom-right of the bright sub-cells. The glyph is
// the sextantGlyphs entry for that mask; the foreground is the average of the
// bright sub-cells, the background the average of the dark ones. A flat cell
// (mask 0 or 63) emits its glyph with foreground equal to background,
// mirroring binarizeQuadrant.
func binarizeSextant(tl, tr, ml, mr, bl, br color.RGBA) (glyph rune, fg, bg color.RGBA) {
	lTL := luma(tl)
	lTR := luma(tr)
	lML := luma(ml)
	lMR := luma(mr)
	lBL := luma(bl)
	lBR := luma(br)

	lo := min(min(min(lTL, lTR), min(lML, lMR)), min(lBL, lBR))
	hi := max(max(max(lTL, lTR), max(lML, lMR)), max(lBL, lBR))
	mid := lo + (hi-lo)/2.0

	mask := boolBit(lTL > mid) | boolBit(lTR > mid)<<1 | boolBit(lML > mid)<<2 |
		boolBit(lMR > mid)<<3 | boolBit(lBL > mid)<<4 | boolBit(lBR > mid)<<5

	glyph = sextantGlyphs[mask]
	fg, bg = sextantColours(mask, tl, tr, ml, mr, bl, br)
	return glyph, fg, bg
}

// sextantColours returns the (foreground, background) colour pair for a 6-bit
// mask. The six sub-cell colours are tl, tr, ml, mr, bl, br, matching mask
// bit0..bit5. A sub-cell is bright when its mask bit is set. The foreground is
// the average of the bright sub-cells, the background the average of the dark
// ones. The two flat masks (0 all dark, 63 all bright) have no split, so both
// colours are the average of all six sub-cells, mirroring quadrantColours.
func sextantColours(mask int, tl, tr, ml, mr, bl, br color.RGBA) (fg, bg color.RGBA) {
	if mask == 0 || mask == 63 {
		all := avgRGBA(tl, tr, ml, mr, bl, br)
		return all, all
	}
	// Accumulate the bright and dark channel sums in scalars rather than two
	// heap-allocated slices, mirroring quadrantColours: the compose path runs this
	// per non-flat cell. The arithmetic matches avgRGBA summed over the same
	// sub-cells in cell order, so the result is byte-identical.
	cells := [6]color.RGBA{tl, tr, ml, mr, bl, br}
	var brs, bgs, bbs, bn int
	var drs, dgs, dbs, dn int
	for i, c := range cells {
		if mask&(1<<i) != 0 {
			brs += int(c.R)
			bgs += int(c.G)
			bbs += int(c.B)
			bn++
		} else {
			drs += int(c.R)
			dgs += int(c.G)
			dbs += int(c.B)
			dn++
		}
	}
	return avgChannels(brs, bgs, bbs, bn), avgChannels(drs, dgs, dbs, dn)
}

// composeSextant binarises the frame image onto a fresh Cols x Rows sextant
// grid. Each cell samples a 2x3 source block; an empty or nil image yields an
// all-black grid (every sub-cell black, the flat mask-0 space glyph).
func (br *Renderer) composeSextant(f render.Frame) Grid {
	g := Grid{
		Cols:  br.Cols,
		Rows:  br.Rows,
		Cells: make([]Cell, br.Cols*br.Rows),
	}
	br.composeSextantInto(&g, f)
	return g
}

// composeSextantInto binarises the frame image onto g, which the caller has
// already sized to Cols x Rows. It is the shared body of composeSextant (fresh
// grid) and ComposeInto (reused grid), so both fill cells the same way.
func (br *Renderer) composeSextantInto(g *Grid, f render.Frame) {
	br.composeSextantRows(g, f, 0, g.Rows)
}

// composeSextantRows binarises grid rows [row0, row1) of g from the frame image.
// It is the per-band body of composeSextantInto: each cell samples its own 2x3
// source block and writes only its own cell, so a call over a disjoint row range
// writes byte-for-byte the same cells the whole-grid pass writes for those rows.
func (br *Renderer) composeSextantRows(g *Grid, f render.Frame, row0, row1 int) {
	for row := row0; row < row1; row++ {
		for col := range g.Cols {
			topLeft, topRight, midLeft, midRight, botLeft, botRight := sampleSextant(f.Img, col, row)
			glyph, fg, bg := binarizeSextant(topLeft, topRight, midLeft, midRight, botLeft, botRight)
			*g.At(col, row) = Cell{Glyph: glyph, FG: fg, BG: bg}
		}
	}
}
