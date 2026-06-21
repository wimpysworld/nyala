package metrics

import (
	"image"
	"image/color"
	"math"
	"testing"

	colorful "github.com/lucasb-eyer/go-colorful"

	"github.com/wimpysworld/nyala/internal/capture"
)

// solidImage returns a w x h opaque RGBA image filled with one colour.
func solidImage(w, h int, c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

// halfSplitImage returns an image whose left source columns are black and right
// columns are white, so each 2x4 cell block splits cleanly into a foreground and
// a background set.
func halfSplitImage(cols, rows int) *image.RGBA {
	w, h := cols*oracleCellW, rows*oracleCellH
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x%oracleCellW == 0 {
				img.SetRGBA(x, y, color.RGBA{R: 0, G: 0, B: 0, A: 0xff})
			} else {
				img.SetRGBA(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 0xff})
			}
		}
	}
	return img
}

func newTestRasteriser(t *testing.T) *capture.Rasteriser {
	t.Helper()
	r, err := capture.NewRasteriser()
	if err != nil {
		t.Fatalf("NewRasteriser: %v", err)
	}
	return r
}

// TestLinearMeanBlockAveragesInLinearSpace asserts the cell mean averages in
// linear RGB, not in sRGB. A 50/50 mix of black and white means 0.5 in linear
// light, which encodes back to sRGB ~188, well above the sRGB-space mean of 128.
func TestLinearMeanBlockAveragesInLinearSpace(t *testing.T) {
	// One 2x4 block: left column black, right column white.
	img := image.NewRGBA(image.Rect(0, 0, 2, 4))
	for y := 0; y < 4; y++ {
		img.SetRGBA(0, y, color.RGBA{R: 0, G: 0, B: 0, A: 0xff})
		img.SetRGBA(1, y, color.RGBA{R: 255, G: 255, B: 255, A: 0xff})
	}

	got := linearMeanBlock(img, 0, 0, 2, 4)

	// Expected: linear mean of 0.0 and 1.0 is 0.5, re-encoded to sRGB.
	want := colorful.LinearRgb(0.5, 0.5, 0.5).Clamped()
	wr, wg, wb := want.RGB255()

	if got.R != wr || got.G != wg || got.B != wb {
		t.Fatalf("linear mean = %v, want R%d G%d B%d", got, wr, wg, wb)
	}
	// Guard the headline claim: linear mean of black and white is far above the
	// naive sRGB average of 128.
	if got.R <= 160 {
		t.Fatalf("linear mean R = %d, want well above the sRGB mean of 128", got.R)
	}
	// Sanity: the naive sRGB-space mean would be 127 or 128, which this must beat.
	srgbMean := (0 + 255) / 2
	if int(got.R) <= srgbMean+10 {
		t.Fatalf("linear mean R = %d not clearly above sRGB mean %d", got.R, srgbMean)
	}
}

// TestOtsuMask4 checks the luma split: equal lumas give an all-background mask,
// and a clear two-level split puts the bright sub-cells in the foreground set.
func TestOtsuMask4(t *testing.T) {
	if m := otsuMask4([4]float64{100, 100, 100, 100}); m != 0 {
		t.Fatalf("equal lumas mask = %04b, want 0000", m)
	}
	// Top row dark, bottom row bright: bottom-left (bit1) and bottom-right
	// (bit0) above the split.
	m := otsuMask4([4]float64{10, 10, 240, 240})
	if m != 0b0011 {
		t.Fatalf("dark-top bright-bottom mask = %04b, want 0011", m)
	}
}

// TestIdealRenderDimensions asserts the ideal render lands at the Route A cell
// size for the requested grid.
func TestIdealRenderDimensions(t *testing.T) {
	rast := newTestRasteriser(t)
	cols, rows := 4, 3
	src := halfSplitImage(cols, rows)
	ideal := IdealRender(rast, src, cols, rows)
	wantW, wantH := cols*capture.CellPxW, rows*capture.CellPxH
	if ideal.Bounds().Dx() != wantW || ideal.Bounds().Dy() != wantH {
		t.Fatalf("ideal render %dx%d, want %dx%d",
			ideal.Bounds().Dx(), ideal.Bounds().Dy(), wantW, wantH)
	}
}

