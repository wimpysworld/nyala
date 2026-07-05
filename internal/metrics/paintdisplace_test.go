package metrics

import (
	"image"
	"math"
	"testing"
)

const paintEps = 1e-9

// paintCellW and paintCellH are the device cell size the paint tests use, matching
// the deterministic headless 8x16 capture. With CellW 8, the device scale is 1, so a
// device pixel is a CSS pixel and box coordinates place directly on the bitmap.
const (
	paintCellW = 8
	paintCellH = 16
)

// newPaintImage returns a white RGBA image of the given device size.
func newPaintImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255
		img.Pix[i+1] = 255
		img.Pix[i+2] = 255
		img.Pix[i+3] = 255
	}
	return img
}

// paintGlyphBlock paints a solid dark block at device rectangle [x0,x1) x [y0,y1),
// standing in for a run's leftmost painted glyph ink.
func paintGlyphBlock(img *image.RGBA, x0, y0, x1, y1 int, c PaintColour) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			off := img.PixOffset(x, y)
			img.Pix[off] = c.R
			img.Pix[off+1] = c.G
			img.Pix[off+2] = c.B
			img.Pix[off+3] = 255
		}
	}
}

// TestPaintDisplacementAligned checks a run whose painted ink sits exactly at its
// DOM-snapped cell scores zero displacement on both axes. The glyph paints at device
// x 24 (col 3) and y 48 (row 3), matching Col 3 Row 3.
func TestPaintDisplacementAligned(t *testing.T) {
	img := newPaintImage(120, 80)
	dark := PaintColour{R: 51, G: 51, B: 51}
	paintGlyphBlock(img, 24, 48, 30, 56, dark)
	runs := []PaintRun{
		{Text: "hi", Col: 3, Row: 3, FG: dark, X: 24, Y: 48, W: 16, H: 16, CellW: paintCellW},
	}
	got := PaintDisplacement(img, runs, paintCellW, paintCellH)
	if got.Measured != 1 {
		t.Fatalf("Measured = %d, want 1", got.Measured)
	}
	if math.Abs(got.MeanAbsH) > paintEps || math.Abs(got.MaxAbsH) > paintEps {
		t.Errorf("horizontal displacement = mean %v max %v, want ~0", got.MeanAbsH, got.MaxAbsH)
	}
	if math.Abs(got.MeanAbsV) > paintEps || math.Abs(got.MaxAbsV) > paintEps {
		t.Errorf("vertical displacement = mean %v max %v, want ~0", got.MeanAbsV, got.MaxAbsV)
	}
	if got.SkippedFraction != 0 {
		t.Errorf("SkippedFraction = %v, want 0", got.SkippedFraction)
	}
}

// TestPaintDisplacementHorizontalShift checks a run whose painted ink sits two cells
// right of its DOM-snapped column reports a +2 horizontal displacement. The DOM says
// Col 3 but the paint lands at device x 40 (col 5).
func TestPaintDisplacementHorizontalShift(t *testing.T) {
	img := newPaintImage(120, 80)
	dark := PaintColour{R: 51, G: 51, B: 51}
	paintGlyphBlock(img, 40, 48, 46, 56, dark)
	runs := []PaintRun{
		{Text: "hi", Col: 3, Row: 3, FG: dark, X: 24, Y: 48, W: 16, H: 16, CellW: paintCellW},
	}
	got := PaintDisplacement(img, runs, paintCellW, paintCellH)
	if got.Measured != 1 {
		t.Fatalf("Measured = %d, want 1", got.Measured)
	}
	if math.Abs(got.MeanAbsH-2) > paintEps {
		t.Errorf("MeanAbsH = %v, want 2", got.MeanAbsH)
	}
	if len(got.PerRun) != 1 || got.PerRun[0].H != 2 {
		t.Errorf("PerRun H = %+v, want H 2", got.PerRun)
	}
	if math.Abs(got.MeanAbsV) > paintEps {
		t.Errorf("MeanAbsV = %v, want 0", got.MeanAbsV)
	}
}

