package metrics

import (
	"image"
	"image/color"
	"math"
	"testing"
)

// solid returns a width x height *image.RGBA filled with one opaque colour.
func solid(w, h int, c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

// TestMSEIdentical checks identical images give MSE 0.
func TestMSEIdentical(t *testing.T) {
	a := solid(8, 8, color.RGBA{R: 120, G: 200, B: 40, A: 255})
	b := solid(8, 8, color.RGBA{R: 120, G: 200, B: 40, A: 255})
	if got := MSE(a, b); got != 0 {
		t.Errorf("MSE = %v, want 0", got)
	}
}

// TestMSEKnown checks MSE on a known constant luma difference. Both images are
// flat grey, so every pixel luma is the channel value. Image a is 100, image b
// is 110, so each squared error is 100 and the mean is 100.
func TestMSEKnown(t *testing.T) {
	a := solid(4, 4, color.RGBA{R: 100, G: 100, B: 100, A: 255})
	b := solid(4, 4, color.RGBA{R: 110, G: 110, B: 110, A: 255})
	const want = 100.0
	if got := MSE(a, b); math.Abs(got-want) > 1e-9 {
		t.Errorf("MSE = %v, want %v", got, want)
	}
}

// TestPSNRIdentical checks identical images return the infinite sentinel.
func TestPSNRIdentical(t *testing.T) {
	a := solid(8, 8, color.RGBA{R: 10, G: 20, B: 30, A: 255})
	b := solid(8, 8, color.RGBA{R: 10, G: 20, B: 30, A: 255})
	if got := PSNR(a, b); got != PSNRInfinite {
		t.Errorf("PSNR = %v, want PSNRInfinite", got)
	}
}

// TestPSNRKnown checks PSNR against MSE = 100, peak 255.
// PSNR = 10*log10(255^2/100) = 10*log10(650.25) = 28.1308...
func TestPSNRKnown(t *testing.T) {
	a := solid(4, 4, color.RGBA{R: 100, G: 100, B: 100, A: 255})
	b := solid(4, 4, color.RGBA{R: 110, G: 110, B: 110, A: 255})
	want := 10 * math.Log10(255*255/100.0)
	if got := PSNR(a, b); math.Abs(got-want) > 1e-9 {
		t.Errorf("PSNR = %v, want %v", got, want)
	}
}

// TestGMSDIdentical checks GMSD of identical images is 0. Equal gradient maps
// make every GMS value 1, so the standard deviation is 0.
func TestGMSDIdentical(t *testing.T) {
	a := solid(16, 16, color.RGBA{R: 90, G: 90, B: 90, A: 255})
	// Add a gradient feature so the gradient map is non-trivial.
	for y := range 16 {
		for x := range 16 {
			v := uint8(x * 8 % 256)
			a.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	b := image.NewRGBA(a.Bounds())
	copy(b.Pix, a.Pix)
	if got := GMSD(a, b); math.Abs(got) > 1e-12 {
		t.Errorf("GMSD = %v, want 0", got)
	}
}

// TestGMSDKnown checks GMSD against a hand-computed value on a 3x3 luma pair.
//
// Image a luma plane (flat grey channels, so luma equals the channel value):
//
//	0   0   0
//	0  30   0
//	0   0   0
//
// Image b is all zero. With replicate edges, b's Prewitt magnitude is 0
// everywhere, so gb=0 and each GMS = c/(ga^2+c). Image a's centre pixel is the
// only one with a zero Prewitt response (its 3x3 neighbourhood is symmetric in
// both kernel directions): gx = gy = 0 there, so its GMS = 1. Every other pixel
// of a has a non-zero gradient, driving its GMS toward 0.
//
// Hand-computed reference. With replicate edges and the Prewitt kernels divided
// by 3, a's gradient magnitudes are:
//
//	14.142135623730951  10.0  14.142135623730951
//	10.0                 0.0  10.0
//	14.142135623730951  10.0  14.142135623730951
//
// With gb=0 each GMS is c/(ga^2+c), c=0.0026. The eight edge pixels collapse
// toward 0, the centre is 1, and the standard deviation of the nine GMS values
// is 0.31426355246110704 (verified by an independent Python computation).
func TestGMSDKnown(t *testing.T) {
	const w, h = 3, 3
	a := image.NewRGBA(image.Rect(0, 0, w, h))
	b := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			a.SetRGBA(x, y, color.RGBA{A: 255})
			b.SetRGBA(x, y, color.RGBA{A: 255})
		}
	}
	a.SetRGBA(1, 1, color.RGBA{R: 30, G: 30, B: 30, A: 255})

	const want = 0.31426355246110704
	if got := GMSD(a, b); math.Abs(got-want) > 1e-4 {
		t.Errorf("GMSD = %v, want %v (within 1e-4)", got, want)
	}
}

// TestSSIMIdentical checks SSIM of identical images is exactly 1.0. With equal
// inputs every windowed statistic matches, so each SSIM map value is 1 and their
// mean is 1.
func TestSSIMIdentical(t *testing.T) {
	a := image.NewRGBA(image.Rect(0, 0, 32, 24))
	for y := range 24 {
		for x := range 32 {
			v := uint8((x*7 + y*3) % 256)
			a.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	b := image.NewRGBA(a.Bounds())
	copy(b.Pix, a.Pix)
	if got := SSIM(a, b); got != 1.0 {
		t.Errorf("SSIM = %v, want exactly 1.0", got)
	}
}

// TestSSIMRange checks SSIM stays in 0..1 for a lightly degraded pair and drops
// below 1. The expected operating range for similar renders is 0..1; the index
// is mathematically bounded by [-1, 1], reached only by anti-correlated content.
func TestSSIMRange(t *testing.T) {
	a := image.NewRGBA(image.Rect(0, 0, 32, 24))
	b := image.NewRGBA(image.Rect(0, 0, 32, 24))
	for y := range 24 {
		for x := range 32 {
			va := uint8((x * 8) % 256)
			// b is a's plane plus a small uniform offset: a degraded but still
			// positively correlated copy, so SSIM stays in 0..1.
			vb := uint8((x*8 + 12) % 256)
			a.SetRGBA(x, y, color.RGBA{R: va, G: va, B: va, A: 255})
			b.SetRGBA(x, y, color.RGBA{R: vb, G: vb, B: vb, A: 255})
		}
	}
	got := SSIM(a, b)
	if got < 0 || got > 1 {
		t.Errorf("SSIM = %v, want within [0,1] for a degraded but correlated pair", got)
	}
	if got >= 1 {
		t.Errorf("SSIM = %v, want < 1 for a differing pair", got)
	}
}

// TestSSIMAntiCorrelatedBound checks the index respects its true [-1, 1] bound
// even when content is anti-correlated (an inverted gradient), where SSIM goes
// negative. This guards the port against producing values outside [-1, 1].
func TestSSIMAntiCorrelatedBound(t *testing.T) {
	a := image.NewRGBA(image.Rect(0, 0, 32, 24))
	b := image.NewRGBA(image.Rect(0, 0, 32, 24))
	for y := range 24 {
		for x := range 32 {
			va := uint8((x * 8) % 256)
			vb := uint8((255 - x*8 + 256) % 256)
			a.SetRGBA(x, y, color.RGBA{R: va, G: va, B: va, A: 255})
			b.SetRGBA(x, y, color.RGBA{R: vb, G: vb, B: vb, A: 255})
		}
	}
	got := SSIM(a, b)
	if got < -1 || got > 1 {
		t.Errorf("SSIM = %v, want within [-1, 1]", got)
	}
}

// TestSSIMSymmetric checks SSIM is symmetric in its arguments, a property the
// scikit reference shares: swapping X and Y leaves the index unchanged.
func TestSSIMSymmetric(t *testing.T) {
	a := image.NewRGBA(image.Rect(0, 0, 20, 16))
	b := image.NewRGBA(image.Rect(0, 0, 20, 16))
	for y := range 16 {
		for x := range 20 {
			va := uint8((x*5 + y*9) % 256)
			vb := uint8((x*3 + y*11 + 40) % 256)
			a.SetRGBA(x, y, color.RGBA{R: va, G: va, B: va, A: 255})
			b.SetRGBA(x, y, color.RGBA{R: vb, G: vb, B: vb, A: 255})
		}
	}
	ab := SSIM(a, b)
	ba := SSIM(b, a)
	if math.Abs(ab-ba) > 1e-12 {
		t.Errorf("SSIM not symmetric: SSIM(a,b) = %v, SSIM(b,a) = %v", ab, ba)
	}
}

// TestSSIMFlatPair checks SSIM against a closed-form reference on two flat
// images. With both inputs uniform, every windowed mean equals the constant and
// every variance and covariance is zero, regardless of the Gaussian window or
// the border mode. The contrast and structure term collapses to C2/C2 = 1, so
// the SSIM map is the luminance term everywhere:
//
//	SSIM = (2*mx*my + C1) / (mx^2 + my^2 + C1)
//
// with C1 = (0.01*255)^2 = 6.5025. For luma mx = 100 and my = 150:
//
//	(2*100*150 + 6.5025) / (100^2 + 150^2 + 6.5025)
//	= 30006.5025 / 32506.5025 = 0.9230938...
//
// This reference needs no scikit-image; it follows from the SSIM definition.
func TestSSIMFlatPair(t *testing.T) {
	a := solid(16, 16, color.RGBA{R: 100, G: 100, B: 100, A: 255})
	b := solid(16, 16, color.RGBA{R: 150, G: 150, B: 150, A: 255})
	const c1 = (0.01 * 255.0) * (0.01 * 255.0)
	want := (2*100.0*150.0 + c1) / (100.0*100.0 + 150.0*150.0 + c1)
	if got := SSIM(a, b); math.Abs(got-want) > 1e-9 {
		t.Errorf("SSIM = %v, want %v (within 1e-9)", got, want)
	}
}

// gradientRGBA returns a w x h grey image whose luma ramps with x and y so the
// plane has enough structure for the multiscale path to exercise every scale.
func gradientRGBA(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			v := uint8((x*5 + y*3) % 256)
			img.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	return img
}

// TestMSSSIMIdentical checks MS-SSIM of identical images is exactly 1.0. Every
// scale's SSIM and CS term is 1, so each weighted factor is 1 and the product
// is 1, independent of how many scales run or how the weights renormalise.
func TestMSSSIMIdentical(t *testing.T) {
	a := gradientRGBA(200, 200)
	b := image.NewRGBA(a.Bounds())
	copy(b.Pix, a.Pix)
	got, scales := MSSSIM(a, b)
	if got != 1.0 {
		t.Errorf("MSSSIM = %v, want exactly 1.0", got)
	}
	if scales != 5 {
		t.Errorf("scales = %d, want 5 for a 200x200 image", scales)
	}
}

// TestMSSSIMFiveScales checks a large enough image runs all five Wang scales.
// Each scale halves the side, so the smallest side must stay >= ssimWinSize
// through four halvings. The integer-halving sequence from 200 is
// 200->100->50->25->12, and 12 >= 11, so a 200x200 image fits all five scales.
func TestMSSSIMFiveScales(t *testing.T) {
	a := gradientRGBA(200, 200)
	b := image.NewRGBA(a.Bounds())
	copy(b.Pix, a.Pix)
	if _, scales := MSSSIM(a, b); scales != 5 {
		t.Errorf("scales = %d, want 5 for a 200x200 image", scales)
	}
}

// TestMSSSIMFewerScales checks the scale count and weight renormalisation when
// fewer than five scales fit. A 24x24 image halves to 12x12 (still >= 11) then
// to 6x6 (< 11), so exactly two scales run. With two scales, the reported count
// is 2 and the two Wang weights renormalise to sum 1; the identical-image
// result must still be exactly 1.0.
func TestMSSSIMFewerScales(t *testing.T) {
	a := gradientRGBA(24, 24)
	b := image.NewRGBA(a.Bounds())
	copy(b.Pix, a.Pix)
	got, scales := MSSSIM(a, b)
	if scales != 2 {
		t.Errorf("scales = %d, want 2 for a 24x24 image", scales)
	}
	if got != 1.0 {
		t.Errorf("MSSSIM = %v, want exactly 1.0 with renormalised weights", got)
	}
}

// TestMSSSIMSingleScale checks an image that fits only one scale collapses
// MS-SSIM to the full single-scale SSIM. At one scale the lone weight
// renormalises to 1 and the coarsest-scale rule takes the full SSIM, so the
// MS-SSIM value equals SSIM exactly. A 16x16 image halves to 8x8 (< 11), so one
// scale runs.
func TestMSSSIMSingleScale(t *testing.T) {
	a := solid(16, 16, color.RGBA{R: 100, G: 100, B: 100, A: 255})
	b := solid(16, 16, color.RGBA{R: 150, G: 150, B: 150, A: 255})
	got, scales := MSSSIM(a, b)
	if scales != 1 {
		t.Fatalf("scales = %d, want 1 for a 16x16 image", scales)
	}
	want := SSIM(a, b)
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("MSSSIM = %v, want SSIM %v at a single scale", got, want)
	}
}

// TestMSSSIMRange checks MS-SSIM stays in 0..1 for a degraded but positively
// correlated pair and drops below 1.
func TestMSSSIMRange(t *testing.T) {
	a := image.NewRGBA(image.Rect(0, 0, 64, 48))
	b := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := range 48 {
		for x := range 64 {
			va := uint8((x * 4) % 256)
			vb := uint8((x*4 + 16) % 256)
			a.SetRGBA(x, y, color.RGBA{R: va, G: va, B: va, A: 255})
			b.SetRGBA(x, y, color.RGBA{R: vb, G: vb, B: vb, A: 255})
		}
	}
	got, _ := MSSSIM(a, b)
	if got < 0 || got > 1 {
		t.Errorf("MSSSIM = %v, want within [0,1] for a degraded but correlated pair", got)
	}
	if got >= 1 {
		t.Errorf("MSSSIM = %v, want < 1 for a differing pair", got)
	}
}

// TestDownsample2x checks the 2x2 average-pool downsample halves both sides and
// returns the mean of each non-overlapping 2x2 block. A 4x2 plane reduces to
// 2x1, each output the average of its source block.
func TestDownsample2x(t *testing.T) {
	// plane laid out row-major, 4 wide x 2 high:
	//   0  2  4  6
	//   8 10 12 14
	plane := []float64{0, 2, 4, 6, 8, 10, 12, 14}
	out := downsample2x(plane, 4, 2)
	want := []float64{(0 + 2 + 8 + 10) / 4.0, (4 + 6 + 12 + 14) / 4.0}
	if len(out) != len(want) {
		t.Fatalf("len(out) = %d, want %d", len(out), len(want))
	}
	for i := range want {
		if math.Abs(out[i]-want[i]) > 1e-12 {
			t.Errorf("out[%d] = %v, want %v", i, out[i], want[i])
		}
	}
}

// TestReflectIndex checks the reflect border mapping against the scipy ndimage
// "reflect" convention: mirror without repeating the edge sample. For n = 4 the
// virtual sequence around the edges is (... 2 1 0 | 0 1 2 3 | 3 2 1 0 ...).
func TestReflectIndex(t *testing.T) {
	const n = 4
	cases := []struct {
		in, want int
	}{
		{-1, 0},
		{-2, 1},
		{-3, 2},
		{-4, 3},
		{-5, 3},
		{0, 0},
		{3, 3},
		{4, 3},
		{5, 2},
		{6, 1},
		{7, 0},
		{8, 0},
	}
	for _, c := range cases {
		if got := reflectIndex(c.in, n); got != c.want {
			t.Errorf("reflectIndex(%d, %d) = %d, want %d", c.in, n, got, c.want)
		}
	}
	if got := reflectIndex(5, 1); got != 0 {
		t.Errorf("reflectIndex(5, 1) = %d, want 0", got)
	}
}

// TestGaussianKernel1D checks the window length, symmetry, unit sum, and peak.
func TestGaussianKernel1D(t *testing.T) {
	k := gaussianKernel1D(ssimWinSize, ssimSigma)
	if len(k) != ssimWinSize {
		t.Fatalf("kernel length = %d, want %d", len(k), ssimWinSize)
	}
	var sum float64
	for _, w := range k {
		sum += w
	}
	if math.Abs(sum-1.0) > 1e-12 {
		t.Errorf("kernel sum = %v, want 1.0", sum)
	}
	mid := (ssimWinSize - 1) / 2
	for i := range mid {
		if math.Abs(k[i]-k[ssimWinSize-1-i]) > 1e-15 {
			t.Errorf("kernel not symmetric at tap %d", i)
		}
		if k[i] >= k[mid] {
			t.Errorf("kernel tap %d (%v) not below centre (%v)", i, k[i], k[mid])
		}
	}
}
