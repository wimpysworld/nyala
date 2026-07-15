package block

import (
	"image"
	"image/color"

	"github.com/rivo/uniseg"

	"github.com/wimpysworld/nyala/internal/render"
)

// applyOverlay folds the frame's text runs onto the composed grid. Each
// render.GlyphRun places its graphemes left to right starting at (Col, Row).
// The overlay is first-writer-wins: a claimed cell (one that already carries
// overlay Text or is a Wide trailing cell) is never overwritten, so the earliest
// run to reach a cell keeps it. A width-1 grapheme claims its cell; a width-2
// grapheme claims its lead cell and the next cell as a Wide trailing cell, and
// is skipped whole when either cell is already claimed, so a wide glyph is never
// half-written over a prior run. Runs that run past the grid edge are clipped. A
// width-2 grapheme at the right edge writes a background space instead of
// half-emitting. A cluster clipped past the left edge advances col by its display
// width, so the partition stays monotonic and the visible remainder lands in the
// right columns. On the skip path col advances by the grapheme width, so the
// partition stays monotonic there too. The cell background is the average of the
// source pixels the run's box does not cover (cellBGAverageFrom), read in RGBA
// order, so a snapped run that only partly covers its lead cell keeps the
// uncovered sub-columns out of the average. After the run's own cells, the two
// bitmap cells beside it (recomposeBoundaryCellsFrom) are repainted to drop the
// sub-cell residue the box paints into a neighbour the overlay never wrote.
// Paint-order z-ordering (a later run overlaying an earlier one) stays deferred.
func applyOverlay(g *Grid, f render.Frame, scheme Scheme) {
	img, _ := f.Img.(*image.RGBA)
	applyOverlayFrom(g, f, scheme, rgbaPixelSource{img: img})
}

// applyOverlayYCbCr folds the overlay against direct native-YCbCr samples. The
// sampler returns the same sub-cell colours the former intermediate RGBA image
// held, so overlay background and boundary recomposition stay byte-identical.
func applyOverlayYCbCr(g *Grid, f render.Frame, scheme Scheme, src *ycbcrSampler) {
	applyOverlayFrom(g, f, scheme, src)
}

type pixelSource interface {
	pixelAt(x, y int) (r, g, b uint8)
}

type rgbaPixelSource struct {
	img *image.RGBA
}

func (s rgbaPixelSource) pixelAt(x, y int) (r, g, b uint8) {
	return pixel(s.img, x, y)
}

func applyOverlayFrom(g *Grid, f render.Frame, scheme Scheme, src pixelSource) {
	for _, run := range f.Text {
		row := run.Row
		if row < 0 || row >= g.Rows {
			continue
		}
		cellCSSW := runCellCSSWidth(run)
		col := run.Col
		off := 0 // display-cell offset of col within the run
		gr := uniseg.NewGraphemes(run.Text)
		for gr.Next() {
			cluster := gr.Str()
			w := render.CellWidth(cluster)
			if col < 0 {
				col += w
				off += w
				continue
			}
			if col >= g.Cols {
				break
			}
			cell := g.At(col, row)
			if w == 2 {
				if col+1 >= g.Cols {
					// A wide glyph in the last column would cross the right edge;
					// write a background space instead of half-emitting it.
					if !claimed(cell) {
						bg := cellBGAverageFrom(src, col, row, scheme, run, off, cellCSSW)
						*cell = Cell{Glyph: cell.Glyph, FG: run.FG, BG: bg, Text: " "}
					}
					col++
					off++
					continue
				}
				trailing := g.At(col+1, row)
				if claimed(cell) || claimed(trailing) {
					col += 2
					off += 2
					continue
				}
				bg := cellBGAverageFrom(src, col, row, scheme, run, off, cellCSSW)
				*cell = Cell{Glyph: cell.Glyph, FG: run.FG, BG: bg, Text: cluster}
				*trailing = Cell{Glyph: trailing.Glyph, FG: run.FG, BG: bg, Wide: true}
				col += 2
				off += 2
				continue
			}
			if claimed(cell) {
				col++
				off++
				continue
			}
			bg := cellBGAverageFrom(src, col, row, scheme, run, off, cellCSSW)
			*cell = Cell{Glyph: cell.Glyph, FG: run.FG, BG: bg, Text: cluster}
			col++
			off++
		}
		recomposeBoundaryCellsFrom(g, src, run, scheme, cellCSSW)
	}
}

