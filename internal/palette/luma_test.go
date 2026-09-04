package palette

import (
	"image"
	"image/color"
	"slices"
	"testing"

	"github.com/wimpysworld/nyala/internal/render/block"
)

// driveLumaFrame runs one frame through the greyscale engine in the order a
// backend does: sample, drift test, and re-fit only when reuse fails.
func driveLumaFrame(e *LumaEngine, img *image.RGBA) {
	sample := e.Sample(img)
	if !e.Reuse(img.Bounds(), sample) {
		e.Requantise(img.Bounds(), sample)
	}
}

// greyImage builds a w by h frame where column x carries the grey level
// levelAt(x). Every row repeats the same ramp, so the luma histogram is the
// column set with equal populations.
func greyImage(w, h int, levelAt func(x int) uint8) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			v := levelAt(x)
			img.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v, A: 0xff})
		}
	}
	return img
}

// lumaGradient builds a 60-row grey ramp whose column x carries luma x, so a
// width of w occupies exactly the levels 0..w-1.
func lumaGradient(w int) *image.RGBA {
	return greyImage(w, 60, func(x int) uint8 {
		return uint8(x) //nolint:gosec // The callers keep w at or below 256.
	})
}

// shiftLuma returns a copy of src with every channel raised by delta inside
// the region, saturating at 255. A grey source stays grey, so the luma moves
// by exactly delta.
func shiftLuma(src *image.RGBA, region image.Rectangle, delta uint8) *image.RGBA {
	dst := image.NewRGBA(src.Bounds())
	copy(dst.Pix, src.Pix)
	for y := region.Min.Y; y < region.Max.Y; y++ {
		for x := region.Min.X; x < region.Max.X; x++ {
			c := dst.RGBAAt(x, y)
			c.R = uint8(min(int(c.R)+int(delta), 255)) //nolint:gosec // Clamped to 255.
			c.G = uint8(min(int(c.G)+int(delta), 255)) //nolint:gosec // Clamped to 255.
			c.B = uint8(min(int(c.B)+int(delta), 255)) //nolint:gosec // Clamped to 255.
			dst.SetRGBA(x, y, c)
		}
	}
	return dst
}

// rgb555Key is the LUT index of a colour, in the layout BuildLumaLUT documents.
func rgb555Key(r, g, b uint8) int {
	return int(r>>3)<<10 | int(g>>3)<<5 | int(b>>3)
}

// greyLevel returns the 8-bit grey level of a palette entry and reports
// whether the entry is a pure grey.
func greyLevel(c color.Color) (uint8, bool) {
	r, g, b, _ := c.RGBA()
	if r != g || g != b {
		return 0, false
	}
	return uint8(r >> 8), true //nolint:gosec // 16-bit channel shifted to 8 bits.
}

// paletteLevels returns the grey level of every palette entry, failing the
// test on a non-grey entry.
func paletteLevels(t *testing.T, palette color.Palette) []uint8 {
	t.Helper()
	levels := make([]uint8, 0, len(palette))
	for i, entry := range palette {
		v, ok := greyLevel(entry)
		if !ok {
			t.Fatalf("Palette[%d] = %v, want a grey colour", i, entry)
		}
		levels = append(levels, v)
	}
	return levels
}

// sweepColours returns a deterministic sweep of colours over the RGB cube on a
// coarse lattice, plus the grey diagonal.
func sweepColours() []color.RGBA {
	colours := make([]color.RGBA, 0, 18*18*18+256)
	for r := 0; r < 256; r += 15 {
		for g := 0; g < 256; g += 15 {
			for b := 0; b < 256; b += 15 {
				colours = append(colours, color.RGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: 0xff}) //nolint:gosec // Bounded by the loop limits.
			}
		}
	}
	for v := range 256 {
		colours = append(colours, color.RGBA{R: uint8(v), G: uint8(v), B: uint8(v), A: 0xff}) //nolint:gosec // v ranges over 0..255.
	}
	return colours
}

func TestLuma8GreyIsIdentity(t *testing.T) {
	t.Parallel()
	for v := range 256 {
		g := uint8(v) //nolint:gosec // v ranges over 0..255.
		if got := Luma8(g, g, g); got != g {
			t.Fatalf("Luma8(%d, %d, %d) = %d, want %d", g, g, g, got, g)
		}
	}
}

