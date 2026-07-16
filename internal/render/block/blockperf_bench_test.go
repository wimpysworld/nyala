package block

import (
	"image/color"
	"io"
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/rowband"
)

// bpSize is one grid geometry, cols x rows terminal cells. The block source
// bitmap the compose samples is not cols*16 x rows*33: the engine downsamples the
// screencast to the sub-cell footprint before compose, so the source extent is
// render.BlockCaptureSize (cols*2 x rows*4 quadrant, cols*2 x rows*3 sextant). The
// 16x33 terminal cell sizes only the graphics backends, not the block path.
type bpSize struct {
	name       string
	cols, rows int
}

// bpSizes is the two measured geometries: a 120x40 grid (4,800 cells) and a
// 240x86 grid (20,640 cells), both at the terminal's 16x33 cell.
var bpSizes = []bpSize{
	{"120x40", 120, 40},
	{"240x86", 240, 86},
}

// BenchmarkBlockCompose measures the downsample + two-colour pick + binarise over
// a full grid with no overlay, for both schemes and both sizes. It reuses the
// benchImage worst case (every cell non-flat) and the ComposeInto reused-grid path
// the live viewer drives.
func BenchmarkBlockCompose(b *testing.B) {
	for _, scheme := range []struct {
		name string
		s    Scheme
		rs   render.BlockScheme
	}{
		{"quadrant", SchemeQuadrant, render.BlockQuadrant},
		{"sextant", SchemeSextant, render.BlockSextant},
		{"half", SchemeHalfBlock, render.BlockHalf},
	} {
		b.Run(scheme.name, func(b *testing.B) {
			for _, sz := range bpSizes {
				b.Run(sz.name, func(b *testing.B) {
					br := Renderer{Cols: sz.cols, Rows: sz.rows, Scheme: scheme.s}
					pxW, pxH := render.BlockCaptureSize(scheme.rs, sz.cols, sz.rows)
					f := render.Frame{Img: benchImage(pxW, pxH)}
					var dst Grid
					br.ComposeInto(&dst, f)
					b.ReportAllocs()
					for b.Loop() {
						br.ComposeInto(&dst, f)
					}
				})
			}
		})
	}
}

// BenchmarkBlockComposeParallel measures the same compose worst case as
// BenchmarkBlockCompose, but split across a rowband.Pool by grid-row bands, for both
// schemes and both sizes. It reuses one pool and one ParallelCompose driver built
// once outside the timed loop, so it times the per-frame fan-out, not the pool
// build, and pairs with BenchmarkBlockCompose to report the parallel speedup.
func BenchmarkBlockComposeParallel(b *testing.B) {
	pool := rowband.New()
	defer pool.Close()
	pc := NewParallelCompose()
	for _, scheme := range []struct {
		name string
		s    Scheme
		rs   render.BlockScheme
	}{
		{"quadrant", SchemeQuadrant, render.BlockQuadrant},
		{"sextant", SchemeSextant, render.BlockSextant},
		{"half", SchemeHalfBlock, render.BlockHalf},
	} {
		b.Run(scheme.name, func(b *testing.B) {
			for _, sz := range bpSizes {
				b.Run(sz.name, func(b *testing.B) {
					br := Renderer{Cols: sz.cols, Rows: sz.rows, Scheme: scheme.s}
					pxW, pxH := render.BlockCaptureSize(scheme.rs, sz.cols, sz.rows)
					f := render.Frame{Img: benchImage(pxW, pxH)}
					var dst Grid
					pc.ComposeInto(&br, &dst, f, pool)
					b.ReportAllocs()
					for b.Loop() {
						pc.ComposeInto(&br, &dst, f, pool)
					}
				})
			}
		})
	}
}

var (
	benchColourFG, benchColourBG color.RGBA
	benchQuadrantSample          [4]color.RGBA
)

