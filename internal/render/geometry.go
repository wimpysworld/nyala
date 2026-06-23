package render

// BlockCaptureSize returns the source-pixel extent a cols x rows block grid
// samples. The quadrant scheme maps each cell to a 2x4 source block, so the
// extent is (cols*2, rows*4); the half-block scheme maps each cell to a 1x2
// source column, so the extent is (cols, rows*2). The quadrant flag selects the
// scheme: true for quadrant, false for half-block. The scheme is passed as a
// bool, not a block.Scheme, because the block package imports render, so render
// must not import block.
func BlockCaptureSize(quadrant bool, cols, rows int) (pxW, pxH int) {
	if quadrant {
		return cols * 2, rows * 4
	}
	return cols, rows * 2
}

// GraphicsCaptureSize returns the source-pixel extent a cols x rows grid samples
// for a graphics backend, given the terminal cell pixel size: (cols*cellW,
// rows*cellH). The graphics backends draw a bitmap, so each cell maps to a real
// terminal cell's pixels.
func GraphicsCaptureSize(cols, rows, cellW, cellH int) (pxW, pxH int) {
	return cols * cellW, rows * cellH
}
