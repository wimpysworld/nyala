package metrics

import (
	"image"
	"math"
)

// PSNRInfinite is the sentinel PSNR returned when two images are identical and
// the mean squared error is zero. PSNR is unbounded in that case, so a large
// finite sentinel stands in for positive infinity.
const PSNRInfinite = math.MaxFloat64

// gmsdConstant is the stabilising constant c from Xue et al. 2014, "Gradient
// Magnitude Similarity Deviation". It keeps the gradient magnitude similarity
// map well behaved where both gradients are near zero. The 0.0026 value is the
// paper's default for inputs scaled to 0..255.
const gmsdConstant = 0.0026

// requireEqualDims panics when a and b differ in width or height. name is the
// calling metric, so the message reads "metrics: <name> requires equal
// dimensions", matching the per-metric guards it replaces. Channel order is
// RGBA.
func requireEqualDims(a, b *image.RGBA, name string) {
	ba, bb := a.Bounds(), b.Bounds()
	if ba.Dx() != bb.Dx() || ba.Dy() != bb.Dy() {
		panic("metrics: " + name + " requires equal dimensions")
	}
}

// lumaBT601 returns the BT.601 luma of an 8-bit RGB triple (0.299 R, 0.587 G,
// 0.114 B) in 0..255. It is the single definition of the coefficients the luma
// plane and the oracle both split on, so they divide a cell the same way. The
// body is small enough for the compiler to inline in the per-pixel loop.
func lumaBT601(r, g, b uint8) float64 {
	return 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)
}

// luma converts an *image.RGBA to a width*height grey plane using the BT.601
// coefficients (0.299 R, 0.587 G, 0.114 B). The result holds one float64 per
// pixel in row-major order, each in 0..255. Channel order is RGBA.
func luma(img *image.RGBA) (plane []float64, w, h int) {
	b := img.Bounds()
	w, h = b.Dx(), b.Dy()
	plane = make([]float64, w*h)
	for y := range h {
		for x := range w {
			c := img.RGBAAt(b.Min.X+x, b.Min.Y+y)
			plane[y*w+x] = lumaBT601(c.R, c.G, c.B)
		}
	}
	return plane, w, h
}

// MSE returns the mean squared error between two images on BT.601 luma. The
// images must share dimensions. Identical images return 0. Channel order is
// RGBA.
func MSE(a, b *image.RGBA) float64 {
	requireEqualDims(a, b, "MSE")
	pa, _, _ := luma(a)
	pb, _, _ := luma(b)
	var sum float64
	for i := range pa {
		d := pa[i] - pb[i]
		sum += d * d
	}
	if len(pa) == 0 {
		return 0
	}
	return sum / float64(len(pa))
}

// PSNR returns the peak signal-to-noise ratio in decibels between two images on
// BT.601 luma, with a 255 peak. Identical images return PSNRInfinite. The images
// must share dimensions. Channel order is RGBA.
func PSNR(a, b *image.RGBA) float64 {
	mse := MSE(a, b)
	if mse == 0 {
		return PSNRInfinite
	}
	const peak = 255.0
	return 10 * math.Log10(peak*peak/mse)
}

// GMSD returns the Gradient Magnitude Similarity Deviation between two images on
// BT.601 luma, per Xue et al. 2014. It computes Prewitt gradient magnitude maps
// for both images, forms the gradient magnitude similarity map, and returns the
// standard deviation of that map. Lower is better; identical images return 0.
// The images must share dimensions. Channel order is RGBA.
func GMSD(a, b *image.RGBA) float64 {
	requireEqualDims(a, b, "GMSD")
	pa, wa, ha := luma(a)
	pb, wb, hb := luma(b)
	ga := prewittMagnitude(pa, wa, ha)
	gb := prewittMagnitude(pb, wb, hb)
	n := len(ga)
	if n == 0 {
		return 0
	}
	gms := make([]float64, n)
	var mean float64
	for i := range ga {
		v := (2*ga[i]*gb[i] + gmsdConstant) / (ga[i]*ga[i] + gb[i]*gb[i] + gmsdConstant)
		gms[i] = v
		mean += v
	}
	mean /= float64(n)
	var variance float64
	for _, v := range gms {
		d := v - mean
		variance += d * d
	}
	variance /= float64(n)
	return math.Sqrt(variance)
}

