package block

import (
	"image/color"
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
)

// TestFitMatchedAspectFills asserts an image whose aspect matches the grid at the
// 1:2 cell aspect fills the whole grid with no letterbox. A 10x10 grid spans
// 10 wide by 20 tall on screen, so a 1:2 (width:height) image fits exactly.
func TestFitMatchedAspectFills(t *testing.T) {
	f := FitImage(100, 200, 10, 10)
	if f.FitCols != 10 || f.FitRows != 10 {
		t.Fatalf("fit extent: got %dx%d, want 10x10", f.FitCols, f.FitRows)
	}
	if f.OffsetCol != 0 || f.OffsetRow != 0 {
		t.Fatalf("offset: got (%d,%d), want (0,0)", f.OffsetCol, f.OffsetRow)
	}
	if n := f.LetterboxCells(); n != 0 {
		t.Fatalf("letterbox cells: got %d, want 0", n)
	}
}

// TestFitSquareLetterboxes asserts a square image in a square grid letterboxes
// top and bottom. Cells are twice as tall as wide, so a square image fills the
// full width but only half the rows. The 10x10 grid keeps 10x5 cells and blanks
// the other 50.
func TestFitSquareLetterboxes(t *testing.T) {
	f := FitImage(100, 100, 10, 10)
	if f.FitCols != 10 || f.FitRows != 5 {
		t.Fatalf("fit extent: got %dx%d, want 10x5", f.FitCols, f.FitRows)
	}
	if f.OffsetCol != 0 || f.OffsetRow != 2 {
		t.Fatalf("offset: got (%d,%d), want (0,2)", f.OffsetCol, f.OffsetRow)
	}
	if n := f.LetterboxCells(); n != 50 {
		t.Fatalf("letterbox cells: got %d, want 50", n)
	}
}

// TestFitTallLetterboxesSides asserts a tall image letterboxes left and right.
// A 100x800 image is taller than the grid spans, so it fills the height and
// keeps a narrow column of cells, blanking the side margins.
func TestFitTallLetterboxesSides(t *testing.T) {
	// Grid 20x20 spans 20 wide by 40 tall. Image aspect 100:800 = 1:8.
	// Height-fill: FitRows=20, FitCols=(20*2*100)/(800*1)=4000/800=5.
	f := FitImage(100, 800, 20, 20)
	if f.FitCols != 5 || f.FitRows != 20 {
		t.Fatalf("fit extent: got %dx%d, want 5x20", f.FitCols, f.FitRows)
	}
	if f.OffsetCol != 7 || f.OffsetRow != 0 {
		t.Fatalf("offset: got (%d,%d), want (7,0)", f.OffsetCol, f.OffsetRow)
	}
	if n := f.LetterboxCells(); n != 20*20-5*20 {
		t.Fatalf("letterbox cells: got %d, want %d", n, 20*20-5*20)
	}
}

// TestFitNeverStretches asserts the filled extent never exceeds the grid and the
// image aspect is preserved (the filled extent stays within the grid bounds), so
// no axis is stretched past the grid.
func TestFitNeverStretches(t *testing.T) {
	cases := []struct{ imgW, imgH, cols, rows int }{
		{1920, 1080, 120, 40},
		{640, 480, 80, 50},
		{1, 1000, 30, 30},
		{1000, 1, 30, 30},
	}
	for _, c := range cases {
		f := FitImage(c.imgW, c.imgH, c.cols, c.rows)
		if f.FitCols < 1 || f.FitCols > c.cols {
			t.Fatalf("%v: FitCols %d out of [1,%d]", c, f.FitCols, c.cols)
		}
		if f.FitRows < 1 || f.FitRows > c.rows {
			t.Fatalf("%v: FitRows %d out of [1,%d]", c, f.FitRows, c.rows)
		}
		if f.OffsetCol < 0 || f.OffsetCol+f.FitCols > c.cols {
			t.Fatalf("%v: column extent escapes grid", c)
		}
		if f.OffsetRow < 0 || f.OffsetRow+f.FitRows > c.rows {
			t.Fatalf("%v: row extent escapes grid", c)
		}
		// Aspect preservation: the on-screen filled extent is FitCols*cellAspectW
		// wide by FitRows*cellAspectH tall and must match the image aspect
		// imgW:imgH. Cross-multiply: fitW*imgH should equal fitH*imgW. Fit rounds
		// one axis down by integer division and clamps at 1, so allow a tolerance
		// of one rounding step on each axis. A bug that fit the wrong axis but
		// stayed in bounds breaks this equality well beyond the tolerance.
		fitW := f.FitCols * cellAspectW
		fitH := f.FitRows * cellAspectH
		lhs := fitW * c.imgH
		rhs := fitH * c.imgW
		diff := lhs - rhs
		if diff < 0 {
			diff = -diff
		}
		tol := cellAspectW*c.imgH + cellAspectH*c.imgW
		if diff > tol {
			t.Fatalf("%v: aspect not preserved: fitW*imgH=%d fitH*imgW=%d diff=%d tol=%d",
				c, lhs, rhs, diff, tol)
		}
	}
}

// TestFitImageDegenerate asserts a non-positive dimension yields a zero-fill Fit
// whose every cell is letterbox.
func TestFitImageDegenerate(t *testing.T) {
	f := FitImage(0, 100, 10, 10)
	if f.FitCols != 0 || f.FitRows != 0 {
		t.Fatalf("degenerate fit extent: got %dx%d, want 0x0", f.FitCols, f.FitRows)
	}
	if n := f.LetterboxCells(); n != 100 {
		t.Fatalf("degenerate letterbox: got %d, want 100", n)
	}
}

// TestFitGridLetterboxBlankCells composes a square image into a square grid and
// asserts the letterbox rows are blank cells while the filled rows carry image
// glyphs. The grid keeps the requested dimensions.
func TestFitGridLetterboxBlankCells(t *testing.T) {
	const cols, rows = 8, 8
	// Square image: fills 8 cols x 4 rows, letterboxes 2 rows top and bottom.
	imgW, imgH := 80, 80
	br := &Renderer{Cols: cols, Rows: rows}
	compose := func(c, r int) Grid {
		sub := &Renderer{Cols: c, Rows: r}
		return sub.composeQuadrant(render.Frame{
			Img: fillRGBA(c*cellPxW, r*cellPxH, color.RGBA{R: 200, G: 120, B: 40, A: 0xff}),
		})
	}
	g := br.FitGrid(imgW, imgH, compose)

	if g.Cols != cols || g.Rows != rows {
		t.Fatalf("grid dimensions: got %dx%d, want %dx%d", g.Cols, g.Rows, cols, rows)
	}
	fit := FitImage(imgW, imgH, cols, rows)
	if fit.FitRows != 4 || fit.OffsetRow != 2 {
		t.Fatalf("precondition: got FitRows=%d OffsetRow=%d, want 4 and 2", fit.FitRows, fit.OffsetRow)
	}

	blank := 0
	for row := 0; row < rows; row++ {
		inFill := row >= fit.OffsetRow && row < fit.OffsetRow+fit.FitRows
		for col := 0; col < cols; col++ {
			c := g.At(col, row)
			if inFill {
				if c.Glyph == ' ' {
					t.Fatalf("filled cell (%d,%d) is blank", col, row)
				}
			} else {
				if *c != blankCell {
					t.Fatalf("letterbox cell (%d,%d): got %v, want blank", col, row, *c)
				}
				blank++
			}
		}
	}
	if want := fit.LetterboxCells(); blank != want {
		t.Fatalf("blank cell count: got %d, want %d", blank, want)
	}
}
