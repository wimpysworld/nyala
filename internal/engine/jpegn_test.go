package engine

// jpegn wiring guard: the live block screencast path decodes through
// github.com/gen2brain/jpegn (SIMD, native image.YCbCr) with a stdlib fallback
// (decodeJPEGFast in frame.go). These tests guard the near-identical decode
// guarantee, prove the fallback engages on an input jpegn declines, and keep a
// jpegn-vs-stdlib decode benchmark so the win stays measurable.
//
// Run:
//   go test -run 'TestJpegn|TestDecodeJPEGFast' ./internal/engine/
//   go test -bench BenchmarkJPEGDecode -benchmem ./internal/engine/

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"testing"

	"github.com/gen2brain/jpegn"
)

// jpegnSize is one benchmark corpus entry.
type jpegnSize struct {
	name string
	w, h int
}

var jpegnSizes = []jpegnSize{
	{"1920x1080", 1920, 1080},
	{"2528x2838_7MP", 2528, 2838},
	{"3840x2838", 3840, 2838},
}

// loadSourceRGBA loads the most photographic committed test image as RGBA.
func loadSourceRGBA(tb testing.TB) *image.RGBA {
	tb.Helper()
	raw, err := os.ReadFile("../../testdata/linuxmatters.png")
	if err != nil {
		tb.Fatalf("read source png: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		tb.Fatalf("decode source png: %v", err)
	}
	return imageToRGBA(img)
}

// tileTo returns an RGBA of size w x h, tiling src to fill it. Tiling keeps real
// photographic content (edges, gradients, text) across the whole frame, so the
// IDCT and upsample work is representative of a Chromium screencast frame.
func tileTo(src *image.RGBA, w, h int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	for y := range h {
		sy := sb.Min.Y + (y % sh)
		for x := range w {
			sx := sb.Min.X + (x % sw)
			dst.Set(x, y, src.At(sx, sy))
		}
	}
	return dst
}

// encodeBaseline420 encodes img to a baseline (sequential) JPEG at the live q90
// quality. The stdlib encoder emits baseline 4:2:0 for a colour image, the exact
// class Chromium's screencast produces.
func encodeBaseline420(tb testing.TB, img image.Image) []byte {
	tb.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: movingScreencastQuality}); err != nil {
		tb.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

// corpusJPEG builds and encodes the corpus JPEG for one size.
func corpusJPEG(tb testing.TB, src *image.RGBA, s jpegnSize) []byte {
	return encodeBaseline420(tb, tileTo(src, s.w, s.h))
}

// maxRGBADelta converts both images to RGBA and returns the max absolute
// per-channel difference across all pixels.
func maxRGBADelta(tb testing.TB, a, b image.Image) int {
	tb.Helper()
	ra, rb := imageToRGBA(a), imageToRGBA(b)
	if !ra.Bounds().Eq(rb.Bounds()) {
		tb.Fatalf("bounds differ: %v vs %v", ra.Bounds(), rb.Bounds())
	}
	max := 0
	for i := range ra.Pix {
		d := int(ra.Pix[i]) - int(rb.Pix[i])
		if d < 0 {
			d = -d
		}
		if d > max {
			max = d
		}
	}
	return max
}

// TestJpegnEquivalence confirms the live decode helper's jpegn path returns a
// native YCbCr 4:2:0 image that matches stdlib within a small bounded per-channel
// RGBA delta, so the fused sub-cell downscale stays near-identical. The spike
// measured a max delta of 3 on amd64; the tolerance leaves headroom for the arm64
// FMA rounding difference.
func TestJpegnEquivalence(t *testing.T) {
	const tolerance = 8
	src := loadSourceRGBA(t)
	for _, s := range jpegnSizes {
		jbytes := corpusJPEG(t, src, s)

		stdImg, err := jpeg.Decode(bytes.NewReader(jbytes))
		if err != nil {
			t.Fatalf("%s: stdlib decode: %v", s.name, err)
		}

		got, err := decodeJPEGFast(jbytes, false)
		if err != nil {
			t.Fatalf("%s: decodeJPEGFast: %v", s.name, err)
		}
		yc, ok := got.(*image.YCbCr)
		if !ok {
			t.Fatalf("%s: decodeJPEGFast returned %T, want *image.YCbCr; the fused downscale needs YCbCr", s.name, got)
		}
		if yc.SubsampleRatio != image.YCbCrSubsampleRatio420 {
			t.Errorf("%s: subsample ratio = %v, want 420", s.name, yc.SubsampleRatio)
		}

		maxD := maxRGBADelta(t, stdImg, got)
		t.Logf("%-14s jpegn YCbCr 420, max per-channel RGBA delta vs stdlib = %d", s.name, maxD)
		if maxD > tolerance {
			t.Errorf("%s: max per-channel delta %d exceeds tolerance (%d)", s.name, maxD, tolerance)
		}
	}
}

// TestDecodeJPEGFastRGBAExact pins the Sixel fast path to jpegn's fused RGBA
// result. Geometry and every decoded byte must match the direct decoder call.
func TestDecodeJPEGFastRGBAExact(t *testing.T) {
	raw := encodeBaseline420(t, loadSourceRGBA(t))
	wantImg, err := jpegn.Decode(bytes.NewReader(raw), &jpegn.Options{ToRGBA: true})
	if err != nil {
		t.Fatalf("direct jpegn RGBA decode: %v", err)
	}
	want, ok := wantImg.(*image.RGBA)
	if !ok {
		t.Fatalf("direct jpegn RGBA decode returned %T, want *image.RGBA", wantImg)
	}

	gotImg, err := decodeJPEGFast(raw, true)
	if err != nil {
		t.Fatalf("decodeJPEGFast RGBA: %v", err)
	}
	got, ok := gotImg.(*image.RGBA)
	if !ok {
		t.Fatalf("decodeJPEGFast RGBA returned %T, want *image.RGBA", gotImg)
	}
	if got.Rect != want.Rect || got.Stride != want.Stride {
		t.Fatalf("RGBA geometry = rect %v stride %d, want rect %v stride %d", got.Rect, got.Stride, want.Rect, want.Stride)
	}
	if !bytes.Equal(got.Pix, want.Pix) {
		t.Fatal("decodeJPEGFast RGBA pixels differ from direct jpegn output")
	}
}

// TestDecodeJPEGFastFallback proves both jpegn modes fall back on input they decline.
// PNG bytes make jpegn error, then decodeFrameImage must decode the settled frame
// unchanged. This exercises the same branch as jpegn.ErrUnsupported JPEG input.
func TestDecodeJPEGFastFallback(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/linuxmatters.png")
	if err != nil {
		t.Fatalf("read source png: %v", err)
	}

	// Confirm jpegn declines the bytes, so the fallback branch is the one taken.
	if _, err := jpegn.Decode(bytes.NewReader(raw)); err == nil {
		t.Fatal("jpegn decoded PNG bytes without error; the fallback branch is not exercised")
	}

	want, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("stdlib png decode: %v", err)
	}
	for _, toRGBA := range []bool{false, true} {
		got, err := decodeJPEGFast(raw, toRGBA)
		if err != nil {
			t.Fatalf("decodeJPEGFast(toRGBA=%v) fallback: %v", toRGBA, err)
		}
		if d := maxRGBADelta(t, want, got); d != 0 {
			t.Errorf("toRGBA=%v fallback differs from stdlib PNG: max delta %d, want 0", toRGBA, d)
		}
	}
}

