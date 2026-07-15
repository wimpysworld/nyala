package viewer

import (
	"bytes"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"testing"

	xdraw "golang.org/x/image/draw"

	"github.com/wimpysworld/nyala/internal/render"
)

const (
	sixelBenchWidth  = 2560
	sixelBenchHeight = 2880
)

func BenchmarkSixelYCbCrFrame(b *testing.B) {
	ycbcr := fixedSixelBenchYCbCr(b)

	b.Run("isolated-conversion", func(b *testing.B) {
		b.StopTimer()
		dst := image.NewRGBA(ycbcr.Bounds())
		b.ReportAllocs()
		b.StartTimer()
		for range b.N {
			draw.Draw(dst, dst.Bounds(), ycbcr, ycbcr.Bounds().Min, draw.Src)
		}
	})

	b.Run("production-moving-ycbcr", func(b *testing.B) {
		b.StopTimer()
		s := State{Backend: BackendSixel, Cols: 1, Rows: 1}
		terminalBytes := sixelBenchBytes(b, &s, ycbcr)
		if err := s.RenderStreamFrame(render.Frame{Img: ycbcr}, io.Discard); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.StartTimer()
		for range b.N {
			if err := s.RenderStreamFrame(render.Frame{Img: ycbcr}, io.Discard); err != nil {
				b.Fatal(err)
			}
		}
		b.StopTimer()
		b.ReportMetric(float64(terminalBytes), "terminal-B")
	})
}

func sixelBenchBytes(b *testing.B, s *State, img image.Image) int64 {
	b.Helper()
	var count countingWriter
	if err := s.Renderer().Render(render.Frame{Img: img}, &count); err != nil {
		b.Fatal(err)
	}
	return count.n
}

type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

func fixedSixelBenchYCbCr(tb testing.TB) *image.YCbCr {
	tb.Helper()
	f, err := os.Open("../../testdata/linuxmatters.png")
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	src, err := png.Decode(f)
	if err != nil {
		tb.Fatal(err)
	}
	scaled := image.NewRGBA(image.Rect(0, 0, sixelBenchWidth, sixelBenchHeight))
	xdraw.CatmullRom.Scale(scaled, scaled.Bounds(), src, src.Bounds(), draw.Src, nil)
	var payload bytes.Buffer
	if err := jpeg.Encode(&payload, scaled, &jpeg.Options{Quality: 90}); err != nil {
		tb.Fatal(err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(payload.Bytes()))
	if err != nil {
		tb.Fatal(err)
	}
	ycbcr, ok := decoded.(*image.YCbCr)
	if !ok {
		tb.Fatalf("JPEG decode = %T, want *image.YCbCr", decoded)
	}
	return ycbcr
}
