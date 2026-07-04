package metrics

import (
	"image"
	"math"
	"testing"
)

// stripeLuma returns a deterministic pseudo-random grey value for source column
// x. A non-repeating per-column value gives cross-correlation a single sharp
// peak at the true shift, unlike a periodic pattern that could alias to a false
// peak one period away. The hash is a fixed integer mix, so the value is
// reproducible and identical across architectures. The result is in 0..255.
func stripeLuma(x int) uint8 {
	// A 32-bit integer avalanche mix (Wang-style), deterministic and
	// well-spread across neighbouring x, so adjacent columns differ sharply. x
	// is a non-negative column index; mask to 32 bits before the mix so the
	// conversion is defined and overflow-free.
	u := uint32(uint(x)&0xffffffff)*2654435761 + 0x9e3779b9
	u ^= u >> 16
	u *= 0x45d9f3b
	u ^= u >> 16
	return uint8(u & 0xff)
}

// setGrey writes an opaque grey pixel at (x,y) in RGBA order.
func setGrey(img *image.RGBA, x, y int, v uint8) {
	i := img.PixOffset(x, y)
	img.Pix[i+0], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = v, v, v, 255
}

// stripeImage builds a w*h image whose columns follow stripeLuma, so each
// vertical column band carries the same structure at every row and correlates
// cleanly against a horizontally shifted copy.
func stripeImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		v := stripeLuma(x)
		for y := range h {
			setGrey(img, x, y, v)
		}
	}
	return img
}

// bandShiftImage builds a copy of the stripe pattern where the whole vertical
// band that column x falls in carries a constant per-band shift, so every column
// inside a band shares one shift. That keeps each band a pure local translation
// of a, which is what the per-band cross-correlation recovers; a shift that
// varied column-to-column inside a band would warp the pattern and defeat the
// correlation. Output column x samples the source at x+shift, chosen so
// SkewBands returns that same shift value with a positive sign: the metric's
// best-fit Shift for this construction equals the shift passed here.
func bandShiftImage(w, h, bands int, shift func(band int) int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		band := x * bands / w
		src := max(x+shift(band), 0)
		if src >= w {
			src = w - 1
		}
		v := stripeLuma(src)
		for y := range h {
			setGrey(img, x, y, v)
		}
	}
	return img
}

// TestSkewBandsUniformShift asserts a constant horizontal shift yields a
// near-constant per-band displacement at the known value and a near-zero slope.
// The shift is a whole number of pixels, so the integer-pixel correlation search
// recovers it exactly; the epsilon absorbs only the arm64 FMA residue in the
// regression maths, not any pixel-quantisation error.
func TestSkewBandsUniformShift(t *testing.T) {
	const wantShift = 4
	const bands = 8
	a := stripeImage(320, 40)
	b := bandShiftImage(320, 40, bands, func(int) int { return wantShift })

	// Bound the search to a modest multiple of the true shift. A wide window
	// over narrow bands admits a spurious peak from the few columns that still
	// overlap at a large shift, so the metric expects a small maxShift ("a few
	// cells at most"); wide bands keep plenty of overlap at the true shift.
	skew := SkewBands(a, b, bands, wantShift*3)

	if len(skew.Bands) == 0 {
		t.Fatal("SkewBands returned no bands")
	}
	for i, band := range skew.Bands {
		if math.Abs(band.Shift-float64(wantShift)) > 1e-9 {
			t.Errorf("band %d (centreX %.1f): Shift = %.6f, want %d", i, band.CentreX, band.Shift, wantShift)
		}
	}
	// A uniform shift is flat in x, so the regression slope is ~0.
	if math.Abs(skew.Slope) > 1e-9 {
		t.Errorf("uniform shift: Slope = %.9f, want ~0", skew.Slope)
	}
	if math.Abs(skew.Intercept-float64(wantShift)) > 1e-6 {
		t.Errorf("uniform shift: Intercept = %.6f, want ~%d", skew.Intercept, wantShift)
	}
}

