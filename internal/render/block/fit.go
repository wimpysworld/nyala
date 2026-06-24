package block

// cellAspectW and cellAspectH are the on-screen aspect of one terminal cell:
// a cell is twice as tall as wide, so width:height is 1:2. Fit uses this to map
// image pixels to cells without distorting the image on screen.
const (
	cellAspectW = 1
	cellAspectH = 2
)

// Fit is the placement of an image inside a Cols x Rows cell grid. Cols and Rows
// are the grid size; FitCols and FitRows are the cell extent the image fills,
// preserving its aspect at the 1:2 cell aspect; OffsetCol and OffsetRow centre
// that extent inside the grid. The cells outside the extent are the letterbox.
type Fit struct {
	Cols, Rows           int // full grid size in cells
	FitCols, FitRows     int // cells the image fills
	OffsetCol, OffsetRow int // top-left of the filled region
}

// FitImage computes how an imgW x imgH source image places inside a cols x rows
// cell grid. It preserves the image aspect at the assumed 1:2 cell aspect (a cell
// is twice as tall as wide) and never stretches: the image fills the largest
// centred cell extent that keeps its aspect, and the remaining cells letterbox.
// A non-positive grid or image dimension yields a zero-fill Fit.
func FitImage(imgW, imgH, cols, rows int) Fit {
	f := Fit{Cols: cols, Rows: rows}
	if imgW <= 0 || imgH <= 0 || cols <= 0 || rows <= 0 {
		return f
	}

	// On-screen, a cols x rows grid spans cols*cellAspectW wide by
	// rows*cellAspectH tall. The image aspect imgW:imgH must be preserved across
	// the filled cell extent fc x fr, so fc*cellAspectW : fr*cellAspectH equals
	// imgW : imgH. Take the largest such extent that fits the grid.
	gridW := cols * cellAspectW
	gridH := rows * cellAspectH

	// Compare image aspect to grid aspect with cross-multiplication to avoid
	// floating point. imgW/imgH vs gridW/gridH -> imgW*gridH vs imgH*gridW.
	if imgW*gridH >= imgH*gridW {
		// Image is wider than the grid (or equal): fill the width, scale height.
		f.FitCols = cols
		f.FitRows = (cols * cellAspectW * imgH) / (imgW * cellAspectH)
		if f.FitRows < 1 {
			f.FitRows = 1
		}
		if f.FitRows > rows {
			f.FitRows = rows
		}
	} else {
		// Image is taller than the grid: fill the height, scale width.
		f.FitRows = rows
		f.FitCols = (rows * cellAspectH * imgW) / (imgH * cellAspectW)
		if f.FitCols < 1 {
			f.FitCols = 1
		}
		if f.FitCols > cols {
			f.FitCols = cols
		}
	}

	f.OffsetCol = (cols - f.FitCols) / 2
	f.OffsetRow = (rows - f.FitRows) / 2
	return f
}

// LetterboxCells returns the number of grid cells outside the filled region,
// the cells drawn blank to letterbox the remainder.
func (f Fit) LetterboxCells() int {
	return f.Cols*f.Rows - f.FitCols*f.FitRows
}

// blankCell is the cell drawn in the letterbox margin: an opaque-black space.
var blankCell = Cell{Glyph: ' '}

// FitGrid composes the frame onto a fit-and-letterbox grid. It computes the
// placement of the image inside the Cols x Rows grid with FitImage, composes the
// image at FitCols x FitRows so it keeps its aspect, then copies that sub-grid
// into a full Cols x Rows grid centred by the fit offsets. Every cell outside the
// filled region is a blank letterbox cell. The image is never stretched.
func (br *Renderer) FitGrid(imgW, imgH int, compose func(cols, rows int) Grid) Grid {
	fit := FitImage(imgW, imgH, br.Cols, br.Rows)
	out := Grid{
		Cols:  br.Cols,
		Rows:  br.Rows,
		Cells: make([]Cell, br.Cols*br.Rows),
	}
	for i := range out.Cells {
		out.Cells[i] = blankCell
	}
	if fit.FitCols <= 0 || fit.FitRows <= 0 {
		return out
	}
	inner := compose(fit.FitCols, fit.FitRows)
	for row := range inner.Rows {
		for col := range inner.Cols {
			dstCol := fit.OffsetCol + col
			dstRow := fit.OffsetRow + row
			if dstCol < 0 || dstCol >= out.Cols || dstRow < 0 || dstRow >= out.Rows {
				continue
			}
			*out.At(dstCol, dstRow) = *inner.At(col, row)
		}
	}
	return out
}
