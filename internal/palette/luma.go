package palette

import (
	"image"
	"image/color"
)

// Rec.601 luma weights, scaled to 256 and rounded. They are the constants
// that block.luma uses, repeated here so the palette package never imports the
// Block backend. They sum to 256, so a grey pixel (v, v, v) maps to luma v.
const (
	lumaWeightR = 77
	lumaWeightG = 150
	lumaWeightB = 29
)

// lumaLevels is the number of distinct 8-bit luma values, and so the size of
// the histogram and the nearest-level table.
const lumaLevels = 256

// Drift-triggered ramp reuse for the greyscale path. The trigger form is the
// one Engine uses: relative to the baseline recorded at the last fit, with an
// absolute floor for near-zero baselines.
const (
	// LumaDriftRelativeTrigger re-fits the ramp when the sample mapping error
	// grows more than 15% over the retained baseline, the same relative margin
	// as DriftRelativeTrigger.
	LumaDriftRelativeTrigger = 1.15
	// LumaDriftAbsoluteFloor is the mean squared luma error always tolerated
	// regardless of baseline. 6.25 is an RMS of 2.5 luma levels, one quarter
	// of the fixed ramp's 10-level step, with margin for the RGB555 key
	// rounding that the fitted path always carries.
	LumaDriftAbsoluteFloor = 6.25
	// LumaFreezeMaxDelta is the largest luma move, in sRGB code values, that
	// a re-fit level keeps its previous value across. Two sRGB code values sit
	// under 1.0 L* at every point of the range, below the 2.3 CIEDE2000 JND
	// that FreezeMaxDeltaE uses.
	LumaFreezeMaxDelta = 2
)

// greyColours holds one boxed color.RGBA per grey level, so Palette entries
// copy an existing interface value instead of boxing a new one per fit.
var greyColours = func() [lumaLevels]color.Color {
	var greys [lumaLevels]color.Color
	for v := range greys {
		g := uint8(v) //nolint:gosec // v ranges over 0..255.
		greys[v] = color.RGBA{R: g, G: g, B: g, A: 0xff}
	}
	return greys
}()

// Luma8 returns the 8-bit Rec.601 luma of an RGB triple, rounded to nearest.
// The weights sum to 256, so a grey pixel (v, v, v) returns v.
func Luma8(r, g, b uint8) uint8 {
	return uint8((lumaWeightR*uint32(r) + lumaWeightG*uint32(g) + lumaWeightB*uint32(b) + 128) >> 8) //nolint:gosec // The weights sum to 256, so the shifted sum is at most 255.
}

// LumaEngine holds the retained scene-fitted greyscale ramp for one Block
// output stream. It is the greyscale sibling of Engine: it quantises the
// one-dimensional 8-bit luma of the sample instead of RGB, always runs in the
// budget mode, and keeps its ramp and LUT across frames until the drift test
// in Reuse fails. It is stateful and must not be used concurrently.
type LumaEngine struct {
	// Palette is the retained ramp as grey colours in ascending order, one
	// entry per level, for AppendOSC4. Callers read it; only the engine writes
	// it.
	Palette color.Palette
	// LUT is the retained RGB555 lookup table for Palette. As with Engine.LUT
	// it holds absolute terminal register numbers: key k maps to Base()+j,
	// where j is the position of the ramp level nearest to the luma of k.
	LUT []uint8
	// RequantCount counts full re-fits. Tests and benchmarks read it as the
	// reuse hook; production ignores it.
	RequantCount int

	// sample is the reusable downsample scratch.
	sample *image.RGBA
	// hist counts the sample pixels at each 8-bit luma value.
	hist [lumaLevels]uint32
	// levels is the retained ramp, ascending, at most BudgetColours entries.
	levels []uint8
	// pops is the population of each entry of levels during a fit.
	pops []uint32
	// previous is scratch holding the previous ramp during hysteresis.
	previous []uint8
	// valid reports whether Palette, LUT, bounds and mappingError describe a
	// retained state the drift test can trust.
	valid bool
	// bounds is the frame geometry the retained ramp was built for.
	bounds image.Rectangle
	// mappingError is the sample mapping error recorded when the retained
	// ramp was built; it is the drift test's baseline.
	mappingError float64
}

// Invalidate drops the retained state, so the next frame re-fits from scratch
// without hysteresis. Callers use it when a frame leaves the greyscale path.
func (e *LumaEngine) Invalidate() { e.valid = false }

// Base returns the terminal register number that entry 0 of Palette maps to.
// The greyscale engine always runs in the budget mode, so it is BudgetBase.
func (e *LumaEngine) Base() int { return BudgetBase }

// Sample builds this frame's palette sample and retains the downsample
// scratch. The sample feeds both the drift test and the re-fit, so it is built
// every frame regardless of whether the retained ramp is reused.
func (e *LumaEngine) Sample(rgba *image.RGBA) *image.RGBA {
	src, sample := SampleInto(rgba, e.sample)
	e.sample = sample
	return src
}