// TestPaintDisplacementVerticalShift checks a run whose painted ink sits one cell
// below its DOM-snapped row reports a +1 vertical displacement. The DOM says Row 3 but
// the paint lands at device y 64 (row 4), one cell below the box band.
func TestPaintDisplacementVerticalShift(t *testing.T) {
	img := newPaintImage(120, 96)
	dark := PaintColour{R: 51, G: 51, B: 51}
	paintGlyphBlock(img, 24, 64, 30, 72, dark)
	runs := []PaintRun{
		{Text: "hi", Col: 3, Row: 3, FG: dark, X: 24, Y: 48, W: 16, H: 16, CellW: paintCellW},
	}
	got := PaintDisplacement(img, runs, paintCellW, paintCellH)
	if got.Measured != 1 {
		t.Fatalf("Measured = %d, want 1", got.Measured)
	}
	if math.Abs(got.MeanAbsV-1) > paintEps {
		t.Errorf("MeanAbsV = %v, want 1", got.MeanAbsV)
	}
	if math.Abs(got.MeanAbsH) > paintEps {
		t.Errorf("MeanAbsH = %v, want 0", got.MeanAbsH)
	}
}

// TestPaintDisplacementVerticalDrift checks the vertical blind-spot fix: a run whose
// painted ink sits three cells below its DOM-snapped row is now MEASURED as vdrift +3,
// not skipped. The old tight half-cell band returned no-ink at this distance and the
// run vanished from the score; the bounded nearest-first row window catches it. The DOM
// says Row 3 but the paint lands at device y 96 (row 6).
func TestPaintDisplacementVerticalDrift(t *testing.T) {
	img := newPaintImage(120, 128)
	dark := PaintColour{R: 51, G: 51, B: 51}
	paintGlyphBlock(img, 24, 96, 30, 104, dark)
	runs := []PaintRun{
		{Text: "hi", Col: 3, Row: 3, FG: dark, X: 24, Y: 48, W: 16, H: 16, CellW: paintCellW},
	}
	got := PaintDisplacement(img, runs, paintCellW, paintCellH)
	if got.Measured != 1 {
		t.Fatalf("Measured = %d, want 1", got.Measured)
	}
	if len(got.PerRun) != 1 || got.PerRun[0].V != 3 {
		t.Errorf("PerRun V = %+v, want V 3", got.PerRun)
	}
	if math.Abs(got.MeanAbsH) > paintEps {
		t.Errorf("MeanAbsH = %v, want 0", got.MeanAbsH)
	}
	if got.SkippedFraction != 0 {
		t.Errorf("SkippedFraction = %v, want 0 (the drifted run is measured, not skipped)", got.SkippedFraction)
	}
}

// TestPaintDisplacementLeftDrift checks the widened horizontal window: a run whose only
// painted ink sits one cell LEFT of its DOM-snapped column is now MEASURED as hdrift
// -1, not skipped. The old left pad reached only a fraction of a cell and let a
// one-cell-left run escape; the symmetric nearest-first column window finds it. The DOM
// says Col 3 but the paint lands at device x 16 (col 2).
func TestPaintDisplacementLeftDrift(t *testing.T) {
	img := newPaintImage(120, 80)
	dark := PaintColour{R: 51, G: 51, B: 51}
	paintGlyphBlock(img, 16, 48, 22, 56, dark)
	runs := []PaintRun{
		{Text: "hi", Col: 3, Row: 3, FG: dark, X: 24, Y: 48, W: 16, H: 16, CellW: paintCellW},
	}
	got := PaintDisplacement(img, runs, paintCellW, paintCellH)
	if got.Measured != 1 {
		t.Fatalf("Measured = %d, want 1", got.Measured)
	}
	if len(got.PerRun) != 1 || got.PerRun[0].H != -1 {
		t.Errorf("PerRun H = %+v, want H -1", got.PerRun)
	}
	if got.SkippedFraction != 0 {
		t.Errorf("SkippedFraction = %v, want 0 (the left-drifted run is measured, not skipped)", got.SkippedFraction)
	}
}

// TestPaintDisplacementSkipsWideScript checks a wide-script run is skipped, not
// scored, and counts toward SkippedFraction. No ink is painted for it, but the skip is
// the script test, not the missing ink.
func TestPaintDisplacementSkipsWideScript(t *testing.T) {
	img := newPaintImage(120, 80)
	dark := PaintColour{R: 51, G: 51, B: 51}
	runs := []PaintRun{
		{Text: "世界", Col: 3, Row: 3, FG: dark, X: 24, Y: 48, W: 32, H: 16, CellW: paintCellW},
	}
	got := PaintDisplacement(img, runs, paintCellW, paintCellH)
	if got.Measured != 0 {
		t.Errorf("Measured = %d, want 0", got.Measured)
	}
	if got.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", got.Skipped)
	}
	if got.SkippedFraction != 1 {
		t.Errorf("SkippedFraction = %v, want 1", got.SkippedFraction)
	}
}

