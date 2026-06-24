package metrics

import (
	"image"
	"math"
	"sort"

	colorful "github.com/lucasb-eyer/go-colorful"
)

// CIEDE2000Scale converts go-colorful's DistanceCIEDE2000 result to the dE
// shown in reports. go-colorful keeps L*, a*, and b* in 0..1, so its CIEDE2000
// distance is also 0..1; multiplying by 100 yields the conventional dE on the
// 0..100 CIELAB scale. Every dE this package reports has this factor applied.
const CIEDE2000Scale = 100.0

// DeltaE2000 returns the CIEDE2000 colour difference (dE) between two sRGB
// colours, each given as 8-bit RGB. The returned value is on the conventional
// 0..100 dE scale: it is go-colorful's DistanceCIEDE2000 multiplied by
// CIEDE2000Scale. Identical colours return 0. The alpha channel is ignored.
func DeltaE2000(r1, g1, b1, r2, g2, b2 uint8) float64 {
	c1 := colorful.Color{R: float64(r1) / 255, G: float64(g1) / 255, B: float64(b1) / 255}
	c2 := colorful.Color{R: float64(r2) / 255, G: float64(g2) / 255, B: float64(b2) / 255}
	return c1.DistanceCIEDE2000(c2) * CIEDE2000Scale
}

// CIEDE2000Stats holds the per-pixel CIEDE2000 summary between a render and its
// reference: the mean dE and the 95th-percentile dE, both on the 0..100 scale.
// Mean tracks overall colour fidelity; P95 surfaces the worst-affected pixels
// without letting a single outlier dominate.
type CIEDE2000Stats struct {
	// Mean is the arithmetic mean per-pixel dE across the image.
	Mean float64
	// P95 is the 95th-percentile per-pixel dE across the image.
	P95 float64
}

// CIEDE2000 returns the mean and 95th-percentile per-pixel CIEDE2000 dE between
// two images on the 0..100 scale. The images must share dimensions. Each pixel
// pair contributes one dE; identical images yield a zero Mean and P95. Channel
// order is RGBA and the alpha channel is ignored.
func CIEDE2000(a, b *image.RGBA) CIEDE2000Stats {
	requireEqualDims(a, b, "CIEDE2000")
	ba, bb := a.Bounds(), b.Bounds()
	w, h := ba.Dx(), ba.Dy()
	n := w * h
	if n == 0 {
		return CIEDE2000Stats{}
	}
	des := make([]float64, 0, n)
	var sum float64
	for y := range h {
		for x := range w {
			ca := a.RGBAAt(ba.Min.X+x, ba.Min.Y+y)
			cb := b.RGBAAt(bb.Min.X+x, bb.Min.Y+y)
			de := DeltaE2000(ca.R, ca.G, ca.B, cb.R, cb.G, cb.B)
			des = append(des, de)
			sum += de
		}
	}
	return CIEDE2000Stats{
		Mean: sum / float64(n),
		P95:  percentile(des, 0.95),
	}
}

// histBins is the per-channel bin count for the colour histograms. Each 8-bit
// channel value maps directly to one of 256 bins, so no quantisation loss
// occurs before the distance is taken.
const histBins = 256

// channelHist returns the per-channel normalised histograms (red, green, blue)
// of an image. Each returned slice has histBins entries summing to 1 over a
// non-empty image; an empty image yields all-zero histograms. Channel order is
// RGBA and the alpha channel is ignored.
func channelHist(img *image.RGBA) (r, g, b []float64) {
	r = make([]float64, histBins)
	g = make([]float64, histBins)
	b = make([]float64, histBins)
	bnd := img.Bounds()
	w, h := bnd.Dx(), bnd.Dy()
	n := w * h
	if n == 0 {
		return r, g, b
	}
	for y := range h {
		for x := range w {
			c := img.RGBAAt(bnd.Min.X+x, bnd.Min.Y+y)
			r[c.R]++
			g[c.G]++
			b[c.B]++
		}
	}
	inv := 1.0 / float64(n)
	for i := range histBins {
		r[i] *= inv
		g[i] *= inv
		b[i] *= inv
	}
	return r, g, b
}

