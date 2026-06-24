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

// frameChannelBuffer is the buffer size of the emitted-frame channel. A small
// buffer stops a transient restartStream stall from parking the frame loop on the
// frames send while the viewer control goroutine runs the restart; the latest-wins
// slot already bounds upstream backpressure, so a couple of slots is enough.
const frameChannelBuffer = 2

// ackBufferDepth is the buffer size of the ack channel the ack pump drains. The
// latest-wins slot holds at most one unconsumed frame, so at most one consume
// ack plus a small burst of displaced-frame acks are in flight at once; a few
// slots of headroom keep the listener's select-with-default enqueue from ever
// dropping a real ack under normal cadence.
const ackBufferDepth = 8

// snapshotCellPx returns the layout pixels per terminal cell, the divisor
// decodeGlyphRuns floors the DOM box bounds by. The CSS layout viewport is the
// real surface (surfaceSize, set by SetDeviceMetricsOverride and SetContentsSize),
// so the snapshot box bounds are in surface pixels; dividing the surface viewport
// by the grid maps a box to its cell. This is the real cell px (cellW x cellH),
// independent of the sub-cell client-side downscale the block frame undergoes. A
// zero grid falls back to the resolved cell px.
func (e *ChromiumEngine) snapshotCellPx() (cellW, cellH int) {
	if e.cols > 0 && e.rows > 0 {
		surfW, surfH := e.surfaceSize(e.cols, e.rows)
		return surfW / e.cols, surfH / e.rows
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
