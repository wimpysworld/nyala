//go:build diagnostics && ocr

// The gosseract differentiator for the overlay grade. It builds the OCR ladder
// twice: once as a crisp text overlay grid, once as the Stage 0 bitmap-only
// floor, OCRs both through the same gosseract engine, and reports the overlay CER
// against the bitmap CER. The overlay reads page text where the bitmap floor's
// 10 px legibility limit fails, so the overlay CER should sit well below the
// bitmap CER. It reports only; Tesseract output varies, so it is never a gate.
//
// It carries both the diagnostics and ocr tags: ocr.NewEngine needs cgo and
// lives behind the ocr tag in tesseract.go, so this hook stays apart from the
// untagged matcher track. Channel order is RGBA throughout.
package harness

import (
	"image"
	"image/color"
	"testing"

	"github.com/wimpysworld/nyala/internal/capture"
	"github.com/wimpysworld/nyala/internal/ocr"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// overlayLadderCellRows is the per-rung row height of the overlay ladder grid in
// cells, chosen so the rasterised overlay rung matches the rasterised bitmap
// rung in output pixels. The bitmap path composes the ladder (a quadrant cell
// spans four source rows, so ladderRowH source pixels become ladderRowH/4 cells)
// and rasterises each cell to capture.CellPxH output pixels, giving a bitmap rung
// of ladderRowH/4 * capture.CellPxH output pixels. The overlay grid rasterises
// through the same cell height, so ladderRowH/4 cells per rung reproduces that
// output rung height.
const overlayLadderCellRows = ladderRowH / 4

// overlayLadderRowHeight is the per-rung crop height in output pixels for the
// rasterised ladder images. Both the bitmap and overlay ladders rasterise at
// capture.CellPxH per cell, so a rung occupies overlayLadderCellRows cells, i.e.
// overlayLadderCellRows * capture.CellPxH output pixels. The OCR spec must crop
// this output-pixel band, not the ladderRowH source-pixel band, or the overlay
// text (centred in the output rung) falls outside the crop.
const overlayLadderRowHeight = overlayLadderCellRows * capture.CellPxH

// TestOverlayGosseractDifferentiator reports the gosseract CER on the overlay
// ladder against the bitmap ladder. It is a reported measure, not a gate, so it
// never fails on Tesseract variance; it skips when no engine is present.
func TestOverlayGosseractDifferentiator(t *testing.T) {
	eng, err := ocr.NewEngine()
	if err != nil {
		t.Skipf("no Tesseract engine: %v", err)
	}
	defer eng.Close()

	rast := newRasteriser(t)
	sizes := ocr.LadderSizes

	bitmapLadder, err := buildLadder(sizes)
	if err != nil {
		t.Fatalf("buildLadder: %v", err)
	}
	bitmapImg := roundTripBlockLadder(rast, bitmapLadder)

	overlayGrid := overlayLadderGrid(sizes, bitmapLadder.Bounds().Dx()/2)
	overlayImg := rast.Rasterise(overlayGrid)

	spec := ocr.Spec{Sizes: sizes, RowHeight: overlayLadderRowHeight}
	bitmapCER := meanLadderCER(t, bitmapImg, spec, eng)
	overlayCER := meanLadderCER(t, overlayImg, spec, eng)

	t.Logf("overlay gosseract differentiator: overlay CER %.4f vs bitmap CER %.4f", overlayCER, bitmapCER)
	if overlayCER > bitmapCER {
		t.Logf("note: overlay CER above bitmap CER this run; reported only, Tesseract output varies")
	}
}

// overlayLadderGrid builds an overlay grid sized to the bitmap ladder, placing
// each rung's "Text Size NNpx" run as a white-on-black text overlay one rung per
// band. The grid has cols columns and one band of overlayLadderCellRows cells per
// rung, so the rasterised image lines up with the bitmap ladder spec geometry.
func overlayLadderGrid(sizes []int, cols int) block.Grid {
	if cols < 1 {
		cols = 1
	}
	white := color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}
	black := color.RGBA{A: 0xFF}
	rows := overlayLadderCellRows * len(sizes)
	g := block.Grid{Cols: cols, Rows: rows, Cells: make([]block.Cell, cols*rows)}
	for i := range g.Cells {
		g.Cells[i] = block.Cell{FG: white, BG: black}
	}
	for i, sz := range sizes {
		row := i*overlayLadderCellRows + overlayLadderCellRows/2
		col := 1
		for _, r := range ocr.LadderText(sz) {
			if row < 0 || row >= g.Rows || col < 0 || col >= g.Cols {
				col++
				continue
			}
			*g.At(col, row) = block.Cell{FG: white, BG: black, Text: string(r)}
			col++
		}
	}
	return g
}

// meanLadderCER OCRs each rung of img through eng over the spec geometry and
// returns the mean CER across the rungs. An empty result returns 1, the worst
// CER, so the report still prints a comparable number.
func meanLadderCER(t *testing.T, img *image.RGBA, spec ocr.Spec, eng ocr.LineRecogniser) float64 {
	t.Helper()
	results, err := ocr.Ladder(img, spec, eng, ladderCERBound)
	if err != nil {
		t.Fatalf("ocr.Ladder: %v", err)
	}
	if len(results) == 0 {
		return 1
	}
	var sum float64
	for _, r := range results {
		sum += r.Result.CER
	}
	return sum / float64(len(results))
}
