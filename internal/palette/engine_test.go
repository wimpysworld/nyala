package palette

import (
	"image"
	"image/color"
	"slices"
	"testing"

	"github.com/wimpysworld/nyala/internal/metrics"
)

func solidFrame(c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := range 8 {
		for x := range 8 {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

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
	first := reuseGradient(128)
	second := reuseGradient(120)
	driveFrame(&engine, first)
	sample := engine.Sample(second)
	if engine.Reuse(second.Bounds(), sample) {
		t.Fatal("a geometry change reused the retained palette")
	}
	result := engine.Requantise(second.Bounds(), sample, paletteColours)
	if result.PriorValid || result.ExactNoOp {
		t.Fatalf("geometry-change result = %+v, want no valid prior state", result)
	}
	if engine.RequantCount != 2 {
		t.Fatalf("RequantCount = %d, want 2 (a geometry change must re-quantise)", engine.RequantCount)
	}
}

func TestRequantiseReportsExactNoOpAndKeepsBytes(t *testing.T) {
	t.Parallel()
	img := reuseGradient(128)
	var engine Engine
	sample := engine.Sample(img)
	first := engine.Requantise(img.Bounds(), sample, paletteColours)
	if first.PriorValid || first.LengthEqual || first.PaletteEqual || first.LUTEqual || first.ExactNoOp {
		t.Fatalf("initial result = %+v, want no prior equality", first)
	}
	if first.ChangedEntries != len(engine.Palette) {
		t.Fatalf("initial ChangedEntries = %d, want %d", first.ChangedEntries, len(engine.Palette))
	}
	wantPalette := slices.Clone(AppendOSC4(nil, engine.Palette, engine.Base()))
	wantLUT := slices.Clone(engine.LUT)

	result := engine.Requantise(img.Bounds(), sample, paletteColours)
	if !result.PriorValid || !result.LengthEqual || !result.PaletteEqual || !result.LUTEqual || !result.ExactNoOp {
		t.Fatalf("second result = %+v, want exact no-op", result)
	}
	if result.ChangedEntries != 0 {
		t.Fatalf("second ChangedEntries = %d, want 0", result.ChangedEntries)
	}
	if got := AppendOSC4(nil, engine.Palette, engine.Base()); !slices.Equal(got, wantPalette) {
		t.Fatal("an equal re-fit changed the emitted palette bytes")
	}
	if !slices.Equal(engine.LUT, wantLUT) {
		t.Fatal("an equal re-fit changed the LUT bytes")
	}
}

func TestRequantiseReportsOneChangedEntry(t *testing.T) {
	t.Parallel()
	black := solidFrame(color.RGBA{A: 0xff})
	white := solidFrame(color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff})
	var engine Engine
	engine.Requantise(black.Bounds(), engine.Sample(black), 2)
	result := engine.Requantise(white.Bounds(), engine.Sample(white), 2)
	if !result.PriorValid || !result.LengthEqual || result.PaletteEqual || !result.LUTEqual || result.ExactNoOp {
		t.Fatalf("changed-colour result = %+v", result)
	}
	if result.ChangedEntries != 1 {
		t.Fatalf("ChangedEntries = %d, want 1", result.ChangedEntries)
	}
}

func TestRequantiseReportsLUTDifference(t *testing.T) {
	t.Parallel()
	img := reuseGradient(128)
	var engine Engine
	sample := engine.Sample(img)
	engine.Requantise(img.Bounds(), sample, paletteColours)
	engine.LUT[123] ^= 1
	result := engine.Requantise(img.Bounds(), sample, paletteColours)
	if !result.PaletteEqual || result.LUTEqual || result.ExactNoOp {
		t.Fatalf("LUT-change result = %+v", result)
	}
}

func TestRequantiseAfterInvalidationIsNotNoOp(t *testing.T) {
	t.Parallel()
	img := reuseGradient(128)
	var engine Engine
	sample := engine.Sample(img)
	engine.Requantise(img.Bounds(), sample, paletteColours)
	engine.Invalidate()
	result := engine.Requantise(img.Bounds(), sample, paletteColours)
	if result.PriorValid || result.LengthEqual || result.PaletteEqual || result.LUTEqual || result.ExactNoOp {
		t.Fatalf("invalidated result = %+v, want no prior equality", result)
	}
	if result.ChangedEntries != len(engine.Palette) {
		t.Fatalf("ChangedEntries = %d, want %d", result.ChangedEntries, len(engine.Palette))
	}
}

func TestComparePaletteRefitUsesEmittedRGBAndActiveLength(t *testing.T) {
	t.Parallel()
	previous := color.Palette{
		color.RGBA64{R: 0x1200, G: 0x3400, B: 0x5600, A: 0xffff},
		color.RGBA64{R: 0x7800, G: 0x9a00, B: 0xbc00, A: 0xffff},
	}
	current := color.Palette{
		color.RGBA64{R: 0x12ff, G: 0x34ff, B: 0x56ff, A: 0xff00},
		color.RGBA64{R: 0x7800, G: 0x9a00, B: 0xbd00, A: 0xffff},
	}
	changed := comparePaletteRefit(true, previous, current)
	if !changed.LengthEqual || changed.PaletteEqual || changed.ChangedEntries != 1 {
		t.Fatalf("emitted-RGB result = %+v, want one changed entry", changed)
	}

	removed := comparePaletteRefit(true, previous, current[:1])
	if removed.LengthEqual || removed.PaletteEqual || removed.ChangedEntries != 0 {
		t.Fatalf("removed-tail result = %+v, want unequal length and no changed active entries", removed)
	}
	added := comparePaletteRefit(true, previous[:1], previous)
	if added.LengthEqual || added.PaletteEqual || added.ChangedEntries != 1 {
		t.Fatalf("added-entry result = %+v, want unequal length and one changed active entry", added)
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
