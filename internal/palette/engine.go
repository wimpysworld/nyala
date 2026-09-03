// Package palette owns the shared adaptive-palette engine: median-cut
// quantisation, delta-triggered palette reuse, sub-JND register hysteresis,
// the frame sample, and the RGB555 lookup table. Backends hold an Engine and
// drive it once per frame; the engine owns every retained palette value.
package palette

import (
	"image"
	"image/color"

	"github.com/soniakeys/quant/median"

	"github.com/wimpysworld/nyala/internal/metrics"
)

// Delta-triggered palette reuse. The adaptive path keeps the palette and
// RGB555 LUT of the previous frame and re-quantises only when the fresh
// sample's mapping error drifts past the error recorded when the retained
// palette was built. Between re-quantisations the registers, the LUT, and
// therefore the mapping of unchanged pixels are identical, which removes
// palette shimmer by construction. The trigger is relative with an absolute
// floor: a relative trigger tolerates content whose fresh palette would also
// carry error (photographs, gradients), while the floor stops near-zero
// baselines (flat pages) from re-quantising on invisible noise. The defaults
// are conservative: they prefer re-quantising too often over visible drift.
const (
	// DriftRelativeTrigger re-quantises when the sample mapping error
	// grows more than 15% over the retained baseline. Scene cuts exceed this
	// by orders of magnitude; sub-threshold motion reuses the palette.
	DriftRelativeTrigger = 1.15
	// DriftAbsoluteFloor is the mean squared RGB error (summed over
	// the three channels) always tolerated regardless of baseline. 12 equals
	// a uniform two-level shift per 8-bit channel, below one visible step.
	DriftAbsoluteFloor = 12.0
	// FreezeMaxDeltaE is the just-noticeable CIEDE2000 difference. At
	// re-quantisation, a new register whose nearest old register is closer
	// than this keeps the old value, damping sub-visible register churn.
	FreezeMaxDeltaE = 2.3
)

// The budget mode. A terminal palette has 256 entries and the first 16 carry
// the user's theme, which the shell prompt, the status bar, and every other
// program on the screen still use. An engine in budget mode therefore leaves
// 0..15 alone and writes 16..255 only.
const (
	// BudgetBase is the terminal palette index that entry 0 of a budget-mode
	// palette maps to. Indexes 0..15 stay with the terminal's own theme.
	BudgetBase = 16
	// BudgetColours is the entry count of the budget mode: 256 minus the 16
	// reserved theme entries. With BudgetBase the top index is 255, so a LUT
	// register stays within uint8.
	BudgetColours = 256 - BudgetBase
)

// Engine holds the retained adaptive-palette state for one output stream. It
// is stateful and must not be used concurrently. While the retained state is
// valid, Palette and LUT carry over between frames and the caller re-quantises
// only when the drift test in Reuse fails. Geometry changes and leaving the
// adaptive path invalidate the retained state.
type Engine struct {
	// Palette is the retained quantised palette. Callers read it; only the
	// engine writes it.
	Palette color.Palette
	// LUT is the retained RGB555 lookup table for Palette. It holds absolute
	// terminal register numbers, not positions in Palette: key k maps to
	// Base()+j, where j is the nearest entry of Palette. Absolute is the
	// deliberate choice, because a consumer reads one index out of the LUT and
	// writes it to both the SGR stream and, through AppendOSC4 with Base(), the
	// palette-set sequence. Storing positions would make every consumer add the
	// base itself, and one that forgot would paint the terminal's theme colours.
	// Outside the budget mode the base is zero and the two forms coincide.
	LUT []uint8
	// RequantCount counts full re-quantisations. Tests and benchmarks read it
	// as the reuse hook; production ignores it.
	RequantCount int

	// sample is the reusable downsample scratch.
	sample *image.RGBA
	// budget reports whether the engine is in the 240-entry budget mode. The
	// zero value is false: 256 entries based at zero, which is what the Sixel
	// encoder needs.
	budget bool
	// valid reports whether Palette, LUT, bounds and mappingError describe a
	// retained state the drift test can trust.
	valid bool
	// bounds is the frame geometry the retained palette was built for.
	bounds image.Rectangle
	// mappingError is the sample mapping error recorded when the retained
	// palette was built; it is the drift test's baseline.
	mappingError float64
	// oldPalette is scratch holding the previous palette during hysteresis.
	oldPalette color.Palette
}

// Invalidate drops the retained state, so the next frame re-quantises from
// scratch without hysteresis. Callers use it when a frame leaves the adaptive
// path.
func (e *Engine) Invalidate() { e.valid = false }

// SetBudget selects or clears the 240-entry budget mode. In budget mode the
// engine quantises at most BudgetColours entries and its LUT holds terminal
// register numbers from BudgetBase upwards. The mode is opt-in: an engine that
// never calls SetBudget keeps the 256-entry, base-zero behaviour. A change of
// mode invalidates the retained state, because the retained LUT holds register
// numbers for the previous base.
func (e *Engine) SetBudget(budget bool) {
	if budget == e.budget {
		return
	}
	e.budget = budget
	e.valid = false
}

