package block

import (
	"image"
	"image/color"

	"github.com/rivo/uniseg"

	"github.com/wimpysworld/nyala/internal/render"
)

// applyOverlay folds the frame's text runs onto the composed grid. Each
// render.GlyphRun places its graphemes left to right starting at (Col, Row).
// The overlay overwrites the bitmap glyph in each touched cell, so the grapheme
// wins; there is no z-buffer. Runs that run past the grid edge are clipped. A
// width-2 grapheme claims the next cell as a Wide trailing cell, or writes a
// background space when it would cross the right edge, so it is never
// half-emitted. A cluster clipped past the left edge advances col by its display
// width, so the partition stays monotonic and the visible remainder lands in the
// right columns. The cell background is the exact whole-cell average of the
// sampled source pixels, read in RGBA order. When a width-1 grapheme overwrites a
// cell that was a former width-2 lead, it clears the stale Wide on the trailing
// cell to its right and restores it to a plain bitmap cell.
func applyOverlay(g *Grid, f render.Frame, scheme Scheme) {
	for _, run := range f.Text {
		row := run.Row
		if row < 0 || row >= g.Rows {
			continue
		}
		col := run.Col
		gr := uniseg.NewGraphemes(run.Text)
		for gr.Next() {
			cluster := gr.Str()
			if col < 0 {
				col += render.CellWidth(cluster)
				continue
			}
			if col >= g.Cols {
				break
			}
			bg := cellBGAverage(f.Img, col, row, scheme)
			cell := g.At(col, row)
			if render.CellWidth(cluster) == 2 {
				if col+1 >= g.Cols {
					// A wide glyph in the last column would cross the right edge;
					// write a background space instead of half-emitting it.
					*cell = Cell{Glyph: cell.Glyph, FG: run.FG, BG: bg, Text: " "}
					col++
					continue
				}
				*cell = Cell{Glyph: cell.Glyph, FG: run.FG, BG: bg, Text: cluster}
				trailing := g.At(col+1, row)
				*trailing = Cell{Glyph: trailing.Glyph, FG: run.FG, BG: bg, Wide: true}
				col += 2
				continue
			}
			*cell = Cell{Glyph: cell.Glyph, FG: run.FG, BG: bg, Text: cluster}
			if col+1 < g.Cols {
				if trailing := g.At(col+1, row); trailing.Wide {
					*trailing = bitmapCell(f.Img, col+1, row, scheme)
				}
			}
			col++
		}
	}
}

// bitmapCell recomputes one bitmap cell for the scheme, mirroring composeQuadrant
// and composeHalfBlock for a single cell. It restores a cell to its plain bitmap
// state, with no overlay Text and not Wide.
func bitmapCell(img *image.RGBA, col, row int, scheme Scheme) Cell {
	if scheme == SchemeHalfBlock {
		top, bottom := sampleHalfBlock(img, col, row)
		return Cell{Glyph: lowerHalfBlock, FG: bottom, BG: top}
	}
	x, y, z, w := sampleQuadrant(img, col, row)
	glyph, fg, bg := binarizeQuadrant(x, y, z, w)
	return Cell{Glyph: glyph, FG: fg, BG: bg}
}

// graphemeCells segments text into grapheme clusters with rivo/uniseg, so a
// multi-rune cluster (for example a ZWJ emoji sequence) stays one element. The
// result preserves order, one entry per cluster.
func graphemeCells(text string) []string {
	var cells []string
	gr := uniseg.NewGraphemes(text)
	for gr.Next() {
		cells = append(cells, gr.Str())
	}
	return cells
}

// cellBGAverage averages the whole source footprint of cell (col, row) into one
// RGBA colour, in RGBA channel order. The footprint is the scheme's per-cell
// source block: 2x4 (cellPxW x cellPxH) for the quadrant scheme, 1x2
// (halfCellPxW x halfCellPxH) for the half-block scheme. Out-of-bounds reads
// return opaque black via pixel, matching the bitmap path.
func cellBGAverage(img *image.RGBA, col, row int, scheme Scheme) color.RGBA {
	pxW, pxH := cellPxW, cellPxH
	if scheme == SchemeHalfBlock {
		pxW, pxH = halfCellPxW, halfCellPxH
	}
	x0 := col * pxW
	y0 := row * pxH
	var rs, gs, bs int
	for dy := range pxH {
		for dx := range pxW {
			r, gc, b := pixel(img, x0+dx, y0+dy)
			rs += int(r)
			gs += int(gc)
			bs += int(b)
		}
	}
	n := pxW * pxH
	return color.RGBA{
		R: clampByte(rs / n),
		G: clampByte(gs / n),
		B: clampByte(bs / n),
		A: 0xff,
	}
}