// TestPaintDisplacementSkipsUnreadableInk checks a run whose ink the window never
// finds is skipped and counted, so an unreadable run inflates SkippedFraction rather
// than scoring a false zero. The DOM box is at col 3, but the only dark ink sits far
// right, outside the four-cell window.
func TestPaintDisplacementSkipsUnreadableInk(t *testing.T) {
	img := newPaintImage(200, 80)
	dark := PaintColour{R: 51, G: 51, B: 51}
	paintGlyphBlock(img, 160, 48, 166, 56, dark)
	runs := []PaintRun{
		{Text: "hi", Col: 3, Row: 3, FG: dark, X: 24, Y: 48, W: 16, H: 16, CellW: paintCellW},
	}
	got := PaintDisplacement(img, runs, paintCellW, paintCellH)
	if got.Measured != 0 {
		t.Errorf("Measured = %d, want 0", got.Measured)
	}
	if got.Skipped != 1 || got.SkippedFraction != 1 {
		t.Errorf("Skipped = %d frac %v, want 1 and 1", got.Skipped, got.SkippedFraction)
	}
}

// TestPaintDisplacementSkipsLowContrast checks a run whose foreground barely differs
// from its band background is skipped, not scored on paint the colour scan cannot
// separate from noise. A near-white run on a white page is unmeasurable.
func TestPaintDisplacementSkipsLowContrast(t *testing.T) {
	img := newPaintImage(120, 80)
	faint := PaintColour{R: 250, G: 250, B: 250}
	paintGlyphBlock(img, 24, 48, 30, 56, faint)
	runs := []PaintRun{
		{Text: "hi", Col: 3, Row: 3, FG: faint, X: 24, Y: 48, W: 16, H: 16, CellW: paintCellW},
	}
	got := PaintDisplacement(img, runs, paintCellW, paintCellH)
	if got.Measured != 0 {
		t.Errorf("Measured = %d, want 0", got.Measured)
	}
	if got.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", got.Skipped)
	}
}

// TestPaintDisplacementLeftNeighbour checks nearest-first does not mistake a
// left-neighbour's ink for the run's own start. A neighbour glyph paints one cell left
// of the box (col 2); the run's own ink sits at its box col 3. The scan sweeps columns
// nearest first from the box origin, so it finds the run's own ink at col 3 (offset 0)
// before the neighbour at col 2, and reports zero displacement.
func TestPaintDisplacementLeftNeighbour(t *testing.T) {
	img := newPaintImage(120, 80)
	dark := PaintColour{R: 51, G: 51, B: 51}
	// Neighbour ink one cell left (col 2).
	paintGlyphBlock(img, 16, 48, 22, 56, dark)
	// The run's own ink at its box col 3.
	paintGlyphBlock(img, 24, 48, 30, 56, dark)
	runs := []PaintRun{
		{Text: "hi", Col: 3, Row: 3, FG: dark, X: 24, Y: 48, W: 16, H: 16, CellW: paintCellW},
	}
	got := PaintDisplacement(img, runs, paintCellW, paintCellH)
	if got.Measured != 1 {
		t.Fatalf("Measured = %d, want 1", got.Measured)
	}
	if got.PerRun[0].H != 0 {
		t.Errorf("PerRun H = %v, want 0 (nearest-first finds the run's own ink at the box origin)", got.PerRun[0].H)
	}
}

// TestPaintDisplacementNilAndEmpty checks the guards: a nil image, a non-positive
// cell, and no runs each return the zero value.
func TestPaintDisplacementNilAndEmpty(t *testing.T) {
	runs := []PaintRun{{Text: "hi", Col: 0, Row: 0, X: 0, Y: 0, W: 8, H: 16, CellW: paintCellW}}
	if got := PaintDisplacement(nil, runs, paintCellW, paintCellH); got.Measured != 0 || got.Skipped != 0 {
		t.Errorf("nil image = %+v, want zero", got)
	}
	img := newPaintImage(40, 40)
	if got := PaintDisplacement(img, runs, 0, paintCellH); got.Measured != 0 {
		t.Errorf("zero cell width = %+v, want zero", got)
	}
	if got := PaintDisplacement(img, nil, paintCellW, paintCellH); got.Measured != 0 {
		t.Errorf("no runs = %+v, want zero", got)
	}
}