// Reuse reports whether the retained ramp and LUT stay valid for a frame with
// the given geometry and palette sample. It is the drift test: the sample maps
// through the retained LUT and its mean squared luma error is compared against
// the baseline recorded at the last re-fit.
func (e *LumaEngine) Reuse(bounds image.Rectangle, sample *image.RGBA) bool {
	if e.valid && e.bounds != bounds {
		// A geometry change invalidates the retained state entirely: the
		// following re-fit starts fresh, without hysteresis.
		e.valid = false
	}
	if !e.valid || len(e.levels) == 0 {
		return false
	}
	drift := LumaMappingError(sample, e.levels, e.LUT, e.Base())
	return drift <= max(e.mappingError*LumaDriftRelativeTrigger, LumaDriftAbsoluteFloor)
}

// Requantise fits the ramp to the luma histogram of the sample, freezes levels
// that moved by no more than LumaFreezeMaxDelta, rebuilds the LUT and Palette,
// and records the new drift baseline and geometry. It returns a comparison with
// the prior valid state after it records the new state.
func (e *LumaEngine) Requantise(bounds image.Rectangle, sample *image.RGBA) RefitResult {
	priorValid := e.valid
	e.previous = e.previous[:0]
	if priorValid {
		e.previous = append(e.previous, e.levels...)
	}
	e.histogram(sample)
	e.fit()
	e.levels = FreezeSubJNDLevels(e.levels, e.previous)
	result := compareLumaRefit(priorValid, e.previous, e.levels)
	var lutEqual bool
	e.LUT, lutEqual = buildLumaLUT(e.LUT, e.levels, e.Base())
	result.LUTEqual = priorValid && lutEqual
	result.ExactNoOp = result.PaletteEqual && result.LUTEqual
	e.Palette = e.Palette[:0]
	for _, v := range e.levels {
		e.Palette = append(e.Palette, greyColours[v])
	}
	e.mappingError = LumaMappingError(sample, e.levels, e.LUT, e.Base())
	e.bounds = bounds
	e.valid = true
	e.RequantCount++
	return result
}

func compareLumaRefit(priorValid bool, previous, current []uint8) RefitResult {
	result := RefitResult{
		PriorValid:  priorValid,
		LengthEqual: priorValid && len(current) == len(previous),
	}
	result.PaletteEqual = result.LengthEqual
	for i, level := range current {
		if i >= len(previous) || level != previous[i] {
			result.ChangedEntries++
			result.PaletteEqual = false
		}
	}
	return result
}

// histogram counts the sample pixels at each 8-bit luma value into hist.
func (e *LumaEngine) histogram(sample *image.RGBA) {
	clear(e.hist[:])
	bounds := sample.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		offset := sample.PixOffset(bounds.Min.X, y)
		for range bounds.Dx() {
			e.hist[Luma8(sample.Pix[offset], sample.Pix[offset+1], sample.Pix[offset+2])]++
			offset += 4
		}
	}
}

// fit rebuilds levels from hist. When at most BudgetColours luma values are
// occupied the ramp is those values, ascending. Otherwise adjacent levels
// merge greedily until BudgetColours remain: each step merges the adjacent
// pair whose merge adds the least population-weighted squared error, and
// replaces it by its population-weighted mean. A tie takes the lower pair. A
// scene with no sample pixels keeps the previous ramp, or a single level 0
// when there is none.
func (e *LumaEngine) fit() {
	occupied := 0
	for _, n := range e.hist {
		if n > 0 {
			occupied++
		}
	}
	if occupied == 0 {
		if len(e.levels) == 0 {
			e.levels = append(e.levels[:0], 0)
		}
		return
	}
	e.levels = e.levels[:0]
	e.pops = e.pops[:0]
	for v, n := range e.hist {
		if n > 0 {
			e.levels = append(e.levels, uint8(v)) //nolint:gosec // v ranges over 0..255.
			e.pops = append(e.pops, n)
		}
	}
	for len(e.levels) > BudgetColours {
		best, bestCost := 0, 0.0
		for i := range len(e.levels) - 1 {
			pa, pb := float64(e.pops[i]), float64(e.pops[i+1])
			d := float64(e.levels[i+1]) - float64(e.levels[i])
			cost := pa * pb / (pa + pb) * d * d
			if i == 0 || cost < bestCost {
				best, bestCost = i, cost
			}
		}
		pa, pb := uint64(e.pops[best]), uint64(e.pops[best+1])
		la, lb := uint64(e.levels[best]), uint64(e.levels[best+1])
		e.levels[best] = uint8((pa*la + pb*lb + (pa+pb)/2) / (pa + pb)) //nolint:gosec // A weighted mean of two 8-bit levels lies between them.
		e.pops[best] += e.pops[best+1]
		e.levels = append(e.levels[:best+1], e.levels[best+2:]...)
		e.pops = append(e.pops[:best+1], e.pops[best+2:]...)
	}
}

