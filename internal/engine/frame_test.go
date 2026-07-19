package engine

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"testing"
)

// layoutScaleEps is the tolerance for the fractional layout-scale compare. The
// image-maths note bars exact float equality; a small epsilon absorbs the residue.
const layoutScaleEps = 1e-9

// TestLayoutScale checks the pure block-only device scale Z = max(1, cols*cellW /
// layoutWidth). A grid at or under the target width keeps Z == 1; a wider grid
// shrinks by the continuous ratio. The 120- and 240-column cases are the criterion.
func TestLayoutScale(t *testing.T) {
	cases := []struct {
		name        string
		cols        int
		cellW       int
		layoutWidth int
		want        float64
	}{
		// 120 cols * 8 = 960 <= 1280, so Z clamps to 1.
		{"120 cols under target holds at 1", 120, 8, 1280, 1},
		// 240 cols * 8 = 1920, so Z = 1920/1280 = 1.5.
		{"240 cols over target scales to 1.5", 240, 8, 1280, 1.5},
		// A grid exactly at the target keeps Z == 1, not a rounding step over.
		{"grid at target holds at 1", 160, 8, 1280, 1},
		// A non-positive target falls back to 1 rather than dividing by zero.
		{"zero target falls back to 1", 240, 8, 0, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := layoutScale(c.cols, c.cellW, c.layoutWidth)
			if math.Abs(got-c.want) > layoutScaleEps {
				t.Errorf("layoutScale(%d, %d, %d) = %g, want %g", c.cols, c.cellW, c.layoutWidth, got, c.want)
			}
		})
	}
}

// TestLayoutScaleBackendGate checks the engine method gates Z to the block
// backend: a graphics backend returns 1 even on a wide grid, and the block backend
// returns the pure layoutScale for the same grid.
func TestLayoutScaleBackendGate(t *testing.T) {
	const cols, cellW, layoutWidth = 240, 8, 1280

	block := &ChromiumEngine{backend: "block", cols: cols, cellW: cellW, layoutWidth: layoutWidth}
	if got := block.layoutScale(); math.Abs(got-1.5) > layoutScaleEps {
		t.Errorf("block layoutScale = %g, want 1.5", got)
	}
	for _, backend := range []string{"sixel", "kitty"} {
		g := &ChromiumEngine{backend: backend, cols: cols, cellW: cellW, layoutWidth: layoutWidth}
		if got := g.layoutScale(); math.Abs(got-1) > layoutScaleEps {
			t.Errorf("%s layoutScale = %g, want 1 (graphics stays at DSF 1)", backend, got)
		}
	}
}