// TestKittyScoresCollapseToAbsolute asserts the Kitty property: a lossless
// backend reproduces the input, so for the kitty path the ideal render is the
// input and the relative score equals the absolute score. The test builds a flat
// source whose ideal render and downscaled input agree, renders that ideal as the
// lossless output, then checks relative and absolute match metric by metric.
func TestKittyScoresCollapseToAbsolute(t *testing.T) {
	rast := newTestRasteriser(t)
	cols, rows := 6, 4
	// Flat source: every cell's ideal is one flat colour, equal to the
	// downscaled input, so the ideal render and the aligned input are the same
	// image. This is the kitty case where ideal == input.
	src := solidImage(cols*oracleCellW, rows*oracleCellH, color.RGBA{R: 90, G: 140, B: 200, A: 0xff})
	render := IdealRender(rast, src, cols, rows)

	s := Score(rast, render, src, cols, rows)

	if math.Abs(s.Relative.MSE-s.Absolute.MSE) > 1e-9 {
		t.Errorf("relative MSE %g != absolute MSE %g", s.Relative.MSE, s.Absolute.MSE)
	}
	if math.Abs(s.Relative.SSIM-s.Absolute.SSIM) > 1e-9 {
		t.Errorf("relative SSIM %g != absolute SSIM %g", s.Relative.SSIM, s.Absolute.SSIM)
	}
	if math.Abs(s.Relative.MSSSIM-s.Absolute.MSSSIM) > 1e-9 {
		t.Errorf("relative MSSSIM %g != absolute MSSSIM %g", s.Relative.MSSSIM, s.Absolute.MSSSIM)
	}
	if math.Abs(s.Relative.DeltaEMean-s.Absolute.DeltaEMean) > 1e-9 {
		t.Errorf("relative dE mean %g != absolute %g", s.Relative.DeltaEMean, s.Absolute.DeltaEMean)
	}
	if math.Abs(s.Relative.DeltaEP95-s.Absolute.DeltaEP95) > 1e-9 {
		t.Errorf("relative dE p95 %g != absolute %g", s.Relative.DeltaEP95, s.Absolute.DeltaEP95)
	}
	if math.Abs(s.Relative.HistIntersection-s.Absolute.HistIntersection) > 1e-9 {
		t.Errorf("relative HistIntersection %g != absolute %g",
			s.Relative.HistIntersection, s.Absolute.HistIntersection)
	}
	// The render is the ideal render itself, so the relative score is the
	// identity maximum: zero error and full similarity.
	if s.Relative.MSE != 0 {
		t.Errorf("relative MSE = %g, want 0 for ideal == render", s.Relative.MSE)
	}
	if s.Relative.PSNR != PSNRInfinite {
		t.Errorf("relative PSNR = %g, want the infinite sentinel", s.Relative.PSNR)
	}
}

// TestScoreCollapsesWhenIdealEqualsInput builds a source that is already a flat
// block render so the ideal render and the downscaled input agree. The absolute
// and relative scores then collapse together, the lossless-backend property.
func TestScoreCollapsesWhenIdealEqualsInput(t *testing.T) {
	rast := newTestRasteriser(t)
	cols, rows := 5, 4
	// A flat mid-grey source: every cell's ideal is the same flat colour, and
	// the downscaled input is that same flat colour, so the ideal render and the
	// aligned input match. The render is the ideal render itself (lossless).
	src := solidImage(cols*oracleCellW, rows*oracleCellH, color.RGBA{R: 120, G: 130, B: 140, A: 0xff})
	render := IdealRender(rast, src, cols, rows)

	s := Score(rast, render, src, cols, rows)
	if s.Relative.MSE != 0 {
		t.Fatalf("relative MSE = %g, want 0", s.Relative.MSE)
	}
	// Absolute compares against the downscaled flat input; on a flat field the
	// ideal render is the same flat colour, so the absolute MSE is near zero.
	if s.Absolute.MSE > 1.0 {
		t.Fatalf("absolute MSE = %g, want near 0 on a flat field", s.Absolute.MSE)
	}
}

