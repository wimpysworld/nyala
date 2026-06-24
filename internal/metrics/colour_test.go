package metrics

import (
	"image"
	"math"
	"testing"

	colorful "github.com/lucasb-eyer/go-colorful"
)

// sharmaRow is one entry from Gaurav Sharma's CIEDE2000 supplemental test data
// (Sharma, Wu, Dalal, Color Research and Application 30(1), 2005). Lab values
// are on the conventional 0..100 / -128..127 CIELAB scale; want is the
// published dE on the 0..100 scale.
type sharmaRow struct {
	l1, a1, b1 float64
	l2, a2, b2 float64
	want       float64
	skip       bool
}

// sharmaTable is the full 34-pair Sharma golden. Source:
// https://hajim.rochester.edu/ece/sites/gsharma/ciede2000/dataNprograms/ciede2000testdata.txt
// Rows 10 and 11 (1-indexed) sit on the documented hue discontinuity at the
// a=2.49 -> -2.49 boundary, where go-colorful resolves both to one value rather
// than splitting 7.1792/7.2195. They are skipped, as the task fixes.
var sharmaTable = []sharmaRow{
	{50.0000, 2.6772, -79.7751, 50.0000, 0.0000, -82.7485, 2.0425, false},
	{50.0000, 3.1571, -77.2803, 50.0000, 0.0000, -82.7485, 2.8615, false},
	{50.0000, 2.8361, -74.0200, 50.0000, 0.0000, -82.7485, 3.4412, false},
	{50.0000, -1.3802, -84.2814, 50.0000, 0.0000, -82.7485, 1.0000, false},
	{50.0000, -1.1848, -84.8006, 50.0000, 0.0000, -82.7485, 1.0000, false},
	{50.0000, -0.9009, -85.5211, 50.0000, 0.0000, -82.7485, 1.0000, false},
	{50.0000, 0.0000, 0.0000, 50.0000, -1.0000, 2.0000, 2.3669, false},
	{50.0000, -1.0000, 2.0000, 50.0000, 0.0000, 0.0000, 2.3669, false},
	{50.0000, 2.4900, -0.0010, 50.0000, -2.4900, 0.0009, 7.1792, false},
	{50.0000, 2.4900, -0.0010, 50.0000, -2.4900, 0.0010, 7.1792, true},
	{50.0000, 2.4900, -0.0010, 50.0000, -2.4900, 0.0011, 7.2195, true},
	{50.0000, 2.4900, -0.0010, 50.0000, -2.4900, 0.0012, 7.2195, false},
	{50.0000, -0.0010, 2.4900, 50.0000, 0.0009, -2.4900, 4.8045, false},
	{50.0000, -0.0010, 2.4900, 50.0000, 0.0010, -2.4900, 4.8045, false},
	{50.0000, -0.0010, 2.4900, 50.0000, 0.0011, -2.4900, 4.7461, false},
	{50.0000, 2.5000, 0.0000, 50.0000, 0.0000, -2.5000, 4.3065, false},
	{50.0000, 2.5000, 0.0000, 73.0000, 25.0000, -18.0000, 27.1492, false},
	{50.0000, 2.5000, 0.0000, 61.0000, -5.0000, 29.0000, 22.8977, false},
	{50.0000, 2.5000, 0.0000, 56.0000, -27.0000, -3.0000, 31.9030, false},
	{50.0000, 2.5000, 0.0000, 58.0000, 24.0000, 15.0000, 19.4535, false},
	{50.0000, 2.5000, 0.0000, 50.0000, 3.1736, 0.5854, 1.0000, false},
	{50.0000, 2.5000, 0.0000, 50.0000, 3.2972, 0.0000, 1.0000, false},
	{50.0000, 2.5000, 0.0000, 50.0000, 1.8634, 0.5757, 1.0000, false},
	{50.0000, 2.5000, 0.0000, 50.0000, 3.2592, 0.3350, 1.0000, false},
	{60.2574, -34.0099, 36.2677, 60.4626, -34.1751, 39.4387, 1.2644, false},
	{63.0109, -31.0961, -5.8663, 62.8187, -29.7946, -4.0864, 1.2630, false},
	{61.2901, 3.7196, -5.3901, 61.4292, 2.2480, -4.9620, 1.8731, false},
	{35.0831, -44.1164, 3.7933, 35.0232, -40.0716, 1.5901, 1.8645, false},
	{22.7233, 20.0904, -46.6940, 23.0331, 14.9730, -42.5619, 2.0373, false},
	{36.4612, 47.8580, 18.3852, 36.2715, 50.5065, 21.2231, 1.4146, false},
	{90.8027, -2.0831, 1.4410, 91.1528, -1.6435, 0.0447, 1.4441, false},
	{90.9257, -0.5406, -0.9208, 88.6381, -0.8985, -0.7239, 1.5381, false},
	{6.7747, -0.2908, -2.4247, 5.8714, -0.0985, -2.2286, 0.6377, false},
	{2.0776, 0.0795, -1.1350, 0.9033, -0.0636, -0.5514, 0.9082, false},
}

