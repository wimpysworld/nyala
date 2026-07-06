package block

import (
	"image"
	"image/color"
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
)

// benchImage builds a pxW x pxH RGBA image whose pixels vary within every
// scheme sub-cell block, so each cell binarises to a non-flat mask (neither all
// dark nor all bright). That is the worst case for the compose colour sums: the
// flat-cell fast path never fires, so every cell runs the bright/dark split.
func benchImage(pxW, pxH int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, pxW, pxH))
	for y := range pxH {
		for x := range pxW {
			// A checkerboard of dark and bright pixels within each block splits the
			// luma every cell, and a slow colour drift keeps the averaged colours
			// distinct rather than a single flat value.
			v := byte(((x*7 + y*13) & 0x3f) + 64)
			if (x+y)&1 == 0 {
				v = byte(((x*5 + y*11) & 0x1f) + 192)
			}
			i := img.PixOffset(x, y)
			img.Pix[i] = v
			img.Pix[i+1] = v ^ 0x33
			img.Pix[i+2] = v + 19
			img.Pix[i+3] = 0xff
		}
	}
	return img
}

// BenchmarkComposeQuadrant composes a non-flat 120x40 quadrant grid into a
// reused grid, so the measured allocations are the per-cell colour sums alone.
// It pins the compose hot path's allocation cost for the quadrant scheme.
func BenchmarkComposeQuadrant(b *testing.B) {
	const cols, rows = 120, 40
	br := Renderer{Cols: cols, Rows: rows, Scheme: SchemeQuadrant}
	f := render.Frame{Img: benchImage(cols*cellPxW, rows*cellPxH)}
	var dst Grid
	br.ComposeInto(&dst, f)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		br.ComposeInto(&dst, f)
	}
}

// BenchmarkComposeSextant composes a non-flat 120x40 sextant grid into a reused
// grid, pinning the sextant scheme's per-cell colour-sum allocation cost.
func BenchmarkComposeSextant(b *testing.B) {
	const cols, rows = 120, 40
	br := Renderer{Cols: cols, Rows: rows, Scheme: SchemeSextant}
	f := render.Frame{Img: benchImage(cols*sextCellPxW, rows*sextCellPxH)}
	var dst Grid
	br.ComposeInto(&dst, f)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		br.ComposeInto(&dst, f)
	}
}

// benchRuns builds one overlay run per row that carries box geometry and CellW,
// so the compose boundary recompose runs and the sub-cell background average
// fires. cached controls whether each run carries its DisplayCells, so the same
// benchmark exercises the cached-width path and the re-segment-every-frame
// fallback. The runs are short words at a fractional box offset, the shape the
// engine decode produces.
func benchRuns(rows int, cached bool) []render.GlyphRun {
	const cellCSSW = 8.0
	runs := make([]render.GlyphRun, 0, rows)
	for row := range rows {
		text := "example words on a line to compose"
		col := 1
		w := float64(len(text)) * cellCSSW
		run := render.GlyphRun{
			Col:   col,
			Row:   row,
			Text:  text,
			FG:    color.RGBA{R: 220, G: 220, B: 220, A: 0xff},
			X:     float64(col)*cellCSSW + 3, // fractional offset so the recompose has residue
			Y:     float64(row) * 16,
			W:     w,
			H:     16,
			CellW: cellCSSW,
		}
		if cached {
			run.DisplayCells = render.DisplayWidth(text)
		}
		runs = append(runs, run)
	}
	return runs
}

// BenchmarkComposeOverlay composes a non-flat 120x40 quadrant grid with one
// overlay run per row into a reused grid. The CachedWidth sub-benchmark carries
// each run's decode-cached DisplayCells; the Uncached sub-benchmark leaves it
// zero, so the boundary recompose re-segments the run every frame. The delta is
// the width-cache saving on the compose path.
func BenchmarkComposeOverlay(b *testing.B) {
	const cols, rows = 120, 40
	br := Renderer{Cols: cols, Rows: rows, Scheme: SchemeQuadrant}
	img := benchImage(cols*cellPxW, rows*cellPxH)
	for _, tc := range []struct {
		name   string
		cached bool
	}{
		{"CachedWidth", true},
		{"Uncached", false},
	} {
		f := render.Frame{Img: img, Text: benchRuns(rows, tc.cached)}
		var dst Grid
		br.ComposeInto(&dst, f)
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				br.ComposeInto(&dst, f)
			}
		})
	}
}
