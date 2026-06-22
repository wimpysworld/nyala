// The ideal-block oracle and absolute/relative scoring.
//
// Each quality metric is scored twice: absolute against the downscaled input,
// and relative against an ideal block render. The ideal render is the best a
// block backend could do per cell, so the relative score isolates the loss the
// real renderer adds over that ceiling, separate from the loss the block format
// imposes on any renderer.
//
// # Per-cell algorithm (decision gate)
//
// For every cell the oracle reads the same 2x4 source block the quadrant scheme
// uses (two columns, four rows, matching block's cell footprint). It splits the
// four 2x2 quadrant sub-cells into a foreground set and a background set by an
// Otsu luma threshold over the four sub-cell luma values. Otsu over four values
// picks the split point (among the sorted lumas) that maximises between-class
// variance; on 1-D data this is the 2-means optimum. The fg and bg colours are
// the linear-light mean of their set (averaged in linear RGB, converted back to
// sRGB). The quadrant glyph is the rune whose 4-bit above/below-threshold mask
// matches that same split.
//
// The choice is joint, not separate: the fg/bg colours and the quadrant glyph
// both derive from one shared luma mask. A separate glyph choice could pick a
// rune whose foreground pixels disagree with the cells the fg colour was
// averaged from, so the rasterised cell would not match its own colour pick.
// Sharing the mask keeps the ideal cell self-consistent and is the tightest
// honest ceiling for the quadrant family the block backend renders.
//
// # Linear-light cell-mean floor
//
// The floor is a second, weaker oracle: each cell is one flat colour, the mean
// of its 2x4 block taken in linear RGB (never sRGB) and converted back, with a
// space glyph and no structure. It is the lower bound a block renderer that drew
// no glyph could reach, so a real render scoring near the floor gained nothing
// from glyph structure.
//
// # Best-of-small-shift positional floor
//
// Cell boundaries fall on a half-cell grid the source never aligns to. The
// best-of-small-shift variant shifts the source by {-1, 0, +1} cells on each
// axis, builds the ideal render for each shift, scores it, and keeps the best
// score per metric. The (0, 0) shift is always a candidate, so the result never
// scores worse than the no-shift case.
//
// Channel order is RGBA throughout.

package metrics

