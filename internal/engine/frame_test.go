package engine

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

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
// base64-encodes it. JPEG decodes to YCbCr, so this exercises the non-RGBA to
// RGBA conversion path in decodeScreencastImage. A solid fill avoids JPEG chroma
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

// TestDecodeScreencastImagePNGChannelOrder checks the PNG fast path returns RGBA
// channel order, not BGRA. A channel swap (the render-pipeline gotcha) would put
// the red value in B, which the per-channel asserts catch.
func TestDecodeScreencastImagePNGChannelOrder(t *testing.T) {
	rgba, err := decodeScreencastImage(encodeBase64PNG(t))
	if err != nil {
		t.Fatalf("decode PNG: %v", err)
	}
	got := rgba.RGBAAt(0, 0)
	if got.R != 200 || got.G != 10 || got.B != 20 || got.A != 0xff {
		t.Fatalf("pixel (0,0) = %+v, want R=200 G=10 B=20 A=255 (RGBA order, not BGRA)", got)
	}
}

// TestDecodeScreencastImageJPEGConversion checks the non-RGBA conversion path:
// JPEG decodes to YCbCr, so the image is not already *image.RGBA. The decoded
// pixel must be approximately the source red in RGBA order. JPEG is lossy, so
// each channel is compared with a tolerance, but R must stay clearly the high
// channel, proving the order is RGBA not BGRA.
func TestDecodeScreencastImageJPEGConversion(t *testing.T) {
	rgba, err := decodeScreencastImage(encodeBase64JPEG(t))
	if err != nil {
		t.Fatalf("decode JPEG: %v", err)
	}
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

// TestDecodeScreencastImageErrors checks bad input returns an error rather than
// panicking: malformed base64, and valid base64 that is not a decodable image.
func TestDecodeScreencastImageErrors(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"invalid base64", "not valid base64 !!!"},
		{"valid base64 but not an image", base64.StdEncoding.EncodeToString([]byte("not an image"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeScreencastImage(tc.data); err == nil {
				t.Fatalf("decodeScreencastImage(%q) returned nil error, want an error", tc.name)
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
