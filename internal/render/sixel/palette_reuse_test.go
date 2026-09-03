package sixel

import (
	"bytes"
	"image"
	"image/color"
	"testing"
)

// reuseGradient builds a smooth 128x60 gradient with far more than 255
// unique colours, forcing the adaptive palette path.
func reuseGradient() *image.RGBA {
	const w, h = 128, 60
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(x * 2), //nolint:gosec // Deliberate wrapping test pattern.
				G: uint8(y * 4), //nolint:gosec // Deliberate wrapping test pattern.
				B: uint8(x + y), //nolint:gosec // Deliberate wrapping test pattern.
				A: 0xff,
			})
		}
	}
	return img
}

// invertRGB returns a copy with every channel inverted, a full scene change
// relative to the source.
func invertRGB(src *image.RGBA) *image.RGBA {
	dst := image.NewRGBA(src.Bounds())
	copy(dst.Pix, src.Pix)
	for i := 0; i < len(dst.Pix); i += 4 {
		dst.Pix[i] = 255 - dst.Pix[i]
		dst.Pix[i+1] = 255 - dst.Pix[i+1]
		dst.Pix[i+2] = 255 - dst.Pix[i+2]
	}
	return dst
}

func encodeReuseFrame(t *testing.T, enc *opaqueEncoder, img image.Image) []byte {
	t.Helper()
	var out bytes.Buffer
	enc.w = &out
	if err := enc.encode(img); err != nil {
		t.Fatal(err)
	}
	enc.w = nil
	return out.Bytes()
}

func TestPaletteReuseStaticSequenceSkipsQuantisation(t *testing.T) {
	t.Parallel()
	img := reuseGradient()
	if _, exact := palettedFromRGBA(img, 255); exact {
		t.Fatal("fixture did not take the adaptive path")
	}
	enc := newOpaqueEncoder(nil)
	first := encodeReuseFrame(t, enc, img)
	second := encodeReuseFrame(t, enc, img)
	third := encodeReuseFrame(t, enc, img)
	if !bytes.Equal(second, first) || !bytes.Equal(third, first) {
		t.Fatal("static frames are not byte-identical under palette reuse")
	}
	if enc.RequantCount != 1 {
		t.Fatalf("RequantCount = %d, want 1 (later static frames must skip quantisation)", enc.RequantCount)
	}
}

func TestPaletteReuseExactPathInterludeInvalidates(t *testing.T) {
	t.Parallel()
	img := reuseGradient()
	flat := image.NewRGBA(img.Bounds())
	for i := 0; i < len(flat.Pix); i += 4 {
		flat.Pix[i], flat.Pix[i+1], flat.Pix[i+2], flat.Pix[i+3] = 30, 40, 50, 0xff
	}
	if _, exact := palettedFromRGBA(flat, 255); !exact {
		t.Fatal("interlude fixture did not take the exact path")
	}
	enc := newOpaqueEncoder(nil)
	encodeReuseFrame(t, enc, img)
	encodeReuseFrame(t, enc, flat)
	encodeReuseFrame(t, enc, img)
	if enc.RequantCount != 2 {
		t.Fatalf("RequantCount = %d, want 2 (leaving the adaptive path must invalidate reuse)", enc.RequantCount)
	}
}

func TestPaletteReuseDeterministicSequence(t *testing.T) {
	t.Parallel()
	img := reuseGradient()
	moved := reuseGradient()
	for x := 10; x < 20; x++ {
		c := moved.RGBAAt(x, 30)
		c.G += 4
		moved.SetRGBA(x, 30, c)
	}
	frames := []image.Image{img, moved, invertRGB(img), img}
	run := func() [][]byte {
		enc := newOpaqueEncoder(nil)
		payloads := make([][]byte, 0, len(frames))
		for _, frame := range frames {
			payloads = append(payloads, encodeReuseFrame(t, enc, frame))
		}
		return payloads
	}
	first, second := run(), run()
	for i := range frames {
		if !bytes.Equal(first[i], second[i]) {
			t.Fatalf("frame %d payload differs between identical runs", i)
		}
	}
}