import (
	"image"
	"image/color"

	colorful "github.com/lucasb-eyer/go-colorful"

	"github.com/wimpysworld/nyala/internal/capture"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// oracleCellW and oracleCellH are the source-pixel footprint of one ideal cell:
// two columns by four rows, matching the quadrant scheme's 2x4 block so the
// oracle reads the same source region the real block renderer does.
const (
	oracleCellW = 2
	oracleCellH = 4
)

// quadrantGlyphForMask maps a 4-bit foreground mask (bit3 top-left, bit2
// top-right, bit1 bottom-left, bit0 bottom-right) to the quadrant block rune
// whose filled quadrants match the foreground set. It is the full distinct
// quadrant set, the best-achievable quadrant cell, so the oracle measures a
// full-quadrant ideal ceiling. The block backend now uses the same
// full-quadrant mapping, but splits on a luma-midpoint mask rather than the
// Otsu split this oracle uses, so the two can pick different masks for the same
// cell. So these are not always the exact runes the block renderer emits; they
// are the ideal ceiling those renders are scored against.
// Bit layout: bit3 top-left, bit2 top-right, bit1 bottom-left, bit0
// bottom-right. Each entry is the quadrant block rune whose filled quadrants
// are exactly the set bits.
var quadrantGlyphForMask = [16]rune{
	0x0020, // 0000 none filled: space (flat background)
	0x2597, // 0001 bottom-right
	0x2596, // 0010 bottom-left
	0x2584, // 0011 bottom-left + bottom-right: lower half
	0x259D, // 0100 top-right
	0x2590, // 0101 top-right + bottom-right: right half
	0x259E, // 0110 top-right + bottom-left: upper-right and lower-left
	0x259F, // 0111 all but top-left
	0x2598, // 1000 top-left
	0x259A, // 1001 top-left + bottom-right: upper-left and lower-right
	0x258C, // 1010 top-left + bottom-left: left half
	0x2599, // 1011 all but top-right
	0x2580, // 1100 top-left + top-right: upper half
	0x259C, // 1101 all but bottom-left
	0x259B, // 1110 all but bottom-right
	0x2588, // 1111 full block
}

// IdealGrid builds the ideal block grid for a source image at the given cell
// dimensions. Each cell is the per-cell optimum: an Otsu luma split of its 2x4
// source block into foreground and background, linear-light mean colours per
// set, and the quadrant glyph that matches the split. It is the tightest block
// render a quadrant backend could produce, the ceiling the relative scores
// measure against. Channel order is RGBA.
func IdealGrid(src *image.RGBA, cols, rows int) block.Grid {
	g := block.Grid{Cols: cols, Rows: rows, Cells: make([]block.Cell, cols*rows)}
	for row := 0; row < rows; row++ {
		for col := 0; col < cols; col++ {
			*g.At(col, row) = idealCell(src, col, row, 0, 0)
		}
	}
	return g
}

// FloorGrid builds the linear-light cell-mean floor grid for a source image:
// each cell is one flat colour, the mean of its 2x4 source block taken in
// linear RGB, drawn with a space glyph and no structure. It is the lower bound a
// block renderer that drew no glyph could reach. Channel order is RGBA.
func FloorGrid(src *image.RGBA, cols, rows int) block.Grid {
	g := block.Grid{Cols: cols, Rows: rows, Cells: make([]block.Cell, cols*rows)}
	for row := 0; row < rows; row++ {
		for col := 0; col < cols; col++ {
			mean := linearMeanBlock(src, col*oracleCellW, row*oracleCellH, oracleCellW, oracleCellH)
			*g.At(col, row) = block.Cell{Glyph: ' ', FG: mean, BG: mean}
		}
	}
	return g
}

// idealCell computes the ideal cell at (col, row), reading the 2x4 source block
// offset by (shiftX, shiftY) source pixels. It splits the four quadrant
// sub-cells by an Otsu luma threshold, sets fg and bg to the linear-light mean
// of each set, and picks the quadrant glyph matching the split.
func idealCell(src *image.RGBA, col, row, shiftX, shiftY int) block.Cell {
	x0 := col*oracleCellW + shiftX
	y0 := row*oracleCellH + shiftY

	// Four quadrant sub-cells: each one source column by two source rows.
	q := [4]color.RGBA{
		linearMeanBlock(src, x0, y0, 1, 2),     // top-left
		linearMeanBlock(src, x0+1, y0, 1, 2),   // top-right
		linearMeanBlock(src, x0, y0+2, 1, 2),   // bottom-left
		linearMeanBlock(src, x0+1, y0+2, 1, 2), // bottom-right
	}
	var l [4]float64
	for i, c := range q {
		l[i] = luma601(c)
	}

	mask := otsuMask4(l)
	glyph := quadrantGlyphForMask[mask]

	// Foreground is the set above the threshold (mask bit 1); background the
	// rest. An all-or-none mask makes fg and bg the same flat block mean.
	fg := linearMeanColours(q, mask, true)
	bg := linearMeanColours(q, mask, false)
	return block.Cell{Glyph: glyph, FG: fg, BG: bg}
}

// otsuMask4 returns the 4-bit foreground mask for four luma values. It chooses
// the threshold, among the sorted distinct lumas, that maximises between-class
// variance (Otsu); the foreground set is the values strictly above that
// threshold. With four values this equals the 2-means optimum on the lumas.
// Equal lumas yield an all-background mask (0), the flat-cell case. Bit order is
// top-left bit3, top-right bit2, bottom-left bit1, bottom-right bit0.
func otsuMask4(l [4]float64) int {
	// Candidate thresholds are the distinct luma values; foreground is "> t".
	// Try each and keep the split with the largest between-class variance.
	bestVar := -1.0
	bestT := l[0]
	allEqual := true
	for i := 1; i < 4; i++ {
		if l[i] != l[0] {
			allEqual = false
		}
	}
	if allEqual {
		return 0
	}
	for _, t := range l {
		var sumHi, sumLo float64
		var nHi, nLo int
		for _, v := range l {
			if v > t {
				sumHi += v
				nHi++
			} else {
				sumLo += v
				nLo++
			}
		}
		if nHi == 0 || nLo == 0 {
			continue
		}
		meanHi := sumHi / float64(nHi)
		meanLo := sumLo / float64(nLo)
		d := meanHi - meanLo
		// Between-class variance is w_hi * w_lo * (meanHi - meanLo)^2; the 1/16
		// weight normaliser is constant across thresholds, so drop it.
		v := float64(nHi) * float64(nLo) * d * d
		if v > bestVar {
			bestVar = v
			bestT = t
		}
	}
	mask := 0
	for i, v := range l {
		if v > bestT {
			mask |= 1 << (3 - i)
		}
	}
	return mask
}

// linearMeanColours returns the linear-light mean of the quadrant sub-cells
// selected by the mask. When wantForeground is true it averages the sub-cells
// whose mask bit is set; otherwise the unset ones. An empty selection returns
// the mean of all four sub-cells, so a flat cell yields one consistent colour.
func linearMeanColours(q [4]color.RGBA, mask int, wantForeground bool) color.RGBA {
	var rl, gl, bl float64
	var n int
	for i, c := range q {
		set := mask&(1<<(3-i)) != 0
		if set != wantForeground {
			continue
		}
		r, g, b := linearRGB(c)
		rl += r
		gl += g
		bl += b
		n++
	}
	if n == 0 {
		// Empty set: fall back to the mean of all four sub-cells.
		for _, c := range q {
			r, g, b := linearRGB(c)
			rl += r
			gl += g
			bl += b
			n++
		}
	}
	return linearToRGBA(rl/float64(n), gl/float64(n), bl/float64(n))
}

// linearRGB converts an 8-bit sRGB RGBA colour to its linear-RGB triple via
// go-colorful, so per-set means stay correct in light rather than in the
// non-linear sRGB encoding. The alpha channel is ignored. Channel order is RGBA.
func linearRGB(c color.RGBA) (r, g, b float64) {
	cf := colorful.Color{R: float64(c.R) / 255, G: float64(c.G) / 255, B: float64(c.B) / 255}
	return cf.LinearRgb()
}

// linearMeanBlock averages a w x h source region anchored at (x0, y0) in linear
// RGB and returns the sRGB result. It converts each source pixel to linear RGB,
// means the channels, then converts back, so the mean is correct in light
// rather than in the non-linear sRGB encoding. Out-of-bound or nil pixels
// contribute opaque black. Channel order is RGBA.
func linearMeanBlock(img *image.RGBA, x0, y0, w, h int) color.RGBA {
	var rl, gl, bl float64
	n := w * h
	for dy := 0; dy < h; dy++ {
		for dx := 0; dx < w; dx++ {
			r, g, b := srcPixel(img, x0+dx, y0+dy)
			lr, lg, lb := linearRGB(color.RGBA{R: r, G: g, B: b, A: 0xff})
			rl += lr
			gl += lg
			bl += lb
		}
	}
	return linearToRGBA(rl/float64(n), gl/float64(n), bl/float64(n))
}

// linearToRGBA converts a linear-RGB triple back to an opaque 8-bit sRGB RGBA
// colour, clamping each channel to 0..1 before encoding. Channel order is RGBA.
func linearToRGBA(r, g, b float64) color.RGBA {
	cf := colorful.LinearRgb(clamp01(r), clamp01(g), clamp01(b)).Clamped()
	rr, gg, bb := cf.RGB255()
	return color.RGBA{R: rr, G: gg, B: bb, A: 0xff}
}

// clamp01 clamps a value to the 0..1 range.
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// luma601 returns the BT.601 luma of an RGBA colour (0.299 R, 0.587 G, 0.114 B)
// in 0..255. It matches the luma the block compositor splits on, so the oracle
// and the real renderer divide a cell the same way. Channel order is RGBA.
func luma601(c color.RGBA) float64 {
	return lumaBT601(c.R, c.G, c.B)
}

// srcPixel reads the R, G, B channels at (x, y). Pixels outside the image or a
// nil image read as opaque black. Channel order is RGBA.
func srcPixel(img *image.RGBA, x, y int) (r, g, b uint8) {
	if img == nil {
		return 0, 0, 0
	}
	if !(image.Point{X: x, Y: y}).In(img.Bounds()) {
		return 0, 0, 0
	}
	c := img.RGBAAt(x, y)
	return c.R, c.G, c.B
}

// IdealRender rasterises the ideal block grid for a source image to an
// *image.RGBA at the Route A cell size, so it can be scored against a real
// render in the same pixel geometry. The cols and rows are the render grid; the
// source should already be sized so each cell maps to a 2x4 block (for example
// cols*2 by rows*4). Channel order is RGBA.
func IdealRender(rast *capture.Rasteriser, src *image.RGBA, cols, rows int) *image.RGBA {
	return rast.Rasterise(IdealGrid(src, cols, rows))
}

// MetricSet is the quality scores for one image pair: the structural and colour
// headline metrics this package emits. Lower is better for the error metrics
// (MSE, GMSD, DeltaEMean, DeltaEP95); higher is better for the similarity
// metrics (SSIM, MSSSIM, PSNR, HistIntersection).
type MetricSet struct {
	// MSE is the mean squared error on BT.601 luma. Lower is better; 0 is best.
	MSE float64
	// PSNR is the peak signal-to-noise ratio in dB. Higher is better.
	PSNR float64
	// GMSD is the gradient magnitude similarity deviation. Lower is better.
	GMSD float64
	// SSIM is the structural similarity index in 0..1. Higher is better.
	SSIM float64
	// MSSSIM is the multiscale structural similarity index in 0..1. Higher is
	// better.
	MSSSIM float64
	// DeltaEMean is the mean per-pixel CIEDE2000 dE. Lower is better.
	DeltaEMean float64
	// DeltaEP95 is the 95th-percentile per-pixel CIEDE2000 dE. Lower is better.
	DeltaEP95 float64
	// HistIntersection is the colour-histogram intersection in 0..1. Higher is
	// better.
	HistIntersection float64
}

// metricDir records one MetricSet field's optimisation direction together with
// its accessor and setter, so the per-field "higher-better or lower-better?"
// fact lives in one place rather than restated in bestOf's min/max calls. It is
// the single source of truth the best-of-shift reduction reads.
type metricDir struct {
	// higherIsBetter is true for a similarity metric (SSIM, MSSSIM, PSNR,
	// HistIntersection) and false for an error metric (MSE, GMSD, DeltaEMean,
	// DeltaEP95).
	higherIsBetter bool
	// get reads the field from a MetricSet; set writes it into one.
	get func(MetricSet) float64
	set func(*MetricSet, float64)
}

// metricDirs lists every MetricSet field with its direction. bestOf reduces over
// it, so the direction knowledge cannot drift from this one table.
var metricDirs = []metricDir{
	{false, func(m MetricSet) float64 { return m.MSE }, func(m *MetricSet, v float64) { m.MSE = v }},
	{true, func(m MetricSet) float64 { return m.PSNR }, func(m *MetricSet, v float64) { m.PSNR = v }},
	{false, func(m MetricSet) float64 { return m.GMSD }, func(m *MetricSet, v float64) { m.GMSD = v }},
	{true, func(m MetricSet) float64 { return m.SSIM }, func(m *MetricSet, v float64) { m.SSIM = v }},
	{true, func(m MetricSet) float64 { return m.MSSSIM }, func(m *MetricSet, v float64) { m.MSSSIM = v }},
	{false, func(m MetricSet) float64 { return m.DeltaEMean }, func(m *MetricSet, v float64) { m.DeltaEMean = v }},
	{false, func(m MetricSet) float64 { return m.DeltaEP95 }, func(m *MetricSet, v float64) { m.DeltaEP95 = v }},
	{true, func(m MetricSet) float64 { return m.HistIntersection }, func(m *MetricSet, v float64) { m.HistIntersection = v }},
}

// ScorePair computes the full MetricSet between a render and a reference of the
// same dimensions. It is the single scoring path the absolute and relative
// scores both call, so the two scores stay comparable. Channel order is RGBA.
func ScorePair(render, reference *image.RGBA) MetricSet {
	msssim, _ := MSSSIM(render, reference)
	de := CIEDE2000(render, reference)
	hist := Histogram(render, reference)
	return MetricSet{
		MSE:              MSE(render, reference),
		PSNR:             PSNR(render, reference),
		GMSD:             GMSD(render, reference),
		SSIM:             SSIM(render, reference),
		MSSSIM:           msssim,
		DeltaEMean:       de.Mean,
		DeltaEP95:        de.P95,
		HistIntersection: hist.Intersection,
	}
}

// Scores holds a quality metric scored twice: absolute against the downscaled
// input, and relative against the ideal block render. The relative score
// isolates the loss the real renderer adds over the ideal block ceiling. For a
// lossless backend the ideal render equals the input, so the two scores match.
type Scores struct {
	// Absolute scores the render against the downscaled input image.
	Absolute MetricSet
	// Relative scores the render against the ideal block render.
	Relative MetricSet
}

// Score builds the ideal render for the source and scores a real render twice:
// absolute against the input downscaled to the render size, and relative
// against the ideal render. The render, the downscaled input, and the ideal
// render all share the render's dimensions. cols and rows are the block grid;
// the source must be sized so each cell maps to a 2x4 block. Channel order is
// RGBA.
func Score(rast *capture.Rasteriser, render, src *image.RGBA, cols, rows int) Scores {
	b := render.Bounds()
	w, h := b.Dx(), b.Dy()
	input := Downscale(src, w, h)
	ideal := IdealRender(rast, src, cols, rows)
	return Scores{
		Absolute: ScorePair(render, input),
		Relative: ScorePair(render, ideal),
	}
}

// shiftOffsets are the per-axis cell shifts the positional floor searches: one
// cell each way and no shift. (0, 0) is always present, so the best-of-shift
// result never scores worse than the no-shift ideal render.
var shiftOffsets = []int{-1, 0, 1}

// BestShiftRelative scores a render against the ideal render at the best of a
// small positional shift (plus or minus one cell on each axis) and returns the
// best MetricSet per metric. The (0, 0) shift is a candidate, so each returned
// metric is at least as good as the no-shift relative score: error metrics are
// minimised, similarity metrics maximised. It is the half-cell positional floor
// the relative score should not be penalised below. Channel order is RGBA.
func BestShiftRelative(rast *capture.Rasteriser, render, src *image.RGBA, cols, rows int) MetricSet {
	b := render.Bounds()
	w, h := b.Dx(), b.Dy()

	var best MetricSet
	first := true
	for _, sy := range shiftOffsets {
		for _, sx := range shiftOffsets {
			ideal := rast.Rasterise(shiftedIdealGrid(src, cols, rows, sx*oracleCellW, sy*oracleCellH))
			// Guard against a degenerate raster size mismatch; the ideal render
			// is always cols*CellPxW by rows*CellPxH, matching a render built
			// from the same grid, so this should hold for valid inputs.
			if ideal.Bounds().Dx() != w || ideal.Bounds().Dy() != h {
				continue
			}
			ms := ScorePair(render, ideal)
			if first {
				best = ms
				first = false
				continue
			}
			best = bestOf(best, ms)
		}
	}
	return best
}

// shiftedIdealGrid builds the ideal grid with the source read at a pixel shift,
// for the positional-floor search. The shift moves the 2x4 sampling window so a
// cell boundary that fell mid-feature can land on it.
func shiftedIdealGrid(src *image.RGBA, cols, rows, shiftX, shiftY int) block.Grid {
	g := block.Grid{Cols: cols, Rows: rows, Cells: make([]block.Cell, cols*rows)}
	for row := 0; row < rows; row++ {
		for col := 0; col < cols; col++ {
			*g.At(col, row) = idealCell(src, col, row, shiftX, shiftY)
		}
	}
	return g
}

// bestOf returns the per-metric best of two MetricSets: the smaller value for
// the error metrics and the larger for the similarity metrics. It reduces over
// the metricDirs direction table, so the shift search keeps the best of each
// metric independently and the direction lives in one place.
func bestOf(a, b MetricSet) MetricSet {
	var out MetricSet
	for _, d := range metricDirs {
		av, bv := d.get(a), d.get(b)
		if d.higherIsBetter {
			d.set(&out, max(av, bv))
		} else {
			d.set(&out, min(av, bv))
		}
	}
	return out
}
