// Package metrics aligns and compares rendered frames against their reference.
//
// Alignment downscales the reference image to the render-grid dimensions before
// any metric runs, so the reference and the render share one pixel geometry.
// Channel order is RGBA throughout: the resizer reads and writes *image.RGBA.
package metrics

import (
	"image"

	"golang.org/x/image/draw"
)

// AlignFilter is the pinned resampling filter for the whole corpus. CatmullRom
// is fixed here so every alignment uses the same kernel; do not vary it per
// call. Downscaling the reference to the render grid is the agreed resize
// direction.
var AlignFilter draw.Scaler = draw.CatmullRom

// Downscale resizes src to a width x height *image.RGBA using the pinned
// AlignFilter. It is the single alignment entry point: pass the reference image
// and the render-grid dimensions, and receive the reference at that exact size
// in RGBA order. Width and height must be positive.
func Downscale(src image.Image, width, height int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	AlignFilter.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	return dst
}

// DownscaleLiveInto resizes src into dst by area-averaging, returning the same dst.
// Only the interactive diff'd block draw calls it; the FitGrid block path and the
// graphics path still use Downscale (CatmullRom). Area-averaging is the correct
// high-quality method for the large reductions the live block draw performs (a full
// page into an 80-column grid): each destination pixel is the mean of the source
// pixels its footprint covers, so no aliasing or speckle survives, unlike a bilinear
// tap that samples only a few source pixels. It is deliberately separate from the
// pinned AlignFilter: the diagnostics harness and the amd64 byte goldens pin
// CatmullRom, so this live path must never reach the measured paths. dst's bounds set
// the output size, and the average fully overwrites every destination pixel, so a
// reused dst carries no stale content.
func DownscaleLiveInto(dst *image.RGBA, src image.Image) *image.RGBA {
	areaAverage(dst, toRGBA(src))
	return dst
}

// toRGBA returns src as *image.RGBA without a copy when it already is one, else it
// copies into a fresh RGBA buffer so areaAverage reads a packed RGBA plane.
func toRGBA(src image.Image) *image.RGBA {
	if r, ok := src.(*image.RGBA); ok {
		return r
	}
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Copy(dst, image.Point{}, src, b, draw.Src, nil)
	return dst
}

// areaAverage box-downscales src into dst: each destination pixel is the unweighted
// mean of every source pixel whose centre maps into that destination cell. The source
// splits into dst.W x dst.H tiles by the src/dst size ratio, and each output pixel is
// the average over its tile. Byte order is RGBA throughout. When the source is not
// larger than the destination a tile can be empty; the nearest source pixel fills it,
// so an upscale or a no-op size still writes every destination pixel.
func areaAverage(dst, src *image.RGBA) {
	db := dst.Bounds()
	dw, dh := db.Dx(), db.Dy()
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if dw == 0 || dh == 0 || sw == 0 || sh == 0 {
		return
	}
	for dy := range dh {
		sy0 := dy * sh / dh
		sy1 := (dy + 1) * sh / dh
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for dx := range dw {
			sx0 := dx * sw / dw
			sx1 := (dx + 1) * sw / dw
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var r, g, b, a, n uint64
			for sy := sy0; sy < sy1; sy++ {
				row := src.PixOffset(sb.Min.X+sx0, sb.Min.Y+sy)
				for sx := sx0; sx < sx1; sx++ {
					r += uint64(src.Pix[row])
					g += uint64(src.Pix[row+1])
					b += uint64(src.Pix[row+2])
					a += uint64(src.Pix[row+3])
					row += 4
					n++
				}
			}
			o := dst.PixOffset(db.Min.X+dx, db.Min.Y+dy)
			// The mean of bytes stays in 0..255, so the low-byte mask is a no-op that
			// keeps the conversion in range without a bounds check per pixel.
			dst.Pix[o] = byte(r / n & 0xFF)
			dst.Pix[o+1] = byte(g / n & 0xFF)
			dst.Pix[o+2] = byte(b / n & 0xFF)
			dst.Pix[o+3] = byte(a / n & 0xFF)
		}
	}
}