// SSIM parameters fixed to the scikit-image structural_similarity reference,
// called with gaussian_weights=True, sigma=1.5, use_sample_covariance=False,
// and an 8-bit data range. The port matches that configuration exactly.
const (
	// ssimWinSize is the side length of the square sliding window. The scikit
	// reference derives 11 from a sigma 1.5 Gaussian truncated at 3.5 sigma:
	// radius = int(3.5*1.5 + 0.5) = 5, so the window is 2*5+1 = 11.
	ssimWinSize = 11
	// ssimSigma is the Gaussian window standard deviation.
	ssimSigma = 1.5
	// ssimDataRange is the dynamic range of the luma plane (0..255).
	ssimDataRange = 255.0
	// ssimK1 and ssimK2 are the SSIM stabilising constants from Wang et al.
	// 2004, "Image Quality Assessment: From Error Visibility to Structural
	// Similarity".
	ssimK1 = 0.01
	ssimK2 = 0.03
)

// SSIM returns the mean structural similarity index between two images on BT.601
// luma, ported from the scikit-image structural_similarity reference with
// gaussian_weights=True. It uses an 11x11 Gaussian window (sigma 1.5), reflect
// border handling, population (not sample) covariance, an 8-bit data range, and
// the documented 5-pixel border crop. The result is a single scalar in 0..1;
// identical images return 1.0. The images must share dimensions and each side
// must be at least ssimWinSize. Channel order is RGBA.
func SSIM(a, b *image.RGBA) float64 {
	requireEqualDims(a, b, "SSIM")
	pa, wa, ha := luma(a)
	pb, _, _ := luma(b)
	if wa < ssimWinSize || ha < ssimWinSize {
		panic("metrics: SSIM requires each side to be at least the window size")
	}
	ssim, _ := ssimAndCS(pa, pb, wa, ha)
	return ssim
}

// ssimAndCS computes the mean SSIM and the mean contrast-structure (CS) term
// over the cropped interior of two luma planes that share dimensions. It is the
// shared core behind SSIM and MSSSIM. The CS term is the second factor of the
// SSIM map, (2*vxy + C2) / (vx + vy + C2); MS-SSIM combines the CS term across
// fine scales with the full SSIM at the coarsest scale, per Wang et al. 2003.
// Both returns average over the same 5-pixel border crop the scikit reference
// uses. Each plane side must be at least ssimWinSize.
func ssimAndCS(pa, pb []float64, w, h int) (ssim, cs float64) {
	kernel := gaussianKernel1D(ssimWinSize, ssimSigma)

	// Products needed for the windowed means, computed per pixel up front.
	paa := make([]float64, len(pa))
	pbb := make([]float64, len(pb))
	pab := make([]float64, len(pa))
	for i := range pa {
		paa[i] = pa[i] * pa[i]
		pbb[i] = pb[i] * pb[i]
		pab[i] = pa[i] * pb[i]
	}

	ux := gaussianFilter(pa, w, h, kernel)
	uy := gaussianFilter(pb, w, h, kernel)
	uxx := gaussianFilter(paa, w, h, kernel)
	uyy := gaussianFilter(pbb, w, h, kernel)
	uxy := gaussianFilter(pab, w, h, kernel)

	// use_sample_covariance=False fixes the covariance normaliser to 1, so the
	// windowed second moments give the population covariance directly.
	c1 := (ssimK1 * ssimDataRange) * (ssimK1 * ssimDataRange)
	c2 := (ssimK2 * ssimDataRange) * (ssimK2 * ssimDataRange)

	// Crop the border by (winSize-1)/2 = 5 pixels each side and average the
	// SSIM and CS maps over the valid interior only, matching the scikit
	// reference.
	const pad = (ssimWinSize - 1) / 2
	var sumSSIM, sumCS float64
	var count int
	for y := pad; y < h-pad; y++ {
		for x := pad; x < w-pad; x++ {
			i := y*w + x
			vx := uxx[i] - ux[i]*ux[i]
			vy := uyy[i] - uy[i]*uy[i]
			vxy := uxy[i] - ux[i]*uy[i]
			a1 := 2*ux[i]*uy[i] + c1
			a2 := 2*vxy + c2
			b1 := ux[i]*ux[i] + uy[i]*uy[i] + c1
			b2 := vx + vy + c2
			csVal := a2 / b2
			sumSSIM += (a1 / b1) * csVal
			sumCS += csVal
			count++
		}
	}
	if count == 0 {
		return 1.0, 1.0
	}
	n := float64(count)
	return sumSSIM / n, sumCS / n
}

