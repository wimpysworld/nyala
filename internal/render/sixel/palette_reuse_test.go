package sixel

import (
	"bytes"
	"image"
	"image/color"
	"testing"

	"github.com/wimpysworld/nyala/internal/metrics"
)

// reuseGradient builds a smooth 60-row gradient with far more than 255
// unique colours, forcing the adaptive palette path.
func reuseGradient(w int) *image.RGBA {
	const h = 60
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
	img := reuseGradient(128)
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
	if enc.requantCount != 1 {
		t.Fatalf("requantCount = %d, want 1 (later static frames must skip quantisation)", enc.requantCount)
	}
}

func TestPaletteReuseSmallMotionBelowThreshold(t *testing.T) {
	t.Parallel()
	img := reuseGradient(128)
	moved := reuseGradient(128)
	for y := 20; y < 28; y++ {
		for x := 40; x < 48; x++ {
			c := moved.RGBAAt(x, y)
			c.R += 6
			c.B += 6
			moved.SetRGBA(x, y, c)
		}
	}
	enc := newOpaqueEncoder(nil)
	encodeReuseFrame(t, enc, img)
	encodeReuseFrame(t, enc, moved)
	if enc.requantCount != 1 {
		t.Fatalf("requantCount = %d, want 1 (small motion must reuse the palette)", enc.requantCount)
	}
}

func TestPaletteReuseSceneChangeRequantises(t *testing.T) {
	t.Parallel()
	img := reuseGradient(128)
	enc := newOpaqueEncoder(nil)
	encodeReuseFrame(t, enc, img)
	encodeReuseFrame(t, enc, invertRGB(img))
	if enc.requantCount != 2 {
		t.Fatalf("requantCount = %d, want 2 (a scene change must re-quantise immediately)", enc.requantCount)
	}
}

func TestPaletteReuseGeometryChangeInvalidates(t *testing.T) {
	t.Parallel()
	enc := newOpaqueEncoder(nil)
	encodeReuseFrame(t, enc, reuseGradient(128))
	encodeReuseFrame(t, enc, reuseGradient(120))
	if enc.requantCount != 2 {
		t.Fatalf("requantCount = %d, want 2 (a geometry change must re-quantise)", enc.requantCount)
	}
}

func TestPaletteReuseExactPathInterludeInvalidates(t *testing.T) {
	t.Parallel()
	img := reuseGradient(128)
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
	if enc.requantCount != 2 {
		t.Fatalf("requantCount = %d, want 2 (leaving the adaptive path must invalidate reuse)", enc.requantCount)
	}
}

// hysteresisFrame fills the left half with a flat colour and the right half
// with the given gradient, so median cut yields the flat colour exactly.
func hysteresisFrame(flat color.RGBA, right func(x, y int) color.RGBA) *image.RGBA {
	const w, h = 128, 60
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := flat
			if x >= w/2 {
				c = right(x, y)
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func paletteContainsRGBA(palette color.Palette, want color.RGBA) bool {
	for _, entry := range palette {
		r, g, b, _ := entry.RGBA()
		if uint8(r>>8) == want.R && uint8(g>>8) == want.G && uint8(b>>8) == want.B { //nolint:gosec // 8-bit channel values.
			return true
		}
	}
	return false
}

func TestPaletteHysteresisFreezesSubJNDRegisters(t *testing.T) {
	t.Parallel()
	base := color.RGBA{R: 200, G: 60, B: 60, A: 0xff}
	shifted := color.RGBA{R: 202, G: 60, B: 60, A: 0xff}
	if de := metrics.DeltaE2000(base.R, base.G, base.B, shifted.R, shifted.G, shifted.B); de >= paletteFreezeMaxDeltaE {
		t.Fatalf("fixture colours differ by %.2f dE, want < %.2f", de, paletteFreezeMaxDeltaE)
	}
	frameA := hysteresisFrame(base, func(x, y int) color.RGBA {
		return color.RGBA{
			R: 0,
			G: uint8(80 + y*2), //nolint:gosec // Bounded by the fixture height.
			B: uint8(255 - x),  //nolint:gosec // Bounded by the fixture width.
			A: 0xff,
		}
	})
	frameB := hysteresisFrame(shifted, func(x, y int) color.RGBA {
		return color.RGBA{
			R: uint8(255 - x),   //nolint:gosec // Bounded by the fixture width.
			G: uint8(200 - y*3), //nolint:gosec // Bounded by the fixture height.
			B: 0,
			A: 0xff,
		}
	})
	// Premise: quantising frame B without history yields the shifted flat
	// colour exactly, so any surviving base entry proves the freeze.
	fresh := samplePalette(frameB, 255)
	if !paletteContainsRGBA(fresh, shifted) {
		t.Fatal("fresh quantisation did not isolate the shifted flat colour; fixture invalid")
	}
	if paletteContainsRGBA(fresh, base) {
		t.Fatal("fresh quantisation already contains the old colour; fixture invalid")
	}

	enc := newOpaqueEncoder(nil)
	encodeReuseFrame(t, enc, frameA)
	if !paletteContainsRGBA(enc.palette, base) {
		t.Fatal("frame A palette did not isolate the flat base colour; fixture invalid")
	}
	encodeReuseFrame(t, enc, frameB)
	if enc.requantCount != 2 {
		t.Fatalf("requantCount = %d, want 2 (frame B must re-quantise)", enc.requantCount)
	}
	if !paletteContainsRGBA(enc.palette, base) {
		t.Fatal("hysteresis did not freeze the sub-JND register at its old value")
	}
	if paletteContainsRGBA(enc.palette, shifted) {
		t.Fatal("sub-JND replacement value survived alongside the frozen register")
	}
}

func TestPaletteReuseDeterministicSequence(t *testing.T) {
	t.Parallel()
	img := reuseGradient(128)
	moved := reuseGradient(128)
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
