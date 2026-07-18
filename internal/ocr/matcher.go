package ocr

import (
	"image"
	"image/color"

	"github.com/rivo/uniseg"

	"github.com/wimpysworld/nyala/internal/cellraster"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// Matcher is the exact-template nearest-glyph matcher. It reads the crisp text
// overlay Nyala rasterised by comparing each text cell against per-grapheme
// templates rendered through the same font face the rasteriser uses, picking the
// nearest by binarised pixel distance. Its scope is overlay placement and font
// coverage: a near-zero CER proves each run landed in the right cells with the
// right glyphs, and a block glyph recovered where a letter was expected flags a
// cell where the overlay failed to apply.
//
// The candidate set is fixed and grid-independent: the distinct graphemes of the
// run text plus the block glyph set. That set is every glyph binarizeQuadrant can
// emit: the ten quadrant glyphs U+2596..U+259F, the half blocks U+2580 upper,
// U+2584 lower, U+258C left, U+2590 right, the full block U+2588, and space. The
// fixed block glyphs let the matcher catch a block showing through where the
// overlay should have won.
//
// Matcher is pure Go and carries no build tag, so it builds and unit-tests under
// the routine gate. Only the gosseract engine in tesseract.go needs cgo.
type Matcher struct {
	candidates []candidate
}

// candidate is one matchable grapheme and its binarised template, a bit per cell
// pixel set when the rendered glyph pixel falls in the brighter Otsu class.
// Templates render white-on-black, so the set bits mark the glyph ink.
type candidate struct {
	text     string
	template []bool
}

// blockGlyphs is the fixed block glyph set the matcher always considers: every
// glyph binarizeQuadrant can emit. That is the ten quadrant glyphs
// U+2596..U+259F, the upper-half-block U+2580, the lower-half-block U+2584, the
// left-half-block U+258C, the right-half-block U+2590, the full block U+2588, and
// the space. They let the matcher recover a block cell that showed through where
// the overlay should have placed a grapheme.
var blockGlyphs = []string{
	"▖", "▗", "▘", "▙", "▚",
	"▛", "▜", "▝", "▞", "▟",
	"▀", "▄", "▌", "▐", "█", " ",
}

// matchFG and matchBG are the fixed colours the matcher renders every template
// with: opaque white foreground on opaque black background. The matcher
// binarises each cell by an adaptive Otsu split and compares polarity-invariantly
// (template and its inverse), so the absolute template colours do not matter and
// a dark-on-light cell matches a white-on-black template. Fixing them keeps the
// templates deterministic and independent of any run's foreground colour.
var (
	matchFG = color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}
	matchBG = color.RGBA{A: 0xFF}
)

// NewMatcher builds a matcher whose candidate set is the distinct graphemes of
// runTexts plus the fixed block glyph set. It renders each candidate to a single
// cell through rast.RenderGlyphCell, binarises it by an adaptive Otsu split over
// luma, and stores the bit template. The templates render through the same face
// path the rasteriser
// draws cells with, so a matched cell compares against a like-for-like glyph.
func NewMatcher(rast *cellraster.Rasteriser, runTexts []string) *Matcher {
	seen := make(map[string]bool)
	var texts []string
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		texts = append(texts, s)
	}
	for _, run := range runTexts {
		gr := uniseg.NewGraphemes(run)
		for gr.Next() {
			add(gr.Str())
		}
	}
	for _, b := range blockGlyphs {
		add(b)
	}

	cands := make([]candidate, 0, len(texts))
	for _, s := range texts {
		tile := rast.RenderGlyphCell(s, matchFG, matchBG)
		cands = append(cands, candidate{text: s, template: binarise(tile)})
	}
	return &Matcher{candidates: cands}
}

// Match binarises cell by an adaptive Otsu split and returns the candidate
// grapheme whose template is the nearest by Hamming distance. The match is
// polarity-invariant: each cell compares against every template and its inverse,
// taking the smaller distance, so a dark-on-light cell and a light-on-dark cell
// both recover the same glyph. An empty candidate set returns the empty string.
func (m *Matcher) Match(cell *image.RGBA) string {
	if len(m.candidates) == 0 {
		return ""
	}
	got := binarise(cell)
	best := m.candidates[0]
	bestDist := polarityDistance(got, best.template)
	for _, c := range m.candidates[1:] {
		if d := polarityDistance(got, c.template); d < bestDist {
			bestDist = d
			best = c
		}
	}
	return best.text
}