// BenchmarkMaskColours isolates the mask-keyed foreground/background averaging
// used after quadrant and sextant luma selection.
func BenchmarkMaskColours(b *testing.B) {
	cs := [6]color.RGBA{
		{R: 1, G: 254, B: 63, A: 0xff},
		{R: 255, G: 2, B: 191, A: 0xff},
		{R: 17, G: 99, B: 240, A: 0xff},
		{R: 203, G: 71, B: 5, A: 0xff},
		{R: 88, G: 144, B: 222, A: 0xff},
		{R: 250, G: 31, B: 127, A: 0xff},
	}
	b.Run("quadrant", func(b *testing.B) {
		mask := 0
		var fg, bg color.RGBA
		b.ReportAllocs()
		for b.Loop() {
			mask = mask%14 + 1
			fg, bg = quadrantColours(mask, cs[0], cs[1], cs[2], cs[3])
		}
		benchColourFG, benchColourBG = fg, bg
	})
	b.Run("sextant", func(b *testing.B) {
		mask := 0
		var fg, bg color.RGBA
		b.ReportAllocs()
		for b.Loop() {
			mask = mask%62 + 1
			fg, bg = sextantColours(mask, cs[0], cs[1], cs[2], cs[3], cs[4], cs[5])
		}
		benchColourFG, benchColourBG = fg, bg
	})
}

// BenchmarkQuadrantSampling isolates the exact-size source reads and vertical
// two-pixel averages that produce one cell's four quadrant colours.
func BenchmarkQuadrantSampling(b *testing.B) {
	const cols, rows = 240, 86
	img := benchImage(cols*cellPxW, rows*cellPxH)
	cell := 0
	var x, y, z, w color.RGBA
	b.ReportAllocs()
	for b.Loop() {
		col, row := cell%cols, cell/cols
		x, y, z, w = sampleQuadrantExact(img, col, row)
		cell++
		if cell == cols*rows {
			cell = 0
		}
	}
	benchQuadrantSample = [4]color.RGBA{x, y, z, w}
}

// BenchmarkBlockDiff drives the differ over a full-motion grid (every cell changes
// each pass) in truecolour, for both sizes. It is the worst-case live paint: no
// cell coalesces, so every cell rebuilds its SGR and glyph and writes.
func BenchmarkBlockDiff(b *testing.B) {
	for _, sz := range bpSizes {
		b.Run(sz.name, func(b *testing.B) {
			frames := [2]Grid{
				motionGrid(sz.cols, sz.rows, 0),
				motionGrid(sz.cols, sz.rows, 1),
			}
			var d GridDiff
			if err := d.EmitTrueColour(io.Discard, frames[0]); err != nil { // seed prev
				b.Fatal(err)
			}
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				if err := d.EmitTrueColour(io.Discard, frames[i&1]); err != nil {
					b.Fatal(err)
				}
				i++
			}
		})
	}
}

// BenchmarkBlockOverlay isolates the overlay fold: a pristine composed grid is
// copied into a work grid each pass (a Cells memmove), then the runs fold on. It
// measures applyOverlay alone (grapheme segmentation, cell claiming, cellBGAverage,
// boundary recompose), not the downsample, for both sizes. Runs carry cached
// DisplayCells, the engine's decode-time shape.
func BenchmarkBlockOverlay(b *testing.B) {
	for _, sz := range bpSizes {
		b.Run(sz.name, func(b *testing.B) {
			br := Renderer{Cols: sz.cols, Rows: sz.rows, Scheme: SchemeQuadrant}
			pxW, pxH := render.BlockCaptureSize(render.BlockQuadrant, sz.cols, sz.rows)
			img := benchImage(pxW, pxH)
			pristine := br.Compose(render.Frame{Img: img})
			f := render.Frame{Img: img, Text: benchRuns(sz.rows, true)}
			work := Grid{Cols: sz.cols, Rows: sz.rows, Cells: make([]Cell, len(pristine.Cells))}
			b.ReportAllocs()
			for b.Loop() {
				copy(work.Cells, pristine.Cells)
				applyOverlay(&work, f, SchemeQuadrant)
			}
		})
	}
}