// TestBestShiftNeverWorseThanNoShift asserts the positional floor never scores
// worse than the no-shift relative score on any metric: errors do not rise,
// similarities do not fall.
func TestBestShiftNeverWorseThanNoShift(t *testing.T) {
	rast := newTestRasteriser(t)
	cols, rows := 8, 5
	src := halfSplitImage(cols, rows)

	// A real render that differs from the ideal, so there is room for a shift to
	// help: render the floor grid (flat cells) instead of the ideal.
	render := rast.Rasterise(FloorGrid(src, cols, rows))

	noShift := ScorePair(render, IdealRender(rast, src, cols, rows))
	best := BestShiftRelative(rast, render, src, cols, rows)

	if best.MSE > noShift.MSE+1e-9 {
		t.Errorf("best-shift MSE %g worse than no-shift %g", best.MSE, noShift.MSE)
	}
	if best.GMSD > noShift.GMSD+1e-9 {
		t.Errorf("best-shift GMSD %g worse than no-shift %g", best.GMSD, noShift.GMSD)
	}
	if best.DeltaEMean > noShift.DeltaEMean+1e-9 {
		t.Errorf("best-shift dE mean %g worse than no-shift %g", best.DeltaEMean, noShift.DeltaEMean)
	}
	if best.DeltaEP95 > noShift.DeltaEP95+1e-9 {
		t.Errorf("best-shift dE p95 %g worse than no-shift %g", best.DeltaEP95, noShift.DeltaEP95)
	}
	if best.SSIM < noShift.SSIM-1e-9 {
		t.Errorf("best-shift SSIM %g worse than no-shift %g", best.SSIM, noShift.SSIM)
	}
	if best.MSSSIM < noShift.MSSSIM-1e-9 {
		t.Errorf("best-shift MSSSIM %g worse than no-shift %g", best.MSSSIM, noShift.MSSSIM)
	}
	if best.PSNR < noShift.PSNR-1e-9 {
		t.Errorf("best-shift PSNR %g worse than no-shift %g", best.PSNR, noShift.PSNR)
	}
	if best.HistIntersection < noShift.HistIntersection-1e-9 {
		t.Errorf("best-shift HistIntersection %g worse than no-shift %g",
			best.HistIntersection, noShift.HistIntersection)
	}
}

// TestIdealGridSplitMatchesGlyph checks the joint fg/bg-and-glyph choice: a
// left-dark right-bright cell yields the right-half-block glyph with the bright
// colour in the foreground.
func TestIdealGridSplitMatchesGlyph(t *testing.T) {
	cols, rows := 1, 1
	// Left source column black, right column white.
	img := image.NewRGBA(image.Rect(0, 0, oracleCellW, oracleCellH))
	for y := 0; y < oracleCellH; y++ {
		img.SetRGBA(0, y, color.RGBA{R: 0, G: 0, B: 0, A: 0xff})
		img.SetRGBA(1, y, color.RGBA{R: 255, G: 255, B: 255, A: 0xff})
	}
	g := IdealGrid(img, cols, rows)
	c := g.At(0, 0)
	// Right column bright: top-right (bit2) and bottom-right (bit0) set => mask
	// 0b0101 maps to the right-half block U+2590.
	if c.Glyph != 0x2590 {
		t.Fatalf("glyph = U+%04X, want U+2590 right half block", c.Glyph)
	}
	if luma601(c.FG) <= luma601(c.BG) {
		t.Fatalf("foreground luma %g not brighter than background %g",
			luma601(c.FG), luma601(c.BG))
	}
}

// TestScoreReusesSharedPath asserts Score and ScorePair agree, so the absolute
// and relative scores run through one comparison path.
func TestScoreReusesSharedPath(t *testing.T) {
	rast := newTestRasteriser(t)
	cols, rows := 4, 3
	src := halfSplitImage(cols, rows)
	render := IdealRender(rast, src, cols, rows)

	s := Score(rast, render, src, cols, rows)
	direct := ScorePair(render, IdealRender(rast, src, cols, rows))
	if math.Abs(s.Relative.SSIM-direct.SSIM) > 1e-12 {
		t.Fatalf("Score relative SSIM %g != ScorePair %g", s.Relative.SSIM, direct.SSIM)
	}
}
