package ocr

import (
	"fmt"
	"image"
	"sort"
)

// LadderSizes are the pinned "Text Size NNpx" ladder rung sizes, 8 px to 28 px,
// matching the horizontal text ladder baked into each test card's left column
// (see docs/STAGE-0.md "Text legibility"). The minimum legible size reported per
// mode is drawn from this set.
var LadderSizes = []int{8, 10, 12, 14, 16, 18, 20, 22, 24, 28}

// LadderText returns the known ground-truth string for a ladder rung at the
// given pixel size: "Text Size NNpx". The string is baked into the fixture, so
// the OCR round-trip needs no reference image.
func LadderText(sizePx int) string {
	return fmt.Sprintf("Text Size %dpx", sizePx)
}

// Rung is one ladder line: its pixel size, the known truth string, the source
// crop holding that line in the captured render, and the source capital height
// in source pixels that drives OCR conditioning.
type Rung struct {
	SizePx       int         // the rung's nominal text size in pixels
	Truth        string      // the known ground-truth string for this rung
	Crop         image.Image // the captured-render crop holding this line
	SrcCapHeight int         // source capital height in pixels, feeds conditioning
}

// Spec describes the ladder geometry in a captured render: the left-column band
// holding the ladder and the per-rung row height. It turns a captured image into
// the per-rung crops the OCR pass reads.
type Spec struct {
	// Sizes are the rung sizes in top-to-bottom order. Defaults to LadderSizes
	// when nil.
	Sizes []int
	// Left and Width bound the ladder's horizontal band in source pixels. A zero
	// Width spans from Left to the image's right edge.
	Left, Width int
	// Top is the first rung's top edge in source pixels. RowHeight is the height
	// of each rung's row band in source pixels.
	Top, RowHeight int
	// CapHeightFor maps a rung size to its source capital height. When nil, the
	// rung size itself is used as the capital height.
	CapHeightFor func(sizePx int) int
}

// sizes returns the spec's rung sizes, falling back to LadderSizes.
func (s Spec) sizes() []int {
	if len(s.Sizes) == 0 {
		return LadderSizes
	}
	return s.Sizes
}

// capHeight returns the source capital height for a rung size.
func (s Spec) capHeight(sizePx int) int {
	if s.CapHeightFor != nil {
		return s.CapHeightFor(sizePx)
	}
	return sizePx
}

// Rungs crops src into one Rung per ladder size, using the spec's left-column
// band and per-row geometry. The crop for rung i spans the horizontal band
// [Left, Left+Width) and the vertical band [Top+i*RowHeight, Top+(i+1)*RowHeight),
// clipped to src's bounds. A zero Width spans to the right edge.
func (s Spec) Rungs(src image.Image) []Rung {
	b := src.Bounds()
	left := b.Min.X + s.Left
	right := b.Max.X
	if s.Width > 0 {
		right = left + s.Width
	}
	if right > b.Max.X {
		right = b.Max.X
	}
	szs := s.sizes()
	out := make([]Rung, 0, len(szs))
	for i, sz := range szs {
		top := b.Min.Y + s.Top + i*s.RowHeight
		bot := top + s.RowHeight
		if bot > b.Max.Y {
			bot = b.Max.Y
		}
		r := image.Rect(left, top, right, bot).Intersect(b)
		out = append(out, Rung{
			SizePx:       sz,
			Truth:        LadderText(sz),
			Crop:         subImage(src, r),
			SrcCapHeight: s.capHeight(sz),
		})
	}
	return out
}

// subImage returns the r sub-image of src when src supports SubImage, else src
// itself. The pinned captures (image.RGBA, image.Gray) both support SubImage.
func subImage(src image.Image, r image.Rectangle) image.Image {
	type subImager interface {
		SubImage(image.Rectangle) image.Image
	}
	if si, ok := src.(subImager); ok {
		return si.SubImage(r)
	}
	return src
}

// LineRecogniser OCRs one conditioned source crop and scores it against a known
// truth. The cgo Engine (tesseract.go, ocr build tag) satisfies it; tests use a
// pure-Go fake so the ladder logic stays testable without Tesseract.
type LineRecogniser interface {
	// ScoreImage conditions and OCRs src, then scores the result against truth.
	// srcCapHeight is the source capital height in source pixels.
	ScoreImage(src image.Image, srcCapHeight int, truth string) (Result, error)
}

// LadderResult is one rung's outcome: its size, the OCR Result, and whether the
// line round-tripped below the CER bound for the mode.
type LadderResult struct {
	SizePx  int    // the rung's nominal text size in pixels
	Result  Result // the CER/WER-scored OCR round-trip for this rung
	Legible bool   // true when Result.CER is at or below the CER bound
}

// Ladder runs the OCR ladder: it crops src per the spec, OCRs each rung through
// rec, scores CER and WER per line, and marks each rung legible when its CER is
// at or below cerBound. It returns one LadderResult per rung in size order.
func Ladder(src image.Image, spec Spec, rec LineRecogniser, cerBound float64) ([]LadderResult, error) {
	rungs := spec.Rungs(src)
	out := make([]LadderResult, 0, len(rungs))
	for _, r := range rungs {
		res, err := rec.ScoreImage(r.Crop, r.SrcCapHeight, r.Truth)
		if err != nil {
			return nil, fmt.Errorf("ladder rung %dpx: %w", r.SizePx, err)
		}
		out = append(out, LadderResult{
			SizePx:  r.SizePx,
			Result:  res,
			Legible: res.CER <= cerBound,
		})
	}
	return out, nil
}

// MinLegibleSize returns the smallest rung size whose line is legible (CER at or
// below the bound recorded on the result). It returns the size and true when at
// least one rung is legible, else 0 and false. It does not assume the results
// arrive sorted.
func MinLegibleSize(results []LadderResult) (int, bool) {
	best := 0
	found := false
	for _, r := range results {
		if !r.Legible {
			continue
		}
		if !found || r.SizePx < best {
			best = r.SizePx
			found = true
		}
	}
	return best, found
}

// SortBySize returns the results ordered by ascending rung size, leaving the
// input untouched. The sweep report lists rungs smallest-first.
func SortBySize(results []LadderResult) []LadderResult {
	out := append([]LadderResult(nil), results...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].SizePx < out[j].SizePx })
	return out
}
