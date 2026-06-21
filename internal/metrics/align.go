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
