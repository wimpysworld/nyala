package palette

import (
	"image"
	"image/color"
	"testing"

	"github.com/wimpysworld/nyala/internal/metrics"
)

// paletteColours is the register budget the backends quantise to: 256 minus the
// reserved transparent register.
const paletteColours = 255

// driveFrame runs one frame through the engine in the order a backend does:
// sample, drift test, and re-quantise only when reuse fails.
func driveFrame(e *Engine, img *image.RGBA) {
	sample := e.Sample(img)
	if !e.Reuse(img.Bounds(), sample) {
		e.Requantise(img.Bounds(), sample, paletteColours)
	}
}

// reuseGradient builds a smooth 60-row gradient with far more than 255
// unique colours, so every frame drives median-cut quantisation.
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
	var engine Engine
	driveFrame(&engine, img)
	driveFrame(&engine, moved)
	if engine.RequantCount != 1 {
		t.Fatalf("RequantCount = %d, want 1 (small motion must reuse the palette)", engine.RequantCount)
	}
}

func TestPaletteReuseSceneChangeRequantises(t *testing.T) {
	t.Parallel()
	img := reuseGradient(128)
	var engine Engine
	driveFrame(&engine, img)
	driveFrame(&engine, invertRGB(img))
	if engine.RequantCount != 2 {
		t.Fatalf("RequantCount = %d, want 2 (a scene change must re-quantise immediately)", engine.RequantCount)
	}
}

func TestPaletteReuseGeometryChangeInvalidates(t *testing.T) {
	t.Parallel()
	var engine Engine
	driveFrame(&engine, reuseGradient(128))
	driveFrame(&engine, reuseGradient(120))
	if engine.RequantCount != 2 {
		t.Fatalf("RequantCount = %d, want 2 (a geometry change must re-quantise)", engine.RequantCount)
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

func containsRGBA(palette color.Palette, want color.RGBA) bool {
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
	if de := metrics.DeltaE2000(base.R, base.G, base.B, shifted.R, shifted.G, shifted.B); de >= FreezeMaxDeltaE {
		t.Fatalf("fixture colours differ by %.2f dE, want < %.2f", de, FreezeMaxDeltaE)
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
	fresh := SamplePalette(frameB, paletteColours)
	if !containsRGBA(fresh, shifted) {
		t.Fatal("fresh quantisation did not isolate the shifted flat colour; fixture invalid")
	}
	if containsRGBA(fresh, base) {
		t.Fatal("fresh quantisation already contains the old colour; fixture invalid")
	}

	var engine Engine
	driveFrame(&engine, frameA)
	if !containsRGBA(engine.Palette, base) {
		t.Fatal("frame A palette did not isolate the flat base colour; fixture invalid")
	}
	driveFrame(&engine, frameB)
	if engine.RequantCount != 2 {
		t.Fatalf("RequantCount = %d, want 2 (frame B must re-quantise)", engine.RequantCount)
	}
	if !containsRGBA(engine.Palette, base) {
		t.Fatal("hysteresis did not freeze the sub-JND register at its old value")
	}
	if containsRGBA(engine.Palette, shifted) {
		t.Fatal("sub-JND replacement value survived alongside the frozen register")
	}
}
