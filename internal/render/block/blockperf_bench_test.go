package block

import (
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

// BenchmarkBlockDiff drives the differ over a full-motion grid (every cell changes
// each pass) in truecolour, for both sizes. It is the worst-case live paint: no
// cell coalesces, so every cell rebuilds its SGR and glyph and writes.
func BenchmarkBlockDiff(b *testing.B) {
	fg, bg := SGRFunc(AppendTrueColorFG), SGRFunc(AppendTrueColorBG)
	for _, sz := range bpSizes {
		b.Run(sz.name, func(b *testing.B) {
			frames := [2]Grid{
				motionGrid(sz.cols, sz.rows, 0),
				motionGrid(sz.cols, sz.rows, 1),
			}
			var d GridDiff
			if err := d.Emit(io.Discard, frames[0], fg, bg); err != nil { // seed prev
				b.Fatal(err)
			}
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				if err := d.Emit(io.Discard, frames[i&1], fg, bg); err != nil {
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