// Base returns the terminal register number that entry 0 of Palette maps to:
// BudgetBase in the budget mode and zero otherwise. Callers pass it to
// AppendOSC4, which then names the same registers the LUT holds.
func (e *Engine) Base() int {
	if e.budget {
		return BudgetBase
	}
	return 0
}

// colours caps the caller's register budget at BudgetColours in the budget
// mode, so Base()+len(Palette)-1 can never leave the 0..255 register range
// whatever the caller asks for.
func (e *Engine) colours(maxColours int) int {
	if e.budget {
		return min(maxColours, BudgetColours)
	}
	return maxColours
}

// Reuse reports whether the retained palette and LUT stay valid for a
// frame with the given geometry and palette sample. It is the drift test: the
// sample maps through the retained LUT and its mean squared RGB error is
// compared against the baseline recorded at the last re-quantisation.
func (e *Engine) Reuse(bounds image.Rectangle, sample *image.RGBA) bool {
	if e.valid && e.bounds != bounds {
		// A geometry change invalidates the retained state entirely: the
		// following re-quantisation starts fresh, without hysteresis.
		e.valid = false
	}
	if !e.valid || len(e.Palette) == 0 {
		return false
	}
	drift := MappingError(sample, e.Palette, e.LUT, e.Base())
	return drift <= max(e.mappingError*DriftRelativeTrigger, DriftAbsoluteFloor)
}

// Requantise rebuilds the adaptive palette from the sample, freezes
// registers that moved less than a just-noticeable difference, rebuilds the
// LUT, and records the new drift baseline and geometry. In the budget mode it
// caps maxColours at BudgetColours.
func (e *Engine) Requantise(bounds image.Rectangle, sample *image.RGBA, maxColours int) {
	maxColours = e.colours(maxColours)
	e.oldPalette = e.oldPalette[:0]
	if e.valid {
		e.oldPalette = append(e.oldPalette, e.Palette...)
	}
	// The quantiser reads cap, not a count argument, so the capacity must
	// equal the budget rather than merely reach it. A mode change lowers the
	// budget, and a palette left over from the wider mode would otherwise
	// quantise to the old count and push the top register past 255.
	if cap(e.Palette) != maxColours {
		e.Palette = make(color.Palette, 0, maxColours)
	}
	e.Palette = median.Quantizer(0).Quantize(e.Palette[:0], sample)
	FreezeSubJNDRegisters(e.Palette, e.oldPalette)
	e.LUT = BuildLUT(e.LUT, e.Palette, e.Base())
	e.mappingError = MappingError(sample, e.Palette, e.LUT, e.Base())
	e.bounds = bounds
	e.valid = true
	e.RequantCount++
}

// FreezeSubJNDRegisters matches each new palette entry to its nearest previous
// entry and keeps the previous value when the pair differs by less than the
// just-noticeable CIEDE2000 difference. Nearest matching uses squared RGB
// distance, the same space the RGB555 LUT maps in; the freeze judgement uses
// metrics.DeltaE2000. Freeze-or-replace per register, never interpolation. Two
// new entries can freeze to the same previous value; the duplicate register is
// harmless because the LUT resolves ties deterministically.
func FreezeSubJNDRegisters(palette, previous color.Palette) {
	if len(previous) == 0 {
		return
	}
	var pr, pg, pb [256]int32
	for i, colour := range previous {
		r, g, b, _ := colour.RGBA()
		pr[i], pg[i], pb[i] = int32(r>>8), int32(g>>8), int32(b>>8)
	}
	for i, entry := range palette {
		r16, g16, b16, _ := entry.RGBA()
		r, g, b := int32(r16>>8), int32(g16>>8), int32(b16>>8)
		best, bestDistance := 0, int32(1<<31-1)
		for j := range previous {
			dr, dg, db := r-pr[j], g-pg[j], b-pb[j]
			if distance := dr*dr + dg*dg + db*db; distance < bestDistance {
				best, bestDistance = j, distance
			}
		}
		de := metrics.DeltaE2000(
			uint8(r), uint8(g), uint8(b), //nolint:gosec // 8-bit channel values.
			uint8(pr[best]), uint8(pg[best]), uint8(pb[best]), //nolint:gosec // 8-bit channel values.
		)
		if de < FreezeMaxDeltaE {
			palette[i] = previous[best]
		}
	}
}

// MappingError returns the mean squared RGB error, summed over the three
// channels, of mapping every sample pixel through the LUT onto the palette.
// At re-quantisation it records the baseline; on later frames it measures
// drift. An empty sample returns zero. base is the register number the LUT
// gives entry 0 of the palette, so it must be the base the LUT was built with
// and base+len(palette) must not exceed 256.
func MappingError(sample *image.RGBA, palette color.Palette, lut []uint8, base int) float64 {
	var pr, pg, pb [256]int32
	for i, colour := range palette {
		r, g, b, _ := colour.RGBA()
		pr[base+i], pg[base+i], pb[base+i] = int32(r>>8), int32(g>>8), int32(b>>8)
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
			dr, dg, db := int32(r)-pr[idx], int32(g)-pg[idx], int32(b)-pb[idx]
			sum += uint64(dr*dr + dg*dg + db*db) //nolint:gosec // Squared 8-bit deltas are non-negative.
			offset += 4
		}
	}
	return float64(sum) / float64(pixels)
}