// TestLuma8OrderingAgreesWithXTermGrey checks that the palette luma and the
// Block backend's fixed greyscale ramp never reverse the order of two
// colours: a strictly darker Luma8 never gets a lighter XTermGrey entry, and a
// strictly darker XTermGrey entry never gets a lighter Luma8. Both weight the
// channels 77, 150, 29, and rounding cannot reverse a strict order. Two
// colours with equal Luma8 can still straddle an XTermGrey step, because
// XTermGrey reads the unrounded luma, so ties are not compared.
func TestLuma8OrderingAgreesWithXTermGrey(t *testing.T) {
	t.Parallel()
	colours := sweepColours()
	luma := func(c color.RGBA) int { return int(Luma8(c.R, c.G, c.B)) }
	grey := func(c color.RGBA) int { return int(block.XTermGrey(c)) }
	assertMonotone(t, colours, "Luma8", luma, "XTermGrey", grey)
	assertMonotone(t, colours, "XTermGrey", grey, "Luma8", luma)
}

// assertMonotone sorts colours by the key and fails when some colour with a
// strictly lower key has a strictly higher value than a later colour.
func assertMonotone(t *testing.T, colours []color.RGBA, keyName string, key func(color.RGBA) int, valueName string, value func(color.RGBA) int) {
	t.Helper()
	slices.SortStableFunc(colours, func(a, b color.RGBA) int { return key(a) - key(b) })
	// below is the colour with the largest value among every colour whose key
	// is strictly below the current group; top is the largest of the current
	// group so far. A value of -1 marks an empty set.
	var below, top color.RGBA
	belowValue, topValue := -1, -1
	for i, c := range colours {
		if i > 0 && key(c) != key(colours[i-1]) && topValue > belowValue {
			below, belowValue = top, topValue
		}
		if value(c) < belowValue {
			t.Fatalf("%s(%v) = %d < %s(%v) = %d, but %s orders them %d > %d",
				valueName, c, value(c), valueName, below, belowValue,
				keyName, key(c), key(below))
		}
		if value(c) > topValue {
			top, topValue = c, value(c)
		}
	}
}

// TestLumaFitKeepsSparseLevelsExactly checks that a scene with 100 distinct
// grey levels yields exactly those levels, ascending, and that the LUT sends
// every RGB555 key to the level nearest the luma of its centre colour.
func TestLumaFitKeepsSparseLevelsExactly(t *testing.T) {
	t.Parallel()
	const count = 100
	want := make([]uint8, 0, count)
	for i := range count {
		// A non-uniform spacing, so equal gaps do not hide an ordering bug.
		want = append(want, uint8(i*2+i%2)) //nolint:gosec // At most 199.
	}
	img := greyImage(count, 8, func(x int) uint8 { return want[x] })

	var engine LumaEngine
	driveLumaFrame(&engine, img)
	got := paletteLevels(t, engine.Palette)
	if !slices.Equal(got, want) {
		t.Fatalf("palette levels = %v, want %v", got, want)
	}
	if engine.Base() != BudgetBase {
		t.Fatalf("Base() = %d, want %d", engine.Base(), BudgetBase)
	}
	if len(engine.LUT) != 1<<15 {
		t.Fatalf("len(LUT) = %d, want %d", len(engine.LUT), 1<<15)
	}
	for key, register := range engine.LUT {
		r := uint8(key>>10&31<<3 | 4) //nolint:gosec // A 15-bit index bounds this to 252.
		g := uint8(key>>5&31<<3 | 4)  //nolint:gosec // A 15-bit index bounds this to 252.
		b := uint8(key&31<<3 | 4)     //nolint:gosec // A 15-bit index bounds this to 252.
		centre := int(Luma8(r, g, b))
		j := int(register) - engine.Base()
		if j < 0 || j >= len(got) {
			t.Fatalf("LUT[%#x] = %d, want a register in %d..%d", key, register, engine.Base(), engine.Base()+len(got)-1)
		}
		bestDistance := 256
		for _, level := range got {
			bestDistance = min(bestDistance, absInt(centre-int(level)))
		}
		if d := absInt(centre - int(got[j])); d != bestDistance {
			t.Fatalf("LUT[%#x] maps luma %d to level %d at distance %d, want distance %d", key, centre, got[j], d, bestDistance)
		}
	}
}

