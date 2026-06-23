package block

import (
	"image"
	"image/color"

	"github.com/mattn/go-runewidth"
	"github.com/rivo/uniseg"

	"github.com/wimpysworld/nyala/internal/render"
)

// applyOverlay folds the frame's text runs onto the composed grid. Each
// render.GlyphRun places its graphemes left to right starting at (Col, Row).
// The overlay overwrites the bitmap glyph in each touched cell, so the grapheme
// wins; there is no z-buffer. Runs that run past the grid edge are clipped. A
// width-2 grapheme claims the next cell as a Wide trailing cell, or writes a
// background space when it would cross the right edge, so it is never
// half-emitted. The cell background is the exact whole-cell average of the
// sampled source pixels, read in RGBA order.
func applyOverlay(g *Grid, f render.Frame, scheme Scheme) {
	for _, run := range f.Text {
		row := run.Row
		if row < 0 || row >= g.Rows {
			continue
		}
		col := run.Col
		for _, cluster := range graphemeCells(run.Text) {
			if col < 0 {
				col++
				continue
			}
			if col >= g.Cols {
				break
			}
			bg := cellBGAverage(f.Img, col, row, scheme)
			cell := g.At(col, row)
			if cellWidth(cluster) == 2 {
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
			col++
		}
	}
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

// cellWidth measures the display width of one grapheme cluster with
// mattn/go-runewidth, clamped to 1 or 2. A width-0 cluster (a standalone
// combining mark uniseg did not fold into a preceding cluster) is treated as
// width 1, so it still occupies a cell.
func cellWidth(grapheme string) int {
	w := runewidth.StringWidth(grapheme)
	if w >= 2 {
		return 2
	}
	return 1
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
	samples := make([]color.RGBA, 0, pxW*pxH)
	for dy := 0; dy < pxH; dy++ {
		for dx := 0; dx < pxW; dx++ {
			r, gc, b := pixel(img, x0+dx, y0+dy)
			samples = append(samples, color.RGBA{R: r, G: gc, B: b, A: 0xff})
		}
	}
	return avgRGBA(samples...)
}
