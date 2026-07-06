package engine

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg" // register the JPEG decoder for image.Decode
	_ "image/png"  // register the PNG decoder for image.Decode

	emmb64 "github.com/emmansun/base64"
	"github.com/gen2brain/jpegn"
)

// decodeBase64Fast decodes one base64 screencast payload to raw bytes, preferring
// the SIMD emmansun decoder and falling back to stdlib encoding/base64 on any error,
// so a declined input decodes exactly as it does today. The Chromium screencast Data
// field is standard base64; emmansun's StdEncoding is a drop-in for the stdlib
// StdEncoding, dispatches to a SIMD kernel at runtime (SSE/AVX2/AVX512-VBMI on amd64,
// NEON on arm64), and ignores embedded newlines the same way. It sits inside the
// measured in_decode span, ahead of the image decode. A pooled Decode(dst, src) form
// would also kill the per-call output alloc; it is a follow-up, held back here so the
// raw bytes' lifetime across the downstream image decode stays trivially correct.
func decodeBase64Fast(data string) ([]byte, error) {
	if raw, err := emmb64.StdEncoding.DecodeString(data); err == nil {
		return raw, nil
	}
	return base64.StdEncoding.DecodeString(data)
}

// screencastQuality is a JPEG compression quality value. The live screencast is
// now lossless PNG, so it no longer sets the stream quality; it survives as the
// quality recorded in the capture bundle's screencast config and as the encode
// quality for the JPEG decode benchmark fixture. Lossless frames are what the
// planned per-frame damage diff needs: JPEG's 16x16 MCU decode shifts around every
// change and its banding can flip gradient cells near the 2-colour threshold, so a
// tolerance cannot cleanly catch a real change; PNG gives an exact per-cell diff.
const screencastQuality = 90

// movingScreencastQuality is the JPEG quality the adaptive live screencast uses when
// the page is moving (and the block backend uses for its whole live stream). JPEG at
// this quality decodes far faster than lossless PNG, cutting the ingest decode cost
// during animation and scroll; its MCU-scale artefacts are hidden by the motion on
// the graphics backends and washed out by the block backend's own sub-cell
// downsample. The capture path never uses it: capture forces PNG for a byte-exact
// corpus.
const movingScreencastQuality = 80

// frameChannelBuffer is the buffer size of the emitted-frame channel. A small
// buffer stops a transient restartStream stall from parking the frame loop on the
// frames send while the viewer control goroutine runs the restart; the latest-wins
// slot already bounds upstream backpressure, so a couple of slots is enough.
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