// TestDecodeJPEGFastCorruptReturnsError proves corrupt input survives both jpegn
// attempts and reaches the fallback error without producing a partial frame.
func TestDecodeJPEGFastCorruptReturnsError(t *testing.T) {
	raw := []byte{0xff, 0xd8, 0xff, 0xdb, 0x00}
	for _, toRGBA := range []bool{false, true} {
		if img, err := decodeJPEGFast(raw, toRGBA); err == nil {
			t.Fatalf("decodeJPEGFast(toRGBA=%v) returned %T, want error", toRGBA, img)
		}
	}
}

func benchDecodeStdlib(b *testing.B, jbytes []byte) {
	b.SetBytes(int64(len(jbytes)))
	b.ReportAllocs()
	r := bytes.NewReader(jbytes)
	for range b.N {
		r.Reset(jbytes)
		img, err := jpeg.Decode(r)
		if err != nil {
			b.Fatal(err)
		}
		_ = img
	}
}

func benchDecodeJpegn(b *testing.B, jbytes []byte) {
	b.SetBytes(int64(len(jbytes)))
	b.ReportAllocs()
	r := bytes.NewReader(jbytes)
	for range b.N {
		r.Reset(jbytes)
		img, err := jpegn.Decode(r)
		if err != nil {
			b.Fatal(err)
		}
		_ = img
	}
}

// BenchmarkJPEGDecode benchmarks jpegn vs stdlib per corpus size. Compare the two
// sub-benchmarks at each size for the speedup ratio.
func BenchmarkJPEGDecode(b *testing.B) {
	src := loadSourceRGBA(b)
	for _, s := range jpegnSizes {
		jbytes := corpusJPEG(b, src, s)
		b.Run(fmt.Sprintf("%s/stdlib", s.name), func(b *testing.B) { benchDecodeStdlib(b, jbytes) })
		b.Run(fmt.Sprintf("%s/jpegn", s.name), func(b *testing.B) { benchDecodeJpegn(b, jbytes) })
	}
}
