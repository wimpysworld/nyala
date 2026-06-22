package metrics

import (
	"image"
	"math"

	"github.com/wimpysworld/nyala/internal/render/block"
)

// WCAGThreshold is the WCAG 2.x AA contrast ratio for normal-size text: a
// foreground/background pair at or above 4.5:1 is legible. The legibility
// co-signal counts cells whose contrast falls below this bound.
const WCAGThreshold = 4.5

// WCAGContrast returns the WCAG 2.x contrast ratio between two sRGB colours,
// each given as 8-bit RGB. The ratio is (Llighter + 0.05) / (Ldarker + 0.05),
// where L is the sRGB relative luminance; it ranges from 1:1 (identical
// luminance) to 21:1 (black on white). Channel order is RGBA and the alpha
// channel is ignored.
func WCAGContrast(r1, g1, b1, r2, g2, b2 uint8) float64 {
	l1 := relativeLuminance(r1, g1, b1)
	l2 := relativeLuminance(r2, g2, b2)
	lighter, darker := l1, l2
	if darker > lighter {
		lighter, darker = darker, lighter
	}
	return (lighter + 0.05) / (darker + 0.05)
}

// relativeLuminance returns the sRGB relative luminance of an 8-bit RGB colour
// per WCAG 2.x: linearise each channel through the sRGB transfer function, then
// weight by 0.2126 R, 0.7152 G, 0.0722 B. The result lies in 0..1.
func relativeLuminance(r, g, b uint8) float64 {
	return 0.2126*srgbToLinear(r) + 0.7152*srgbToLinear(g) + 0.0722*srgbToLinear(b)
}

// srgbToLinear linearises one 8-bit sRGB channel to 0..1 light-linear space
// using the WCAG transfer function: a 0.03928 linear toe, then a 2.4 power
// curve above it.
func srgbToLinear(v uint8) float64 {
	c := float64(v) / 255.0
	if c <= 0.03928 {
		return c / 12.92
	}
	return math.Pow((c+0.055)/1.055, 2.4)
}

// WCAGStats holds the WCAG contrast summary over an owned cell grid: the
// per-cell foreground/background contrast ratios reduced to a minimum, a mean,
// and the count of cells below the WCAGThreshold of 4.5:1. It is a no-reference
// legibility co-signal: it reads only the grid the renderer owns, never OCR.
type WCAGStats struct {
	// Min is the lowest per-cell contrast ratio across the grid. It surfaces
	// the worst-legibility cell.
	Min float64
	// Mean is the arithmetic mean per-cell contrast ratio across the grid.
	Mean float64
	// BelowThreshold is the count of cells whose contrast is below 4.5:1.
	BelowThreshold int
}

// WCAGGrid summarises the WCAG contrast over every cell of an owned block grid,
// taking each cell's foreground against its background. It returns the minimum
// ratio, the mean ratio, and the count of cells below 4.5:1. An empty grid
// yields a zero-value WCAGStats. Channel order is RGBA and alpha is ignored.
func WCAGGrid(g block.Grid) WCAGStats {
	n := len(g.Cells)
	if n == 0 {
		return WCAGStats{}
	}
	min := math.Inf(1)
	var sum float64
	var below int
	for i := range g.Cells {
		c := g.Cells[i]
		ratio := WCAGContrast(c.FG.R, c.FG.G, c.FG.B, c.BG.R, c.BG.G, c.BG.B)
		if ratio < min {
			min = ratio
		}
		sum += ratio
		if ratio < WCAGThreshold {
			below++
		}
	}
	return WCAGStats{
		Min:            min,
		Mean:           sum / float64(n),
		BelowThreshold: below,
	}
}

// LaplacianVariance returns the variance of the discrete Laplacian over the
// BT.601 luma of img, a no-reference sharpness co-signal. A sharper crop has
// stronger high-frequency edges, so its Laplacian response varies more; a
// blurred crop flattens the response and lowers the variance. The 4-neighbour
// kernel is applied at interior pixels; an image smaller than 3x3 in either
// axis has no interior and returns 0. Channel order is RGBA.
func LaplacianVariance(img *image.RGBA) float64 {
	plane, w, h := luma(img)
	if w < 3 || h < 3 {
		return 0
	}
	resp := make([]float64, 0, (w-2)*(h-2))
	for y := 1; y < h-1; y++ {
		for x := 1; x < w-1; x++ {
			c := plane[y*w+x]
			lap := plane[y*w+(x-1)] + plane[y*w+(x+1)] +
				plane[(y-1)*w+x] + plane[(y+1)*w+x] - 4*c
			resp = append(resp, lap)
		}
	}
	return variance(resp)
}

