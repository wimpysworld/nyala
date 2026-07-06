package engine

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/draw"
	_ "image/jpeg" // register the JPEG decoder for image.Decode
	_ "image/png"  // register the PNG decoder for image.Decode
)

// screencastQuality is a JPEG compression quality value. The live screencast is
// now lossless PNG, so it no longer sets the stream quality; it survives as the
// quality recorded in the capture bundle's screencast config and as the encode
// quality for the JPEG decode benchmark fixture. Lossless frames are what the
// planned per-frame damage diff needs: JPEG's 16x16 MCU decode shifts around every
// change and its banding can flip gradient cells near the 2-colour threshold, so a
// tolerance cannot cleanly catch a real change; PNG gives an exact per-cell diff.
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

// layoutScale is the pure, block-only continuous device scale Z. It returns
// max(1, cols*cellW / layoutWidth), so a grid wider than the target layout width
// shrinks by Z and lays out at the target; a grid at or under the target keeps
// Z == 1. It is continuous (fractional), not stepped, and independent of the
// backend; the ChromiumEngine.layoutScale method gates it to the block backend.
func layoutScale(cols, cellW, layoutWidth int) float64 {
	if layoutWidth <= 0 {
		return 1
	}
	z := float64(cols*cellW) / float64(layoutWidth)
	if z < 1 {
		return 1
	}
	return z
}

// layoutScale returns the layout scale Z for the active backend, read under the
// caller's existing e.mu hold. Only the block backend folds text onto the cell
// grid and needs the shrink, so it returns 1 for a graphics backend and the pure
// layoutScale(cols, cellW, layoutWidth) for block.
func (e *ChromiumEngine) layoutScale() float64 {
	if e.backend != "block" {
		return 1
	}
	return layoutScale(e.cols, e.cellW, e.layoutWidth)
}

// snapshotCellPx returns the fractional CSS-pixel cell size, the forced cell the
// in-page walk snaps origins against and calibration corrects the font-size for.
// The CSS layout viewport is the real surface (surfaceSize) divided by Z: under Z
// the page lays out at surf/Z CSS pixels, so a client rect is in CSS pixels and
// dividing the CSS-pixel viewport by the grid maps a rect to its cell. The result is
// fractional so no premature int cast reintroduces the sub-cell binning error; the
// page rounds only at the final cell bin. A zero grid falls back to the resolved
// cell px.
func (e *ChromiumEngine) snapshotCellPx() (cellW, cellH float64) {
	if e.cols > 0 && e.rows > 0 {
		surfW, surfH := e.surfaceSize(e.cols, e.rows)
		z := e.layoutScale()
		return float64(surfW) / z / float64(e.cols), float64(surfH) / z / float64(e.rows)
	}
	return float64(e.cellW), float64(e.cellH)
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
	return decodeImageBytes(raw)
}

// decodeImageBytes decodes raw image bytes to an *image.RGBA in RGBA channel order.
// It is the shared decode both the base64 screencast payload and the CDP verification
// screenshot feed: page.captureScreenshot's Do already base64-decodes to raw bytes, so
// the verification path skips the base64 step and hands the bytes straight here. A
// non-RGBA decode is converted to RGBA with a bulk draw.Draw in draw.Src mode, which
// runs the same colour conversion the render path expects far faster than a per-pixel
// copy, so the render and anchor paths read one pixel layout.
func decodeImageBytes(raw []byte) (*image.RGBA, error) {
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("engine: decode frame image: %w", err)
	}
	if rgba, ok := img.(*image.RGBA); ok {
		return rgba, nil
	}
	b := img.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(rgba, rgba.Bounds(), img, b.Min, draw.Src)
	return rgba, nil
}
