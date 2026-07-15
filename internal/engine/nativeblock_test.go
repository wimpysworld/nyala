package engine

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// TestDecodeLiveFrameKeepsNativeYCbCr verifies that JPEG decode preserves the
// standard decoder's native full-size image layout.
func TestDecodeLiveFrameKeepsNativeYCbCr(t *testing.T) {
	const width, height = 96, 83
	src := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			src.SetRGBA(x, y, color.RGBA{
				R: uint8(x*7 + y),
				G: uint8(x + y*11),
				B: uint8(x*3 + y*5),
				A: 0xff,
			})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: blockScreencastQuality}); err != nil {
		t.Fatalf("encode JPEG: %v", err)
	}
	e := &ChromiumEngine{}
	got, err := e.decodeLiveFrame(base64.StdEncoding.EncodeToString(buf.Bytes()))
	if err != nil {
		t.Fatalf("decode live frame: %v", err)
	}
	yc, ok := got.(*image.YCbCr)
	if !ok {
		t.Fatalf("decoded frame is %T, want native *image.YCbCr", got)
	}
	if yc.Bounds().Dx() != width || yc.Bounds().Dy() != height {
		t.Fatalf("decoded Block frame bounds = %v, want %dx%d full surface", yc.Bounds(), width, height)
	}
}