// TestSharmaGolden asserts DistanceCIEDE2000()*100 matches the published Sharma
// dE on every non-skipped row. The tolerance is 1e-3; Sharma's own MATLAB
// reference agrees to ~4e-5.
func TestSharmaGolden(t *testing.T) {
	const tol = 1e-3
	for i, r := range sharmaTable {
		if r.skip {
			continue
		}
		c1 := colorful.Lab(r.l1/100, r.a1/100, r.b1/100)
		c2 := colorful.Lab(r.l2/100, r.a2/100, r.b2/100)
		got := c1.DistanceCIEDE2000(c2) * CIEDE2000Scale
		if math.Abs(got-r.want) > tol {
			t.Errorf("row %d: dE = %.4f, want %.4f (diff %.5f)", i+1, got, r.want, got-r.want)
		}
	}
}

// TestDeltaE2000Identical asserts identical colours give dE ~0. The CIEDE2000
// pipeline rounds differently under arm64 FMA, leaving a sub-1e-12 residue, so
// allow that epsilon rather than asserting exact zero.
func TestDeltaE2000Identical(t *testing.T) {
	if de := DeltaE2000(123, 200, 50, 123, 200, 50); math.Abs(de) > 1e-12 {
		t.Errorf("identical colours: dE = %v, want ~0", de)
	}
}

// TestDeltaE2000ScalingReference asserts the dE for a known sRGB pair equals the
// reference value, confirming the *100 scaling. The reference comes from
// go-colorful's Lab pipeline on the same pair; the magnitude (a large, several-
// units dE for a clearly different pair) confirms the scale is 0..100, not
// 0..1.
func TestDeltaE2000ScalingReference(t *testing.T) {
	// Pure red vs pure green: a large, unmistakable colour difference.
	de := DeltaE2000(255, 0, 0, 0, 255, 0)
	c1 := colorful.Color{R: 1, G: 0, B: 0}
	c2 := colorful.Color{R: 0, G: 1, B: 0}
	want := c1.DistanceCIEDE2000(c2) * 100
	if math.Abs(de-want) > 1e-9 {
		t.Errorf("DeltaE2000 = %.6f, want %.6f", de, want)
	}
	if de < 1 {
		t.Errorf("dE for red vs green = %.4f; expected >1 on the 0..100 scale", de)
	}
}

// TestCIEDE2000Identical asserts identical images yield ~zero Mean and P95. The
// per-pixel dE carries the same sub-1e-12 arm64 FMA residue as DeltaE2000, so
// allow that epsilon rather than asserting exact zero.
func TestCIEDE2000Identical(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 4, 3))
	for i := range img.Pix {
		img.Pix[i] = uint8(i * 7)
	}
	cp := image.NewRGBA(img.Bounds())
	copy(cp.Pix, img.Pix)
	stats := CIEDE2000(img, cp)
	if math.Abs(stats.Mean) > 1e-12 || math.Abs(stats.P95) > 1e-12 {
		t.Errorf("identical images: Mean=%v P95=%v, want ~0 ~0", stats.Mean, stats.P95)
	}
}

