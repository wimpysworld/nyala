//go:build ocr

// The OCR half of the calibration: the gosseract CER ladder. It builds only
// under the "ocr" build tag, so the default build and the shipped nyala binary
// stay cgo-free. It renders the "Text Size NNpx" ladder at the pinned sizes,
// OCRs each rung through a live Tesseract engine, records the CER and WER per
// size, and merges the ladder into the committed calibration file. It skips when
// the engine is absent, so the tagged run stays green without Tesseract.
//
// The CER per size is measured on a rendered ladder, the same path the OCR
// ladder test exercises. The card label records the ladder's provenance;
// gosseract reads the rendered glyph image, so the CER does not vary by display
// backend.
package harness

import (
	"image"
	"image/color"
	"image/draw"
	"testing"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"github.com/wimpysworld/nyala/internal/ocr"
	rfont "github.com/wimpysworld/nyala/internal/render/font"
)

// calibCERBound is the CER bound the calibration ladder marks legibility at. It
// matches the bound the OCR ladder test uses, so the recorded minimum legible
// size is comparable.
const calibCERBound = 0.15

// calibLadderRowH is the per-rung row height in the rendered calibration ladder,
// in source pixels. It is tall enough to hold the largest rung with margin.
const calibLadderRowH = 48

// renderCalibLine draws s as dark text on a light ground at the given pixel cap
// height using the embedded FiraCode Nerd Font Mono face, mirroring the OCR package's render
// helper so the calibration ladder matches the ladder test's geometry.
func renderCalibLine(t *testing.T, s string, capPx int) *image.RGBA {
	t.Helper()
	a, err := rfont.Load()
	if err != nil {
		t.Fatalf("font atlas load: %v", err)
	}
	fnt, _, ok := a.Resolve('A')
	if !ok {
		t.Fatalf("font atlas has no glyph for 'A'")
	}
	size := float64(capPx) / 0.7
	face, err := opentype.NewFace(fnt, &opentype.FaceOptions{Size: size, DPI: 72})
	if err != nil {
		t.Fatalf("new face: %v", err)
	}
	defer face.Close()

	adv := font.MeasureString(face, s)
	w := adv.Ceil() + 8
	h := int(size*1.4) + 8
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 255 // opaque white ground
	}
	d := font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(color.RGBA{A: 255}),
		Face: face,
		Dot:  fixed.P(4, int(size)),
	}
	d.DrawString(s)
	return img
}

// renderCalibLadder stacks one "Text Size NNpx" line per size into a tall
// left-column ladder image, returning the composite for the OCR pass.
func renderCalibLadder(t *testing.T, sizes []int) *image.RGBA {
	t.Helper()
	maxW := 0
	lines := make([]*image.RGBA, len(sizes))
	for i, sz := range sizes {
		img := renderCalibLine(t, ocr.LadderText(sz), sz)
		lines[i] = img
		if img.Bounds().Dx() > maxW {
			maxW = img.Bounds().Dx()
		}
	}
	out := image.NewRGBA(image.Rect(0, 0, maxW+8, calibLadderRowH*len(sizes)))
	for i := range out.Pix {
		out.Pix[i] = 255 // opaque white ground
	}
	for i, img := range lines {
		dy := i*calibLadderRowH + (calibLadderRowH-img.Bounds().Dy())/2
		if dy < i*calibLadderRowH {
			dy = i * calibLadderRowH
		}
		r := image.Rect(4, dy, 4+img.Bounds().Dx(), dy+img.Bounds().Dy())
		draw.Draw(out, r, img, img.Bounds().Min, draw.Over)
	}
	return out
}

// TestCalibrationOCR measures the gosseract CER per ladder size and merges the
// ladder into the committed calibration file. It skips when Tesseract is absent.
// Under UPDATE_CALIBRATION it rewrites the committed file carrying both the
// metric ranges and the freshly measured CER ladder.
func TestCalibrationOCR(t *testing.T) {
	eng, err := ocr.NewEngine()
	if err != nil {
		t.Skipf("Tesseract engine unavailable, skipping CER calibration: %v", err)
	}
	defer eng.Close()

	sizes := ocr.LadderSizes
	src := renderCalibLadder(t, sizes)
	spec := ocr.Spec{Sizes: sizes, RowHeight: calibLadderRowH}

	results, err := ocr.Ladder(src, spec, eng, calibCERBound)
	if err != nil {
		t.Fatalf("Ladder: %v", err)
	}

	rungs := make([]CalibLadderRung, 0, len(results))
	for _, r := range results {
		rungs = append(rungs, CalibLadderRung{
			SizePx:  r.SizePx,
			CER:     r.Result.CER,
			WER:     r.Result.WER,
			Legible: r.Legible,
		})
		t.Logf("CER ladder %dpx CER=%.3f WER=%.3f legible=%v",
			r.SizePx, r.Result.CER, r.Result.WER, r.Legible)
	}
	var minLeg *int
	if m, ok := ocr.MinLegibleSize(results); ok {
		minLeg = &m
	}

	// Rebuild the metric ranges so the merged file carries both halves.
	rast := newRasteriser(t)
	cal := MeasureCalibration(rast, loadCalibInputs(t))
	cal.OCRBuilt = true
	cal.CERBound = calibCERBound
	cal.Ladder = []CalibCardLadder{{
		Card:         "synthetic-ladder",
		Rungs:        rungs,
		MinLegiblePx: minLeg,
	}}

	update := updateCalibration()
	ok, wrote, err := CompareCalibration(calibrationPath(), cal, update)
	if err != nil {
		t.Fatalf("CompareCalibration: %v", err)
	}
	if !ok {
		t.Fatalf("calibration OCR ladder mismatch; rerun with UPDATE_CALIBRATION=1 to refresh")
	}
	if update && wrote {
		t.Logf("calibration with CER ladder written to %s", calibrationPath())
	}
}
