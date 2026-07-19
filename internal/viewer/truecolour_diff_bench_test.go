package viewer

import (
	"bytes"
	"image"
	"strings"
	"testing"

	"github.com/wimpysworld/nyala/internal/cellraster"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

type trueColourCountingBuffer struct {
	bytes.Buffer
	calls int
}

func (w *trueColourCountingBuffer) Write(p []byte) (int, error) {
	w.calls++
	return w.Buffer.Write(p)
}

func trueColourBenchFrame(cols, rows, phase int) render.Frame {
	w, h := cols*cellraster.CellPxW, rows*cellraster.CellPxH
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			i := img.PixOffset(x, y)
			img.Pix[i] = uint8((x*5 + phase*17) & 0xff)
			img.Pix[i+1] = uint8((y*3 + phase*29) & 0xff)
			img.Pix[i+2] = uint8((x + y + phase*43) & 0xff)
			img.Pix[i+3] = 0xff
		}
	}
	return render.Frame{Img: img, Text: benchStreamRuns(rows)}
}

func trueColourBenchState(cols, rows int) *State {
	s := NewState(cols, rows)
	s.SetEnabled([]Backend{BackendBlock})
	s.SetDiff(&block.GridDiff{})
	return &s
}

// BenchmarkTrueColourDiffPath measures production-shaped changed and unchanged
// Block frames written into the reusable outer bytes.Buffer.
func BenchmarkTrueColourDiffPath(b *testing.B) {
	for _, sz := range []struct {
		name       string
		cols, rows int
	}{{"120x40", 120, 40}, {"240x86", 240, 86}} {
		frames := [2]render.Frame{
			trueColourBenchFrame(sz.cols, sz.rows, 0),
			trueColourBenchFrame(sz.cols, sz.rows, 1),
		}
		for _, changed := range []bool{false, true} {
			name := "unchanged"
			if changed {
				name = "changed"
			}
			b.Run(sz.name+"/"+name, func(b *testing.B) {
				s := trueColourBenchState(sz.cols, sz.rows)
				var out bytes.Buffer
				out.Grow(sz.cols*sz.rows*48 + 256)
				if err := s.RenderStreamFrame(frames[0], &out); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := range b.N {
					out.Reset()
					frame := frames[0]
					if changed {
						frame = frames[(i+1)&1]
					}
					if err := s.RenderStreamFrame(frame, &out); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				measured := trueColourBenchState(sz.cols, sz.rows)
				var sample trueColourCountingBuffer
				if err := measured.RenderStreamFrame(frames[0], &sample); err != nil {
					b.Fatal(err)
				}
				sample.Reset()
				sample.calls = 0
				frame := frames[0]
				if changed {
					frame = frames[1]
				}
				if err := measured.RenderStreamFrame(frame, &sample); err != nil {
					b.Fatal(err)
				}
				b.ReportMetric(float64(sample.Len()), "output-B/frame")
				b.ReportMetric(float64(strings.Count(sample.String(), "\x1b[38;2;")+strings.Count(sample.String(), "\x1b[48;2;")), "SGR/frame")
				b.ReportMetric(float64(sample.calls), "writer-calls/frame")
			})
		}
	}
}