// msssimWeights are the five per-scale weights from Wang, Simoncelli, and
// Bovik 2003, "Multiscale Structural Similarity for Image Quality Assessment",
// table after equation 7. They run from the finest scale to the coarsest and
// sum to 0.9999 in the paper; the port renormalises whatever subset of scales
// actually runs to sum 1, so the result stays a weighted geometric mean.
var msssimWeights = []float64{0.0448, 0.2856, 0.3001, 0.2363, 0.1333}

// MSSSIM returns the multiscale structural similarity index between two images
// on BT.601 luma, ported from Wang et al. 2003 over the scikit-image SSIM core
// (gaussian_weights=True, sigma 1.5, reflect border, population covariance,
// 5-pixel crop). At each fine scale it takes the contrast-structure (CS) term;
// at the coarsest scale it takes the full SSIM. Between scales it low-pass
// filters with a 2x2 average and subsamples by 2, the standard MS-SSIM
// downsample.
//
// The block grid is often too small for the paper's five scales: each scale
// halves the side and every scale still needs at least ssimWinSize per side. So
// MSSSIM runs as many scales as fit (at least one), reports that count through
// scales, and renormalises the matching weights to sum 1. The result is the
// weighted geometric mean prod(CS_i^w_i) for the fine scales times SSIM_M^w_M
// at the coarsest, a single scalar in 0..1; identical images return 1.0. The
// images must share dimensions and each side must be at least ssimWinSize.
// Channel order is RGBA.
func MSSSIM(a, b *image.RGBA) (value float64, scales int) {
	requireEqualDims(a, b, "MSSSIM")
	pa, wa, ha := luma(a)
	pb, _, _ := luma(b)
	if wa < ssimWinSize || ha < ssimWinSize {
		panic("metrics: MSSSIM requires each side to be at least the window size")
	}

	// Count how many scales fit before any side drops below the window. The
	// first scale uses the full-resolution planes; each later scale halves both
	// sides via 2x2 average pooling.
	scales = 0
	for w, h := wa, ha; w >= ssimWinSize && h >= ssimWinSize && scales < len(msssimWeights); w, h = w/2, h/2 {
		scales++
	}

	// Renormalise the first `scales` Wang weights to sum 1 so the weighted
	// geometric mean stays a proper mean when fewer than five scales run.
	weights := make([]float64, scales)
	var wsum float64
	for i := range scales {
		weights[i] = msssimWeights[i]
		wsum += weights[i]
	}
	for i := range weights {
		weights[i] /= wsum
	}

	curA, curB := pa, pb
	cw, ch := wa, ha
	value = 1.0
	for i := range scales {
		ssim, cs := ssimAndCS(curA, curB, cw, ch)
		// Fine scales contribute the CS term; the coarsest scale (the last one
		// run) contributes the full SSIM, per Wang et al. 2003 equation 7. Clamp
		// the base to >= 0 so a negative CS on anti-correlated content cannot
		// produce a NaN from a fractional power.
		base := cs
		if i == scales-1 {
			base = ssim
		}
		if base < 0 {
			base = 0
		}
		value *= math.Pow(base, weights[i])
		if i < scales-1 {
			curA, curB = downsample2x(curA, cw, ch), downsample2x(curB, cw, ch)
			cw, ch = cw/2, ch/2
		}
	}
	return value, scales
}