// FreezeSubJNDLevels matches each new ramp level to its nearest previous level
// and keeps the previous value when the two differ by at most
// LumaFreezeMaxDelta. It follows FreezeSubJNDRegisters: freeze-or-replace per
// level, never interpolation. Two new levels can freeze to the same previous
// value; the duplicate level is harmless because the LUT resolves ties
// deterministically. It writes levels in place and returns it.
func FreezeSubJNDLevels(levels, previous []uint8) []uint8 {
	if len(previous) == 0 {
		return levels
	}
	for i, v := range levels {
		best, bestDistance := 0, lumaLevels
		for j, old := range previous {
			distance := int(v) - int(old)
			if distance < 0 {
				distance = -distance
			}
			if distance < bestDistance {
				best, bestDistance = j, distance
			}
		}
		if bestDistance <= LumaFreezeMaxDelta {
			levels[i] = previous[best]
		}
	}
	return levels
}

// BuildLumaLUT fills lut with the register of the ramp level nearest in luma
// to every RGB555 key, and returns the table it wrote. It grows lut only when
// its capacity is short of the 32768 entries. The key layout is the one the
// Block backend indexes by: the top five bits of red, then green, then blue.
// Each key is judged by the Luma8 of its centre colour, so the table is a luma
// match, not the squared RGB match that BuildLUT makes. An exact midpoint
// between two levels picks the lighter level, which matches the rounding of
// the fixed greyscale ramp. Each entry is base plus the ramp position, so the
// table holds absolute terminal register numbers; base+len(levels) must not
// exceed 256.
func BuildLumaLUT(lut []uint8, levels []uint8, base int) []uint8 {
	lut, _ = buildLumaLUT(lut, levels, base)
	return lut
}

// buildLumaLUT has the same allocation and output behaviour as BuildLumaLUT.
// It also reports whether the complete overwritten table equals the input.
func buildLumaLUT(lut []uint8, levels []uint8, base int) ([]uint8, bool) {
	var nearest [lumaLevels]uint8
	for v := range nearest {
		best, bestDistance := 0, lumaLevels
		for j, level := range levels {
			distance := v - int(level)
			if distance < 0 {
				distance = -distance
			}
			// <= lets a later, lighter level win an exact midpoint.
			if distance <= bestDistance {
				best, bestDistance = j, distance
			}
		}
		nearest[v] = uint8(best) //nolint:gosec // base+len(levels) is bounded by 256.
	}
	equal := len(lut) == 1<<15
	if cap(lut) < 1<<15 {
		lut = make([]uint8, 1<<15)
		equal = false
	}
	lut = lut[:1<<15]
	register := uint8(base) //nolint:gosec // base+len(levels) is bounded by 256.
	for i := range lut {
		r := uint8(i>>10&31<<3 | 4) //nolint:gosec // A 15-bit LUT index bounds this to 252.
		g := uint8(i>>5&31<<3 | 4)  //nolint:gosec // A 15-bit LUT index bounds this to 252.
		b := uint8(i&31<<3 | 4)     //nolint:gosec // A 15-bit LUT index bounds this to 252.
		value := register + nearest[Luma8(r, g, b)]
		if equal && lut[i] != value {
			equal = false
		}
		lut[i] = value
	}
	return lut, equal
}

// LumaMappingError returns the mean squared luma error, in 8-bit luma units
// squared, of mapping every sample pixel through the LUT onto the ramp. At a
// re-fit it records the baseline; on later frames it measures drift. An empty
// sample returns zero. base is the register number the LUT gives level 0 of
// the ramp, so it must be the base the LUT was built with and base+len(levels)
// must not exceed 256.
func LumaMappingError(sample *image.RGBA, levels []uint8, lut []uint8, base int) float64 {
	var level [lumaLevels]int32
	for i, v := range levels {
		level[base+i] = int32(v)
	}
	bounds := sample.Bounds()
	pixels := bounds.Dx() * bounds.Dy()
	if pixels == 0 {
		return 0
	}
	var sum uint64
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		offset := sample.PixOffset(bounds.Min.X, y)
		for range bounds.Dx() {
			r, g, b := sample.Pix[offset], sample.Pix[offset+1], sample.Pix[offset+2]
			idx := lut[int(r>>3)<<10|int(g>>3)<<5|int(b>>3)]
			d := int32(Luma8(r, g, b)) - level[idx]
			sum += uint64(d * d) //nolint:gosec // A squared 8-bit delta is non-negative.
			offset += 4
		}
	}
	return float64(sum) / float64(pixels)
}
