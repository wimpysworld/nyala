package engine

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	_ "image/jpeg" // register the JPEG decoder for the image.Decode JPEG fallback
	"sync"
	"unsafe"

	emmb64 "github.com/emmansun/base64"
	"github.com/gen2brain/jpegn"
	woozypng "github.com/woozymasta/png"
)

// pngSignature is the 8-byte PNG file magic. decodeFrameImage sniffs it to route
// PNG frames explicitly through the woozymasta decoder instead of the image.Decode
// registry, so a second registered "png" format cannot make dispatch init-order
// dependent (correctness packages also import stdlib image/png for encoding).
var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// decodeFrameImage decodes one raw image payload, routing by file signature. A PNG
// (matched by its 8-byte magic) decodes through woozymasta/png, a byte-identical and
// faster drop-in for image/png; everything else (JPEG) decodes through the stdlib
// image.Decode registry. The explicit PNG route avoids registering a second "png"
// format alongside correctness code's stdlib image/png import, which would make
// image.Decode dispatch init-order dependent.
func decodeFrameImage(raw []byte) (image.Image, error) {
	if bytes.HasPrefix(raw, pngSignature) {
		return woozypng.Decode(bytes.NewReader(raw))
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	return img, err
}

// base64DecodeBufPool pools the decoded-bytes output buffer for decodeBase64Pooled,
// so the ~188 KB output allocation each screencast frame makes is reused across frames
// instead of allocated fresh. sync.Pool is safe under the concurrent live decode-pool
// workers: each Get hands one worker its own buffer.
var base64DecodeBufPool = sync.Pool{New: func() any { b := make([]byte, 0); return &b }}

// decodeBase64Pooled decodes one base64 screencast payload into a pooled output buffer,
// returning the raw bytes plus a release func the caller must call once the downstream
// image decode has finished with the bytes. It prefers the SIMD emmansun Decode(dst,
// src) form, which decodes into the reused pooled dst and so makes no per-call output
// allocation. A decode error is authoritative and returns the borrowed buffer to the
// pool before reaching the caller.
//
// src is a read-only, zero-copy view of data via unsafe.Slice: the decoder only reads
// src, never writes it, so aliasing the string's backing bytes is safe, and it avoids
// the ~245 KB string-to-[]byte copy a plain conversion would make. This mirrors what
// emmansun's own DecodeString does internally.
//
// The returned raw slice aliases the pooled buffer, so it must not escape past release:
// decodeScreencastNative and decodeLiveFrame consume it synchronously into an independent
// image and defer release before returning, so the buffer never reaches the decode
// collector or the emitted frame.
func decodeBase64Pooled(data string) (raw []byte, release func(), err error) {
	decLen := emmb64.StdEncoding.DecodedLen(len(data))
	bp, _ := base64DecodeBufPool.Get().(*[]byte)
	buf := *bp
	if cap(buf) < decLen {
		buf = make([]byte, decLen)
	} else {
		buf = buf[:decLen]
	}
	// Read-only zero-copy view of the string bytes; the decoder never writes src.
	src := unsafe.Slice(unsafe.StringData(data), len(data))
	n, derr := emmb64.StdEncoding.Decode(buf, src)
	*bp = buf
	if derr == nil {
		return buf[:n], func() { base64DecodeBufPool.Put(bp) }, nil
	}
	base64DecodeBufPool.Put(bp)
	return nil, nil, derr
}

// screencastQuality is the JPEG quality used by the live Block screencast.
// Kitty and Sixel use PNG, so this value does not select their wire format.
const screencastQuality = 90

// blockScreencastQuality is the JPEG quality the block backend uses for its live
// stream. Quality 90 retains page detail while native-YCbCr composition keeps the
// live Block path within its processing budget.
const blockScreencastQuality = 90

// frameChannelBuffer is the buffer size of the emitted-frame channel. A small
// buffer stops a transient restartStream stall from parking the frame loop on the
// frames send while the viewer control goroutine runs the restart; the latest-wins
// raw queue already bounds upstream backpressure, so a couple of results are enough.
const frameChannelBuffer = 2

// ackBufferDepth is the buffer size of the ack channel the ack pump drains. Every
// delivered frame is acked once on receive, and Chrome self-throttles the screencast
// to at most a few frames in flight, so only a few acks are ever pending at once; a
// few slots of headroom keep the listener's select-with-default enqueue from ever
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

// decodeScreencastNative decodes one base64 screencast payload without forcing
// the decoder result into RGBA. Pixel consumers use this path so PNG retains its
// decoder-native layout across the engine/render seam.
func decodeScreencastNative(data string) (image.Image, error) {
	raw, release, err := decodeBase64Pooled(data)
	if err != nil {
		return nil, fmt.Errorf("engine: base64-decode frame: %w", err)
	}
	defer release()
	img, err := decodeFrameImage(raw)
	if err != nil {
		return nil, fmt.Errorf("engine: decode frame image: %w", err)
	}
	return img, nil
}

// decodeImageBytes decodes raw image bytes to an *image.RGBA in RGBA channel order.
// It is the shared decode both the base64 screencast payload and the CDP verification
// screenshot feed: page.captureScreenshot's Do already base64-decodes to raw bytes, so
// the verification path skips the base64 step and hands the bytes straight here. The
// decode routes through decodeFrameImage, so a PNG frame decodes via woozymasta and a
// JPEG via the stdlib registry. A non-RGBA decode is converted to RGBA with a bulk
// draw.Draw in draw.Src mode, which runs the same colour conversion the render path
// expects far faster than a per-pixel copy, so the render and anchor paths read one
// pixel layout.
func decodeImageBytes(raw []byte) (*image.RGBA, error) {
	img, err := decodeFrameImage(raw)
	if err != nil {
		return nil, fmt.Errorf("engine: decode frame image: %w", err)
	}
	return imageToRGBA(img), nil
}

// imageToRGBA returns img as an *image.RGBA in RGBA channel order. An image that is
// already *image.RGBA is returned as-is with no copy; any other layout (a JPEG
// YCbCr, for example) is converted with a bulk draw.Draw in draw.Src mode, the same
// colour conversion the render path expects, far faster than a per-pixel copy.
func imageToRGBA(img image.Image) *image.RGBA {
	if rgba, ok := img.(*image.RGBA); ok {
		return rgba
	}
	b := img.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(rgba, rgba.Bounds(), img, b.Min, draw.Src)
	return rgba
}

// decodeJPEGFast decodes one live-frame image, preferring the SIMD jpegn decoder
// and falling back to decodeFrameImage on the same bytes. JPEG remains native
// *image.YCbCr for Block and other pixel consumers. Any non-nil jpegn error falls
// back, including jpegn.ErrUnsupported and non-JPEG input. Each call gets its own
// reader, so parallel workers share no decode state.
func decodeJPEGFast(raw []byte) (image.Image, error) {
	if img, err := jpegn.Decode(bytes.NewReader(raw)); err == nil {
		return img, nil
	}
	return decodeFrameImage(raw)
}

// decodeLiveFrame is the live decode-pool's pixel decode. It keeps the decoder's
// native image layout. Block averages native JPEG YCbCr planes directly while
// composing its final grid. Sixel decodes PNG; Kitty passes its PNG through without
// calling this method. Capture does not call this path and remains lossless PNG.
func (e *ChromiumEngine) decodeLiveFrame(data string) (image.Image, error) {
	raw, release, err := decodeBase64Pooled(data)
	if err != nil {
		return nil, fmt.Errorf("engine: base64-decode frame: %w", err)
	}
	defer release()
	img, err := decodeJPEGFast(raw)
	if err != nil {
		return nil, fmt.Errorf("engine: decode frame image: %w", err)
	}
	return img, nil
}
