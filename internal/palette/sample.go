package palette

import (
	"image"
	"image/color"

	"github.com/soniakeys/quant/median"
)

// SamplePalette quantises a fresh palette of at most maxColours entries from
// the frame, allocating both the palette and any downsample scratch it needs.
// It retains nothing; SamplePaletteInto is the allocation-free form.
func SamplePalette(rgba *image.RGBA, maxColours int) color.Palette {
	palette, _ := SamplePaletteInto(rgba, maxColours, nil, nil)
	return palette
}

// SamplePaletteInto quantises a fresh palette of at most maxColours entries
// into dst and returns it with the (possibly reallocated) downsample scratch.
func SamplePaletteInto(rgba *image.RGBA, maxColours int, dst color.Palette, sample *image.RGBA) (color.Palette, *image.RGBA) {
	// The quantiser reads cap, not a count argument, so the capacity must equal
	// the limit rather than merely reach it. A wider destination would otherwise
	// quantise to its own capacity and return more than maxColours entries. This
	// is the rule Engine.Requantise applies to the retained palette.
	if cap(dst) != maxColours {
		dst = make(color.Palette, 0, maxColours)
	}
	src, sample := SampleInto(rgba, sample)
	return median.Quantizer(0).Quantize(dst[:0], src), sample
}

// Sample builds this frame's palette sample and retains the downsample
// scratch. The sample feeds both the drift test and re-quantisation, so it is
// built every frame regardless of whether the retained palette is reused.
func (e *Engine) Sample(rgba *image.RGBA) *image.RGBA {
	src, sample := SampleInto(rgba, e.sample)
	e.sample = sample
	return src
}

// SampleInto returns the image the quantiser and drift test read: the
// frame itself when it is within the 64K pixel budget, otherwise a fixed-phase
// strided downsample written into the reusable sample scratch. It also returns
// the (possibly reallocated) scratch.
func SampleInto(rgba, sample *image.RGBA) (*image.RGBA, *image.RGBA) {
	const budget = 1 << 16
	bounds := rgba.Bounds()
	if bounds.Dx()*bounds.Dy() <= budget {
		return rgba, sample
	}
	step := 2
	width, height := max(1, bounds.Dx()/step), max(1, bounds.Dy()/step)
	for width > budget/height {
		step++
		width, height = max(1, bounds.Dx()/step), max(1, bounds.Dy()/step)
	}
	sampleBounds := image.Rect(0, 0, width, height)
	if sample == nil || sample.Bounds() != sampleBounds {
		sample = image.NewRGBA(sampleBounds)
	}
	for y := range height {
		dstOffset := sample.PixOffset(0, y)
		for x := range width {
			srcOffset := rgba.PixOffset(bounds.Min.X+x*step, bounds.Min.Y+y*step)
			copy(sample.Pix[dstOffset:dstOffset+4], rgba.Pix[srcOffset:srcOffset+4])
			dstOffset += 4
		}
	}
	return sample, sample
}
