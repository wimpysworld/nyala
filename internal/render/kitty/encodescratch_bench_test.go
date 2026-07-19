package kitty

import (
	"bytes"
	"image"
	"image/color"
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
)

// benchFrame builds a page-like RGBA frame: solid colour bands with a few noisy
// blocks and opaque alpha, so zlib compresses roughly like a real web page rather
// than worst-case noise. A 1920x1080 frame provides a representative workload.
func benchFrame(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		band := (y / 40) % 4
		var base color.RGBA
		switch band {
		case 0:
			base = color.RGBA{0xff, 0xff, 0xff, 0xff}
		case 1:
			base = color.RGBA{0x22, 0x44, 0x88, 0xff}
		case 2:
			base = color.RGBA{0xf0, 0xf0, 0xf0, 0xff}
		default:
			base = color.RGBA{0x10, 0x10, 0x10, 0xff}
		}
		off := img.PixOffset(0, y)
		for x := range w {
			c := base
			if (x/60+y/60)%7 == 0 {
				//nolint:gosec // deliberate 8-bit wrap for test pixel noise
				c = color.RGBA{uint8(x * 3), uint8(y * 5), uint8(x ^ y), 0xff}
			}
			p := img.Pix[off : off+4 : off+4]
			p[0], p[1], p[2], p[3] = c.R, c.G, c.B, c.A
			off += 4
		}
	}
	return img
}

const benchW, benchH = 1920, 1080

// BenchmarkBaselineUpstream measures the stateless Renderer, which drives the
// upstream per-pixel encoder plus a fresh base64 destination per frame.
func BenchmarkBaselineUpstream(b *testing.B) {
	img := benchFrame(benchW, benchH)
	var r Renderer
	var dst bytes.Buffer
	b.ReportAllocs()
	for b.Loop() {
		dst.Reset()
		if err := r.RenderID(render.Frame{Img: img}, 1, &dst); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(dst.Len()), "payloadB")
}