// TestLumaMappingErrorIsZeroAtKeyCentres checks the exact case of the drift
// baseline: a grey scene whose levels sit on the luma of an RGB555 key centre
// maps through the LUT with no error at all. The RGB555 key has only 32 grey
// buckets, so a denser grey scene always carries some key rounding error.
func TestLumaMappingErrorIsZeroAtKeyCentres(t *testing.T) {
	t.Parallel()
	const count = 32
	img := greyImage(count, 8, func(x int) uint8 {
		return uint8(x<<3 | 4) //nolint:gosec // At most 252.
	})
	var engine LumaEngine
	driveLumaFrame(&engine, img)
	got := paletteLevels(t, engine.Palette)
	if len(got) != count {
		t.Fatalf("len(Palette) = %d, want %d", len(got), count)
	}
	if err := LumaMappingError(img, got, engine.LUT, engine.Base()); err != 0 {
		t.Fatalf("LumaMappingError = %v, want 0 for a scene on the key centres", err)
	}
}

// TestLumaFitMergesToBudget checks that a scene with all 256 luma levels fits
// into the 240-entry budget, stays non-decreasing, and sends every input luma
// through the LUT to a register in the budget range.
func TestLumaFitMergesToBudget(t *testing.T) {
	t.Parallel()
	img := lumaGradient(256)
	var engine LumaEngine
	driveLumaFrame(&engine, img)
	got := paletteLevels(t, engine.Palette)
	if len(got) != BudgetColours {
		t.Fatalf("len(Palette) = %d, want %d", len(got), BudgetColours)
	}
	if !slices.IsSorted(got) {
		t.Fatalf("palette levels are not non-decreasing: %v", got)
	}
	if len(engine.LUT) != 1<<15 {
		t.Fatalf("len(LUT) = %d, want %d", len(engine.LUT), 1<<15)
	}
	top := engine.Base() + len(got) - 1
	if top != 255 {
		t.Fatalf("top register = %d, want 255", top)
	}
	for v := range 256 {
		g := uint8(v) //nolint:gosec // v ranges over 0..255.
		register := int(engine.LUT[rgb555Key(g, g, g)])
		if register < engine.Base() || register > top {
			t.Fatalf("luma %d maps to register %d, want %d..%d", v, register, engine.Base(), top)
		}
	}
	for key, register := range engine.LUT {
		if int(register) < engine.Base() || int(register) > top {
			t.Fatalf("LUT[%#x] = %d, want %d..%d", key, register, engine.Base(), top)
		}
	}
}

// TestBuildLumaLUTMidpointPicksLighter checks the tie rule and the register
// range of the table.
func TestBuildLumaLUTMidpointPicksLighter(t *testing.T) {
	t.Parallel()
	const base = 200
	levels := []uint8{96, 104, 120}
	lut := BuildLumaLUT(nil, levels, base)
	if len(lut) != 1<<15 {
		t.Fatalf("len(lut) = %d, want %d", len(lut), 1<<15)
	}
	// Grey 100 is the centre colour of its own key and the exact midpoint of
	// 96 and 104, so the lighter level 104 must win.
	if got := lut[rgb555Key(100, 100, 100)]; got != base+1 {
		t.Fatalf("midpoint luma 100 maps to register %d, want %d (the lighter level)", got, base+1)
	}
	// Grey 108 is the centre of its key and the midpoint of 104 and 120 is
	// 112, so 104 stays nearest; the tie rule must not leak into a plain win.
	if got := lut[rgb555Key(108, 108, 108)]; got != base+1 {
		t.Fatalf("luma 108 maps to register %d, want %d", got, base+1)
	}
	// One key centre below the midpoint keeps the darker level.
	darker := color.RGBA{R: 100, G: 100, B: 92}
	if l := Luma8(darker.R, darker.G, darker.B); l != 99 {
		t.Fatalf("fixture luma = %d, want 99", l)
	}
	if got := lut[rgb555Key(darker.R, darker.G, darker.B)]; got != base {
		t.Fatalf("luma 99 maps to register %d, want %d", got, base)
	}
	for key, register := range lut {
		if int(register) < base || int(register) > base+len(levels)-1 {
			t.Fatalf("lut[%#x] = %d, want %d..%d", key, register, base, base+len(levels)-1)
		}
	}
	// A table with enough capacity is reused in place.
	again := BuildLumaLUT(lut, levels, base)
	if &again[0] != &lut[0] {
		t.Fatal("BuildLumaLUT reallocated a table that already had 32768 entries")
	}
}