// recomposeBoundaryCells repaints the two bitmap cells beside the run, at
// grid columns run.Col-1 and run.Col+displayCells, whose source columns the
// run's box overlaps by a sub-cell residue. The overlay never writes these
// cells, so a positive residue paints the box's bitmap glyph sliver into the
// neighbour; averaging only the neighbour's uncovered source columns removes
// that sliver. Each is a background-only recompose (no glyph, no text): the
// cell keeps its half-block or quadrant footprint from the uncovered columns
// alone, the half-cell floor for the boundary case. A boundary cell is touched
// only when it is on the grid, is not itself claimed by an overlay run
// (Text set or a Wide trailing cell), and the run covers at least one of its
// source columns. A run without geometry (cellCSSW <= 0) covers nothing, so the
// whole-cell identity is preserved.
func recomposeBoundaryCellsFrom(g *Grid, src pixelSource, run render.GlyphRun, scheme Scheme, cellCSSW float64) {
	if cellCSSW <= 0 {
		return
	}
	row := run.Row
	// Prefer the run's cached display width; the engine sets it at decode, so the
	// hot path never re-segments here. A hand-built run leaves it zero and falls
	// back to measuring Text, which yields the same count.
	displayCells := run.DisplayCells
	if displayCells == 0 {
		displayCells = runDisplayCells(run.Text)
	}
	for _, gridCol := range [2]int{run.Col - 1, run.Col + displayCells} {
		if gridCol < 0 || gridCol >= g.Cols {
			continue
		}
		cell := g.At(gridCol, row)
		if claimed(cell) {
			continue
		}
		pxW, pxH := cellPxW, cellPxH
		switch scheme {
		case SchemeHalfBlock:
			pxW, pxH = halfCellPxW, halfCellPxH
		case SchemeSextant:
			pxW, pxH = sextCellPxW, sextCellPxH
		}
		covered := coveredColumnsAt(run, gridCol, cellCSSW, pxW)
		if !anyCovered(covered) {
			continue
		}
		bg := averageUncoveredFrom(src, gridCol*pxW, row*pxH, pxW, pxH, covered)
		*cell = Cell{Glyph: lowerHalfBlock, FG: bg, BG: bg}
	}
}

// runDisplayCells is the run's total display-cell width, the sum of each
// grapheme's cell width. It sizes the boundary trailing cell (run.Col + this).
func runDisplayCells(text string) int {
	cells := 0
	gr := uniseg.NewGraphemes(text)
	for gr.Next() {
		cells += render.CellWidth(gr.Str())
	}
	return cells
}

// anyCovered reports whether the mask covers at least one source column.
func anyCovered(mask uint) bool {
	return mask != 0
}

// runCellCSSWidth is the CSS-pixel width of one grid cell the run was snapped
// with. The engine carries this exact value on run.CellW; the render seam does
// not recompute it, because run.W is the untrimmed box width while run.Text is
// trimmed and grid-clipped, so W / display-cells would inflate the width. A zero
// or negative CellW is unknown and returns 0, driving cellBGAverage and the
// boundary recompose back onto the whole-cell average identity.
func runCellCSSWidth(run render.GlyphRun) float64 {
	if run.CellW <= 0 {
		return 0
	}
	return run.CellW
}

// claimed reports whether an earlier overlay run already owns cell c: it carries
// an overlay grapheme (Text) or is the Wide trailing cell of a width-2 glyph.
// A claimed cell is never overwritten under the first-writer-wins rule.
func claimed(c *Cell) bool {
	return c.Text != "" || c.Wide
}

// cellBGAverage averages the uncovered source footprint of cell (col, row) into
// one RGBA colour, in RGBA channel order. The footprint is the scheme's per-cell
// source block: 2x4 (cellPxW x cellPxH) for the quadrant scheme, 1x2
// (halfCellPxW x halfCellPxH) for the half-block scheme, 2x3 (sextCellPxW x
// sextCellPxH) for the sextant scheme. The run's box covers
// part of the cell; averaging only the uncovered source columns keeps the
// covered bitmap ghost out of the recomposed background. The covered span comes
// from run.X and run.W (CSS px) relative to this cell's snapped grid position
// ((run.Col + off) * cellCSSW), quantised to the scheme's source columns.
//
// Half-cell fidelity floor: the quadrant grid has only cellPxW source columns
// per cell (2), so the mask quantises at a half cell; a covered residue finer
// than a half cell is not removed. This is the achievable fix at the block
// sample resolution. When the box covers the whole cell, no geometry is present
// (cellCSSW <= 0), or every source column is covered, the average falls back to
// the whole-cell mean, byte for byte with the geometry-free path.
//
// Out-of-bounds reads return opaque black via pixel, matching the bitmap path.
func cellBGAverage(img *image.RGBA, col, row int, scheme Scheme, run render.GlyphRun, off int, cellCSSW float64) color.RGBA {
	src := rgbaPixelSource{img: img}
	return cellBGAverageFrom(src, col, row, scheme, run, off, cellCSSW)
}

