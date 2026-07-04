package metrics

import (
	"image"
	"math"
)

// SkewBand holds one vertical column band's horizontal displacement. CentreX is
// the band's centre column in pixels; Shift is the displacement in pixels that
// best aligns the second image's band onto the first (positive means the second
// image's content sits to the right of the first). Correlation is the peak
// normalised cross-correlation at that shift, in -1..1, so a caller can weigh a
// weak band.
type SkewBand struct {
	// CentreX is the band's centre column in pixels.
	CentreX float64
	// Shift is the best-fit horizontal displacement in pixels.
	Shift float64
	// Correlation is the peak normalised cross-correlation at Shift, in -1..1.
	Correlation float64
}

// Skew holds the per-band horizontal displacements and the least-squares line
// through them. Bands runs left to right. Slope is the displacement gained per
// pixel of x (the skew: a linear-in-x drift is a nonzero slope); Intercept is
// the fitted displacement at x = 0. A skew-free pair gives a slope near zero.
type Skew struct {
	// Bands holds the per-band displacements, left to right.
	Bands []SkewBand
	// Slope is the displacement gained per pixel of band-centre x.
	Slope float64
	// Intercept is the fitted displacement at x = 0.
	Intercept float64
}

// defaultSkewBands is the band count SkewBands derives when the caller passes a
// non-positive count. It splits the image into this many vertical columns, a
// resolution fine enough to fit a line across a browser-width capture yet coarse
// enough that each band carries enough content to correlate.
const defaultSkewBands = 16

// defaultSkewSearchDivisor bounds the cross-correlation search window when the
// caller passes a non-positive maxShift. The window is +/- width/divisor
// pixels: the expected displacement is a few cells at most, so a small fraction
// of the image width covers it without letting a distant false peak win.
const defaultSkewSearchDivisor = 16

// SkewBands measures a linear-in-x horizontal skew between two equal-size RGBA
// images. It splits the images into bands vertical column bands (a non-positive
// bands derives defaultSkewBands from the width), cross-correlates each band's
// BT.601 luma horizontally to find the whole-pixel shift that best aligns b onto
// a, and regresses those shifts over band-centre x by least squares. The slope
// is the skew.
//
// The search window is bounded to +/- maxShift pixels (a non-positive maxShift
// derives width/defaultSkewSearchDivisor), since the expected displacement is a
// few cells at most; a wider window only admits a distant false peak. A band
// with no horizontal structure (a flat correlation) contributes a zero shift.
//
// The images must share dimensions; SkewBands panics otherwise, in the
// requireEqualDims style. Channel order is RGBA throughout and the maths is
// 64-bit float.
func SkewBands(a, b *image.RGBA, bands, maxShift int) Skew {
	requireEqualDims(a, b, "SkewBands")
	pa, w, h := luma(a)
	pb, _, _ := luma(b)

	if bands <= 0 {
		bands = defaultSkewBands
	}
	if bands > w {
		bands = w
	}
	if maxShift <= 0 {
		maxShift = w / defaultSkewSearchDivisor
	}
	if maxShift < 1 {
		maxShift = 1
	}
	if w == 0 || h == 0 || bands == 0 {
		return Skew{}
	}

	out := make([]SkewBand, bands)
	for i := range bands {
		x0 := i * w / bands
		x1 := (i + 1) * w / bands
		if x1 <= x0 {
			x1 = x0 + 1
		}
		shift, corr := bestBandShift(pa, pb, w, h, x0, x1, maxShift)
		out[i] = SkewBand{
			CentreX:     float64(x0+x1) / 2,
			Shift:       float64(shift),
			Correlation: corr,
		}
	}

	slope, intercept := fitLine(out)
	return Skew{Bands: out, Slope: slope, Intercept: intercept}
}

// bestBandShift finds the integer horizontal shift in [-maxShift, maxShift] that
// maximises the normalised cross-correlation between band [x0,x1) of a and the
// same band of b, and returns that shift and its peak correlation. A positive
// shift means b's content sits to the right of a's, so b is sampled at x-shift
// to line up with a. Columns that fall outside the band under a shift are
// dropped from that shift's sum, so every candidate compares only overlapping
// content. A band with near-zero variance in either image yields a zero shift
// and a zero correlation.
func bestBandShift(pa, pb []float64, w, h, x0, x1, maxShift int) (int, float64) {
	bestShift := 0
	bestCorr := math.Inf(-1)
	for s := -maxShift; s <= maxShift; s++ {
		corr := bandCorrelation(pa, pb, w, h, x0, x1, s)
		if corr > bestCorr {
			bestCorr = corr
			bestShift = s
		}
	}
	if math.IsInf(bestCorr, -1) {
		return 0, 0
	}
	return bestShift, bestCorr
}

// bandCorrelation returns the zero-mean normalised cross-correlation between
// band [x0,x1) of a and b under a horizontal shift of s pixels applied to b. It
// pairs a at column x with b at column x-s over rows y, drops any pair whose b
// column falls outside the band, subtracts each side's mean over the paired
// samples, and divides by the paired standard deviations. The result is in
// -1..1; a band with no overlap or no variance returns negative infinity so it
// never wins a peak.
func bandCorrelation(pa, pb []float64, w, h, x0, x1, s int) float64 {
	var sumA, sumB float64
	var n int
	for y := range h {
		row := y * w
		for x := x0; x < x1; x++ {
			bx := x - s
			if bx < x0 || bx >= x1 {
				continue
			}
			sumA += pa[row+x]
			sumB += pb[row+bx]
			n++
		}
	}
	if n == 0 {
		return math.Inf(-1)
	}
	meanA := sumA / float64(n)
	meanB := sumB / float64(n)

	var cov, varA, varB float64
	for y := range h {
		row := y * w
		for x := x0; x < x1; x++ {
			bx := x - s
			if bx < x0 || bx >= x1 {
				continue
			}
			da := pa[row+x] - meanA
			db := pb[row+bx] - meanB
			cov += da * db
			varA += da * da
			varB += db * db
		}
	}
	denom := math.Sqrt(varA * varB)
	if denom == 0 {
		return math.Inf(-1)
	}
	return cov / denom
}

// fitLine returns the least-squares slope and intercept of band Shift over band
// CentreX. With fewer than two bands, or bands sharing one x, the slope is zero
// and the intercept is the mean shift, so a degenerate split still yields a
// usable displacement.
func fitLine(bands []SkewBand) (slope, intercept float64) {
	n := float64(len(bands))
	if n == 0 {
		return 0, 0
	}
	var sx, sy, sxx, sxy float64
	for _, b := range bands {
		sx += b.CentreX
		sy += b.Shift
		sxx += b.CentreX * b.CentreX
		sxy += b.CentreX * b.Shift
	}
	denom := n*sxx - sx*sx
	if denom == 0 {
		return 0, sy / n
	}
	slope = (n*sxy - sx*sy) / denom
	intercept = (sy - slope*sx) / n
	return slope, intercept
}
