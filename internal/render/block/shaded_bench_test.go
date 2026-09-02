package block

import (
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
)

// BenchmarkShadedComposeMono measures the Shaded compose under the monochrome
// palette at both measured geometries, the luma-scoring counterpart of the
// xterm-256 rows BenchmarkBlockCompose reports for the shaded scheme.
func BenchmarkShadedComposeMono(b *testing.B) {
	for _, sz := range bpSizes {
		b.Run(sz.name, func(b *testing.B) {
			br := Renderer{Cols: sz.cols, Rows: sz.rows, Scheme: SchemeShaded, Palette: ShadedMono}
			pxW, pxH := render.BlockCaptureSize(render.BlockShaded, sz.cols, sz.rows)
			f := render.Frame{Img: benchImage(pxW, pxH)}
			var dst Grid
			br.ComposeInto(&dst, f)
			b.ReportAllocs()
			for b.Loop() {
				br.ComposeInto(&dst, f)
			}
		})
	}
}
