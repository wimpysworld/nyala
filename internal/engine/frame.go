package engine

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/jpeg" // register the JPEG decoder for image.Decode
	_ "image/png"  // register the PNG decoder for image.Decode
)

// deviceScaleFactor is the device pixel ratio the engine pins via DSF 1, so
// captured pixels equal CSS pixels and the DOM box decode divides by 1.
const deviceScaleFactor = 1.0

// screencastQuality is the JPEG compression quality for the screencast. The
// bitmap is downsampled into 2-colour cells, so a high quality keeps JPEG loss
// below cell visibility while keeping the payload small for the moving viewport.
const screencastQuality = 90

// snapshotCellPx returns the capture pixels per terminal cell, the divisor
// decodeGlyphRuns floors the DOM box bounds by. The CSS viewport equals the
// capture size (set by SetDeviceMetricsOverride), so the snapshot bounds are in
// capture pixels; dividing the capture size by the grid maps a box to its cell.
// This holds for both the sub-cell block capture and the cell-pixel graphics
// capture. A zero grid falls back to the resolved cell px.
func (e *ChromiumEngine) snapshotCellPx() (cellW, cellH int) {
	if e.cols > 0 && e.rows > 0 {
		return e.captureW / e.cols, e.captureH / e.rows
	}
	return e.cellW, e.cellH
}

// decodeScreencastImage decodes one base64 screencast payload to an *image.RGBA
// in RGBA channel order. EventScreencastFrame.Data is base64, so it is decoded to
// bytes before the image decoder. A non-RGBA decode is converted with the same
// copy idiom the viewer's LoadPNG uses, so the render path reads one pixel layout.
func decodeScreencastImage(data string) (*image.RGBA, error) {
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, fmt.Errorf("engine: base64-decode frame: %w", err)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("engine: decode frame image: %w", err)
	}
	if rgba, ok := img.(*image.RGBA); ok {
		return rgba, nil
	}
	b := img.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			rgba.Set(x-b.Min.X, y-b.Min.Y, img.At(x, y))
		}
	}
	return rgba, nil
}