// HistogramStats holds the colour-histogram distance summary between a render
// and its reference, computed per channel on RGB and combined across the three
// channels.
type HistogramStats struct {
	// Intersection is the headline similarity in 0..1, where 1 means the two
	// colour distributions are identical. It is the mean over the three
	// channels of the per-channel histogram intersection.
	Intersection float64
	// EMD is the mean over the three channels of the 1-D earth-mover's
	// distance between the per-channel histograms, in bins. It rises with
	// banding, where mass shifts across nearby bins. Identical images give 0.
	EMD float64
	// ChiSquare is the mean over the three channels of the symmetric
	// chi-square distance between the per-channel histograms. It rises on
	// palette collapse, where mass concentrates into few bins. Identical
	// images give 0.
	ChiSquare float64
}

// Histogram returns the colour-histogram distance between two images on RGB.
// Intersection is the headline (0..1, 1 best); EMD flags banding; chi-square
// flags palette collapse. The images must share dimensions. Identical images
// yield Intersection 1, EMD 0, and ChiSquare 0. Channel order is RGBA and the
// alpha channel is ignored.
func Histogram(a, b *image.RGBA) HistogramStats {
	requireEqualDims(a, b, "Histogram")
	ba := a.Bounds()
	if ba.Dx()*ba.Dy() == 0 {
		return HistogramStats{}
	}
	ar, ag, ab := channelHist(a)
	br, bg, bb2 := channelHist(b)
	var inter, emd, chi float64
	for _, p := range [3][2][]float64{{ar, br}, {ag, bg}, {ab, bb2}} {
		inter += histIntersection(p[0], p[1])
		emd += hist1DEMD(p[0], p[1])
		chi += histChiSquare(p[0], p[1])
	}
	return HistogramStats{
		Intersection: inter / 3,
		EMD:          emd / 3,
		ChiSquare:    chi / 3,
	}
}

// histIntersection returns the histogram intersection of two normalised
// histograms: the sum over bins of the smaller of the two bin values. For
// histograms that each sum to 1 the result lies in 0..1, with 1 for identical
// histograms.
func histIntersection(p, q []float64) float64 {
	var sum float64
	for i := range p {
		sum += min(p[i], q[i])
	}
	return sum
}

// hist1DEMD returns the 1-D earth-mover's distance between two normalised
// histograms, in bins. For ordered 1-D bins this equals the sum of the absolute
// differences of the cumulative distributions, the work to turn one histogram
// into the other. Identical histograms give 0.
func hist1DEMD(p, q []float64) float64 {
	var cdf, dist float64
	for i := range p {
		cdf += p[i] - q[i]
		dist += math.Abs(cdf)
	}
	return dist
}

// histChiSquare returns the symmetric chi-square distance between two
// normalised histograms: the sum over bins of (p-q)^2 / (p+q), with empty bins
// (p+q == 0) skipped. Identical histograms give 0; the value rises as mass
// concentrates into few bins, as on palette collapse.
func histChiSquare(p, q []float64) float64 {
	var sum float64
	for i := range p {
		d := p[i] + q[i]
		if d == 0 {
			continue
		}
		diff := p[i] - q[i]
		sum += diff * diff / d
	}
	return sum
}

// percentile returns the p-quantile (0..1) of values using linear interpolation
// between the two nearest ranks. It sorts a copy, so the caller's slice order is
// preserved. An empty slice returns 0.
func percentile(values []float64, p float64) float64 {
	n := len(values)
	if n == 0 {
		return 0
	}
	sorted := make([]float64, n)
	copy(sorted, values)
	sort.Float64s(sorted)
	if n == 1 {
		return sorted[0]
	}
	rank := p * float64(n-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return sorted[lo]
	}
	frac := rank - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}