// Crop returns the sub-image of img bounded by rect, intersected with the image
// bounds, as a fresh *image.RGBA in the cropped coordinate space. It is the
// text-region selector for LaplacianVariance: pass the bounds of the cells that
// hold text. An empty intersection yields a zero-size image. Channel order is
// RGBA.
func Crop(img *image.RGBA, rect image.Rectangle) *image.RGBA {
	r := rect.Intersect(img.Bounds())
	out := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	for y := 0; y < r.Dy(); y++ {
		for x := 0; x < r.Dx(); x++ {
			out.SetRGBA(x, y, img.RGBAAt(r.Min.X+x, r.Min.Y+y))
		}
	}
	return out
}

// PixelContrast derives a WCAG contrast ratio from the decoded pixels of a
// gridless render (sixel, kitty). It splits the render's luminance into two
// clusters by an Otsu threshold, takes each cluster's mean colour as a
// representative foreground and background, and returns the WCAG contrast of that
// pair. It is the pixel-derived contrast co-signal the per-mode matrix wants for
// the gridless modes, where no owned cell grid carries per-cell foreground and
// background colours. An empty image, or one whose pixels do not split, returns a
// 1:1 ratio. Channel order is RGBA.
func PixelContrast(img *image.RGBA) float64 {
	b := img.Bounds()
	if b.Empty() {
		return 1
	}
	// Build the 8-bit luminance histogram and find the Otsu split.
	var hist [256]int
	var total int
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := img.RGBAAt(x, y)
			hist[lumaByte(c.R, c.G, c.B)]++
			total++
		}
	}
	if total == 0 {
		return 1
	}
	t := otsuSplit(hist[:], total)

	// Accumulate each cluster's mean colour: below the threshold is the darker
	// set, at or above is the lighter set.
	var loR, loG, loB, loN float64
	var hiR, hiG, hiB, hiN float64
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := img.RGBAAt(x, y)
			if int(lumaByte(c.R, c.G, c.B)) < t {
				loR, loG, loB, loN = loR+float64(c.R), loG+float64(c.G), loB+float64(c.B), loN+1
			} else {
				hiR, hiG, hiB, hiN = hiR+float64(c.R), hiG+float64(c.G), hiB+float64(c.B), hiN+1
			}
		}
	}
	if loN == 0 || hiN == 0 {
		return 1
	}
	return WCAGContrast(
		meanByte(loR, loN), meanByte(loG, loN), meanByte(loB, loN),
		meanByte(hiR, hiN), meanByte(hiG, hiN), meanByte(hiB, hiN),
	)
}

// lumaByte returns the BT.601 luma of an 8-bit RGB triple, rounded to a byte. It
// feeds the Otsu histogram for the pixel-derived contrast split.
func lumaByte(r, g, b uint8) uint8 {
	y := 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)
	return uint8(y + 0.5)
}

// meanByte returns sum/count rounded and clamped to an 8-bit value.
func meanByte(sum, count float64) uint8 {
	v := sum/count + 0.5
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

// otsuSplit returns the Otsu luminance threshold for a 256-bin histogram: the
// level that maximises the between-class variance, splitting the pixels into a
// dark and a light cluster. total is the pixel count. It returns 0 when the
// histogram has no separable mass, which the caller treats as no split.
func otsuSplit(hist []int, total int) int {
	var sum float64
	for i, n := range hist {
		sum += float64(i) * float64(n)
	}
	var sumB, wB float64
	var best float64
	bestT := 0
	for t := 0; t < 256; t++ {
		wB += float64(hist[t])
		if wB == 0 {
			continue
		}
		wF := float64(total) - wB
		if wF == 0 {
			break
		}
		sumB += float64(t) * float64(hist[t])
		mB := sumB / wB
		mF := (sum - sumB) / wF
		between := wB * wF * (mB - mF) * (mB - mF)
		if between > best {
			best = between
			bestT = t
		}
	}
	// The split lands at the level above the best threshold, so the darker set is
	// strictly below it.
	return bestT + 1
}

// variance returns the population variance of values. An empty slice returns 0.
func variance(values []float64) float64 {
	n := len(values)
	if n == 0 {
		return 0
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	mean := sum / float64(n)
	var ss float64
	for _, v := range values {
		d := v - mean
		ss += d * d
	}
	return ss / float64(n)
}