// downsample2x low-pass filters a row-major plane with a 2x2 box and subsamples
// by 2, returning a plane of size (w/2)x(h/2). Each output sample is the mean
// of one non-overlapping 2x2 input block. This is the MS-SSIM inter-scale
// downsample from Wang et al. 2003. Odd trailing rows or columns are dropped,
// matching the integer halving the scale count assumes.
func downsample2x(plane []float64, w, h int) []float64 {
	ow, oh := w/2, h/2
	out := make([]float64, ow*oh)
	for y := range oh {
		for x := range ow {
			sx, sy := x*2, y*2
			s := plane[sy*w+sx] + plane[sy*w+sx+1] +
				plane[(sy+1)*w+sx] + plane[(sy+1)*w+sx+1]
			out[y*ow+x] = s / 4
		}
	}
	return out
}

// gaussianKernel1D returns a normalised 1-D Gaussian kernel of the given odd
// size with the given standard deviation. The taps run from -radius to +radius
// where radius = (size-1)/2, weighted by exp(-0.5*(x/sigma)^2) and normalised to
// sum 1. This is the separable kernel scipy's gaussian_filter applies along each
// axis.
func gaussianKernel1D(size int, sigma float64) []float64 {
	radius := (size - 1) / 2
	k := make([]float64, size)
	var sum float64
	for i := range size {
		x := float64(i - radius)
		w := math.Exp(-0.5 * (x * x) / (sigma * sigma))
		k[i] = w
		sum += w
	}
	for i := range k {
		k[i] /= sum
	}
	return k
}

// gaussianFilter applies a separable 1-D Gaussian kernel to a row-major plane,
// horizontally then vertically, with reflect border handling. Reflect mirrors
// samples without repeating the edge: (d c b a | a b c d | d c b a), matching
// scipy ndimage's default mode and the scikit SSIM reference. The result is a
// new plane of the same dimensions.
func gaussianFilter(plane []float64, w, h int, kernel []float64) []float64 {
	radius := (len(kernel) - 1) / 2
	tmp := make([]float64, w*h)
	for y := range h {
		for x := range w {
			var acc float64
			for t := -radius; t <= radius; t++ {
				acc += kernel[t+radius] * plane[y*w+reflectIndex(x+t, w)]
			}
			tmp[y*w+x] = acc
		}
	}
	out := make([]float64, w*h)
	for y := range h {
		for x := range w {
			var acc float64
			for t := -radius; t <= radius; t++ {
				acc += kernel[t+radius] * tmp[reflectIndex(y+t, h)*w+x]
			}
			out[y*w+x] = acc
		}
	}
	return out
}

// reflectIndex maps an index that may fall outside 0..n-1 back into range using
// reflect (mirror) handling without repeating the edge sample, the convention
// scipy ndimage names "reflect". For n=4 the virtual sequence around the left
// edge is (... 2 1 0 | 0 1 2 3 | 3 2 1 0 ...).
func reflectIndex(i, n int) int {
	if n == 1 {
		return 0
	}
	period := 2 * n
	i %= period
	if i < 0 {
		i += period
	}
	if i >= n {
		i = period - 1 - i
	}
	return i
}

// prewittMagnitude returns the Prewitt gradient magnitude map of a luma plane.
// It convolves the plane with the horizontal and vertical Prewitt kernels using
// symmetric (replicate) edge handling, then returns sqrt(gx^2 + gy^2) per pixel
// in row-major order. This matches the gradient operator in the GMSD reference.
func prewittMagnitude(plane []float64, w, h int) []float64 {
	out := make([]float64, w*h)
	at := func(x, y int) float64 {
		if x < 0 {
			x = 0
		} else if x >= w {
			x = w - 1
		}
		if y < 0 {
			y = 0
		} else if y >= h {
			y = h - 1
		}
		return plane[y*w+x]
	}
	for y := range h {
		for x := range w {
			tl, tc, tr := at(x-1, y-1), at(x, y-1), at(x+1, y-1)
			ml, mr := at(x-1, y), at(x+1, y)
			bl, bc, br := at(x-1, y+1), at(x, y+1), at(x+1, y+1)
			gx := (tr + mr + br - tl - ml - bl) / 3
			gy := (bl + bc + br - tl - tc - tr) / 3
			out[y*w+x] = math.Sqrt(gx*gx + gy*gy)
		}
	}
	return out
}
