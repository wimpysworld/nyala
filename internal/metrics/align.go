// Package metrics aligns and compares rendered frames against their reference.
//
// Alignment downscales the reference image to the render-grid dimensions before
// any metric runs, so the reference and the render share one pixel geometry.
// Channel order is RGBA throughout: the resizer reads and writes *image.RGBA.
package metrics

import (
	"image"
	"runtime"
	"sync"

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

// downscaleBandMinRows is the smallest destination-row count one band covers. The
// pool never splits below it, so a band always carries at least this many rows and
// the fan-out stays fat: a tiny grid runs fewer, larger bands, or falls back to
// serial when it holds fewer than this many rows.
const downscaleBandMinRows = 16

// DownscalePool parallelises the live block downscale across a persistent set of
// worker goroutines, one per band. It splits the destination into whole-row bands
// and area-averages each band on a worker, so the total is byte-identical to the
// serial DownscaleLiveInto: the maths is per-destination-row independent, each band
// reads the shared source read-only and writes only its own disjoint destination
// rows. The pool is built once and reused every frame, so the hot path spawns no
// goroutine and allocates nothing. It is driven from one goroutine (the viewer draw
// loop), so its reused WaitGroup never overlaps calls.
type DownscalePool struct {
	maxBands int                // upper band count, sized from GOMAXPROCS at build
	jobs     chan downscaleBand // buffered to maxBands so the driver never blocks on send
	wg       sync.WaitGroup     // reused per call; barrier before the driver returns
}

// downscaleBand is one whole-row band of work: area-average dst rows [dy0, dy1)
// from src. wg is the call's barrier the worker signals when the band is done.
type downscaleBand struct {
	dst, src *image.RGBA
	dy0, dy1 int
	wg       *sync.WaitGroup
}

// NewDownscalePool builds a downscale pool sized from runtime.GOMAXPROCS(0), read
// once here. It runs at most GOMAXPROCS-1 bands, leaving one core for the caller
// and the shared headless-Chromium load, and starts that many persistent workers.
// A single-core box gets one worker, so the pool degrades to serial. Call Close to
// stop the workers.
func NewDownscalePool() *DownscalePool {
	return newDownscalePool(runtime.GOMAXPROCS(0) - 1)
}

// newDownscalePool builds a pool with workers persistent goroutines, clamped to at
// least one. It is the shared constructor; NewDownscalePool sizes workers from
// GOMAXPROCS, and tests pass an explicit count to force the multi-band path.
func newDownscalePool(workers int) *DownscalePool {
	if workers < 1 {
		workers = 1
	}
	p := &DownscalePool{maxBands: workers, jobs: make(chan downscaleBand, workers)}
	for range workers {
		go p.worker()
	}
	return p
}

// worker runs one persistent goroutine: it area-averages each band it receives and
// signals the band's barrier. It exits when Close shuts the jobs channel.
func (p *DownscalePool) worker() {
	for b := range p.jobs {
		areaAverageBand(b.dst, b.src, b.dy0, b.dy1)
		b.wg.Done()
	}
}

// Close stops the pool's workers. After Close the pool must not be used again. The
// viewer keeps its pool for the process lifetime, so it never calls Close; tests
// call it to release the goroutines.
func (p *DownscalePool) Close() {
	close(p.jobs)
}

// bands returns how many whole-row bands to split dstRows into: the pool's worker
// count, capped so each band covers at least downscaleBandMinRows rows. It returns
// at least one, so a grid too small to split (fewer than downscaleBandMinRows rows,
// including a single row) runs serially and pays no fan-out overhead.
func (p *DownscalePool) bands(dstRows int) int {
	n := p.maxBands
	if m := dstRows / downscaleBandMinRows; m < n {
		n = m
	}
	if n < 1 {
		return 1
	}
	return n
}

// DownscaleLiveInto resizes src into dst by area-averaging, split across the pool's
// workers by whole destination-row bands, and returns dst. It is the parallel twin
// of the package DownscaleLiveInto and produces byte-identical output: each band
// runs the same areaAverageBand kernel over a disjoint destination-row range, so no
// two bands write the same pixel and the source is read-only. A grid too small to
// split runs serially on the calling goroutine. The reused WaitGroup barriers every
// band before the call returns, so dst is complete on return and the pool is free
// for the next frame.
func (p *DownscalePool) DownscaleLiveInto(dst *image.RGBA, src image.Image) *image.RGBA {
	s := toRGBA(src)
	dh := dst.Bounds().Dy()
	n := p.bands(dh)
	if n <= 1 {
		areaAverage(dst, s)
		return dst
	}
	p.wg.Add(n)
	base, rem := dh/n, dh%n
	y := 0
	for i := range n {
		h := base
		if i < rem {
			h++
		}
		p.jobs <- downscaleBand{dst: dst, src: s, dy0: y, dy1: y + h, wg: &p.wg}
		y += h
	}
	p.wg.Wait()
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
	areaAverageBand(dst, src, 0, dst.Bounds().Dy())
}

// areaAverageBand box-downscales the destination rows in [dy0, dy1) of dst from
// src, the whole-row band the pool hands one worker. It maps each destination row
// to its source row range with the full dst height, so a band writes byte-for-byte
// the same pixels areaAverage writes for those rows: the split is purely which
// rows a call fills. It writes only dst rows [dy0, dy1) and reads only src, so
// bands over disjoint row ranges never race. Byte order is RGBA throughout.
func areaAverageBand(dst, src *image.RGBA, dy0, dy1 int) {
	db := dst.Bounds()
	dw, dh := db.Dx(), db.Dy()
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if dw == 0 || dh == 0 || sw == 0 || sh == 0 {
		return
	}
	for dy := dy0; dy < dy1; dy++ {
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
