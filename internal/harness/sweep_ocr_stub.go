//go:build !ocr

// The default-build OCR hook for the sweep. It builds when the "ocr" build tag
// is absent, so the default build and the shipped nyala binary stay cgo-free. It
// returns null OCR scores, so the sweep emits null ocrCER, ocrWER, and
// minLegiblePx without a Tesseract engine. The tagged file (sweep_ocr.go) fills
// these fields from a live ladder.
package harness

import (
	"image"

	"github.com/wimpysworld/nyala/internal/capture"
)

// ocrScores returns null OCR scores in the default build. The rasteriser, the
// mode, and the captured render are unused here; the tagged build reads them to
// run a live OCR ladder. Channel order is RGBA.
func ocrScores(_ *capture.Rasteriser, _ Mode, _ *image.RGBA) (cer, wer *float64, minLegiblePx *int) {
	return nil, nil, nil
}
