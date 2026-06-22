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
	"testing"

	"github.com/wimpysworld/nyala/internal/ocr"
)

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
	src, err := buildLadder(sizes)
	if err != nil {
		t.Fatalf("buildLadder: %v", err)
	}
	spec := ocr.Spec{Sizes: sizes, RowHeight: ladderRowH}

	results, err := ocr.Ladder(src, spec, eng, ladderCERBound)
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
	cal, err := MeasureCalibration(rast, loadCalibInputs(t), logProgress(t))
	if err != nil {
		t.Fatalf("MeasureCalibration: %v", err)
	}
	cal.OCRBuilt = true
	cal.CERBound = ladderCERBound
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