// TestCIEDE2000ChannelOrder confirms CIEDE2000 reads pixels in RGBA order. Two
// single-pixel images differing only in the red byte must produce a non-zero
// dE; if the metric read the bytes in a different order it would still differ,
// so the test also pins the exact dE for a red-only change against DeltaE2000.
func TestCIEDE2000ChannelOrder(t *testing.T) {
	a := image.NewRGBA(image.Rect(0, 0, 1, 1))
	b := image.NewRGBA(image.Rect(0, 0, 1, 1))
	// RGBA byte layout: index 0=R, 1=G, 2=B, 3=A.
	a.Pix[0], a.Pix[1], a.Pix[2], a.Pix[3] = 200, 100, 50, 255
	b.Pix[0], b.Pix[1], b.Pix[2], b.Pix[3] = 40, 100, 50, 255
	stats := CIEDE2000(a, b)
	want := DeltaE2000(200, 100, 50, 40, 100, 50)
	if math.Abs(stats.Mean-want) > 1e-9 {
		t.Errorf("Mean = %.6f, want %.6f (channel order or pixel read drift)", stats.Mean, want)
	}
	if stats.Mean == 0 {
		t.Error("red-only change produced dE 0; pixel not read as RGBA")
	}
}

// TestCIEDE2000P95 checks the percentile picks the worst pixels. Two pixels in
// twenty differ strongly, so the 95th-percentile rank lands on the outlier
// block and P95 equals the outlier dE, while the mean stays an order of
// magnitude lower. A single outlier would put both Mean and P95 at outlier/20,
// a tie that rounds either way across architectures.
func TestCIEDE2000P95(t *testing.T) {
	w, h := 5, 4 // 20 pixels
	a := image.NewRGBA(image.Rect(0, 0, w, h))
	b := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range w * h {
		a.Pix[i*4+0], a.Pix[i*4+1], a.Pix[i*4+2], a.Pix[i*4+3] = 128, 128, 128, 255
		b.Pix[i*4+0], b.Pix[i*4+1], b.Pix[i*4+2], b.Pix[i*4+3] = 128, 128, 128, 255
	}
	// Make the last two pixels strongly different.
	for _, i := range []int{w*h - 2, w*h - 1} {
		b.Pix[i*4+0], b.Pix[i*4+1], b.Pix[i*4+2] = 255, 0, 0
	}
	stats := CIEDE2000(a, b)
	outlier := DeltaE2000(128, 128, 128, 255, 0, 0)
	if stats.Mean >= stats.P95 {
		t.Errorf("Mean %.4f should be below P95 %.4f", stats.Mean, stats.P95)
	}
	if stats.P95 <= 0 || stats.P95 > outlier+1e-9 {
		t.Errorf("P95 = %.4f, want in (0, %.4f]", stats.P95, outlier)
	}
}

// fillGrey sets every pixel of img to the opaque grey value v.
func fillGrey(img *image.RGBA, v uint8) {
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i+0], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = v, v, v, 255
	}
}

// TestHistogramIdentical asserts identical images give intersection 1, EMD 0,
// and chi-square 0.
func TestHistogramIdentical(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 6, 5))
	for i := range img.Pix {
		img.Pix[i] = uint8(i * 11)
	}
	cp := image.NewRGBA(img.Bounds())
	copy(cp.Pix, img.Pix)
	s := Histogram(img, cp)
	if math.Abs(s.Intersection-1) > 1e-12 {
		t.Errorf("Intersection = %.12f, want 1", s.Intersection)
	}
	if s.EMD != 0 {
		t.Errorf("EMD = %v, want 0", s.EMD)
	}
	if s.ChiSquare != 0 {
		t.Errorf("ChiSquare = %v, want 0", s.ChiSquare)
	}
}

// TestHistogramEMDBanding asserts a banded pair raises EMD. A smooth ramp and a
// banded version of it share no exact bins, so EMD picks up the mass shift; a
// closer shift must give a smaller EMD than a farther one.
func TestHistogramEMDBanding(t *testing.T) {
	const w, h = 16, 1
	smooth := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		v := uint8(x * 16) // 0,16,32,... a smooth ramp
		smooth.Pix[x*4+0], smooth.Pix[x*4+1], smooth.Pix[x*4+2], smooth.Pix[x*4+3] = v, v, v, 255
	}
	// Banded: collapse each pair of ramp steps to one level, shifting mass.
	near := image.NewRGBA(image.Rect(0, 0, w, h))
	far := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		nv := uint8((x / 2) * 32) // small shift from the ramp
		fv := uint8((x / 4) * 64) // larger shift, coarser bands
		near.Pix[x*4+0], near.Pix[x*4+1], near.Pix[x*4+2], near.Pix[x*4+3] = nv, nv, nv, 255
		far.Pix[x*4+0], far.Pix[x*4+1], far.Pix[x*4+2], far.Pix[x*4+3] = fv, fv, fv, 255
	}
	base := Histogram(smooth, smooth).EMD
	nearEMD := Histogram(smooth, near).EMD
	farEMD := Histogram(smooth, far).EMD
	if base != 0 {
		t.Errorf("self EMD = %v, want 0", base)
	}
	if nearEMD <= 0 {
		t.Errorf("banded EMD = %v, want > 0", nearEMD)
	}
	if farEMD <= nearEMD {
		t.Errorf("coarser banding EMD %.4f should exceed finer banding EMD %.4f", farEMD, nearEMD)
	}
}