// polarityDistance returns the smaller of the Hamming distance between got and
// template and the distance to template's inverse. The template renders
// white-on-black, so a light-on-dark cell matches it directly and a dark-on-light
// cell matches its inverse; taking the minimum makes the match polarity-agnostic.
func polarityDistance(got, template []bool) int {
	direct := hamming(got, template)
	inv := len(got) - direct
	if inv < direct {
		return inv
	}
	return direct
}

// Recover walks the overlay text cells of g, crops each from img, matches it,
// and joins the recovered graphemes in placement order (row-major, run order
// follows cell order). It skips bitmap cells and wide trailing cells: a wide
// trailing cell carries no glyph of its own. The result is the recovered text
// for CER scoring against the run truth.
func (m *Matcher) Recover(img *image.RGBA, g block.Grid) string {
	var out []byte
	for row := range g.Rows {
		for col := range g.Cols {
			c := g.At(col, row)
			if c.Text == "" || c.Wide {
				continue
			}
			cell := cropCell(img, col, row)
			out = append(out, m.Match(cell)...)
		}
	}
	return string(out)
}

// cropCell returns the CellPxW x CellPxH tile of img at cell (col, row) as a
// fresh *image.RGBA in cell-local coordinates. Pixels outside img read as
// transparent zero, matching an empty cell.
func cropCell(img *image.RGBA, col, row int) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, cellraster.CellPxW, cellraster.CellPxH))
	x0 := col * cellraster.CellPxW
	y0 := row * cellraster.CellPxH
	b := img.Bounds()
	for dy := range cellraster.CellPxH {
		for dx := range cellraster.CellPxW {
			p := image.Point{X: x0 + dx, Y: y0 + dy}
			if p.In(b) {
				out.SetRGBA(dx, dy, img.RGBAAt(p.X, p.Y))
			}
		}
	}
	return out
}

// binarise reduces a cell tile to one bit per pixel: true when the pixel's
// Rec.601 luma sits in the brighter class of the cell's own Otsu split. The
// threshold adapts to each cell, so a light glyph on a dark cell and a dark glyph
// on a light cell both split cleanly into ink and ground; the polarity-invariant
// Match then maps either onto the white-on-black templates. A flat cell with no
// separable mass splits at the empty histogram's zero level, so all pixels read
// as the brighter class. Channel order is RGBA.
func binarise(img *image.RGBA) []bool {
	b := img.Bounds()
	gray := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
	lumas := make([]float64, 0, b.Dx()*b.Dy())
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := img.RGBAAt(x, y)
			lum := 0.299*float64(c.R) + 0.587*float64(c.G) + 0.114*float64(c.B)
			lumas = append(lumas, lum)
			gray.SetGray(x-b.Min.X, y-b.Min.Y, color.Gray{Y: clampLuma(lum)})
		}
	}
	t := float64(otsuThreshold(gray))
	bits := make([]bool, len(lumas))
	for i, lum := range lumas {
		bits[i] = lum > t
	}
	return bits
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
	for i := range 256 {
		sum += float64(i) * float64(hist[i])
	}

	var sumB, wB float64
	var maxVar float64
	var threshold int
	for i := range 256 {
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

// clampLuma rounds a luma value to its 0..255 grey level.
func clampLuma(lum float64) uint8 {
	i := int(lum + 0.5)
	if i < 0 {
		return 0
	}
	if i > 255 {
		return 255
	}
	return uint8(i)
}

// hamming counts the positions where two equal-length bit slices differ. A
// length mismatch counts the shorter length's differences plus the extra bits as
// all differing, so a malformed crop never panics.
func hamming(a, b []bool) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var d int
	for i := range n {
		if a[i] != b[i] {
			d++
		}
	}
	d += len(a) - n
	d += len(b) - n
	return d
}
