package ocr

import (
	"image"
	"image/color"

	xdraw "golang.org/x/image/draw"
)

// Conditioning holds the fixed OCR conditioning parameters. The Stage 0 path is
// fixed, not tuned per fixture: upscale nearest-neighbour to land capital height
// in Tesseract's sweet spot, greyscale, force dark-on-light, Otsu binarise, then
// pad with a light quiet zone. The same values run on every fixture so the CER
// numbers track regressions, not parameter drift.
type Conditioning struct {
	// TargetCapHeight is the capital height Tesseract reads most reliably,
	// 30-33 px. Conditioning picks an integer upscale that lands the source
	// capital height nearest this value.
	TargetCapHeight int
	// MinScale and MaxScale bound the integer upscale factor (2-4x).
	MinScale, MaxScale int
	// Pad is the light quiet-zone border added on every side, in upscaled pixels.
	Pad int
}

// DefaultConditioning is the fixed Stage 0 conditioning: target capital height
// 32 px, integer upscale 2-4x, 16 px pad.
func DefaultConditioning() Conditioning {
	return Conditioning{
		TargetCapHeight: 32,
		MinScale:        2,
		MaxScale:        4,
		Pad:             16,
	}
}

// Scale picks the integer upscale factor in [MinScale, MaxScale] that lands a
// srcCapHeight-pixel capital nearest TargetCapHeight. A non-positive
// srcCapHeight returns MaxScale.
func (c Conditioning) Scale(srcCapHeight int) int {
	if srcCapHeight <= 0 {
		return c.MaxScale
	}
	best := c.MinScale
	bestErr := -1
	for s := c.MinScale; s <= c.MaxScale; s++ {
		e := srcCapHeight*s - c.TargetCapHeight
		if e < 0 {
			e = -e
		}
		if bestErr < 0 || e < bestErr {
			bestErr = e
			best = s
		}
	}
	return best
}

// Condition runs the fixed conditioning path on src and returns a binarised,
// dark-on-light, padded grey image ready for OCR. srcCapHeight is the capital
// height of the source text in source pixels; it drives the integer upscale.
// The result is an *image.Gray in RGBA-derived BT.601 luma; output values are 0
// (ink) or 255 (paper).
func Condition(src image.Image, srcCapHeight int, c Conditioning) *image.Gray {
	scale := c.Scale(srcCapHeight)

	sb := src.Bounds()
	upW := sb.Dx() * scale
	upH := sb.Dy() * scale

	// Greyscale by BT.601 luma at source resolution.
	grey := toGray(src)

	// Nearest-neighbour upscale, no resampling artefacts that cost CER.
	up := image.NewGray(image.Rect(0, 0, upW, upH))
	xdraw.NearestNeighbor.Scale(up, up.Bounds(), grey, grey.Bounds(), xdraw.Over, nil)

	// Otsu threshold on the upscaled luma.
	t := otsuThreshold(up)

	// Force dark-on-light: if the image is mostly dark, invert so ink is dark on
	// a light ground, which Tesseract expects.
	invert := meanGray(up) < 128

	// Binarise and pad with a light quiet zone.
	out := image.NewGray(image.Rect(0, 0, upW+2*c.Pad, upH+2*c.Pad))
	for i := range out.Pix {
		out.Pix[i] = 255
	}
	for y := 0; y < upH; y++ {
		for x := 0; x < upW; x++ {
			v := up.GrayAt(x, y).Y
			dark := v <= t
			if invert {
				dark = !dark
			}
			px := byte(255)
			if dark {
				px = 0
			}
			out.SetGray(x+c.Pad, y+c.Pad, color.Gray{Y: px})
		}
	}
	return out
}

// toGray converts an image to BT.601 luma. RGBA channel order holds: the red,
// green, and blue weights apply to the R, G, and B accessors in that order.
func toGray(src image.Image) *image.Gray {
	b := src.Bounds()
	g := image.NewGray(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, gg, bb, _ := src.At(x, y).RGBA()
			// RGBA() returns 16-bit channels; BT.601 luma, then narrow to 8 bit.
			l := (299*int(r>>8) + 587*int(gg>>8) + 114*int(bb>>8)) / 1000
			g.SetGray(x, y, color.Gray{Y: clampByte(l)})
		}
	}
	return g
}

// clampByte clamps v to the [0, 255] range and returns it as a uint8. BT.601
// luma already lands within that range, so the clamp never changes a valid
// result; it makes the conversion provably safe.
func clampByte(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

// meanGray returns the mean luma of a grey image.
func meanGray(g *image.Gray) int {
	if len(g.Pix) == 0 {
		return 0
	}
	var sum int
	for _, v := range g.Pix {
		sum += int(v)
	}
	return sum / len(g.Pix)
}

// otsuThreshold computes the Otsu binarisation threshold of a grey image: the
// grey level that maximises between-class variance over the luma histogram.
func otsuThreshold(g *image.Gray) uint8 {
	var hist [256]int
	for _, v := range g.Pix {
		hist[v]++
	}
	total := len(g.Pix)
	if total == 0 {
		return 128
	}

	var sum float64
	for i := 0; i < 256; i++ {
		sum += float64(i) * float64(hist[i])
	}

	var sumB, wB float64
	var maxVar float64
	var threshold int
	for i := 0; i < 256; i++ {
		wB += float64(hist[i])
		if wB == 0 {
			continue
		}
		wF := float64(total) - wB
		if wF == 0 {
			break
		}
		sumB += float64(i) * float64(hist[i])
		mB := sumB / wB
		mF := (sum - sumB) / wF
		between := wB * wF * (mB - mF) * (mB - mF)
		if between > maxVar {
			maxVar = between
			threshold = i
		}
	}
	return uint8(threshold)
}
