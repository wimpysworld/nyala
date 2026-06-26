// The sweep's OCR-score hook. The sweep emits null ocrCER, ocrWER, and
// minLegiblePx, so the shipped nyala binary and every build stay cgo-free. The
// pure-Go matcher (internal/ocr) still drives the Stage 2 diagnostics gate.
package harness

import (
	"image"

	"github.com/wimpysworld/nyala/internal/capture"
)

// ocrScores returns null OCR scores. The rasteriser, the mode, and the captured
// render are unused. Channel order is RGBA.
func ocrScores(_ *capture.Rasteriser, _ Mode, _ *image.RGBA) (cer, wer *float64, minLegiblePx *int) {
	return nil, nil, nil
}
