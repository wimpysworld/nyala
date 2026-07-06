package metrics

import (
	"image"
	"testing"
)

// downscaleBenchCase is one grid at cell 16x33: the full screencast source extent
// (cols*cellW by rows*cellH) downscaled to the quadrant sub-cell grid (cols*2 by
// rows*4), the exact reduction the live block draw performs.
type downscaleBenchCase struct {
	name                   string
	srcW, srcH, dstW, dstH int
}

var downscaleBenchCases = []downscaleBenchCase{
	{"120x40", 120 * 16, 40 * 33, 120 * 2, 40 * 4},
	{"240x86", 240 * 16, 86 * 33, 240 * 2, 86 * 4},
}

// benchSource builds a deterministic gradient source at the given extent, so the
// downscale reads a real varying plane rather than a flat fill.
func benchSource(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = byte(i)
	}
	return img
}

// BenchmarkDownscaleSerial measures the current serial DownscaleLiveInto at both
// grids, the baseline the parallel path is compared against.
func BenchmarkDownscaleSerial(b *testing.B) {
	for _, c := range downscaleBenchCases {
		b.Run(c.name, func(b *testing.B) {
			src := benchSource(c.srcW, c.srcH)
			dst := image.NewRGBA(image.Rect(0, 0, c.dstW, c.dstH))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				DownscaleLiveInto(dst, src)
			}
		})
	}
}

// BenchmarkDownscaleParallel measures the pool downscale at both grids. It uses the
// production pool (sized from GOMAXPROCS) built once outside the timed loop, so the
// benchmark times the per-frame fan-out, not the one-off pool build.
func BenchmarkDownscaleParallel(b *testing.B) {
	pool := NewDownscalePool()
	defer pool.Close()
	for _, c := range downscaleBenchCases {
		b.Run(c.name, func(b *testing.B) {
			src := benchSource(c.srcW, c.srcH)
			dst := image.NewRGBA(image.Rect(0, 0, c.dstW, c.dstH))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				pool.DownscaleLiveInto(dst, src)
			}
		})
	}
}