func TestLumaReuseSmallMotionBelowThreshold(t *testing.T) {
	t.Parallel()
	img := lumaGradient(192)
	whole := shiftLuma(img, img.Bounds(), 1)
	region := shiftLuma(img, image.Rect(40, 20, 48, 28), 2)

	var engine LumaEngine
	driveLumaFrame(&engine, img)
	driveLumaFrame(&engine, whole)
	if engine.RequantCount != 1 {
		t.Fatalf("RequantCount = %d after a +1 luma shift, want 1 (must reuse the ramp)", engine.RequantCount)
	}
	driveLumaFrame(&engine, region)
	if engine.RequantCount != 1 {
		t.Fatalf("RequantCount = %d after a +2 region shift, want 1 (must reuse the ramp)", engine.RequantCount)
	}
}

func TestLumaReuseSceneChangeRequantises(t *testing.T) {
	t.Parallel()
	// The ramp covers 0..191 only, so the inverted frame's 192..255 lands
	// far from every retained level.
	img := lumaGradient(192)
	var engine LumaEngine
	driveLumaFrame(&engine, img)
	driveLumaFrame(&engine, invertRGB(img))
	if engine.RequantCount != 2 {
		t.Fatalf("RequantCount = %d, want 2 (a scene change must re-fit immediately)", engine.RequantCount)
	}
}

func TestLumaReuseGeometryChangeInvalidates(t *testing.T) {
	t.Parallel()
	var engine LumaEngine
	first := lumaGradient(192)
	second := lumaGradient(180)
	driveLumaFrame(&engine, first)
	sample := engine.Sample(second)
	if engine.Reuse(second.Bounds(), sample) {
		t.Fatal("a geometry change reused the retained ramp")
	}
	result := engine.Requantise(second.Bounds(), sample)
	if result.PriorValid || result.ExactNoOp {
		t.Fatalf("geometry-change result = %+v, want no valid prior state", result)
	}
	if engine.RequantCount != 2 {
		t.Fatalf("RequantCount = %d, want 2 (a geometry change must re-fit)", engine.RequantCount)
	}
}

func TestLumaRequantiseReportsExactNoOpAndKeepsBytes(t *testing.T) {
	t.Parallel()
	img := lumaGradient(192)
	var engine LumaEngine
	sample := engine.Sample(img)
	first := engine.Requantise(img.Bounds(), sample)
	if first.PriorValid || first.LengthEqual || first.PaletteEqual || first.LUTEqual || first.ExactNoOp {
		t.Fatalf("initial result = %+v, want no prior equality", first)
	}
	wantPalette := slices.Clone(AppendOSC4(nil, engine.Palette, engine.Base()))
	wantLUT := slices.Clone(engine.LUT)

	result := engine.Requantise(img.Bounds(), sample)
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

	engine.LUT[123] ^= 1
	result = engine.Requantise(img.Bounds(), sample)
	if !result.PaletteEqual || result.LUTEqual || result.ExactNoOp {
		t.Fatalf("LUT-change result = %+v", result)
	}
	if !slices.Equal(engine.LUT, wantLUT) {
		t.Fatal("the re-fit did not restore the expected LUT bytes")
	}
}

func TestLumaRequantiseReportsOneChangedEntry(t *testing.T) {
	t.Parallel()
	before := greyImage(1, 8, func(int) uint8 { return 40 })
	after := greyImage(1, 8, func(int) uint8 { return 200 })
	var engine LumaEngine
	engine.Requantise(before.Bounds(), engine.Sample(before))
	result := engine.Requantise(after.Bounds(), engine.Sample(after))
	if !result.PriorValid || !result.LengthEqual || result.PaletteEqual || !result.LUTEqual || result.ExactNoOp {
		t.Fatalf("changed-level result = %+v", result)
	}
	if result.ChangedEntries != 1 {
		t.Fatalf("ChangedEntries = %d, want 1", result.ChangedEntries)
	}
}

func TestLumaRequantiseAfterInvalidationIsNotNoOp(t *testing.T) {
	t.Parallel()
	img := lumaGradient(192)
	var engine LumaEngine
	sample := engine.Sample(img)
	engine.Requantise(img.Bounds(), sample)
	engine.Invalidate()
	result := engine.Requantise(img.Bounds(), sample)
	if result.PriorValid || result.LengthEqual || result.PaletteEqual || result.LUTEqual || result.ExactNoOp {
		t.Fatalf("invalidated result = %+v, want no prior equality", result)
	}
	if result.ChangedEntries != len(engine.Palette) {
		t.Fatalf("ChangedEntries = %d, want %d", result.ChangedEntries, len(engine.Palette))
	}
}