// decodeScreencastImage decodes one base64 screencast payload to an *image.RGBA
// in RGBA channel order. EventScreencastFrame.Data is base64, so it is decoded to
// bytes before the image decoder. A non-RGBA decode is converted with the same
// copy idiom the viewer's LoadPNG uses, so the render path reads one pixel layout.
func decodeScreencastImage(data string) (*image.RGBA, error) {
	raw, err := decodeBase64Fast(data)
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
// and falling back to the stdlib image.Decode on the same bytes. jpegn returns a
// native *image.YCbCr about four times faster than stdlib on baseline 4:2:0, the
// live block stream's exact class, so the fused YCbCr sub-cell downscale still
// applies. jpegn returns jpegn.ErrUnsupported for inputs it declines (non-8-bit
// precision, unusual subsampling, smoothing); any non-nil jpegn error, not only
// ErrUnsupported, falls back to image.Decode so a declined frame decodes exactly
// as it does today. image.Decode also covers the block PNG frame, which jpegn does
// not handle. Each call gets its own reader, so the decode is safe under the
// parallel decode pool.
func decodeJPEGFast(raw []byte) (image.Image, error) {
	if img, err := jpegn.Decode(bytes.NewReader(raw)); err == nil {
		return img, nil
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	return img, err
}

// decodeLiveFrame is the live decode-pool's decode: it decodes one base64 screencast
// payload to an *image.RGBA, fusing the YCbCr-to-RGBA convert into the sub-cell
// downscale on the live block JPEG path so that path never materialises a
// full-resolution RGBA surface only to average ~15/16 of it away.
//
// The fuse is gated tightly to the case it helps. When the active backend is block
// and the decoded image is *image.YCbCr (a JPEG frame, which the live block stream
// always is) and the engine holds a sub-cell target smaller than the surface, it
// box-averages the Y/Cb/Cr planes straight down to that sub-cell footprint
// (e.captureW x e.captureH, the same BlockCaptureSize the viewer downscales to) and
// converts only the small result, emitting a frame already at sub-cell resolution.
// The viewer's renderBlock detects that resolution and skips its own downscale.
//
// Every other case falls back to the full-surface convert (imageToRGBA): a graphics
// backend JPEG (which needs the full-resolution bitmap and takes no overlay), a block
// PNG frame (already RGBA, no YCbCr to fuse), or an unset/degenerate sub-cell target.
// The capture path never calls this: capture forces PNG and keeps runFrameLoop with
// decodeScreencastImage, so the byte-exact corpus is untouched.
func (e *ChromiumEngine) decodeLiveFrame(data string) (*image.RGBA, error) {
	raw, err := decodeBase64Fast(data)
	if err != nil {
		return nil, fmt.Errorf("engine: base64-decode frame: %w", err)
	}
	img, err := decodeJPEGFast(raw)
	if err != nil {
		return nil, fmt.Errorf("engine: decode frame image: %w", err)
	}
	yc, isYCbCr := img.(*image.YCbCr)
	e.mu.Lock()
	onBlock := e.backend == "block"
	cw, ch := e.captureW, e.captureH
	e.mu.Unlock()
	if onBlock && isYCbCr && cw > 0 && ch > 0 {
		b := yc.Bounds()
		if cw < b.Dx() && ch < b.Dy() {
			dst := image.NewRGBA(image.Rect(0, 0, cw, ch))
			averageYCbCrToRGBA(dst, yc)
			return dst, nil
		}
	}
	return imageToRGBA(img), nil
}

// averageYCbCrToRGBA box-averages the Y/Cb/Cr planes of src straight down to dst's
// sub-cell footprint, then converts each destination pixel once from the averaged
// YCbCr to RGBA. dst's bounds set the output size; every destination pixel is fully
// written, so a reused dst carries no stale content. Byte order is RGBA and alpha is
// opaque, matching the screencast surface.
//
// Averaging in YCbCr then converting equals converting then averaging apart from
// chroma subsampling replication and the clamp in YCbCrToRGB, because YCbCr-to-RGB is
// an affine per-pixel transform; the result is near-identical to the current
// decode-then-convert-then-area-average, not byte-identical. The averaged Y plane is
// read once (the same source reads the full convert makes) but only the small
// sub-cell result is written and converted, so no full-resolution RGBA surface is
// materialised. A per-source-column chroma index is precomputed once from the
// subsample ratio, so the inner loop needs no per-pixel division and the kernel
// handles every subsample ratio, not only 4:2:0.
func averageYCbCrToRGBA(dst *image.RGBA, src *image.YCbCr) {
	db := dst.Bounds()
	dw, dh := db.Dx(), db.Dy()
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if dw == 0 || dh == 0 || sw == 0 || sh == 0 {
		return
	}
	ratH, ratV := subsampleShift(src.SubsampleRatio)
	// Per-source-column chroma index, the same for every source row: it removes the
	// per-pixel divide from the inner loop and folds the subsample ratio in once.
	cxIdx := make([]int, sw)
	for px := range sw {
		cxIdx[px] = (sb.Min.X + px) / ratH
	}
	// Per-destination-column source x-range, the same for every destination row.
	sx0s := make([]int, dw)
	sx1s := make([]int, dw)
	for dx := range dw {
		sx0s[dx] = dx * sw / dw
		sx1s[dx] = (dx + 1) * sw / dw
	}
	// Per-destination-column accumulators for one destination row, reused per row.
	sumY := make([]uint64, dw)
	sumCb := make([]uint64, dw)
	sumCr := make([]uint64, dw)
	cnt := make([]uint64, dw)
	for dy := range dh {
		for dx := range dw {
			sumY[dx], sumCb[dx], sumCr[dx], cnt[dx] = 0, 0, 0, 0
		}
		sy0 := dy * sh / dh
		sy1 := (dy + 1) * sh / dh
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for py := sy0; py < sy1; py++ {
			yRow := src.Y[(sb.Min.Y+py)*src.YStride:]
			cBase := ((sb.Min.Y + py) / ratV) * src.CStride
			cb := src.Cb[cBase:]
			cr := src.Cr[cBase:]
			for dx := range dw {
				sx0, sx1 := sx0s[dx], sx1s[dx]
				if sx1 <= sx0 {
					sx1 = sx0 + 1
				}
				var ay, acb, acr, an uint64
				for px := sx0; px < sx1; px++ {
					ci := cxIdx[px]
					ay += uint64(yRow[sb.Min.X+px])
					acb += uint64(cb[ci])
					acr += uint64(cr[ci])
					an++
				}
				sumY[dx] += ay
				sumCb[dx] += acb
				sumCr[dx] += acr
				cnt[dx] += an
			}
		}
		for dx := range dw {
			n := cnt[dx]
			if n == 0 {
				n = 1
			}
			// The mean of bytes stays in 0..255, so the low-byte mask is a no-op that
			// keeps the conversion in range without a per-pixel bounds check.
			yv := byte(sumY[dx] / n & 0xFF)
			cbv := byte(sumCb[dx] / n & 0xFF)
			crv := byte(sumCr[dx] / n & 0xFF)
			r, g, b := color.YCbCrToRGB(yv, cbv, crv)
			o := dst.PixOffset(db.Min.X+dx, db.Min.Y+dy)
			dst.Pix[o] = r
			dst.Pix[o+1] = g
			dst.Pix[o+2] = b
			dst.Pix[o+3] = 0xff
		}
	}
}

// subsampleShift returns the horizontal and vertical chroma downsample divisors for
// a YCbCr subsample ratio: the amount a luma coordinate divides by to index the
// chroma planes. 4:2:0 is (2, 2), 4:2:2 is (2, 1), 4:4:0 is (1, 2), 4:1:1 is (4, 1),
// and 4:4:4 is (1, 1). The live block JPEG stream is 4:2:0; the others keep the
// kernel correct for any ratio a frame might carry.
func subsampleShift(r image.YCbCrSubsampleRatio) (h, v int) {
	switch r {
	case image.YCbCrSubsampleRatio420:
		return 2, 2
	case image.YCbCrSubsampleRatio422:
		return 2, 1
	case image.YCbCrSubsampleRatio440:
		return 1, 2
	case image.YCbCrSubsampleRatio411:
		return 4, 1
	default: // YCbCrSubsampleRatio444
		return 1, 1
	}
}