// TestSkewBandsLinearShift asserts a displacement that grows linearly across x
// yields a regression slope at the expected rate. The per-column shift is
// round(slope*x), so each band's centre lands on slope*centreX to within the
// half-pixel rounding of the integer search; the slope over many bands averages
// that rounding out, so a tight epsilon holds.
func TestSkewBandsLinearShift(t *testing.T) {
	const wantSlope = 0.02 // pixels of shift gained per pixel of x
	const bands = 12
	w, h := 480, 40
	a := stripeImage(w, h)
	// Each band is translated by round(slope * band-centre-x), so the per-band
	// displacement grows linearly across x at the target slope.
	bandCentreX := func(band int) float64 { return (float64(band) + 0.5) * float64(w) / float64(bands) }
	b := bandShiftImage(w, h, bands, func(band int) int {
		return int(math.Round(wantSlope * bandCentreX(band)))
	})

	// maxShift must cover the largest displacement (slope*width) so the peak is
	// not clipped; derive it from the image width, not a hardcoded number.
	maxShift := int(wantSlope*float64(a.Bounds().Dx())) + 2
	skew := SkewBands(a, b, bands, maxShift)

	if len(skew.Bands) < 2 {
		t.Fatalf("SkewBands returned %d bands, need >= 2 for a slope", len(skew.Bands))
	}
	if math.Abs(skew.Slope-wantSlope) > 2e-3 {
		t.Errorf("linear shift: Slope = %.6f, want %.6f (diff %.6f)", skew.Slope, wantSlope, skew.Slope-wantSlope)
	}
	// Each band is a pure integer translation, so its recovered shift equals
	// the rounded slope*centreX the band was built with, confirming the slope
	// comes from a real per-band trend and not two extreme bands.
	for i, band := range skew.Bands {
		want := math.Round(wantSlope * bandCentreX(i))
		if math.Abs(band.Shift-want) > 1e-9 {
			t.Errorf("band %d (centreX %.1f): Shift = %.3f, want %.0f", i, band.CentreX, band.Shift, want)
		}
	}
}

// stripeImageAt builds the stripe pattern on a rectangle with an arbitrary
// origin, writing each column's luma at its absolute pixel. The pattern indexes
// from the local column (0..w), so the content matches stripeImage cell for
// cell; only the enclosing rectangle's origin differs. It lets a test show
// SkewBands reads through Bounds and is invariant to a non-zero origin.
func stripeImageAt(r image.Rectangle) *image.RGBA {
	img := image.NewRGBA(r)
	for lx := range r.Dx() {
		v := stripeLuma(lx)
		for ly := range r.Dy() {
			setGrey(img, r.Min.X+lx, r.Min.Y+ly, v)
		}
	}
	return img
}

// bandShiftImageAt is bandShiftImage on a rectangle with an arbitrary origin: it
// writes the same per-band shifted pattern at absolute pixels, indexing the
// pattern from the local column. It pairs with stripeImageAt so a test can build
// the uniform-shift pair on a non-zero origin.
func bandShiftImageAt(r image.Rectangle, bands int, shift func(band int) int) *image.RGBA {
	img := image.NewRGBA(r)
	w := r.Dx()
	for lx := range w {
		band := lx * bands / w
		src := max(lx+shift(band), 0)
		if src >= w {
			src = w - 1
		}
		v := stripeLuma(src)
		for ly := range r.Dy() {
			setGrey(img, r.Min.X+lx, r.Min.Y+ly, v)
		}
	}
	return img
}

// TestSkewBandsOriginInvariant asserts SkewBands reads through image.Bounds, so
// a non-zero image origin yields the same Shift, Slope, and Intercept as the
// zero-origin pair. The metric divides bands by width and correlates in local
// coordinates, so an origin offset must not move the result; a helper that read
// img.Pix from index 0 rather than through Bounds would drift here.
func TestSkewBandsOriginInvariant(t *testing.T) {
	const wantShift = 4
	const bands = 8
	const w, h = 320, 40
	const offX, offY = 137, 91

	zeroA := stripeImage(w, h)
	zeroB := bandShiftImage(w, h, bands, func(int) int { return wantShift })
	zero := SkewBands(zeroA, zeroB, bands, wantShift*3)

	r := image.Rect(offX, offY, offX+w, offY+h)
	shiftA := stripeImageAt(r)
	shiftB := bandShiftImageAt(r, bands, func(int) int { return wantShift })
	shifted := SkewBands(shiftA, shiftB, bands, wantShift*3)

	if len(shifted.Bands) != len(zero.Bands) {
		t.Fatalf("band count differs: origin-shifted %d, zero-origin %d", len(shifted.Bands), len(zero.Bands))
	}
	for i := range zero.Bands {
		if math.Abs(shifted.Bands[i].Shift-zero.Bands[i].Shift) > 1e-9 {
			t.Errorf("band %d: Shift = %.9f, want %.9f (zero-origin)", i, shifted.Bands[i].Shift, zero.Bands[i].Shift)
		}
	}
	if math.Abs(shifted.Slope-zero.Slope) > 1e-9 {
		t.Errorf("Slope = %.9f, want %.9f (zero-origin)", shifted.Slope, zero.Slope)
	}
	if math.Abs(shifted.Intercept-zero.Intercept) > 1e-6 {
		t.Errorf("Intercept = %.6f, want %.6f (zero-origin)", shifted.Intercept, zero.Intercept)
	}
}

// TestSkewBandsMismatchPanics asserts mismatched dimensions panic, matching the
// requireEqualDims guard style the other metric tests pin.
func TestSkewBandsMismatchPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("SkewBands on mismatched dimensions did not panic")
		}
	}()
	SkewBands(image.NewRGBA(image.Rect(0, 0, 4, 4)), image.NewRGBA(image.Rect(0, 0, 5, 4)), 8, 0)
}