func TestLumaInvalidateDropsHysteresis(t *testing.T) {
	t.Parallel()
	frameA := greyImage(2, 8, func(x int) uint8 { return []uint8{50, 100}[x] })
	frameB := greyImage(2, 8, func(x int) uint8 { return []uint8{52, 200}[x] })
	var engine LumaEngine
	driveLumaFrame(&engine, frameA)
	engine.Invalidate()
	driveLumaFrame(&engine, frameB)
	if engine.RequantCount != 2 {
		t.Fatalf("RequantCount = %d, want 2 (Invalidate must force a re-fit)", engine.RequantCount)
	}
	if got := paletteLevels(t, engine.Palette); !slices.Equal(got, []uint8{52, 200}) {
		t.Fatalf("palette levels = %v after Invalidate, want [52 200] with no freeze", got)
	}
}

func TestFreezeSubJNDLevels(t *testing.T) {
	t.Parallel()
	previous := []uint8{50, 100, 150}
	levels := []uint8{52, 103, 148, 200}
	got := FreezeSubJNDLevels(levels, previous)
	want := []uint8{50, 103, 150, 200}
	if !slices.Equal(got, want) {
		t.Fatalf("FreezeSubJNDLevels = %v, want %v (freeze at delta <= %d only)", got, want, LumaFreezeMaxDelta)
	}
	if &got[0] != &levels[0] {
		t.Fatal("FreezeSubJNDLevels did not write in place")
	}
	fresh := []uint8{52, 103}
	if got := FreezeSubJNDLevels(fresh, nil); !slices.Equal(got, fresh) {
		t.Fatalf("FreezeSubJNDLevels with no previous ramp = %v, want %v", got, fresh)
	}
}

// TestLumaHysteresisFreezesSubJNDLevels drives the freeze through the engine:
// frame B moves one level by 2 and one by 3, and adds a level far from the
// ramp so the drift test forces the re-fit.
func TestLumaHysteresisFreezesSubJNDLevels(t *testing.T) {
	t.Parallel()
	frameA := greyImage(3, 8, func(x int) uint8 { return []uint8{50, 100, 150}[x] })
	frameB := greyImage(3, 8, func(x int) uint8 { return []uint8{52, 103, 220}[x] })
	var engine LumaEngine
	driveLumaFrame(&engine, frameA)
	if got := paletteLevels(t, engine.Palette); !slices.Equal(got, []uint8{50, 100, 150}) {
		t.Fatalf("frame A palette levels = %v, want [50 100 150]", got)
	}
	driveLumaFrame(&engine, frameB)
	if engine.RequantCount != 2 {
		t.Fatalf("RequantCount = %d, want 2 (frame B must re-fit)", engine.RequantCount)
	}
	got := paletteLevels(t, engine.Palette)
	want := []uint8{50, 103, 220}
	if !slices.Equal(got, want) {
		t.Fatalf("frame B palette levels = %v, want %v (50 frozen at delta 2, 103 replaced at delta 3)", got, want)
	}
}

// TestLumaRequantiseRetainedAllocatesNothing checks that a second pass over
// an unchanged frame allocates nothing, including the downsample scratch for
// a frame above the 64K pixel sample budget.
func TestLumaRequantiseRetainedAllocatesNothing(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 400, 200))
	for y := range 200 {
		for x := range 400 {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(x * 2),   //nolint:gosec // Deliberate wrapping test pattern.
				G: uint8(y * 3),   //nolint:gosec // Deliberate wrapping test pattern.
				B: uint8(x + y*2), //nolint:gosec // Deliberate wrapping test pattern.
				A: 0xff,
			})
		}
	}
	var engine LumaEngine
	driveLumaFrame(&engine, img)
	if engine.RequantCount != 1 {
		t.Fatalf("RequantCount = %d, want 1", engine.RequantCount)
	}
	allocs := testing.AllocsPerRun(20, func() {
		sample := engine.Sample(img)
		_ = engine.Reuse(img.Bounds(), sample)
		engine.Requantise(img.Bounds(), sample)
	})
	if allocs != 0 {
		t.Fatalf("Sample+Reuse+Requantise on a retained engine allocated %v times per run, want 0", allocs)
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