// encodeBase64PNG builds a 2x2 image with a distinct red pixel at (0,0), encodes
// it as PNG, and base64-encodes the bytes, the shape EventScreencastFrame.Data
// carries. The red pixel pins the channel order: R high, G and B low.
func encodeBase64PNG(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 200, G: 10, B: 20, A: 0xff})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test PNG: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// encodeBase64JPEG builds a solid red 8x8 image, encodes it as JPEG, and
// base64-encodes it. JPEG decodes to YCbCr, so this exercises an explicit test-only
// RGBA conversion after the native production decode. A solid fill avoids JPEG chroma
// subsampling bleed (which a lone red pixel on a tiny image suffers), so the
// decoded colour stays close to the source. JPEG is lossy, so it is only
// approximately the source.
func encodeBase64JPEG(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	red := color.RGBA{R: 200, G: 10, B: 20, A: 0xff}
	for y := range 8 {
		for x := range 8 {
			img.Set(x, y, red)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatalf("encode test JPEG: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// TestDecodeScreencastNativePNGChannelOrder checks the PNG fast path returns RGBA
// channel order, not BGRA. A channel swap (the render-pipeline gotcha) would put
// the red value in B, which the per-channel asserts catch.
func TestDecodeScreencastNativePNGChannelOrder(t *testing.T) {
	img, err := decodeScreencastNative(encodeBase64PNG(t))
	if err != nil {
		t.Fatalf("decode PNG: %v", err)
	}
	got := color.RGBAModel.Convert(img.At(0, 0)).(color.RGBA)
	if got.R != 200 || got.G != 10 || got.B != 20 || got.A != 0xff {
		t.Fatalf("pixel (0,0) = %+v, want R=200 G=10 B=20 A=255 (RGBA order, not BGRA)", got)
	}
}

// TestDecodeScreencastNativeJPEGConversion checks the non-RGBA conversion path:
// JPEG decodes to YCbCr, so the image is not already *image.RGBA. The decoded
// pixel must be approximately the source red in RGBA order. JPEG is lossy, so
// each channel is compared with a tolerance, but R must stay clearly the high
// channel, proving the order is RGBA not BGRA.
func TestDecodeScreencastNativeJPEGConversion(t *testing.T) {
	img, err := decodeScreencastNative(encodeBase64JPEG(t))
	if err != nil {
		t.Fatalf("decode JPEG: %v", err)
	}
	rgba := imageToRGBA(img)
	got := rgba.RGBAAt(0, 0)
	const tol = 40
	if absDiff(got.R, 200) > tol || absDiff(got.G, 10) > tol || absDiff(got.B, 20) > tol {
		t.Fatalf("pixel (0,0) = %+v, want approx R=200 G=10 B=20 (RGBA order, tol %d)", got, tol)
	}
	if got.R <= got.G || got.R <= got.B {
		t.Fatalf("pixel (0,0) = %+v, R must dominate G and B (RGBA order, not BGRA)", got)
	}
	if got.A != 0xff {
		t.Fatalf("pixel (0,0) alpha = %d, want 255 (opaque)", got.A)
	}
}

// TestDecodeImageBytesJPEGChannelOrder checks the bulk draw.Draw conversion in
// decodeImageBytes keeps RGBA channel order for a non-RGBA (YCbCr) source. It builds
// a solid image whose three channels are clearly ranked (R > G > B), decodes the
// JPEG, and asserts the ranking survives, so a channel swap in the bulk draw would
// be caught. JPEG is lossy, so each channel is compared with a tolerance.
func TestDecodeImageBytesJPEGChannelOrder(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 8, 8))
	want := color.RGBA{R: 200, G: 100, B: 30, A: 0xff}
	for y := range 8 {
		for x := range 8 {
			src.Set(x, y, want)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatalf("encode test JPEG: %v", err)
	}
	rgba, err := decodeImageBytes(buf.Bytes())
	if err != nil {
		t.Fatalf("decode JPEG bytes: %v", err)
	}
	got := rgba.RGBAAt(0, 0)
	const tol = 40
	if absDiff(got.R, want.R) > tol || absDiff(got.G, want.G) > tol || absDiff(got.B, want.B) > tol {
		t.Fatalf("pixel (0,0) = %+v, want approx %+v (RGBA order, tol %d)", got, want, tol)
	}
	if got.R <= got.G || got.G <= got.B {
		t.Fatalf("pixel (0,0) = %+v, want R>G>B channel ranking preserved (RGBA order, not BGRA)", got)
	}
	if got.A != 0xff {
		t.Fatalf("pixel (0,0) alpha = %d, want 255 (opaque)", got.A)
	}
}

// TestDecodeScreencastNativeErrors checks bad input returns an error rather than
// panicking: malformed base64, and valid base64 that is not a decodable image.
func TestDecodeScreencastNativeErrors(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"invalid base64", "not valid base64 !!!"},
		{"valid base64 but not an image", base64.StdEncoding.EncodeToString([]byte("not an image"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeScreencastNative(tc.data); err == nil {
				t.Fatalf("decodeScreencastNative(%q) returned nil error, want an error", tc.name)
			}
		})
	}
}

// absDiff returns the absolute difference of two bytes as a byte, for the JPEG
// tolerance compare.
func absDiff(a, b uint8) uint8 {
	if a > b {
		return a - b
	}
	return b - a
}

// BenchmarkDecodeImageBytes measures the non-RGBA (YCbCr) conversion path over a
// screencast-sized synthetic JPEG frame, the hot decode the block path runs per
// screencast tick. It encodes once outside the timed loop, so the benchmark isolates
// the decode plus the bulk draw.Draw conversion.
func BenchmarkDecodeImageBytes(b *testing.B) {
	const w, h = 1280, 800
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			src.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: uint8((x + y) % 256), A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: blockScreencastQuality}); err != nil {
		b.Fatalf("encode benchmark JPEG: %v", err)
	}
	raw := buf.Bytes()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := decodeImageBytes(raw); err != nil {
			b.Fatalf("decode benchmark JPEG: %v", err)
		}
	}
}