func cellBGAverageFrom(src pixelSource, col, row int, scheme Scheme, run render.GlyphRun, off int, cellCSSW float64) color.RGBA {
	pxW, pxH := cellPxW, cellPxH
	switch scheme {
	case SchemeHalfBlock:
		pxW, pxH = halfCellPxW, halfCellPxH
	case SchemeSextant:
		pxW, pxH = sextCellPxW, sextCellPxH
	}
	covered := coveredColumns(run, off, cellCSSW, pxW)
	x0 := col * pxW
	y0 := row * pxH
	return averageUncoveredFrom(src, x0, y0, pxW, pxH, covered)
}

// averageUncoveredFrom averages the source pixels of the cell block at (x0, y0)
// whose column is not marked covered, in RGBA channel order. covered is a
// bitmask, one bit per source column. When every source column is covered it
// falls back to the whole-cell mean, so a fully-covered cell keeps a sensible
// background rather than black and reproduces the geometry-free identity byte
// for byte.
func averageUncoveredFrom(src pixelSource, x0, y0, pxW, pxH int, covered uint) color.RGBA {
	var rs, gs, bs, n int
	for dx := range pxW {
		if covered&(1<<uint(dx)) != 0 {
			continue
		}
		for dy := range pxH {
			r, gc, b := src.pixelAt(x0+dx, y0+dy)
			rs += int(r)
			gs += int(gc)
			bs += int(b)
		}
		n += pxH
	}
	if n == 0 {
		return wholeCellAverageFrom(src, x0, y0, pxW, pxH)
	}
	return color.RGBA{
		R: clampByte(rs / n),
		G: clampByte(gs / n),
		B: clampByte(bs / n),
		A: 0xff,
	}
}

// coveredColumns returns a pxW-bit mask of the source columns the run's box
// covers within cell (run.Col + off). A zero mask (cellCSSW <= 0, or the box
// covers no whole source column) leaves the whole-cell average intact, so a run
// without geometry reproduces the geometry-free result. The cell is anchored at
// its snapped grid position: its page CSS span is
// [(run.Col + off) * cellCSSW, +cellCSSW), compared against the box's own span
// [run.X, run.X+run.W). Because the box left edge (run.X) need not equal the
// snapped cell edge, a positive residue (box left of the snapped cells) leaves
// the leading cell's left sub-column genuinely uncovered. A source column dx
// counts as covered only when its whole width lies inside the box, so the mask
// never removes a column the box only partly overlaps.
func coveredColumns(run render.GlyphRun, off int, cellCSSW float64, pxW int) uint {
	return coveredColumnsAt(run, run.Col+off, cellCSSW, pxW)
}

// coveredColumnsAt is the general form of coveredColumns: it masks the source
// columns the run's box covers within the grid column gridCol, anchored at that
// column's snapped page position gridCol*cellCSSW. coveredColumns passes the
// run's own cell (run.Col + off); the boundary recompose passes a neighbour
// column the run never wrote.
func coveredColumnsAt(run render.GlyphRun, gridCol int, cellCSSW float64, pxW int) uint {
	if cellCSSW <= 0 {
		return 0
	}
	var mask uint
	cellLeft := float64(gridCol) * cellCSSW
	boxLeft := run.X
	boxRight := run.X + run.W
	colCSSW := cellCSSW / float64(pxW)
	for dx := range pxW {
		colLeft := cellLeft + float64(dx)*colCSSW
		colRight := colLeft + colCSSW
		if colLeft >= boxLeft && colRight <= boxRight {
			mask |= 1 << uint(dx)
		}
	}
	return mask
}

// wholeCellAverageFrom averages every source pixel of the cell block at (x0, y0)
// into one RGBA colour, in RGBA channel order. It is the geometry-free identity:
// the fully-covered and no-geometry fallbacks reproduce it byte for byte.
func wholeCellAverageFrom(src pixelSource, x0, y0, pxW, pxH int) color.RGBA {
	var rs, gs, bs int
	for dy := range pxH {
		for dx := range pxW {
			r, gc, b := src.pixelAt(x0+dx, y0+dy)
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
