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

// LiveFilter is the fast resampling filter for the interactive live draw only.
// ApproxBiLinear downscales an order of magnitude faster than the pinned
// AlignFilter, which the live path spends per frame; the block cells quantise the
// result into two colours, so the cheaper kernel costs no visible quality. It is
// deliberately separate from AlignFilter: the diagnostics harness and the amd64
// byte goldens pin AlignFilter (CatmullRom), so the live speed-up must never reach
// the measured paths. Only DownscaleLiveInto reads it.
var LiveFilter draw.Scaler = draw.ApproxBiLinear

// DownscaleLiveInto resizes src into dst using LiveFilter, returning the same dst.
// Only the interactive diff'd block draw calls it, trading the pinned kernel's cost
// for ApproxBiLinear's speed; the FitGrid block path and the graphics path still use
// Downscale. The goldens and the diagnostics harness pin AlignFilter and never call
// this helper, so the swap stays scoped to the live path. dst's bounds set the output
// size, and the scaler fully overwrites every destination pixel, so a reused dst
// carries no stale content.
func DownscaleLiveInto(dst *image.RGBA, src image.Image) *image.RGBA {
	LiveFilter.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	return dst
}
