package sixel

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
	// paletteDriftRelativeTrigger re-quantises when the sample mapping error
	// grows more than 15% over the retained baseline. Scene cuts exceed this
	// by orders of magnitude; sub-threshold motion reuses the palette.
	paletteDriftRelativeTrigger = 1.15
	// paletteDriftAbsoluteFloor is the mean squared RGB error (summed over
	// the three channels) always tolerated regardless of baseline. 12 equals
	// a uniform two-level shift per 8-bit channel, below one visible step.
	paletteDriftAbsoluteFloor = 12.0
	// paletteFreezeMaxDeltaE is the just-noticeable CIEDE2000 difference. At
	// re-quantisation, a new register whose nearest old register is closer
	// than this keeps the old value, damping sub-visible register churn.
	paletteFreezeMaxDeltaE = 2.3
)

// reusePalette reports whether the retained palette and LUT stay valid for a
// frame with the given geometry and palette sample. It is the drift test: the
// sample maps through the retained LUT and its mean squared RGB error is
// compared against the baseline recorded at the last re-quantisation.
func (e *opaqueEncoder) reusePalette(bounds image.Rectangle, sample *image.RGBA) bool {
	if e.paletteValid && e.paletteBounds != bounds {
		// A geometry change invalidates the retained state entirely: the
		// following re-quantisation starts fresh, without hysteresis.
		e.paletteValid = false
	}
	if !e.paletteValid || len(e.palette) == 0 {
		return false
	}
	drift := sampleMappingError(sample, e.palette, e.lut)
	return drift <= max(e.paletteError*paletteDriftRelativeTrigger, paletteDriftAbsoluteFloor)
}

// requantisePalette rebuilds the adaptive palette from the sample, freezes
// registers that moved less than a just-noticeable difference, rebuilds the
// LUT, and records the new drift baseline and geometry.
func (e *opaqueEncoder) requantisePalette(bounds image.Rectangle, sample *image.RGBA, maxColours int) {
	e.oldPalette = e.oldPalette[:0]
	if e.paletteValid {
		e.oldPalette = append(e.oldPalette, e.palette...)
	}
	if cap(e.palette) < maxColours {
		e.palette = make(color.Palette, 0, maxColours)
	}
	e.palette = median.Quantizer(0).Quantize(e.palette[:0], sample)
	freezeSubJNDRegisters(e.palette, e.oldPalette)
	e.lut = paletteLUT(e.lut, e.palette)
	e.paletteError = sampleMappingError(sample, e.palette, e.lut)
	e.paletteBounds = bounds
	e.paletteValid = true
	e.requantCount++
}

// freezeSubJNDRegisters matches each new palette entry to its nearest previous
// entry and keeps the previous value when the pair differs by less than the
// just-noticeable CIEDE2000 difference. Nearest matching uses squared RGB
// distance, the same space the RGB555 LUT maps in; the freeze judgement uses
// metrics.DeltaE2000. Freeze-or-replace per register, never interpolation. Two
// new entries can freeze to the same previous value; the duplicate register is
// harmless because the LUT resolves ties deterministically.
func freezeSubJNDRegisters(palette, previous color.Palette) {
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
		if de < paletteFreezeMaxDeltaE {
			palette[i] = previous[best]
		}
	}
}

// sampleMappingError returns the mean squared RGB error, summed over the three
// channels, of mapping every sample pixel through the LUT onto the palette.
// At re-quantisation it records the baseline; on later frames it measures
// drift. An empty sample returns zero.
func sampleMappingError(sample *image.RGBA, palette color.Palette, lut []uint8) float64 {
	var pr, pg, pb [256]int32
	for i, colour := range palette {
		r, g, b, _ := colour.RGBA()
		pr[i], pg[i], pb[i] = int32(r>>8), int32(g>>8), int32(b>>8)
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
