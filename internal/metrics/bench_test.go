package metrics_test

import (
	"image"
	"image/color"
	"io"
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
	"github.com/wimpysworld/nyala/internal/render/kitty"
	"github.com/wimpysworld/nyala/internal/render/sixel"
)

// Pinned benchmark inputs. The grid is the corpus-wide 120x40 sweep grid. The
// quadrant block scheme maps one cell to a 2x4 source block, so the golden frame
// is 240x160 pixels: 120*2 wide by 40*4 tall. All three backends render this one
// frame, so ns/op, B/op, allocs/op, and bytes/frame compare like for like.
const (
	benchCols  = 120
	benchRows  = 40
	benchPxW   = benchCols * 2
	benchPxH   = benchRows * 4
	benchGOGC  = 100
	benchProcs = 1
)

// goldenFrame builds the pinned golden frame: a deterministic RGBA gradient at
// the pinned 240x160 resolution. The pattern varies in all three colour channels
// across the frame so the colour backends do real palette and compression work,
// not a flat fill. Channel order is RGBA.
func goldenFrame() render.Frame {
	img := image.NewRGBA(image.Rect(0, 0, benchPxW, benchPxH))
	for y := range benchPxH {
		for x := range benchPxW {
			c := color.RGBA{
				R: uint8(x % 256),
				G: uint8(y % 256),
				B: uint8((x + y) % 256),
				A: 0xff,
			}
			img.SetRGBA(x, y, c)
		}
	}
	return render.Frame{Img: img}
}

// pinRuntime pins GOMAXPROCS and GOGC for the duration of a benchmark and
// restores both when the returned cleanup runs. Pinning removes scheduler and GC
// pacing variance so the per-backend numbers are comparable across runs.
func pinRuntime(b *testing.B) {
	b.Helper()
	prevProcs := runtime.GOMAXPROCS(benchProcs)
	prevGOGC := debug.SetGCPercent(benchGOGC)
	b.Cleanup(func() {
		runtime.GOMAXPROCS(prevProcs)
		debug.SetGCPercent(prevGOGC)
	})
}

// BenchmarkRenderers runs one sub-benchmark per backend over the same pinned
// golden frame. Each sub-benchmark reports ns/op, B/op, allocs/op, and the
// bytes/frame custom metric, so a single run compares the three backends on cost
// and payload size.
func BenchmarkRenderers(b *testing.B) {
	f := goldenFrame()
	renderers := []render.Renderer{
		&block.Renderer{Cols: benchCols, Rows: benchRows},
		&sixel.Renderer{},
		&kitty.Renderer{},
	}
	for _, r := range renderers {
		b.Run(r.Name(), func(b *testing.B) {
			benchRenderer(b, r, f)
		})
	}
}

// benchRenderer drives one renderer over the golden frame. It pins the runtime,
// reports allocations, sets the per-op byte throughput, and reports the steady
// bytes/frame as a custom metric measured from a CountingWriter on the last
// iteration.
func benchRenderer(b *testing.B, r render.Renderer, f render.Frame) {
	pinRuntime(b)

	// One framed render to size the payload for SetBytes and bytes/frame.
	sizer := metrics.NewCountingWriter(io.Discard)
	if err := r.Render(f, sizer); err != nil {
		b.Fatalf("%s: render: %v", r.Name(), err)
	}
	frameBytes := sizer.N
	b.SetBytes(frameBytes)
	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		if err := r.Render(f, io.Discard); err != nil {
			b.Fatalf("%s: render: %v", r.Name(), err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(frameBytes), "bytes/frame")
}
