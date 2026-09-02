package render

// BlockScheme selects how a block grid maps source pixels to cells. It mirrors
// the block package's Scheme enum without importing it: the block package imports
// render, so render must not import block, and this local enum keeps the
// dependency one way.
type BlockScheme int

const (
	// BlockQuadrant maps each cell to a 2x4 source block via the quadrant (2x2)
	// binarisation. It is the zero value and the default scheme.
	BlockQuadrant BlockScheme = iota
	// BlockHalf maps each cell to a 1x2 source column via the half-block floor.
	BlockHalf
	// BlockSextant maps each cell to a 2x3 source block via the sextant (2x3)
	// binarisation.
	BlockSextant
	// BlockShaded maps each cell to a 2x4 source block, the same footprint as
	// BlockQuadrant, scored against the Shaded scheme's shade glyph candidates.
	BlockShaded
)

// BlockCaptureSize returns the source-pixel extent a cols x rows block grid
// samples in the given scheme. Quadrant and Shaded map each cell to a 2x4
// source block, so the extent is (cols*2, rows*4); half-block maps each cell
// to a 1x2 source column, so the extent is (cols, rows*2); sextant maps each
// cell to a 2x3 source block, so the extent is (cols*2, rows*3). The scheme is
// passed as a BlockScheme, not a block.Scheme, because the block package
// imports render, so render must not import block.
func BlockCaptureSize(scheme BlockScheme, cols, rows int) (pxW, pxH int) {
	switch scheme {
	case BlockHalf:
		return cols, rows * 2
	case BlockSextant:
		return cols * 2, rows * 3
	default:
		return cols * 2, rows * 4
	}
}

// GraphicsCaptureSize returns the source-pixel extent a cols x rows grid samples
// for a graphics backend, given the terminal cell pixel size: (cols*cellW,
// rows*cellH). The graphics backends draw a bitmap, so each cell maps to a real
// terminal cell's pixels.
func GraphicsCaptureSize(cols, rows, cellW, cellH int) (pxW, pxH int) {
	return cols * cellW, rows * cellH
}