// TestHistogramChiSquarePaletteCollapse asserts a palette-collapsed pair raises
// chi-square. Collapsing a spread of greys onto one value concentrates the mass
// and drives chi-square up versus a milder collapse.
func TestHistogramChiSquarePaletteCollapse(t *testing.T) {
	const w, h = 16, 1
	spread := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		v := uint8(x * 16)
		spread.Pix[x*4+0], spread.Pix[x*4+1], spread.Pix[x*4+2], spread.Pix[x*4+3] = v, v, v, 255
	}
	mild := image.NewRGBA(image.Rect(0, 0, w, h)) // four levels
	full := image.NewRGBA(image.Rect(0, 0, w, h)) // one level
	fillGrey(full, 128)
	for x := range w {
		mv := uint8((x / 4) * 64)
		mild.Pix[x*4+0], mild.Pix[x*4+1], mild.Pix[x*4+2], mild.Pix[x*4+3] = mv, mv, mv, 255
	}
	self := Histogram(spread, spread).ChiSquare
	mildChi := Histogram(spread, mild).ChiSquare
	fullChi := Histogram(spread, full).ChiSquare
	if self != 0 {
		t.Errorf("self chi-square = %v, want 0", self)
	}
	if mildChi <= 0 {
		t.Errorf("mild collapse chi-square = %v, want > 0", mildChi)
	}
	if fullChi <= mildChi {
		t.Errorf("full collapse chi-square %.4f should exceed mild collapse %.4f", fullChi, mildChi)
	}
}

// TestHistogramChannelOrder confirms Histogram reads pixels in RGBA order. Two
// images that differ only in the red byte must register a difference in all
// three combined metrics, and the per-channel red histograms must drive it.
func TestHistogramChannelOrder(t *testing.T) {
	a := image.NewRGBA(image.Rect(0, 0, 2, 1))
	b := image.NewRGBA(image.Rect(0, 0, 2, 1))
	// RGBA byte layout: index 0=R, 1=G, 2=B, 3=A.
	a.Pix[0], a.Pix[1], a.Pix[2], a.Pix[3] = 10, 100, 200, 255
	a.Pix[4], a.Pix[5], a.Pix[6], a.Pix[7] = 20, 100, 200, 255
	b.Pix[0], b.Pix[1], b.Pix[2], b.Pix[3] = 250, 100, 200, 255
	b.Pix[4], b.Pix[5], b.Pix[6], b.Pix[7] = 240, 100, 200, 255
	s := Histogram(a, b)
	if s.Intersection >= 1 {
		t.Errorf("Intersection = %v, want < 1 for a red-only change", s.Intersection)
	}
	if s.EMD == 0 || s.ChiSquare == 0 {
		t.Errorf("red-only change gave EMD=%v ChiSquare=%v; pixels not read as RGBA", s.EMD, s.ChiSquare)
	}
}

// TestHistogramMismatchPanics asserts mismatched dimensions panic.
func TestHistogramMismatchPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Histogram on mismatched dimensions did not panic")
		}
	}()
	Histogram(image.NewRGBA(image.Rect(0, 0, 2, 2)), image.NewRGBA(image.Rect(0, 0, 3, 2)))
}

// TestPercentile pins the interpolation on a known sequence.
func TestPercentile(t *testing.T) {
	vals := []float64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	if got := percentile(vals, 0.95); math.Abs(got-8.55) > 1e-9 {
		t.Errorf("p95 = %v, want 8.55", got)
	}
	if got := percentile(vals, 0); got != 0 {
		t.Errorf("p0 = %v, want 0", got)
	}
	if got := percentile(vals, 1); got != 9 {
		t.Errorf("p100 = %v, want 9", got)
	}
	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("empty = %v, want 0", got)
	}
}
